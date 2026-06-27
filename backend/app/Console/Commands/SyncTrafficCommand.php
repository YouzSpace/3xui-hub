<?php

namespace App\Console\Commands;

use App\Models\Node;
use App\Models\User;
use App\Services\BanService;
use App\Services\ThreeXUiClientFactory;
use App\Services\TrafficSyncService;
use Illuminate\Console\Command;

/**
 * 流量自动同步命令。
 * 运行方式：php artisan traffic:sync
 * 后台运行：nohup php artisan traffic:sync &
 */
class SyncTrafficCommand extends Command
{
    protected $signature = 'traffic:sync';
    protected $description = '自动同步所有用户流量并封禁超限用户';

    public function handle(
        ThreeXUiClientFactory $factory,
        TrafficSyncService $syncService,
        BanService $banService,
    ): int {
        $this->info('[' . now()->format('Y-m-d H:i:s') . '] 开始流量同步...');

        $nodes = Node::where('enabled', true)->get();
        $users = User::whereNotNull('plan_id')->get();
        $synced = 0;
        $banned = 0;

        foreach ($users as $user) {
            foreach ($nodes as $node) {
                try {
                    $client = $factory->forNode($node);
                    $traffic = $client->getClientTraffic($user->clientEmail());
                    $syncService->syncUserNode($user, $node, $traffic);
                } catch (\Throwable $e) {
                    // 节点离线，跳过
                }
            }

            // 检查是否需要关闭流量
            $fresh = $user->fresh();
            $fresh->load('plan');
            $reason = $banService->banReason($fresh);
            if ($reason !== false && $fresh->enabled) {
                $banService->toggleClient($fresh, false);
                $banned++;
                $this->line("  关闭流量 #{$fresh->id} ({$fresh->email}): {$reason}");
            }

            $synced++;
        }

        $this->info("同步完成：{$synced} 用户，{$banned} 封禁");

        return self::SUCCESS;
    }
}
