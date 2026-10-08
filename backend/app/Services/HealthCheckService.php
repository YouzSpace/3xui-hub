<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Services\ThreeXUi\ThreeXUiClient;
use GuzzleHttp\Promise\PromiseInterface;
use GuzzleHttp\Promise\Utils;

/**
 * 节点健康检查服务（M9）。
 * healthCheck → 更新 status/latency/last_check_at。
 * maintenance 态由管理员手动设置，本服务不改写。
 */
class HealthCheckService
{
    public function __construct(
        private NodeDriverFactory $driverFactory,
        private ThreeXUiClientFactory $clientFactory,
    ) {
    }

    public function check(Node $node): array
    {
        if ($node->status === 'maintenance') {
            return ['ok' => false, 'status' => 'maintenance', 'latency' => $node->latency, 'skipped' => true];
        }

        try {
            $driver = $this->driverFactory->make($node);
            $health = $driver->healthCheck();
        } catch (\Throwable $e) {
            $this->apply($node, 'offline', 0);
            return ['ok' => false, 'status' => 'offline', 'latency' => 0, 'error' => $e->getMessage()];
        }

        $ok = (bool) $health['ok'];
        $this->apply($node, $ok ? 'online' : 'offline', (int) ($health['latencyMs'] ?? 0));

        return [
            'ok' => $ok,
            'status' => $ok ? 'online' : 'offline',
            'latency' => (int) ($health['latencyMs'] ?? 0),
        ];
    }

    /**
     * 批量健康检查：同进程内并发探测，带并发上限。
     *
     * 节点按「能否无状态并发」分两组：
     * - api_key（Bearer）节点 → 并发组：每批最多 concurrency 个，批内先全部发起 healthCheckAsync()
     *   （不阻塞），再用 GuzzleHttp\Promise\Utils::settle() 等整批出结果，然后逐个写库，再进下一批。
     * - 其余（cookie 模式 / 非 3x-ui 驱动）→ 同步组：逐个复用 check()，行为与拆分前逐字一致。
     *   cookie 模式要维护登录会话（/login + CSRF），不适合并发；非 3x-ui 驱动 check() 会抛
     *   「Unsupported node driver」并落 offline，走同步组才能保持这个既有结论。
     *
     * maintenance 节点跳过且不写库（与 check() 一致）。
     *
     * 判定规则、超时、写库字段（status / latency / last_check_at）与 check() 完全一致 ——
     * 唯一变化是 N 次串行等待变成 ceil(N/concurrency) 批并发等待，结果不变、只是更快。
     * 任何单节点异常都在内部消化（该节点记 offline），不向外抛，不牵连同批其它节点。
     *
     * @param  iterable<Node>  $nodes
     * @return array<int,array> node_id => ['ok','status','latency'(, 'skipped'|'error')]
     */
    public function checkMany(iterable $nodes): array
    {
        $concurrency = $this->concurrency();

        $results = [];
        $concurrent = [];
        $sequential = [];

        foreach ($nodes as $node) {
            if ($this->isMaintenance($node)) {
                $results[$node->id] = [
                    'ok' => false,
                    'status' => 'maintenance',
                    'latency' => $node->latency,
                    'skipped' => true,
                ];
                continue;
            }

            if ($this->canRunConcurrently($node)) {
                $concurrent[] = $node;
            } else {
                $sequential[] = $node;
            }
        }

        foreach (array_chunk($concurrent, $concurrency) as $batch) {
            $results += $this->runBatch($batch);
        }

        foreach ($sequential as $node) {
            $results[$node->id] = $this->check($node);
        }

        return $results;
    }

