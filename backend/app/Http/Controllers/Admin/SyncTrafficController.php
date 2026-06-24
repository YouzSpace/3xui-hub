<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\ThreeXUiClientFactory;
use App\Services\TrafficSyncService;
use App\Traits\ApiResponse;

/**
 * 流量同步接口：管理员调用，批量同步所有用户流量。
 * POST /admin-api/sync-traffic
 */
class SyncTrafficController extends Controller
{
    use ApiResponse;

    public function __construct(
        private ThreeXUiClientFactory $factory,
        private TrafficSyncService $syncService,
        private BanService $banService,
    ) {}

    public function sync(): \Illuminate\Http\JsonResponse
    {
        $nodes = Node::where('enabled', true)->get();
        $users = User::whereNotNull('plan_id')->get();
        $synced = 0;
        $banned = 0;

        foreach ($users as $user) {
            foreach ($nodes as $node) {
                try {
                    $client = $this->factory->forNode($node);
                    $traffic = $client->getClientTraffic($user->clientEmail());
                    $this->syncService->syncUserNode($user, $node, $traffic);
                } catch (\Throwable $e) {
                    // 节点离线，跳过
                }
            }

            // 检查封禁
            $fresh = $user->fresh();
            $fresh->load('plan');
            $reason = $this->banService->banReason($fresh);
            if ($reason !== false && $fresh->enabled) {
                $this->banService->ban($fresh);
                $banned++;
            }

            $synced++;
        }

        return $this->success([
            'synced' => $synced,
            'banned' => $banned,
        ], "同步完成：{$synced} 用户，{$banned} 封禁");
    }
}
