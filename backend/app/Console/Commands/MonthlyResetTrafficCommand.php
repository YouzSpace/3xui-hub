<?php

namespace App\Console\Commands;

use App\Models\User;
use App\Services\BanService;
use App\Services\UserAdminService;
use Illuminate\Console\Command;
use Illuminate\Support\Carbon;

/**
 * 每天检查并重置周期套餐用户的月流量。
 * 基于每个用户的 next_traffic_reset_at 判断是否该重置。
 * 运行方式：php artisan traffic:monthly-reset
 */
class MonthlyResetTrafficCommand extends Command
{
    protected $signature = 'traffic:monthly-reset';
    protected $description = '重置周期套餐用户的月流量并恢复 3x-ui 连接';

    public function handle(UserAdminService $userService, BanService $banService): int
    {
        $this->info('[' . now()->format('Y-m-d H:i:s') . '] 开始检查流量重置...');

        $users = User::whereNotNull('plan_id')
            ->whereNotNull('next_traffic_reset_at')
            ->where('next_traffic_reset_at', '<=', now())
            ->whereHas('plan', fn ($q) => $q->where('type', 'period'))
            ->with('plan')
            ->get();

        $reset = 0;

        foreach ($users as $user) {
            // 清零月流量
            $user->forceFill([
                'monthly_traffic_used' => 0,
                'next_traffic_reset_at' => Carbon::parse($user->next_traffic_reset_at)->addDays(30),
            ])->save();

            // 重置 3x-ui 流量统计
            $userService->resetClientOnNodes($user);

            // 打开 3x-ui 连接
            $banService->toggleClient($user, true);

            $reset++;
            $this->line("  重置用户 #{$user->id} ({$user->email})");
        }

        $this->info("流量重置完成：{$reset} 用户");

        return self::SUCCESS;
    }
}
