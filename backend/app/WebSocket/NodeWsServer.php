<?php

namespace App\WebSocket;

use App\Models\Node;
use App\Models\NodeApiCommand;
use App\Models\SiteConfig;
use App\Services\NodeApiCommandService;
use App\Services\TrafficSyncService;
use Illuminate\Support\Carbon;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Log;
use Workerman\Connection\TcpConnection;
use Workerman\Timer;
use Workerman\Worker;

/**
 * xray 节点 WebSocket 长连接服务（Workerman 常驻，systemd 前台运行）。
 *
 * 定位：面板侧「主通道」。节点 agent 主动出连、鉴权后保持长连接；
 * 面板顺着连接主动推指令（commands）/ 配置变更通知（config.changed），
 * 节点顺路上报流量 + 监控指标（report）与指令回执（ack）。
 *
 * 可靠性设计：
 *   - 指令推送前先原子认领（pending→acknowledged），与 HTTP 长轮询共用一套认领逻辑，
 *     双通道并发也不会重复下发；
 *   - 推送到 agent 的流量/指标与 HTTP /push /alive 走完全相同的落账实现
 *     （TrafficSyncService / driver_config.agent 口径），两条通道永远一致；
 *   - 僵死连接：75s 未收到任何消息（含 pong）即断开，agent 侧会自动重连；
 *   - 所有回调 try-catch 包裹 + DB 断线自动重连：单条消息出错绝不影响服务进程。
 *
 * 消息协议（应用层 JSON：{"event": ..., "data": {...}}）：
 *   上行 hello / report / ack / pong
 *   下行 hello.ok / commands / config.changed / ping
 */
class NodeWsServer
{
    /** 心跳：每 30s 向所有连接发 ping。 */
    private const PING_INTERVAL = 30;

    /** 僵死判定：75s 无任何消息（> 2×心跳周期）即断开。 */
    private const STALE_SECONDS = 75;

    /** 轮询节拍（秒）：pending 指令 + config 版本变更检查。 */
    private const POLL_INTERVAL = 2;

    /** 未鉴权连接存活上限（秒）。 */
    private const AUTH_TIMEOUT = 10;

    /** 单次推送的指令条数上限（与 HTTP execute 一致）。 */
    private const MAX_COMMANDS_PER_PUSH = 5;

    /** 单次上报的流量用户数上限（与 HTTP /push 的 max 校验一致）。 */
    private const MAX_STATS_PER_REPORT = 2000;

    /** 超限告警的节流间隔（秒）：agent 每 5s 推一轮，不节流会把日志刷爆。 */
    private const OVERFLOW_LOG_INTERVAL = 60;

    private Worker $worker;

    public function __construct(string $listen)
    {
        $this->worker = new Worker("websocket://{$listen}");
        $this->worker->count = 1;
        $this->worker->name = 'controlhub-node-ws';
    }

    public function run(): void
    {
        Worker::$logFile = storage_path('logs/node-ws.log');
        Worker::$pidFile = storage_path('logs/controlhub-node-ws.pid');

        // Workerman v5 按参数里的命令词（start/stop/restart/...）决定行为：本命令经
        // `php artisan node:ws-serve` 启动时 argv 里没有任何命令词（artisan 签名也不接受
        // 多余参数），parseCommand 会直接打 usage 退出。Worker::$command 的内容会被
        // getArgv() 追加进命令词扫描，这里固定为 start，保证 systemd 与本地开发用法一致。
        Worker::$command = 'start';

        $this->worker->onWorkerStart = [$this, 'onWorkerStart'];
        $this->worker->onConnect = [$this, 'onConnect'];
        $this->worker->onWebSocketConnect = [$this, 'onWebSocketConnect'];
        $this->worker->onMessage = [$this, 'onMessage'];
        $this->worker->onClose = [$this, 'onClose'];

        Worker::runAll();
    }

    // ── Workerman 回调 ───────────────────────────────────────

