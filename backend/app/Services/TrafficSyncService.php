<?php

namespace App\Services;

use App\Models\Node;
use App\Models\SiteConfig;
use App\Models\TrafficSnapshot;
use App\Models\User;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Log;

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
     * 原始倍率的 site_configs key。
     */
    private const ORIGINAL_MULTIPLIER_KEY = 'default_node_multiplier';

    /**
     * 节点实际生效的流量倍率 —— 全项目唯一取值入口（计费与展示共用），每次直接读库。
     *
     * 口径：traffic_multiplier 为 NULL（继承）就用原始倍率，有值（手动指定）就用它。
     * 改原始倍率只影响继承中的节点。
     *
     * 不做进程内缓存：queue worker 是长驻进程，静态缓存跨 job 复用会让改倍率后的
     * 同步静默按旧值计费；展示侧也必须实时。批量路径在循环外取一次即可。
     */
    public static function multiplierFor(Node $node): float
    {
        $own = $node->traffic_multiplier; // null = 继承
        if ($own !== null) {
            return (float) $own;
        }

        return self::originalMultiplier();
    }

    /** 原始倍率取值（唯一入口）；配置行不存在/空串时兜底 1.0。 */
    public static function originalMultiplier(): float
    {
        $stored = SiteConfig::getValue(self::ORIGINAL_MULTIPLIER_KEY, '');

        return $stored === '' ? 1.0 : (float) $stored;
    }

    /**
     * 兼容原有调用方；远程同步应使用 syncUserNodeFromSource()，
     * 确保远程读取和数据库提交处于同一个节点锁内。
     */
    public function syncUserNode(User $user, Node $node, ?array $traffic): void
    {
        if ($traffic === null) {
            return;
        }

        $this->syncUserNodeFromSource($user, $node, fn () => $traffic);
    }

    /**
     * 在节点锁内读取远程单用户流量并提交，避免旧采样晚于新采样写入。
     *
     * @param callable(): ?array $source
     * @return array{acquired: bool, updated: bool}
     */
    public function syncUserNodeFromSource(User $user, Node $node, callable $source): array
    {
        $lock = Cache::lock($this->lockName($node), 120);
        if (!$lock->get()) {
            return ['acquired' => false, 'updated' => false];
        }

        try {
            $traffic = $source();
            if ($traffic === null) {
                return ['acquired' => true, 'updated' => false];
            }

            DB::transaction(fn () => $this->syncUserNodeUnlocked($user, $node, $traffic));

            return ['acquired' => true, 'updated' => true];
        } finally {
            $lock->release();
        }
    }

    private function syncUserNodeUnlocked(User $user, Node $node, array $traffic): void
    {
        $nowUp = (int) ($traffic['up'] ?? 0);
        $nowDown = (int) ($traffic['down'] ?? 0);
        $nowTotal = $nowUp + $nowDown;

        /** @var TrafficSnapshot|null $last */
        $last = TrafficSnapshot::where('user_id', $user->id)
            ->where('node_id', $node->id)
            ->latest('id')
            ->first();

        $lastUp = $last === null ? null : (int) $last->upload;
        $lastDown = $last === null ? null : (int) $last->download;
        $lastTotal = $lastUp === null ? null : $lastUp + $lastDown;

        // 分方向增量必须先于「零增量跳过」算出：总量一升一降时总量差为 0，
        // 但那笔流量真实发生过，方向增量不能跟着被跳过。
        [$upDelta, $downDelta] = self::directionalDeltas($nowUp, $nowDown, $lastUp, $lastDown);

        if ($lastTotal === null) {
            if ($nowTotal === 0) {
                return; // 首次同步且为 0：不落空快照
            }
            $delta = $nowTotal; // 首次同步：记为基准，不回溯全部历史
        } else {
            // 3x-ui 侧重置或 client 重建 → now < last，重新计起
            $delta = $nowTotal < $lastTotal ? $nowTotal : ($nowTotal - $lastTotal);

            if ($delta === 0) {
                if ($upDelta === 0 && $downDelta === 0) {
                    return; // 完全没变：快照无需重写（快照仅作下次算差值的基准）
                }

                // 总量不变、但方向有变化（一升一降）：只留方向增量，计费增量为 0。
                // 快照必须推进到本轮值，否则同一份采样重放会把这笔方向增量再记一次。
                $this->recordUserTraffic($node, [$user->id => [
                    'up' => $upDelta,
                    'down' => $downDelta,
                    'weighted' => 0,
                ]]);
                TrafficSnapshot::updateOrCreate(
                    ['user_id' => $user->id, 'node_id' => $node->id],
                    ['upload' => $nowUp, 'download' => $nowDown],
                );

                return;
            }
        }

        $weighted = 0;
        if ($delta > 0) {
            // 倍率只乘在 delta 上，绝不碰快照：快照存的是面板侧原始累计字节数，
            // 下一轮 delta = now - last。倍率一旦乘进快照，改倍率那一刻就会因为
            // 「老倍率算过的 last 比面板原值大」触发 now < last 分支，delta 直接等于
            // 全部累计值，用户流量瞬间暴涨。traffic_used 是 bigint，所以向下取整。
            $weighted = (int) floor($delta * self::multiplierFor($node));

            if ($weighted > 0) {
                $user->increment('traffic_used', $weighted);
                // 周期套餐：同时累加当月流量
                if ($user->isPeriodPlan()) {
                    $user->increment('monthly_traffic_used', $weighted);
                }
            }
        }

        // 用户×节点留痕（小时表 + 天表）：与快照 / traffic_used 同一事务
        $this->recordUserTraffic($node, [$user->id => [
            'up' => $upDelta,
            'down' => $downDelta,
            'weighted' => $weighted,
        ]]);

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
     * @return array ['deltaMap' => [userId => ['up'=>int,'down'=>int,'weighted'=>int]], 'snapshotData' => [...]]
     */
    public function syncNodeBatch(Node $node, array $clientStats): array
    {
        if (empty($clientStats)) {
            return ['deltaMap' => [], 'snapshotData' => [], 'driftUserIds' => []];
        }

        // 1. 从 clientEmail 解析 userId
        $emailToUserId = $this->parseUserIds(array_keys($clientStats));
        if (empty($emailToUserId)) {
            return ['deltaMap' => [], 'snapshotData' => [], 'driftUserIds' => []];
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
        $driftUserIds = []; // 节点上已关闭、面板认为正常的用户（人工点同步流量时修复）
        $now = now();

        // 同一节点内所有用户共用一个倍率，循环外取一次（节点级倍率）。
        $multiplier = self::multiplierFor($node);

        foreach ($emailToUserId as $email => $userId) {
            if (!isset($users[$userId])) continue; // 无套餐用户跳过

            // 节点上根本没有这个 client（被人工误删 / 从未建号）：
            // 面板认为该用户正常时记为漂移，由人工点「同步流量」重建（repairDrift → provisionClient）。
            // 这里必须 continue，下面按 $stat 记账的代码拿不到数据。
            if (!isset($clientStats[$email])) {
                $candidate = $users[$userId];
                if ($candidate->enabled
                    && $candidate->traffic_disabled_at === null
                    && app(\App\Services\BanService::class)->banReason($candidate) === false) {
                    $driftUserIds[] = $userId;
                }

                continue;
            }

            $stat = $clientStats[$email];
            $nowUp = (int) ($stat['up'] ?? 0);
            $nowDown = (int) ($stat['down'] ?? 0);
            $nowTotal = $nowUp + $nowDown;

            // 漂移：节点上 client 已关闭，面板却认为该用户正常（未超量/未到期/未被面板关闭）。
            // 只记录不在这里修 —— 修复由人工触发（管理员/用户点「同步流量」）时执行，
            // 避免定时任务在后台和 3x-ui 自己的自动禁用来回拉锯。
            if (($stat['enable'] ?? true) === false) {
                $candidate = $users[$userId];
                if ($candidate->enabled
                    && $candidate->traffic_disabled_at === null
                    && app(\App\Services\BanService::class)->banReason($candidate) === false) {
                    $driftUserIds[] = $userId;
                }
            }

            $last = $latestSnapshots->get($userId);
            $lastUp = $last === null ? null : (int) $last->upload;
            $lastDown = $last === null ? null : (int) $last->download;
            $lastTotal = $lastUp === null ? null : $lastUp + $lastDown;

            // 分方向原始增量（与单用户路径共用同一口径）。必须在下面的 continue 之前算：
            // 总量一升一降时总量差为 0，方向增量却不为 0，跳过就永远拿不到 up/down。
            [$upDelta, $downDelta] = self::directionalDeltas($nowUp, $nowDown, $lastUp, $lastDown);

            if ($lastTotal === null) {
                // 首次同步且计数为 0：没有可记的流量，等真产生流量再落快照，
                // 避免给每个闲号插一行空快照（大用户量下等于每轮白插 N 行）。
                if ($nowTotal === 0) {
                    continue;
                }
                $delta = $nowTotal; // 首次同步
            } else {
                // 3x-ui 侧重置或 client 重建 → now < last，重新计起
                $delta = $nowTotal < $lastTotal ? $nowTotal : ($nowTotal - $lastTotal);

                // 累计值没变 → 跳过写快照。快照只是下次算差值的基准，
                // 数值没变时重写是纯写放大（10 万用户里绝大多数每轮都没流量）。
                // 但方向增量仍不为 0 的「一升一降」样本要留痕：计费增量为 0，
                // 快照照样推进（否则重放会把方向增量再记一次）。
                if ($delta === 0) {
                    if ($upDelta !== 0 || $downDelta !== 0) {
                        $deltaMap[$userId] = ['up' => $upDelta, 'down' => $downDelta, 'weighted' => 0];
                        $snapshotData[] = [
                            'user_id' => $userId,
                            'node_id' => $node->id,
                            'upload' => $nowUp,
                            'download' => $nowDown,
                            'updated_at' => $now,
                        ];
                    }

                    continue;
                }
            }

            // 加权增量：倍率只乘在 delta 上，快照仍存原始字节
            $weighted = $delta > 0 ? (int) floor($delta * $multiplier) : 0;

            if ($upDelta > 0 || $downDelta > 0 || $weighted > 0) {
                $deltaMap[$userId] = [
                    'up' => $upDelta,
                    'down' => $downDelta,
                    'weighted' => $weighted,
                ];
            }

            $snapshotData[] = [
                'user_id' => $userId,
                'node_id' => $node->id,
                'upload' => $nowUp,
                'download' => $nowDown,
                'updated_at' => $now,
            ];
        }

        return ['deltaMap' => $deltaMap, 'snapshotData' => $snapshotData, 'driftUserIds' => $driftUserIds];
    }

    /**
     * 分方向原始增量（up / down 各自钳 0），单用户与批量两条路径共用（口径唯一）。
     *
     * 两种「重新起算」与总量口径一致：
     *   - 首次同步（$lastUp / $lastDown 为 null）→ 整段现值都算增量；
     *   - 累计总量倒退（now < last，3x-ui 侧重置或 client 重建）→ 整段现值都算增量。
     * 其余情况逐方向取差、负差钳 0（同 MonitorService::deltas 的机器口径）。
     *
     * 注：逐方向钳 0 后，up+down 与「总量差」在「一升一降」的极端样本上会有出入；
     * 计费口径以 weighted（由总量差算出）为准，up/down 只用于展示与对账。
     *
     * @return array{0:int, 1:int}
     */
    public static function directionalDeltas(int $nowUp, int $nowDown, ?int $lastUp, ?int $lastDown): array
    {
        if ($lastUp === null || $lastDown === null) {
            return [$nowUp, $nowDown];
        }

        if ($nowUp + $nowDown < $lastUp + $lastDown) {
            return [$nowUp, $nowDown];
        }

        return [max(0, $nowUp - $lastUp), max(0, $nowDown - $lastDown)];
    }

    /** 站点业务时区（天表日界口径，与 monitor_daily 一致）。 */
    private function businessTimezone(): string
    {
        return (string) (config('panel.business_timezone') ?: config('app.timezone'));
    }

    /**
     * 用户×节点 流量留痕：同时累加小时表（24h 窗口）与天表（7/30d 窗口）。
     *
     * 语义是累加（ON DUPLICATE KEY UPDATE 列 = 列 + VALUES(列)），同一小时/同一天被多轮同步各加一段。
     * 幂等前提：入参增量以 traffic_snapshots 为基准算出，且本方法必须与快照写在同一事务；
     * 同一份采样重放时快照已更新 → 增量 0 → 加 0 不变；事务回滚则两张表一起回滚。
     *
     * @param array<int, array{up:int,down:int,weighted:int}> $deltaMap
     */
    public function recordUserTraffic(Node $node, array $deltaMap): void
    {
        if (empty($deltaMap)) {
            return;
        }

        $now = now();
        $hourStart = $now->copy()->startOfHour();
        $date = $now->copy()->setTimezone($this->businessTimezone())->toDateString();

        $rows = [];
        foreach ($deltaMap as $userId => $entry) {
            $up = (int) ($entry['up'] ?? 0);
            $down = (int) ($entry['down'] ?? 0);
            $weighted = (int) ($entry['weighted'] ?? 0);

            if ($up === 0 && $down === 0 && $weighted === 0) {
                continue;
            }

            $rows[$userId] = ['up' => $up, 'down' => $down, 'weighted' => $weighted];
        }

        if ($rows === []) {
            return;
        }

        // 两张表结构一致，用同一份行数据写两次，避免两段逻辑漂移
        $this->accumulateUserTraffic('user_traffic_hourly', 'hour_start', $hourStart->toDateTimeString(), $node, $rows);
        $this->accumulateUserTraffic('user_traffic_daily', 'date', $date, $node, $rows);
    }

    /**
     * 累加型 upsert（小时表与天表共用）。表名/列名仅内部常量，不接受外部输入。
     *
     * @param string $table     user_traffic_hourly | user_traffic_daily
     * @param string $bucketCol hour_start | date
     * @param string $bucket    桶值
     * @param array<int, array{up:int,down:int,weighted:int}> $rows
     */
    private function accumulateUserTraffic(string $table, string $bucketCol, string $bucket, Node $node, array $rows): void
    {
        foreach (array_chunk($rows, 500, true) as $chunk) {
            $insertParts = [];
            $params = [];

            foreach ($chunk as $userId => $entry) {
                $insertParts[] = '(?, ?, ?, ?, ?, ?, NOW(), NOW())';
                $params[] = $userId;
                $params[] = $node->id;
                $params[] = $bucket;
                $params[] = $entry['up'];
                $params[] = $entry['down'];
                $params[] = $entry['weighted'];
            }

            $values = implode(', ', $insertParts);
            $sql = "INSERT INTO {$table}
                        (user_id, node_id, {$bucketCol}, up_delta, down_delta, weighted_delta, created_at, updated_at)
                    VALUES {$values}
                    ON DUPLICATE KEY UPDATE
                        up_delta = up_delta + VALUES(up_delta),
                        down_delta = down_delta + VALUES(down_delta),
                        weighted_delta = weighted_delta + VALUES(weighted_delta),
                        updated_at = NOW()";

            \DB::statement($sql, $params);
        }
    }

    /**
     * 小时表过期清理：与写入同一条同步路径，每小时至多一次（Cache 防重入）。
     *
     * 不做定时任务的理由：表只在同步时长行——不同步就不长、不需要清，清理跟着写入走天然同生命周期。
     * DELETE 走索引前缀 + LIMIT，毫秒级；放事务外、独立 try/catch：失败只记 warning，绝不连累落账回滚。
     */
    public function pruneExpiredHourly(): void
    {
        if (! Cache::add('user-traffic:hourly:prune', 1, 3600)) {
            return; // 这一小时内已经清过（任何节点的任何一轮）
        }

        $retainDays = max(2, (int) config('user_traffic.hourly_retain_days', 7));
        $cutoff = now()->subDays($retainDays)->startOfHour();

        try {
            DB::table('user_traffic_hourly')
                ->where('hour_start', '<', $cutoff)
                ->limit(5000)
                ->delete();
        } catch (\Throwable $e) {
            Log::warning('user_traffic_hourly 过期清理失败（不影响落账）', ['error' => $e->getMessage()]);
        }
    }


    /**
     * 在节点锁内读取远程批量流量并原子提交，保证采样顺序与快照顺序一致。
     *
     * @param callable(): array $source
     * @return array{acquired: bool, deltaMap: array, snapshotData: array}
     */
    public function syncNodeFromSource(Node $node, callable $source): array
    {
        $empty = ['acquired' => false, 'deltaMap' => [], 'snapshotData' => []];
        $lock = Cache::lock($this->lockName($node), 120);
        if (!$lock->get()) {
            return $empty;
        }

        try {
            return $this->commitNodeStats($node, $source());
        } finally {
            $lock->release();
        }
    }

    private function commitNodeStats(Node $node, array $clientStats): array
    {
        // 小时表过期清理（事务外、每小时至多一次、失败不连累落账）
        $this->pruneExpiredHourly();

        return DB::transaction(function () use ($node, $clientStats) {
            $result = $this->syncNodeBatch($node, $clientStats);
            $this->upsertSnapshots($result['snapshotData']);
            $this->applyDeltas($result['deltaMap']);
            // 用户流量留痕（小时表 + 天表）：与快照 / traffic_used 同一事务
            $this->recordUserTraffic($node, $result['deltaMap']);

            return ['acquired' => true] + $result;
        });
    }

    private function lockName(Node $node): string
    {
        return "traffic-sync:node:{$node->id}";
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
     *
     * @param array<int, array{up:int,down:int,weighted:int}> $deltaMap
     */
    public function applyDeltas(array $deltaMap): void
    {
        if (empty($deltaMap)) return;

        // 只把加权值累加进 traffic_used（原始 up/down 不进这里）
        $deltaMap = array_map(fn ($e) => (int) ($e['weighted'] ?? 0), $deltaMap);
        $deltaMap = array_filter($deltaMap, fn ($weighted) => $weighted > 0);
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
            $periodChunk = array_filter($chunk, fn($uid) => isset($periodUserIds[$uid]), ARRAY_FILTER_USE_KEY);
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
