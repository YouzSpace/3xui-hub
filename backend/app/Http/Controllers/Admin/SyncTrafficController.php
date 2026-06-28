<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\NodeDriverFactory;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\TrafficSyncService;
use App\Traits\ApiResponse;
use GuzzleHttp\Promise\Utils;

/**
 * 流量同步接口（批量优化版）：管理员调用，批量同步所有用户流量。
 * POST /admin-api/sync-traffic
 */
class SyncTrafficController extends Controller
{
    use ApiResponse;

    /** 每批并行请求数 */
    private const BATCH_SIZE = 50;

    public function __construct(
        private NodeDriverFactory $driverFactory,
        private TrafficSyncService $syncService,
        private BanService $banService,
    ) {}

    public function sync(): \Illuminate\Http\JsonResponse
    {
        $nodes = Node::where('enabled', true)->get();
        if ($nodes->isEmpty()) {
            return $this->success(['synced' => 0, 'banned' => 0], '无启用节点');
        }

        // 1. 并行拉取所有节点流量
        $allStats = $this->fetchNodesParallel($nodes);

        // 2. 按节点批量同步
        $totalSynced = 0;
        $allDeltaUserIds = [];

        foreach ($nodes as $node) {
            $mergedStats = $allStats[$node->id] ?? [];
            if (empty($mergedStats)) continue;

            $result = $this->syncService->syncNodeBatch($node, $mergedStats);
            $this->syncService->upsertSnapshots($result['snapshotData']);
            $this->syncService->applyDeltas($result['deltaMap']);

            $totalSynced += count($result['deltaMap']);
            $allDeltaUserIds = array_merge($allDeltaUserIds, array_keys($result['deltaMap']));
        }

        // 3. Ban检查（仅检查有流量变化的用户）
        $banned = 0;
        if (!empty($allDeltaUserIds)) {
            $users = User::whereIn('id', array_unique($allDeltaUserIds))->with('plan')->get();
            foreach ($users as $user) {
                $fresh = $user->fresh();
                $fresh->load('plan');
                $reason = $this->banService->banReason($fresh);
                if ($reason !== false && $fresh->enabled) {
                    $this->banService->toggleClient($fresh, false);
                    $banned++;
                }
            }
        }

        return $this->success([
            'synced' => $totalSynced,
            'banned' => $banned,
        ], "同步完成：{$totalSynced} 用户有增量, {$banned} 关停");
    }

    /**
     * 并行拉取所有节点流量数据。
     */
    private function fetchNodesParallel($nodes): array
    {
        $allStats = [];

        $chunks = $nodes->chunk(self::BATCH_SIZE);
        foreach ($chunks as $chunk) {
            $promises = [];
            foreach ($chunk as $node) {
                $driver = $this->driverFactory->make($node);
                $promises[$node->id] = function () use ($driver) {
                    return $driver->getClientStatsGroupedByInbound();
                };
            }

            try {
                $results = Utils::unwrap($promises);
            } catch (\Throwable $e) {
                $results = [];
                foreach ($chunk as $node) {
                    try {
                        $driver = $this->driverFactory->make($node);
                        $results[$node->id] = $driver->getClientStatsGroupedByInbound();
                    } catch (\Throwable) {
                        $results[$node->id] = [];
                    }
                }
            }

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