    public function onWorkerStart(Worker $worker): void
    {
        Log::info('[node-ws] 服务启动');

        // 指令 + 配置变更轮询（2s 节拍：节点正连着才查，零查询开销）
        Timer::add(self::POLL_INTERVAL, function (): void {
            $this->pollAndPush();
        });

        // 心跳与僵死清理
        Timer::add(self::PING_INTERVAL, function (): void {
            $this->heartbeat();
        });
    }

    public function onConnect(TcpConnection $conn): void
    {
        $conn->context->nodeId = 0;
        $conn->context->lastSeen = time();
        $conn->context->notifiedVersion = 0;

        // 鉴权超时：10s 内没完成 hello（鉴权在握手时已做），断开防挂连接
        Timer::add(self::AUTH_TIMEOUT, function () use ($conn): void {
            if (($conn->context->nodeId ?? 0) <= 0 && $conn->getStatus() === TcpConnection::STATUS_ESTABLISHED) {
                $conn->close();
            }
        }, [], false);
    }

    /**
     * WS 握手完成：用 X-Node-Secret 头鉴权（与 HTTP 端点同一套密钥口径）。
     * 鉴权失败立即断开；成功则注册连接、记录节点在线。
     */
    public function onWebSocketConnect(TcpConnection $conn, $httpMessage): void
    {
        try {
            $secret = $this->extractHeader($httpMessage, 'x-node-secret');
            if ($secret === '') {
                $this->closeWith($conn, 'missing node secret');
                return;
            }

            $node = $this->findNodeBySecret($secret);
            if ($node === null) {
                $this->closeWith($conn, 'invalid node secret');
                return;
            }

            $conn->context->nodeId = $node->id;
            $conn->context->lastSeen = time();
            $conn->context->notifiedVersion = $node->xrayConfigVersion();

            NodeRegistry::add($node->id, $conn);

            Log::info("[node-ws] node#{$node->id}({$node->name}) 已连接", [
                'remote' => $conn->getRemoteIp(),
                'total' => NodeRegistry::count(),
            ]);
        } catch (\Throwable $e) {
            Log::error('[node-ws] 握手鉴权失败：' . $e->getMessage());
            try {
                $conn->close();
            } catch (\Throwable) {
            }
        }
    }

    public function onMessage(TcpConnection $conn, $data): void
    {
        $nodeId = (int) ($conn->context->nodeId ?? 0);
        if ($nodeId <= 0) {
            $conn->close();
            return;
        }
        $conn->context->lastSeen = time();

        $msg = json_decode((string) $data, true);
        if (! is_array($msg)) {
            return;
        }
        $event = (string) ($msg['event'] ?? '');
        $payload = is_array($msg['data'] ?? null) ? $msg['data'] : [];

        try {
            match ($event) {
                'hello' => $this->handleHello($conn, $nodeId, $payload),
                'report' => $this->handleReport($conn, $nodeId, $payload),
                'ack' => $this->handleAck($nodeId, $payload),
                'pong' => null, // lastSeen 已刷新
                default => null,
            };
        } catch (\Throwable $e) {
            Log::error("[node-ws] 处理 {$event} 失败：{$e->getMessage()}", ['node_id' => $nodeId]);
            $this->reconnectDb();
        }
    }

    public function onClose(TcpConnection $conn): void
    {
        $nodeId = (int) ($conn->context->nodeId ?? 0);
        if ($nodeId <= 0) {
            return;
        }

        NodeRegistry::remove($nodeId, $conn);

        try {
            $node = Node::find($nodeId);
            if ($node !== null) {
                $cfg = $node->driver_config ?? [];
                $cfg['ws'] = ['connected' => false, 'at' => now()->toIso8601String()];
                $node->forceFill(['driver_config' => $cfg])->save();
            }
        } catch (\Throwable $e) {
            Log::warning("[node-ws] node#{$nodeId} 断线清理失败：{$e->getMessage()}");
        }

        Log::info("[node-ws] node#{$nodeId} 断开", ['total' => NodeRegistry::count()]);
    }

    // ── 上行消息处理 ─────────────────────────────────────────