    /**
     * 并发跑一批节点：先把整批发出去（不阻塞），整批 settle 完再逐个写库。
     *
     * @param  list<Node>  $batch
     * @return array<int,array>
     */
    private function runBatch(array $batch): array
    {
        $promises = [];
        $results = [];

        // 整批共用一个传输层（同一个 curl_multi 事件循环）。各节点 client 若各用各的传输层，
        // settle 时只能逐个 wait，表面并发实际串行；共用后才是真并发。请求级配置（verify /
        // headers / cookie jar / base_uri）仍然各自独立，节点之间不串状态。
        $transport = ThreeXUiClient::newSharedTransport();

        foreach ($batch as $node) {
            try {
                // 每个节点一个独立 client（独立 Guzzle Client / CookieJar），无共享可变状态，可并发
                $client = $this->clientFactory->forNode($node);
                $client->useSharedTransport($transport);
                $promises[$node->id] = $client->healthCheckAsync();
            } catch (\Throwable $e) {
                // 连 client 都建不出来：该节点直接落 offline，不影响本批其它节点
                $this->apply($node, 'offline', 0);
                $results[$node->id] = [
                    'ok' => false,
                    'status' => 'offline',
                    'latency' => 0,
                    'error' => $e->getMessage(),
                ];
            }
        }

        if ($promises === []) {
            return $results;
        }

        $settled = Utils::settle($promises)->wait();

        foreach ($batch as $node) {
            if (!isset($promises[$node->id])) {
                continue; // 上面 catch 里已经处理过
            }

            $results[$node->id] = $this->applySettled($node, $settled[$node->id] ?? null);
        }

        return $results;
    }

    /**
     * 把 settle 出来的单节点结果写库，并归一化成与 check() 同构的返回。
     * healthCheckAsync() 正常情况下只会 fulfilled；rejected 分支是防御性的兜底。
     *
     * @param  ?array{state:string,value?:mixed,reason?:mixed}  $outcome
     */
    private function applySettled(Node $node, ?array $outcome): array
    {
        if ($outcome === null || ($outcome['state'] ?? null) !== PromiseInterface::FULFILLED) {
            $reason = $outcome['reason'] ?? 'health check failed';
            $this->apply($node, 'offline', 0);

            return [
                'ok' => false,
                'status' => 'offline',
                'latency' => 0,
                'error' => $reason instanceof \Throwable ? $reason->getMessage() : (string) $reason,
            ];
        }

        $health = is_array($outcome['value'] ?? null) ? $outcome['value'] : [];
        $ok = (bool) ($health['ok'] ?? false);
        $latency = (int) ($health['latencyMs'] ?? 0);

        $this->apply($node, $ok ? 'online' : 'offline', $latency);

        $result = ['ok' => $ok, 'status' => $ok ? 'online' : 'offline', 'latency' => $latency];

        // check() 只在异常分支带 error；这里把异步探测自带的失败原因带上，便于排查，字段本身不影响写库
        if (!$ok && isset($health['error'])) {
            $result['error'] = $health['error'];
        }

        return $result;
    }

    /** 并发上限：config 缺省 10；<= 0 按 1 处理（退化为「每批一个」，仍分批但不并发）。 */
    private function concurrency(): int
    {
        $concurrency = (int) config('panel.healthcheck_concurrency', 10);

        return $concurrency > 0 ? $concurrency : 1;
    }

    /** 读属性本身也可能抛（如 api_key 密文损坏），一律当作非 maintenance，交给后续分支兜底。 */
    private function isMaintenance(Node $node): bool
    {
        try {
            return $node->status === 'maintenance';
        } catch (\Throwable) {
            return false;
        }
    }

    /**
     * 能否走并发组：3x-ui 驱动 且 api_key 非空（Bearer 无状态）。
     * 判定本身若抛异常（属性读取失败），退回同步组，由 check() 的 try/catch 统一落 offline。
     */
    private function canRunConcurrently(Node $node): bool
    {
        try {
            return ($node->driver_type ?? '3x-ui') === '3x-ui'
                && (string) ($node->api_key ?? '') !== '';
        } catch (\Throwable) {
            return false;
        }
    }

    private function apply(Node $node, string $status, int $latency): void
    {
        $fields = [
            'status' => $status,
            'latency' => $latency,
        ];

        // xray 节点的 last_check_at 是 agent 心跳时间（/node-api/alive 维护）；
        // 健康检查只翻 status，绝不覆盖心跳时间，否则「心跳新鲜度」判定会失真。
        if (! $node->isXray()) {
            $fields['last_check_at'] = now();
        }

        $node->forceFill($fields)->save();
    }
}
