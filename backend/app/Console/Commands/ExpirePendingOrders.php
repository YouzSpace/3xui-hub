<?php

namespace App\Console\Commands;

use App\Models\Order;
use Illuminate\Console\Command;
use Illuminate\Support\Facades\DB;

/**
 * 作废超时未支付的订单。
 *
 * 背景：pending 订单此前没有任何超时机制，会无限堆积。它们有两个直接危害：
 * 1. 带码的 pending 单会被 DiscountCodeService 反复复用，用户重新下单永远拿到同一笔
 *    死单，重复撞网关判重，报「获取支付链接失败」（已在生产出现，见支付问题报告 P0-1）；
 * 2. 无码的 pending 单会被 PaymentController::create 的「重发旧单」分支翻出来反复请求
 *    网关，白打请求并刷 ERROR 日志（现场有 6、7 月的老单被翻出的案例）。
 *
 * 本命令把超过 TTL 的 pending 单置为 expired。判定以「网关是否还有可能回调」为准：
 * 用户真正在支付的话，回调会在几分钟内到达；超过数小时的单，钱已经不可能再进来了。
 *
 * 安全性：
 * - 只动 status='pending' 的行，已支付订单（paid）绝不触碰；
 * - 不改 orders 之外任何数据（折扣码的 used_count 只在支付成功时增加，作废未支付单
 *   不涉及任何次数回滚）；
 * - 默认 TTL 24 小时，可用 --hours 调整；--dry-run 只打印不改库。
 *
 * 运行：php artisan order:expire-pending                 （作废超 24 小时的 pending 单）
 *      php artisan order:expire-pending --hours=6        （自定义 TTL）
 *      php artisan order:expire-pending --dry-run        （只预览，不改库）
 */
class ExpirePendingOrders extends Command
{
    protected $signature = 'order:expire-pending
                            {--hours=24 : 超过多少小时未支付即作废}
                            {--dry-run : 只打印将要作废的订单，不写库}';

    protected $description = '作废超时未支付的订单，防止 pending 单无限堆积';

    public function handle(): int
    {
        $hours   = max(1, (int) $this->option('hours'));
        $dryRun  = (bool) $this->option('dry-run');
        $cutoff  = now()->subHours($hours);

        $this->info(sprintf(
            '[%s] 库：%s · 截止时间：%s（%d 小时前）· 模式：%s',
            now()->format('Y-m-d H:i:s'),
            DB::connection()->getDatabaseName(),
            $cutoff->format('Y-m-d H:i:s'),
            $hours,
            $dryRun ? 'DRY-RUN（只读）' : '写入'
        ));

        $query = Order::where('status', Order::STATUS_PENDING)
            ->where('created_at', '<', $cutoff);

        $total = (clone $query)->count();

        if ($total === 0) {
            $this->info('没有超时未支付订单，无需处理。');
            return self::SUCCESS;
        }

        // 按 id 分批处理，避免一次锁太多行（生产库上长时间大事务会阻塞下单）
        $expired = 0;
        $query->orderBy('id')->chunkById(200, function ($orders) use (&$expired, $dryRun): void {
            foreach ($orders as $order) {
                $this->line(sprintf(
                    '%s #%d %s（用户 %d，%s 元，创建于 %s）',
                    $dryRun ? '[DRY-RUN]' : '[EXPIRED]',
                    $order->id,
                    $order->order_no,
                    $order->user_id,
                    number_format((float) $order->amount, 2, '.', ''),
                    $order->created_at->format('Y-m-d H:i:s')
                ));
            }

            if (! $dryRun) {
                Order::whereIn('id', $orders->pluck('id'))
                    ->where('status', Order::STATUS_PENDING)
                    ->update(['status' => Order::STATUS_EXPIRED, 'updated_at' => now()]);
            }

            $expired += $orders->count();
        });

        $this->info(sprintf(
            '共 %d 笔超时未支付订单%s。',
            $expired,
            $dryRun ? '（dry-run 未写库）' : ' 已作废'
        ));

        return self::SUCCESS;
    }
}
