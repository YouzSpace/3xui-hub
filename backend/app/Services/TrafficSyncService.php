<?php

namespace App\Services;

use App\Models\Node;
use App\Models\TrafficSnapshot;
use App\Models\User;

/**
 * 流量增量计算（M7.4~M7.6 + 套餐适配）。
 * 增量 = (up+down) - 上次 (upload+download)；
 * 若 3x-ui 侧流量被重置（now < last）→ delta = now（重新计起）。
 * 幂等：以最新 snapshot 为基准，重跑不重复扣。
 *
 * 周期套餐用户：同时累加 traffic_used 和 monthly_traffic_used。
 * 其他用户：只累加 traffic_used。
 */
class TrafficSyncService
{
    /**
     * 同步单 user+node 的流量。
     *
     * @param array|null $traffic getClientTraffic 返回的 {up,down,...}，null 表示无 client
     */
    public function syncUserNode(User $user, Node $node, ?array $traffic): void
    {
        if ($traffic === null) {
            return; // 该节点无此 client，跳过
        }

        $nowUp = (int) ($traffic['up'] ?? 0);
        $nowDown = (int) ($traffic['down'] ?? 0);
        $nowTotal = $nowUp + $nowDown;

        /** @var TrafficSnapshot|null $last */
        $last = TrafficSnapshot::where('user_id', $user->id)
            ->where('node_id', $node->id)
            ->latest('id')
            ->first();

        if ($last === null) {
            $delta = $nowTotal; // 首次同步：记为基准，不回溯全部历史
        } else {
            $lastTotal = (int) $last->upload + (int) $last->download;
            // 3x-ui 侧重置或 client 重建 → now < last，重新计起
            $delta = $nowTotal < $lastTotal ? $nowTotal : ($nowTotal - $lastTotal);
        }

        if ($delta > 0) {
            $user->increment('traffic_used', $delta);
            // 周期套餐：同时累加当月流量
            if ($user->isPeriodPlan()) {
                $user->increment('monthly_traffic_used', $delta);
            }
        }

        TrafficSnapshot::create([
            'user_id' => $user->id,
            'node_id' => $node->id,
            'upload' => $nowUp,
            'download' => $nowDown,
        ]);
    }
}
