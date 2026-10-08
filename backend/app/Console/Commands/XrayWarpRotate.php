<?php

namespace App\Console\Commands;

use App\Drivers\Xray\WarpService;
use App\Models\NodeWarpAccount;
use Illuminate\Console\Command;
use Illuminate\Support\Facades\Log;

/**
 * WARP 定时换 IP（站点调度每分钟触发；按账户的 auto_rotate_hours 判定到点）。
 *
 * 到点条件：已应用（enabled）+ last_rotate_at（或 updated_at）距今 ≥ auto_rotate_hours。
 * 换 IP = 重新注册设备 → 重组 wireguard 出站 → bump config（节点秒级拉取生效）。
 */
class XrayWarpRotate extends Command
{
    protected $signature = 'xray:warp-rotate';

    protected $description = 'WARP 定时换 IP（按各节点账户的 auto_rotate_hours 间隔）';

    public function handle(): int
    {
        $accounts = NodeWarpAccount::query()
            ->where('auto_rotate_hours', '>', 0)
            ->where('enabled', true)
            ->get();

        $rotated = 0;
        foreach ($accounts as $account) {
            $base = $account->last_rotate_at ?? $account->updated_at;
            if ($base === null) {
                continue;
            }

            $due = $base->copy()->addHours((int) $account->auto_rotate_hours);
            if (now()->lessThan($due)) {
                continue;
            }

            $node = $account->node;
            if ($node === null) {
                continue;
            }

            try {
                (new WarpService($node))->rotate();
                $rotated++;
                $this->info("node#{$node->id} WARP 已换 IP");
            } catch (\Throwable $e) {
                Log::warning('WARP 定时换 IP 失败', [
                    'node_id' => $node->id,
                    'error' => $e->getMessage(),
                ]);
                // 失败也不阻断其他节点；更新 last_rotate_at 避免每分钟重试风暴
                $account->last_rotate_at = now();
                $account->save();
            }
        }

        $this->info("完成：{$rotated}/{$accounts->count()} 个账户已换 IP");

        return self::SUCCESS;
    }
}