    /** hello：注册元数据（版本/主机信息），回 hello.ok（含当前 config 版本）。 */
    private function handleHello(TcpConnection $conn, int $nodeId, array $data): void
    {
        $node = Node::find($nodeId);
        if ($node === null) {
            $conn->close();
            return;
        }

        $cfg = $node->driver_config ?? [];
        if (! empty($data['xray_version'])) {
            $cfg['xray_version'] = (string) $data['xray_version'];
        }
        if (! empty($data['agent_version'])) {
            $cfg['agent_version'] = (string) $data['agent_version'];
        }
        if (! empty($data['hostname'])) {
            $cfg['hostname'] = (string) $data['hostname'];
        }
        if (! empty($data['kernel_arch'])) {
            $cfg['kernel_arch'] = (string) $data['kernel_arch'];
        }
        $cfg['ws'] = ['connected' => true, 'at' => now()->toIso8601String()];

        $node->forceFill([
            'driver_config' => $cfg,
            'status' => 'online',
            'last_check_at' => now(),
        ])->save();

        $conn->context->notifiedVersion = max(0, (int) ($data['config_version'] ?? 0));

        $this->send($conn, 'hello.ok', [
            'node_id' => $node->id,
            'config_version' => $node->xrayConfigVersion(),
            'pull_interval' => $this->pullInterval(),
        ]);
    }

    /**
     * report：流量绝对值 + 出站流量 + 监控指标 + config 版本回执。
     * 流量走 TrafficSyncService（与 HTTP /push 完全相同的增量结算），
     * 指标写 driver_config.agent（与 HTTP /alive 完全相同的字段口径）。
     */
    private function handleReport(TcpConnection $conn, int $nodeId, array $data): void
    {
        $node = Node::find($nodeId);
        if ($node === null) {
            $conn->close();
            return;
        }

        // 1) 用户流量落账
        $stats = $data['stats'] ?? null;
        if (is_array($stats) && $stats !== []) {
            if (count($stats) > self::MAX_STATS_PER_REPORT) {
                // 超限整包丢弃是既有的防护（避免一条消息把进程内存吃爆），但绝不能静默：
                // 用户数超过上限后该节点流量完全不落账，没有日志就无从察觉。
                // agent 每 5s 推一轮，这里按连接节流，避免刷爆日志。
                $lastLoggedAt = (int) ($conn->context->statsOverflowLoggedAt ?? 0);
                if (time() - $lastLoggedAt >= self::OVERFLOW_LOG_INTERVAL) {
                    Log::error(
                        '[node-ws] node#' . $nodeId . ' 上报流量 ' . count($stats) . ' 条，超过单次上限 '
                        . self::MAX_STATS_PER_REPORT . '，整包已丢弃：该节点流量不会落账（需改为只上报有变化的用户）'
                    );
                    $conn->context->statsOverflowLoggedAt = time();
                }
            } else {
                $clean = [];
                foreach ($stats as $email => $pair) {
                    if (! is_string($email) || ! is_array($pair)) {
                        continue;
                    }
                    $clean[$email] = [
                        'up' => (int) ($pair['up'] ?? 0),
                        'down' => (int) ($pair['down'] ?? 0),
                    ];
                }
                if ($clean !== []) {
                    app(TrafficSyncService::class)->syncNodeFromSource($node, fn () => $clean);
                }
            }
        }

        // 2) 出站流量（出站管理功能的数据源）
        $outbounds = $data['outbounds'] ?? null;
        if (is_array($outbounds) && $outbounds !== []) {
            $this->storeOutboundTraffic($node->id, $outbounds);
        }

        // 3) 监控指标 + 心跳
        $cfg = $node->driver_config ?? [];
        $metrics = $data['metrics'] ?? null;
        if (is_array($metrics)) {
            $cfg['agent'] = [
                'cpu' => isset($metrics['cpu']) ? (float) $metrics['cpu'] : null,
                'mem' => isset($metrics['mem']) ? (float) $metrics['mem'] : null,
                'uptime' => isset($metrics['uptime']) ? (int) $metrics['uptime'] : null,
                'conns' => isset($metrics['conns']) ? (int) $metrics['conns'] : null,
                'active_users' => isset($metrics['active_users']) ? (int) $metrics['active_users'] : null,
                'up_speed' => isset($metrics['up_speed']) ? (float) $metrics['up_speed'] : null,
                'down_speed' => isset($metrics['down_speed']) ? (float) $metrics['down_speed'] : null,
                'at' => now()->toIso8601String(),
            ];
        }

        // 4) config 版本回执 → 滞后计算（与 /alive 同口径）
        $current = $node->xrayConfigVersion();
        $reported = (int) ($data['config_version'] ?? 0);
        if ($reported < $current) {
            $changedAt = $cfg['config_changed_at'] ?? null;
            $cfg['lag_seconds'] = $changedAt
                ? max(0, (int) Carbon::parse($changedAt)->diffInSeconds(now()))
                : 0;
        } else {
            $cfg['lag_seconds'] = 0;
            if ($reported > (int) $conn->context->notifiedVersion) {
                $conn->context->notifiedVersion = $reported;
            }
        }
        $cfg['ws'] = ['connected' => true, 'at' => now()->toIso8601String()];

        $node->forceFill([
            'driver_config' => $cfg,
            'status' => 'online',
            'last_check_at' => now(),
        ])->save();
    }

