<?php

namespace App\Jobs;

use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\ThreeXUiClientFactory;
use App\Services\TrafficSyncService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 单节点流量同步（M7.3~M7.5）。
 * 遍历本地 enabled 用户，getClientTraffic(ch_user_{id}) → 增量累加 → 写 snapshot。
 * 同步后内联封禁检查（M8.4）：超量/到期 → BanService::ban。
 */
class SyncNodeTrafficJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function __construct(public int $nodeId)
    {
    }

    public function handle(
        ThreeXUiClientFactory $factory,
        TrafficSyncService $sync,
        BanService $banService,
    ): void {
        /** @var Node|null $node */
        $node = Node::find($this->nodeId);
        if (!$node || !$node->enabled) {
            return;
        }

        $client = $factory->forNode($node);

        User::where('enabled', true)->with('plan')->each(function (User $user) use ($client, $sync, $node, $banService) {
            try {
                $traffic = $client->getClientTraffic($user->clientEmail());
            } catch (\Throwable $e) {
                report($e);
                return;
            }

            $sync->syncUserNode($user, $node, $traffic);

            // 内联封禁检查（fresh 重新加载 + 加载 plan 关联）
            $fresh = $user->fresh();
            $fresh->load('plan');
            $banService->checkAfterSync($fresh);
        });
    }
}
