<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Models\AsyncTask;
use App\Models\Node;
use App\Models\User;

/**
 * 删除节点后的远端托管客户端清理（异步）。
 *
 * 背景：旧实现在 DELETE /admin/nodes/{id} 的请求里先 healthCheck 再逐用户 deleteClient，
 * 100 用户 = 100+ 次串行 HTTPS，节点慢就必然前端超时。现在节点行照旧立刻删，
 * 远端清理派发成一个后台任务。
 *
 * 粒度：**一台节点一个 Job（Job 内部循环用户）**，而不是按用户拆 item。理由：
 * - 节点行已经删了，Job 只能靠派发时带的连接快照干活。按用户拆 = 快照（含凭据密文）
 *   在 async_tasks.meta / jobs 里复制 N 份，收益只是重试粒度更细；
 * - 清理是「把这台机器上属于我们的东西擦干净」的一次性批处理，单个用户失败没有
 *   单独的补偿动作，重跑整台节点的代价也只是重复几次 delete（删除是幂等的）。
 * 结果仍然完全可见：任务成功/失败 + 失败摘要都落在 AsyncTask（日志页可看、可整任务重试），
 * 节点不可达、列表拉不下来、部分用户删不掉，各自有明确文案，不会静默。
 *
 * 凭据落库：meta 里存的是 Node 的 **原始属性**（password/api_key 仍是 Crypt 密文），
 * 重建模型时按正常解密路径读回，纯文本既不进 async_tasks 也不进 jobs。
 */
class NodeCleanupService
{
    /** 派发任务时要带上的连接相关字段（其余属性与本任务无关，不落库）。 */
    private const SNAPSHOT_FIELDS = [
        'id', 'name', 'host', 'port', 'scheme', 'web_base_path',
        'username', 'password', 'api_key', 'verify_ssl', 'driver_type',
    ];

    public function __construct(
        private AsyncTaskService $tasks,
        private NodeDriverFactory $driverFactory,
    ) {
    }

    /**
     * 派发「清理该节点远端客户端」任务。
     * 必须在 $node->delete() 之前调用 —— 快照取自尚在的节点行。
     */
    public function submit(Node $node): AsyncTask
    {
        $task = $this->tasks->create(
            'node_action',
            'node',
            $node->id,
            1,
            [
                'action' => 'cleanup_clients',
                'node_name' => $node->name,
                'node_snapshot' => array_intersect_key($node->getRawOriginal(), array_flip(self::SNAPSHOT_FIELDS)),
            ],
            3,
            ['node:' . $node->id],
        );

        $this->tasks->dispatchAfterCommit($task);

        return $task;
    }

    /**
     * 在【单个节点】上清理全部托管 client —— 清理 Job 的唯一入口。
     *
     * @param array $snapshot 节点原始属性（密文），空数组表示任务元数据缺失
     * @return array{ok: bool, summary: string} ok=false 时 summary 是要写进任务失败原因的文案
     */
    public function cleanupNode(array $snapshot): array
    {
        if ($snapshot === []) {
            return ['ok' => false, 'summary' => '缺少节点连接信息，无法清理远端客户端'];
        }

        // xray 节点：客户端只存在于节点内核，删除节点后 agent 随之失联（密钥鉴权失效），
        // 面板侧无下发清理的通道 —— 远端内核随 VPS 一并处置，这里直接判完成。
        if (($snapshot['driver_type'] ?? '3x-ui') === 'xray') {
            return ['ok' => true, 'summary' => 'xray 节点：无面板式远端客户端，无需清理'];
        }

        try {
            $node = (new Node())->setRawAttributes($snapshot);
            $driver = $this->driverFactory->make($node);
        } catch (\Throwable $e) {
            // 典型的解密失败（APP_KEY 换过）—— 快照读不出明文凭据
            return ['ok' => false, 'summary' => '节点连接信息无法读取，远端客户端未清理：' . $e->getMessage()];
        }

        // 可达性由清理任务自己判（请求里那步 healthCheck 已去掉）
        $health = $driver->healthCheck();
        if (($health['ok'] ?? false) !== true) {
            return ['ok' => false, 'summary' => '节点不可达，远端客户端未清理：' . ($health['error'] ?? 'unknown')];
        }

        try {
            $existing = $driver->listClients();
        } catch (\Throwable $e) {
            return ['ok' => false, 'summary' => '拉取远端客户端列表失败，未做清理：' . $e->getMessage()];
        }

        // 只删「本地有记录 且 远端确实存在」的 client：不存在的本来就不用删，
        // 逐条 delete 会因为 3x-ui 的 record not found 报错，把一次干净清理染成失败。
        $onPanel = [];
        foreach ($existing as $client) {
            $email = $client['email'] ?? null;
            if (is_string($email) && $email !== '') {
                $onPanel[$email] = true;
            }
        }

        $targets = User::query()
            ->get()
            ->map(fn (User $user) => $user->clientEmail())
            ->filter(fn (string $email) => isset($onPanel[$email]))
            ->values();

        $deleted = 0;
        $failures = [];
        foreach ($targets as $email) {
            try {
                $driver->deleteClient($email, false);
                $deleted++;
            } catch (\Throwable $e) {
                $failures[] = $email . '：' . $e->getMessage();
            }
        }

        if ($failures !== []) {
            return [
                'ok' => false,
                'summary' => sprintf(
                    '远端客户端清理未完成：成功 %d 个 / 失败 %d 个（%s）',
                    $deleted,
                    count($failures),
                    implode('；', array_slice($failures, 0, 3))
                ),
            ];
        }

        return ['ok' => true, 'summary' => "已清理远端客户端 {$deleted} 个"];
    }
}