    /** ack：指令回执（与 HTTP /ack 共用 applyAck 实现）。 */
    private function handleAck(int $nodeId, array $data): void
    {
        $node = Node::find($nodeId);
        if ($node === null) {
            return;
        }
        $results = $data['results'] ?? null;
        if (! is_array($results) || $results === []) {
            return;
        }
        app(NodeApiCommandService::class)->applyAck($node, array_slice($results, 0, 50));
    }

    // ── 下行推送（Timer 轮询） ────────────────────────────────

    /**
     * 2s 节拍：给已连接节点推 pending 指令 + config 变更通知。
     * 只查「当前有连接」的节点，节点数少时查询为零成本。
     */
    private function pollAndPush(): void
    {
        $nodeIds = NodeRegistry::connectedNodeIds();
        if ($nodeIds === []) {
            return;
        }

        try {
            // 1) 指令推送（先原子认领，防止与 HTTP 长轮询重复下发）
            $commands = NodeApiCommand::whereIn('node_id', $nodeIds)
                ->where('status', NodeApiCommand::STATUS_PENDING)
                ->orderBy('id')
                ->limit(100)
                ->get()
                ->groupBy('node_id');

            foreach ($commands as $nodeId => $list) {
                $conn = NodeRegistry::get((int) $nodeId);
                if ($conn === null) {
                    continue;
                }

                $claimed = [];
                foreach ($list as $command) {
                    if (! $command->acknowledge()) {
                        continue; // 被 HTTP 长轮询抢走
                    }
                    $claimed[] = [
                        'id' => $command->id,
                        'action' => $command->action,
                        'payload' => $command->payload,
                    ];
                    if (count($claimed) >= self::MAX_COMMANDS_PER_PUSH) {
                        break;
                    }
                }

                if ($claimed !== []) {
                    $this->send($conn, 'commands', ['commands' => $claimed]);
                    Log::info("[node-ws] 推送 {$nodeId} 指令 " . count($claimed) . ' 条');
                }
            }

            // 2) config 版本变更通知（node 收到后立即走 HTTP 全量拉取）
            $nodes = Node::whereIn('id', $nodeIds)->get(['id', 'driver_config']);
            foreach ($nodes as $node) {
                $conn = NodeRegistry::get($node->id);
                if ($conn === null) {
                    continue;
                }
                $current = $node->xrayConfigVersion();
                if ($current > (int) ($conn->context->notifiedVersion ?? 0)) {
                    $this->send($conn, 'config.changed', ['config_version' => $current]);
                    $conn->context->notifiedVersion = $current;
                    Log::info("[node-ws] 通知 node#{$node->id} 配置变更 → v{$current}");
                }
            }
        } catch (\Throwable $e) {
            Log::error('[node-ws] 轮询推送失败：' . $e->getMessage());
            $this->reconnectDb();
        }
    }

