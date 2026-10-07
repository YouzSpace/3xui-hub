<?php

namespace App\Console\Commands;

use App\Services\ThirdPartyService;
use Illuminate\Console\Command;

/**
 * 第三方订阅拉取命令：抓取所有启用的第三方订阅 → 解析 → 缓存节点链接。
 *
 * 运行方式：php artisan third-party:fetch
 * 定时调度：routes/console.php 每 2 小时
 * 失败保留上次成功缓存（ThirdPartyService 内处理），这里不额外处理。
 */
class FetchThirdPartySubsCommand extends Command
{
    protected $signature = 'third-party:fetch';

    protected $description = '拉取所有启用的第三方订阅并缓存节点链接';

    public function handle(ThirdPartyService $service): int
    {
        $subs = \App\Models\ThirdPartySub::where('enabled', true)->count();
        if ($subs === 0) {
            $this->info('没有启用的第三方订阅，跳过');
            return self::SUCCESS;
        }

        $this->info(now()->format('Y-m-d H:i:s') . " 开始拉取 {$subs} 条第三方订阅...");
        $service->fetchAllEnabled();

        // 汇报结果
        foreach (\App\Models\ThirdPartySub::where('enabled', true)->get() as $sub) {
            $this->line(sprintf(
                '  %s: %s（%d 节点%s）',
                $sub->name,
                $sub->last_status === 'success' ? '成功' : '失败',
                $sub->node_count,
                $sub->last_error ? "，{$sub->last_error}" : ''
            ));
        }

        $this->info('拉取完成');
        return self::SUCCESS;
    }
}
