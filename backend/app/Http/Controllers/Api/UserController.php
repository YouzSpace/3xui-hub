<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\ThreeXUiClientFactory;
use App\Services\TrafficSyncService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端：GET /api/me 返回当前用户信息，同时同步流量。
 */
class UserController extends Controller
{
    use ApiResponse;

    public function __construct(
        private ThreeXUiClientFactory $factory,
        private TrafficSyncService $syncService,
        private BanService $banService,
    ) {}

    public function me(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        if (!$user) {
            return $this->error('未认证', 401);
        }

        // 自动同步该用户的流量
        $this->syncUserTraffic($user);

        // 重新加载用户数据
        $user->refresh();
        $user->load('plan');

        return $this->success([
            'id' => $user->id,
            'email' => $user->email,
            'token' => $user->token,
            'protocol' => $user->protocol,
            'plan_id' => $user->plan_id,
            'plan_type' => $user->plan?->type,
            'plan_name' => $user->plan?->name,
            'traffic_limit' => (int) $user->traffic_limit,
            'traffic_used' => (int) $user->traffic_used,
            'monthly_traffic_used' => (int) $user->monthly_traffic_used,
            'monthly_traffic_limit' => $user->plan?->monthly_traffic ?? 0,
            'expired_at' => $user->expired_at?->toIso8601String(),
            'enabled' => (bool) $user->enabled,
        ]);
    }

    /**
     * 同步用户在所有节点上的流量。
     */
    private function syncUserTraffic(User $user): void
    {
        Node::where('enabled', true)->each(function (Node $node) use ($user) {
            try {
                $client = $this->factory->forNode($node);
                $traffic = $client->getClientTraffic($user->clientEmail());
                $this->syncService->syncUserNode($user, $node, $traffic);
            } catch (\Throwable $e) {
                // 节点离线或异常，跳过
            }
        });

        // 检查是否需要封禁
        $fresh = $user->fresh();
        $fresh->load('plan');
        $this->banService->checkAfterSync($fresh);
    }
}