    /** 心跳：僵死连接断开；存活连接发 ping。 */
    private function heartbeat(): void
    {
        try {
            $now = time();
            foreach (NodeRegistry::all() as $nodeId => $conn) {
                if ($now - (int) ($conn->context->lastSeen ?? 0) > self::STALE_SECONDS) {
                    Log::warning("[node-ws] node#{$nodeId} 存活超时，断开连接");
                    $conn->close();
                    continue;
                }
                $this->send($conn, 'ping', null);
            }
        } catch (\Throwable $e) {
            Log::error('[node-ws] 心跳失败：' . $e->getMessage());
        }
    }

    // ── 工具 ─────────────────────────────────────────────────

    private function send(TcpConnection $conn, string $event, ?array $data): void
    {
        $msg = ['event' => $event];
        if ($data !== null) {
            $msg['data'] = $data;
        }
        $json = json_encode($msg, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
        if ($json === false) {
            return;
        }
        try {
            $conn->send($json);
        } catch (\Throwable) {
        }
    }

    private function closeWith(TcpConnection $conn, string $reason): void
    {
        try {
            $conn->close(json_encode(['event' => 'error', 'data' => ['message' => $reason]]));
        } catch (\Throwable) {
            try {
                $conn->close();
            } catch (\Throwable) {
            }
        }
    }

    /** 从握手请求取 header（兼容 Workerman Request 对象与原始字符串两种形态）。 */
    private function extractHeader($httpMessage, string $name): string
    {
        if ($httpMessage instanceof \Workerman\Protocols\Http\Request) {
            return trim((string) ($httpMessage->header($name) ?? ''));
        }
        if (is_string($httpMessage)) {
            if (preg_match('/^' . preg_quote($name, '/') . ':\s*(.+)$/im', $httpMessage, $m)) {
                return trim($m[1]);
            }
        }

        return '';
    }

    /** 按节点密钥找节点（与 NodeAuth 中间件同口径）。 */
    private function findNodeBySecret(string $secret): ?Node
    {
        foreach (Node::where('driver_type', 'xray')->get() as $node) {
            $candidate = $node->nodeSecret();
            if (is_string($candidate) && $candidate !== '' && hash_equals($candidate, $secret)) {
                return $node;
            }
        }

        return null;
    }

    /** 配置拉取周期（秒）：admin 设置项 xray_pull_interval，缺省 30。 */
    private function pullInterval(): int
    {
        $value = (int) SiteConfig::getValue('xray_pull_interval', '30');

        return $value >= 5 ? $value : 30;
    }

    /**
     * 出站流量落表（S4 出站管理的数据源；表未建时静默跳过）。
     *
     * @param array<string, array{up?: int, down?: int}> $outbounds
     */
    private function storeOutboundTraffic(int $nodeId, array $outbounds): void
    {
        if (! \Illuminate\Support\Facades\Schema::hasTable('xray_outbound_traffic')) {
            return;
        }
        $rows = [];
        $now = now();
        foreach ($outbounds as $tag => $pair) {
            if (! is_string($tag) || ! is_array($pair)) {
                continue;
            }
            $rows[] = [
                'node_id' => $nodeId,
                'tag' => mb_substr($tag, 0, 128),
                'upload' => (int) ($pair['up'] ?? 0),
                'download' => (int) ($pair['down'] ?? 0),
                'updated_at' => $now,
            ];
        }
        if ($rows === []) {
            return;
        }
        DB::table('xray_outbound_traffic')->upsert(
            $rows,
            ['node_id', 'tag'],
            ['upload', 'download', 'updated_at']
        );
    }

    /** DB 断线重连（长驻进程兜底）。 */
    private function reconnectDb(): void
    {
        try {
            DB::reconnect();
        } catch (\Throwable) {
        }
    }
}
