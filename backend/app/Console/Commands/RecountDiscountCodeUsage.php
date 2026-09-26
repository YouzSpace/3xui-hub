<?php

namespace App\Console\Commands;

use App\Models\DiscountCode;
use App\Models\Order;
use Illuminate\Console\Command;
use Illuminate\Support\Facades\DB;

/**
 * 按「已支付订单数」重算折扣码的 used_count，清理旧 bug 留下的脏数据。
 *
 * 背景：修复前的旧逻辑在**下单（未支付）时**就把 used_count +1，于是码会显示
 * 「已用 1 次」而实际一张已支付订单都没有（受邀人页面也自然没有兑换码）。
 * 本命令以 orders 表为准回填真实消耗，只认 status = 'paid' 的订单；
 * 重置流量订单（order_no 以 RST 开头）压根不带 discount_code_id，天然不计入。
 *
 * 默认 dry-run 只打印差异，确认无误后加 --force 才写库 —— 这是生产环境的安全阀。
 * 一次性运维命令：升级后手动跑一次即可，**不要**注册定时任务。
 *
 * 运行：php artisan discount:recount          （只读预览，打印差异但不改库）
 *      php artisan discount:recount --force  （真正写库）
 *
 * 目标库由当前数据库配置决定（不写死库名）：改 .env 的 DB_DATABASE，或临时
 * `$env:DB_DATABASE='controlhub_test'; php artisan discount:recount` 即可指向别的库。
 */
class RecountDiscountCodeUsage extends Command
{
    protected $signature = 'discount:recount {--force : 真正写库（不加则只打印差异，不改数据）}';

    protected $description = '按已支付订单重算折扣码 used_count 并修正 status（默认 dry-run 只读）';

    public function handle(): int
    {
        $force = (bool) $this->option('force');

        // 打头先把「正在动哪个库」喊出来：生产命令最怕的是跑错库
        $this->info(sprintf(
            '[%s] 库：%s@%s · 模式：%s',
            now()->format('Y-m-d H:i:s'),
            DB::connection()->getDatabaseName(),
            DB::connection()->getName(),
            $force ? '写入（--force）' : 'DRY-RUN（只读，不改库）'
        ));

        $checked = 0;
        $fixed   = 0;

        DiscountCode::query()->chunkById(200, function ($codes) use (&$checked, &$fixed, $force): void {
            foreach ($codes as $code) {
                $checked++;

                // 真实消耗 = 该码名下的已支付订单数
                $actual = Order::where('discount_code_id', $code->id)
                    ->where('status', 'paid')
                    ->count();

                $newStatus = $this->resolveStatus($code, $actual);
                $changes   = [];

                // 箭头一律用 ASCII 的 "->"：Windows 上 Laravel 的 console 输出层会把 "→"
                // 整个吃掉（同一行的「（」却能保留），运维看到的就是「used_count 2 1」这种
                // 没法读的差异。ASCII 箭头在 GBK/UTF-8 控制台都稳定显示。
                if ($actual !== $code->used_count) {
                    $changes[] = "used_count {$code->used_count} -> {$actual}（实际已支付订单 {$actual} 笔）";
                }
                if ($newStatus !== $code->status) {
                    $changes[] = "status {$code->status} -> {$newStatus}";
                }

                if (! $changes) {
                    $this->line(($force ? '[OK]' : '[DRY-RUN]') . " {$code->code} ({$code->source}): 无需修正");
                    continue;
                }

                $fixed++;
                $this->line(sprintf(
                    '%s %s (%s): %s',
                    $force ? '[FIXED]' : '[DRY-RUN]',
                    $code->code,
                    $code->source,
                    implode('，', $changes)
                ));

                if ($force) {
                    $code->forceFill(['used_count' => $actual, 'status' => $newStatus])->save();
                }
            }
        });

        $this->info(sprintf(
            '共检查 %d 条，需修正 %d 条%s',
            $checked,
            $fixed,
            $force ? '（已写库）' : '（dry-run 未写库，确认无误后加 --force 执行）'
        ));

        return self::SUCCESS;
    }

    /**
     * 按实际用量推算该有的 status。
     *
     * refreshed / expired 是别的原因（重发邀请码 / 到期）导致的，不是用满导致的，
     * 一律不动 —— 这里只负责把「用满没用满」这个维度纠正回来。
     */
    private function resolveStatus(DiscountCode $code, int $actual): string
    {
        if (in_array($code->status, [DiscountCode::STATUS_REFRESHED, DiscountCode::STATUS_EXPIRED], true)) {
            return $code->status;
        }

        if ($actual >= $code->max_uses) {
            return DiscountCode::STATUS_USED_UP;
        }

        // 用满的正确状态是 active；还标着 used_up 的说明是脏数据，放回 active
        return $code->status === DiscountCode::STATUS_USED_UP
            ? DiscountCode::STATUS_ACTIVE
            : $code->status;
    }
}
