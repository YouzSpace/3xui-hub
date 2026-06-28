<?php

namespace App\Jobs;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\TrafficSyncService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 单节点流量同步（批量优化版）。
 * 一次 listInbounds() 拉取全节点 client 流量，内存匹配后批量写入。
 */
class SyncNodeTrafficJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function __construct(public int $nodeId)
    {
    }

    public function handle(
        NodeDriverFactory $driverFactory,
        TrafficSyncService $sync,
        BanService $banService,
    ): void {
        /** @var Node|null $node */
        $node = Node::find($this->nodeId);
        if (!$node || !$node->enabled) {
            return;
        }

        $driver = $driverFactory->make($node);

        // 1. 一次HTTP拉取全节点所有 inbound 的 client 流量
        try {
            $statsByInbound = $driver->getClientStatsGroupedByInbound();
        } catch (\Throwable $e) {
            report($e);
            return;
        }

        // 2. 合并所有 inbound 的流量（同用户跨 inbound 累加）
        $mergedStats = [];
        foreach ($statsByInbound as $inboundId => $emailStats) {
            foreach ($emailStats as $email => $stat) {
                if (!isset($mergedStats[$email])) {
                    $mergedStats[$email] = ['up' => 0, 'down' => 0];
                }
                $mergedStats[$email]['up'] += $stat['up'];
                $mergedStats[$email]['down'] += $stat['down'];
            }
        }

        // 3. 批量同步（增量计算）
        $result = $sync->syncNodeBatch($node, $mergedStats);

        // 4. 批量写入快照
        $sync->upsertSnapshots($result['snapshotData']);

        // 5. 批量更新用户流量
        $sync->applyDeltas($result['deltaMap']);

        // 6. Ban检查（仅检查有流量变化的用户）
        if (!empty($result['deltaMap'])) {
            $users = User::whereIn('id', array_keys($result['deltaMap']))->with('plan')->get();
            foreach ($users as $user) {
                $fresh = $user->fresh();
                $fresh->load('plan');
                $banService->checkAfterSync($fresh);
            }
        }
    }
}
