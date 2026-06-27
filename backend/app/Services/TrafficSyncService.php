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

        // upsert：存在则更新，不存在则插入
        TrafficSnapshot::updateOrCreate(
            ['user_id' => $user->id, 'node_id' => $node->id],
            ['upload' => $nowUp, 'download' => $nowDown],
        );
    }

    // ===== 批量同步方法（性能优化） =====

    /**
     * 从 clientEmail（ch_user_{id}）解析出 userId。
     * 返回 [clientEmail => userId, ...]
     */
    private function parseUserIds(array $clientEmails): array
    {
        $map = [];
        foreach ($clientEmails as $email) {
            if (preg_match('/^ch_user_(\d+)$/', $email, $m)) {
                $map[$email] = (int) $m[1];
            }
        }
        return $map;
    }

    /**
     * 批量同步单节点所有用户流量。
     *
     * @param Node $node 节点
     * @param array $clientStats [clientEmail => ['up'=>int, 'down'=>int], ...]
     * @return array ['deltaMap' => [userId => deltaBytes], 'snapshotData' => [...]]
     */
    public function syncNodeBatch(Node $node, array $clientStats): array
    {
        if (empty($clientStats)) {
            return ['deltaMap' => [], 'snapshotData' => []];
        }

        // 1. 从 clientEmail 解析 userId
        $emailToUserId = $this->parseUserIds(array_keys($clientStats));
        if (empty($emailToUserId)) {
            return ['deltaMap' => [], 'snapshotData' => []];
        }

        $userIds = array_values($emailToUserId);

        // 2. 批量查所有相关用户的最新快照
        $latestSnapshots = TrafficSnapshot::where('node_id', $node->id)
            ->whereIn('user_id', $userIds)
            ->orderByDesc('id')
            ->get()
            ->keyBy('user_id');

        // 3. 批量查所有相关用户（只查有套餐的）
        $users = User::whereIn('id', $userIds)
            ->whereNotNull('plan_id')
            ->with('plan')
            ->get()
            ->keyBy('id');

        // 4. 计算增量
        $deltaMap = [];
        $snapshotData = [];
        $now = now();

        foreach ($emailToUserId as $email => $userId) {
            if (!isset($clientStats[$email])) continue;
            if (!isset($users[$userId])) continue; // 无套餐用户跳过

            $stat = $clientStats[$email];
            $nowUp = $stat['up'];
            $nowDown = $stat['down'];
            $nowTotal = $nowUp + $nowDown;

            $last = $latestSnapshots->get($userId);
            if ($last === null) {
                $delta = $nowTotal; // 首次同步
            } else {
                $lastTotal = (int) $last->upload + (int) $last->download;
                // 3x-ui 侧重置或 client 重建 → now < last，重新计起
                $delta = $nowTotal < $lastTotal ? $nowTotal : ($nowTotal - $lastTotal);
            }

            if ($delta > 0) {
                $deltaMap[$userId] = $delta;
            }

            $snapshotData[] = [
                'user_id' => $userId,
                'node_id' => $node->id,
                'upload' => $nowUp,
                'download' => $nowDown,
                'updated_at' => $now,
            ];
        }

        return ['deltaMap' => $deltaMap, 'snapshotData' => $snapshotData];
    }

    /**
     * 批量 upsert 快照（每 user+node 只保留1条）。
     * 需要 traffic_snapshots 表有 UNIQUE(user_id, node_id) 索引。
     *
     * @param array $snapshots 待写入的快照数据
     */
    public function upsertSnapshots(array $snapshots): void
    {
        if (empty($snapshots)) return;

        // 分批处理，每批500条
        foreach (array_chunk($snapshots, 500) as $chunk) {
            // 构建 upsert SQL
            $insertParts = [];
            $params = [];
            foreach ($chunk as $s) {
                $insertParts[] = '(?, ?, ?, ?, NOW(), NOW())';
                $params[] = $s['user_id'];
                $params[] = $s['node_id'];
                $params[] = $s['upload'];
                $params[] = $s['download'];
            }

            $values = implode(', ', $insertParts);
            $sql = "INSERT INTO traffic_snapshots (user_id, node_id, upload, download, created_at, updated_at)
                    VALUES {$values}
                    ON DUPLICATE KEY UPDATE
                        upload = VALUES(upload),
                        download = VALUES(download),
                        updated_at = NOW()";

            \DB::statement($sql, $params);
        }
    }

    /**
     * 批量更新用户流量使用量。
     * @param array $deltaMap [userId => deltaBytes]
     */
    public function applyDeltas(array $deltaMap): void
    {
        if (empty($deltaMap)) return;

        // 预加载周期套餐用户ID集合
        $allUserIds = array_keys($deltaMap);
        $periodUserIds = User::whereIn('id', $allUserIds)
            ->whereHas('plan', fn($q) => $q->where('type', 'period'))
            ->pluck('id')
            ->flip(); // [id => 0, ...] 用于快速 in_array

        // 分批处理，每批500条
        foreach (array_chunk($deltaMap, 500, true) as $chunk) {
            $ids = array_keys($chunk);

            // 批量更新 traffic_used
            $caseParts = [];
            $params = [];
            foreach ($chunk as $userId => $delta) {
                $caseParts[] = 'WHEN ? THEN ?';
                $params[] = $userId;
                $params[] = $delta;
            }
            $caseSql = implode(' ', $caseParts);
            $placeholders = implode(',', array_fill(0, count($ids), '?'));

            $sql = "UPDATE users SET traffic_used = traffic_used + CASE id {$caseSql} END WHERE id IN ({$placeholders})";
            \DB::update($sql, array_merge($params, $ids));

            // 周期套餐用户同时更新 monthly_traffic_used
            $periodChunk = array_filter($chunk, fn($uid) => isset($periodUserIds[$uid]));
            if (!empty($periodChunk)) {
                $caseParts2 = [];
                $params2 = [];
                foreach ($periodChunk as $userId => $delta) {
                    $caseParts2[] = 'WHEN ? THEN ?';
                    $params2[] = $userId;
                    $params2[] = $delta;
                }
                $caseSql2 = implode(' ', $caseParts2);
                $ids2 = array_keys($periodChunk);
                $placeholders2 = implode(',', array_fill(0, count($ids2), '?'));

                $sql2 = "UPDATE users SET monthly_traffic_used = monthly_traffic_used + CASE id {$caseSql2} END WHERE id IN ({$placeholders2})";
                \DB::update($sql2, array_merge($params2, $ids2));
            }
        }
    }
}
