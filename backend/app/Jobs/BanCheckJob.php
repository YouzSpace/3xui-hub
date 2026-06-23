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
 * 封禁检查 Job（M8.3）：遍历启用中的用户，满足超量/到期 → ban。
 * 也可由 SyncNodeTrafficJob 内联触发（M8.4）。
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
                    $banService->ban($user);
                }
            });
    }
}
