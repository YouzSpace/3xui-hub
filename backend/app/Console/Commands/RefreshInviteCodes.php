<?php

namespace App\Console\Commands;

use App\Models\DiscountCode;
use App\Services\DiscountCodeService;
use Illuminate\Console\Command;
use Illuminate\Support\Facades\DB;

/**
 * 刷新到期的邀请码：旧码立即失效（status=refreshed），给同一用户发新码。
 * 运行方式：php artisan invite:refresh
 */
class RefreshInviteCodes extends Command
{
    protected $signature = 'invite:refresh';
    protected $description = '刷新到期的邀请码（旧码立即失效，只认最新码）';

    public function handle(DiscountCodeService $service): int
    {
        $this->info('[' . now()->format('Y-m-d H:i:s') . '] 开始刷新邀请码...');

        $codes = DiscountCode::where('source', DiscountCode::SOURCE_INVITE)
            ->where('status', DiscountCode::STATUS_ACTIVE)
            ->whereNotNull('next_refresh_at')
            ->where('next_refresh_at', '<=', now())
            ->get();

        $refreshed = 0;

        foreach ($codes as $code) {
            $done = DB::transaction(function () use ($code, $service) {
                // 锁行重读，重复执行/并发只刷一次
                $locked = DiscountCode::whereKey($code->id)->lockForUpdate()->first();
                if (! $locked || $locked->status !== DiscountCode::STATUS_ACTIVE) {
                    return false;
                }
                if (! $locked->next_refresh_at || $locked->next_refresh_at->isFuture()) {
                    return false;
                }

                $locked->forceFill([
                    'status' => DiscountCode::STATUS_REFRESHED,
                    'refreshed_at' => now(),
                ])->save();

                $user = $locked->user;
                if (! $user) {
                    return false;
                }

                $service->issueInviteCode($user);

                return true;
            });

            if ($done) {
                $refreshed++;
                $this->line("  刷新用户 #{$code->user_id} 的邀请码 {$code->code}");
            }
        }

        $this->info("邀请码刷新完成：{$refreshed} 个");

        return self::SUCCESS;
    }
}
