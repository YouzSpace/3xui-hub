<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\SubscriptionException;
use App\Services\SubscriptionService;
use App\Services\ThreeXUiClientFactory;
use App\Services\TrafficSyncService;
use Illuminate\Http\Request;
use Illuminate\Http\Response;

/**
 * 订阅接口：GET /api/sub/{token}
 * 返回 text/plain 的 Base64 文本，同时同步流量。
 */
class SubscriptionController extends Controller
{
    public function __construct(
        private SubscriptionService $service,
        private ThreeXUiClientFactory $factory,
        private TrafficSyncService $syncService,
        private BanService $banService,
    ) {}

    public function show(Request $request, string $token): Response
    {
        $user = User::where('token', $token)->first();

        if (!$user) {
            return $this->text('[404] 用户不存在', 404, '用户不存在');
        }

        // 同步流量
        $this->syncUserTraffic($user);

        // 重新加载用户数据
        $user->refresh();
        $user->load('plan');

        // 检查封禁状态
        if (!$user->enabled) {
            return $this->text('[403] 账号已禁用', 403, '账号已禁用');
        }

        try {
            $body = $this->service->generate($user);
        } catch (SubscriptionException $e) {
            return $this->text("[{$e->codeValue}] {$e->getMessage()}", $e->codeValue, $e->getMessage());
        }

        // Subscription-Userinfo 头
        $upload = 0;
        $download = (int) $user->traffic_used;
        $total = (int) $user->traffic_limit;
        $expire = $user->expired_at ? $user->expired_at->timestamp : 0;

        return $this->text($body)
            ->header('Subscription-Userinfo', "upload={$upload}; download={$download}; total={$total}; expire={$expire}");
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
                // 节点离线，跳过
            }
        });

        // 检查封禁
        $fresh = $user->fresh();
        $fresh->load('plan');
        $this->banService->checkAfterSync($fresh);
    }

    private function text(string $body, int $code = 0, string $msg = 'ok'): Response
    {
        return response($body, 200)
            ->header('Content-Type', 'text/plain; charset=UTF-8')
            ->header('X-CH-Code', (string) $code)
            ->header('X-CH-Msg', $msg);
    }
}
