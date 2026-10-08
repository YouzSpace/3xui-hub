<?php

namespace App\Console\Commands;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\TrafficSyncService;
use Illuminate\Console\Command;

/**
 * 流量自动同步命令（批量优化版）。
 * 每个节点1次HTTP拉取全量流量，并行请求后批量写入。
 *
 * 运行方式：php artisan traffic:sync
 * 定时调度：routes/console.php 每5分钟
 */
class SyncTrafficCommand extends Command
{
    protected $signature = 'traffic:sync';
    protected $description = '自动同步所有用户流量并关停超限用户';

    public function handle(
        NodeDriverFactory $driverFactory,
        TrafficSyncService $syncService,
        BanService $banService,
    ): int {
        $this->info('[' . now()->format('Y-m-d H:i:s') . '] 开始流量同步...');

        $nodes = Node::where('enabled', true)->get();
        if ($nodes->isEmpty()) {
            $this->info('无启用节点');
            return self::SUCCESS;
        }

        $totalSynced = 0;
        $allDeltaUserIds = [];

        foreach ($nodes as $node) {
            // xray 节点流量由 agent 主动推送（/node-api/push），不做面板侧轮询
            if ($node->isXray()) {
                continue;
            }

            try {
                $result = $syncService->syncNodeFromSource($node, function () use ($driverFactory, $node) {
                    $driver = $driverFactory->make($node);

                    // 数据层已按 email 去重（同 email 跨入站只取首值，避免 N 倍计费）
                    return $driver->getClientStatsByEmail();
                });
            } catch (\Throwable) {
                continue;
            }
            if (!$result['acquired']) continue;

            $totalSynced += count($result['deltaMap']);
            $allDeltaUserIds = array_merge($allDeltaUserIds, array_keys($result['deltaMap']));

            $this->line("  节点 {$node->name}: " . count($result['snapshotData']) . " 用户, " . count($result['deltaMap']) . " 有增量");
        }

        // 3. Ban检查（仅检查有流量变化的用户）
        $banned = 0;
        if (!empty($allDeltaUserIds)) {
            $users = User::whereIn('id', array_unique($allDeltaUserIds))->with('plan')->get();
            foreach ($users as $user) {
                $fresh = $user->fresh();
                $fresh->load('plan');
                $reason = $banService->banReason($fresh);
                if ($reason === false || !$fresh->enabled) {
                    continue;
                }

                // 已确认关闭且未超时效 → 跳过（0 次 HTTP）。
                // 同时也消掉了「同一轮里既被本命令关、又被 BanCheckJob 关」的重复关闭。
                if ($banService->isRecentlyDisabled($fresh)) {
                    continue;
                }

                $banService->toggleClient($fresh, false);
                $banned++;
                $this->line("  关闭流量 #{$fresh->id} ({$fresh->email}): {$reason}");
            }
        }

        $this->info("同步完成: {$totalSynced} 条用户节点记录有增量, {$banned} 关停");
        return self::SUCCESS;
    }
}
