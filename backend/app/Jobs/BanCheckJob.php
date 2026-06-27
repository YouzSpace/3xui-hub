<?php

namespace App\Jobs;

use App\Models\User;
use App\Services\BanService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 流量检查 Job（M8.3）：遍历启用中的用户，满足超量/到期 → 关闭 3x-ui 流量。
 * 不封禁用户，用户仍能登录。
 */
class BanCheckJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function handle(BanService $banService): void
    {
        User::where('enabled', true)
            ->with('plan')
            ->each(function (User $user) use ($banService) {
                $reason = $banService->banReason($user);
                if ($reason !== false) {
                    $banService->toggleClient($user, false);
                }
            });
    }
}
