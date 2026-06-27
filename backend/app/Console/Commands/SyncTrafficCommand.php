<?php

namespace App\Console\Commands;

use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\ThreeXUiClientFactory;
use App\Services\TrafficSyncService;
use GuzzleHttp\Promise\Utils;
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

    /** 每批并行请求数 */
    private const BATCH_SIZE = 50;

    public function handle(
        ThreeXUiClientFactory $factory,
        TrafficSyncService $syncService,
        BanService $banService,
    ): int {
        $this->info('[' . now()->format('Y-m-d H:i:s') . '] 开始流量同步...');

        $nodes = Node::where('enabled', true)->get();
        if ($nodes->isEmpty()) {
            $this->info('无启用节点');
            return self::SUCCESS;
        }

        // 1. 并行拉取所有节点流量
        $allStats = $this->fetchNodesParallel($factory, $nodes);

        // 2. 按节点批量同步
        $totalSynced = 0;
        $allDeltaUserIds = [];

        foreach ($nodes as $node) {
            $mergedStats = $allStats[$node->id] ?? [];
            if (empty($mergedStats)) continue;

            $result = $syncService->syncNodeBatch($node, $mergedStats);
            $syncService->upsertSnapshots($result['snapshotData']);
            $syncService->applyDeltas($result['deltaMap']);

            $totalSynced += count($result['deltaMap']);
            $allDeltaUserIds = array_merge($allDeltaUserIds, array_keys($result['deltaMap']));

            $this->line("  节点 {$node->name}: " . count($mergedStats) . " 用户, " . count($result['deltaMap']) . " 有增量");
        }

        // 3. Ban检查（仅检查有流量变化的用户）
        $banned = 0;
        if (!empty($allDeltaUserIds)) {
            $users = User::whereIn('id', array_unique($allDeltaUserIds))->with('plan')->get();
            foreach ($users as $user) {
                $fresh = $user->fresh();
                $fresh->load('plan');
                $reason = $banService->banReason($fresh);
                if ($reason !== false && $fresh->enabled) {
                    $banService->toggleClient($fresh, false);
                    $banned++;
                    $this->line("  关闭流量 #{$fresh->id} ({$fresh->email}): {$reason}");
                }
            }
        }

        $this->info("同步完成: {$totalSynced} 用户有增量, {$banned} 关停");
        return self::SUCCESS;
    }

    /**
     * 并行拉取所有节点流量数据。
     * 每节点调1次 getClientStatsGroupedByInbound()，分批并行。
     *
     * @return array [nodeId => [clientEmail => ['up'=>int, 'down'=>int], ...], ...]
     */
    private function fetchNodesParallel(ThreeXUiClientFactory $factory, $nodes): array
    {
        $allStats = [];

        // 分批并行
        $chunks = $nodes->chunk(self::BATCH_SIZE);
        foreach ($chunks as $chunk) {
            $promises = [];
            foreach ($chunk as $node) {
                $client = $factory->forNode($node);
                $promises[$node->id] = function () use ($client) {
                    return $client->getClientStatsGroupedByInbound();
                };
            }

            // 并行执行这批请求
            try {
                $results = Utils::unwrap($promises);
            } catch (\Throwable $e) {
                // 并行失败时降级为串行
                $results = [];
                foreach ($chunk as $node) {
                    try {
                        $client = $factory->forNode($node);
                        $results[$node->id] = $client->getClientStatsGroupedByInbound();
                    } catch (\Throwable) {
                        $results[$node->id] = [];
                    }
                }
            }

            // 合并每个节点的 inbound 流量
            foreach ($results as $nodeId => $statsByInbound) {
                $merged = [];
                foreach ($statsByInbound ?? [] as $emailStats) {
                    foreach ($emailStats as $email => $stat) {
                        if (!isset($merged[$email])) {
                            $merged[$email] = ['up' => 0, 'down' => 0];
                        }
                        $merged[$email]['up'] += $stat['up'];
                        $merged[$email]['down'] += $stat['down'];
                    }
                }
                $allStats[$nodeId] = $merged;
            }
        }

        return $allStats;
    }
}
