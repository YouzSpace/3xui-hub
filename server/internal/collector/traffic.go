package collector

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 流量同步：逐行对齐 TrafficSyncService 的
// syncNodeFromSource → commitNodeStats → syncNodeBatch → upsertSnapshots /
// applyDeltas / recordUserTraffic / pruneExpiredHourly，外加漂移告警（issue #56）。
//
// 口径（behavior-contract §2）：
//   - 快照存**原始累计值** traffic_snapshots.upload/.download；
//   - 增量 = 本轮累计 − 上轮快照；本轮 < 上轮 → 计费增量 0、快照推进为新基准；
//   - 加权 = floor(delta × 倍率)，倍率只乘 delta；
//   - 周期套餐同时累加 monthly_traffic_used；
//   - 幂等：以最新快照为基准，重放同一采样不重复扣。

// originalMultiplierKey 是原始倍率的 site_configs key（与 PHP 常量逐字一致）。
const originalMultiplierKey = "default_node_multiplier"

// batchChunkSize 是批量写入/更新的分批大小（PHP 的 array_chunk(..., 500)）。
const batchChunkSize = 500

// trafficLockTTL 是节点流量锁的持有时长（PHP Cache::lock(name, 120)）。
const trafficLockTTL = 120 * time.Second

// ClientStat 是一个 email 在节点上的累计流量（3x-ui 上报口径 up/down）。
type ClientStat struct {
	Up     int64
	Down   int64
	Enable bool
}

// Delta 是单个用户的增量（up/down 用于留痕展示，weighted 才是计费口径）。
type Delta struct {
	Up       int64
	Down     int64
	Weighted int64
}

// SyncResult 复刻 syncNodeFromSource() 的返回值。
type SyncResult struct {
	// Acquired 表示本轮是否拿到节点锁并完成落账（false = 并发同步进行中，本轮跳过）。
	Acquired bool
	// DeltaMap 是 userID → 增量（只含非零项）。
	DeltaMap map[int64]Delta
	// SnapshotCount 是本轮写入的快照行数（PHP 的 count($result['snapshotData'])）。
	SnapshotCount int
	// DriftUserIDs 是「节点上 client 被关、面板仍放行」的用户（仅记录，不自动修）。
	DriftUserIDs []int64
}

// statSource 是流量取数回调（在节点锁内被调用，保证采样顺序与快照顺序一致）。
type statSource func(ctx context.Context) (map[string]ClientStat, error)

// SyncNode 复刻 syncNodeFromSource()：节点锁内「取数 → 算增量 → 落账」。
//
// 锁与 PHP 共用 cache_locks 表（key = {prefix}traffic-sync:node:{id}），
// 所以 PHP 的 traffic:sync 与 Go 的采集不会对同一节点并发落账。
func (c *Collector) SyncNode(ctx context.Context, nodeID int64, source statSource) (SyncResult, error) {
	empty := SyncResult{DeltaMap: map[int64]Delta{}}

	node, err := c.loadNode(ctx, nodeID)
	if err != nil {
		return empty, err
	}
	if node == nil {
		return empty, fmt.Errorf("节点 #%d 不存在", nodeID)
	}

	lockKey := fmt.Sprintf("traffic-sync:node:%d", nodeID)
	owner, ok, err := c.acquireNodeLock(ctx, lockKey, trafficLockTTL)
	if err != nil {
		return empty, err
	}
	if !ok {
		return empty, nil // acquired=false：并发同步正在进行
	}
	defer c.releaseNodeLock(context.WithoutCancel(ctx), lockKey, owner)

	stats, err := source(ctx)
	if err != nil {
		return empty, err
	}

	res, err := c.commitNodeStats(ctx, *node, stats)
	if err != nil {
		return empty, err
	}
	res.Acquired = true
	return res, nil
}

// snapshotRow 是一行待写快照（存原始累计值）。
//
// NodeID 必须带上：upsertSnapshots 的 INSERT 写了 (user_id, node_id, upload, download)
// 四个占位符，缺 NodeID 会让参数少一个 → 整批 node 的流量同步全部报
// "sql: expected N arguments, got M" 而回滚，流量记不上账。
type snapshotRow struct {
	UserID   int64
	NodeID   int64
	Upload   int64
	Download int64
}

// batchResult 是 syncNodeBatch 的产物。
type batchResult struct {
	DeltaMap    map[int64]Delta
	Snapshots   []snapshotRow
	PeriodUsers map[int64]bool
	DriftUsers  []int64
}

// commitNodeStats 复刻 commitNodeStats()：小时表懒删除（事务外）→ 事务内落账。
func (c *Collector) commitNodeStats(ctx context.Context, node nodeRow, clientStats map[string]ClientStat) (SyncResult, error) {
	out := SyncResult{DeltaMap: map[int64]Delta{}}
	if len(clientStats) == 0 {
		return out, nil
	}

	batch, err := c.syncNodeBatch(ctx, node, clientStats)
	if err != nil {
		return out, err
	}

	// 小时表过期清理：事务外、每小时至多一次、失败只记 warning（不连累落账）。
	c.pruneExpiredHourly(ctx)

	now := c.now()
	err = c.inTx(ctx, func(tx *sql.Tx) error {
		if err := upsertSnapshots(ctx, tx, batch.Snapshots); err != nil {
			return err
		}
		if err := applyDeltas(ctx, tx, batch.DeltaMap, batch.PeriodUsers); err != nil {
			return err
		}
		return recordUserTraffic(ctx, tx, node.ID, batch.DeltaMap, now)
	})
	if err != nil {
		return out, err
	}

	out.DeltaMap = batch.DeltaMap
	out.SnapshotCount = len(batch.Snapshots)
	out.DriftUserIDs = batch.DriftUsers
	return out, nil
}

// userRow 是 syncNodeBatch 需要的用户字段。
type userRow struct {
	ID                 int64
	Enabled            bool
	TrafficDisabledAt  sql.NullTime
	ExpiredAt          sql.NullTime
	TrafficLimit       int64
	TrafficUsed        int64
	MonthlyTrafficUsed int64
	MonthlyLimit       int64
	PlanID             sql.NullInt64
	Period             bool
}

// syncNodeBatch 复刻 syncNodeBatch()：批量算增量 + 漂移检测。
func (c *Collector) syncNodeBatch(ctx context.Context, node nodeRow, clientStats map[string]ClientStat) (batchResult, error) {
	out := batchResult{DeltaMap: map[int64]Delta{}, PeriodUsers: map[int64]bool{}}
	if len(clientStats) == 0 {
		return out, nil
	}

	// 1. clientEmail（ch_user_{id}）→ userId
	emailToUserID := map[string]int64{}
	userIDs := make([]int64, 0, len(clientStats))
	emails := make([]string, 0, len(clientStats))
	for email := range clientStats {
		emails = append(emails, email)
	}
	sort.Strings(emails) // 固定遍历顺序：SQL 日志/对拍可复现
	for _, email := range emails {
		if id, ok := parseClientEmail(email); ok {
			emailToUserID[email] = id
			userIDs = append(userIDs, id)
		}
	}
	if len(emailToUserID) == 0 {
		return out, nil
	}

	// 2. 批量查最新快照、3. 批量查有套餐的用户
	snapshots, err := c.latestSnapshots(ctx, node.ID, userIDs)
	if err != nil {
		return out, err
	}
	users, err := c.usersWithPlan(ctx, userIDs)
	if err != nil {
		return out, err
	}

	// 同一节点内所有用户共用一个倍率（PHP 同样循环外取一次）。
	multiplier := c.multiplierFor(ctx, node)

	for _, email := range emails {
		userID, ok := emailToUserID[email]
		if !ok {
			continue
		}
		user, ok := users[userID]
		if !ok {
			continue // 无套餐用户跳过
		}

		stat, ok := clientStats[email]
		if !ok {
			continue
		}

		nowUp, nowDown := stat.Up, stat.Down
		nowTotal := nowUp + nowDown

		// 漂移：节点上 client 已关闭、面板却认为该用户正常 → 只留痕，不在定时任务里修
		// （修复由人工「同步流量」触发，避免和 3x-ui 自己的自动禁用来回拉锯）。
		if !stat.Enable && user.Enabled && !user.TrafficDisabledAt.Valid && banReasonAt(user, c.now()) == "" {
			out.DriftUsers = append(out.DriftUsers, userID)
			c.logf_("节点与面板状态分叉：面板放行、节点 enable=false（node=%d user=%d email=%s）",
				node.ID, userID, email)
			if err := c.recordDrift(ctx, userID, email, node); err != nil {
				c.logf_("写漂移告警失败（不影响落账）: %v", err)
			}
		}

		last, hasLast := snapshots[userID]
		var lastUp, lastDown *int64
		if hasLast {
			lu, ld := last.upload, last.download
			lastUp, lastDown = &lu, &ld
		}

		// 方向增量必须先于「零增量跳过」算出：总量一升一降时总量差为 0，
		// 但那笔流量真实发生过，方向增量不能跟着被跳过。
		upDelta, downDelta := directionalDeltas(nowUp, nowDown, lastUp, lastDown)

		var delta int64
		if !hasLast {
			// 首次同步且计数为 0：不落空快照。
			if nowTotal == 0 {
				continue
			}
			delta = nowTotal // 首次同步：记为基准，不回溯全部历史
		} else {
			lastTotal := last.upload + last.download
			if nowTotal < lastTotal {
				// 计数器倒退（节点重置 / client 重建）：本轮计费增量记 0，快照推进为新基准。
				c.logf_("流量计数倒退（批量同步），本轮计费增量记 0（node=%d user=%d last=%d now=%d）",
					node.ID, userID, lastTotal, nowTotal)
				out.Snapshots = append(out.Snapshots, snapshotRow{
					UserID: userID, NodeID: node.ID, Upload: nowUp, Download: nowDown,
				})
				continue
			}
			delta = nowTotal - lastTotal
			if delta == 0 {
				// 累计值没变 → 不重写快照；但方向增量不为 0 的「一升一降」要留痕，
				// 且快照必须推进（否则重放会把这份方向增量再记一次）。
				if upDelta != 0 || downDelta != 0 {
					out.DeltaMap[userID] = Delta{Up: upDelta, Down: downDelta, Weighted: 0}
					out.Snapshots = append(out.Snapshots, snapshotRow{
						UserID: userID, NodeID: node.ID, Upload: nowUp, Download: nowDown,
					})
				}
				continue
			}
		}

		// 加权增量：倍率只乘在 delta 上，快照仍存原始字节。
		weighted := int64(0)
		if delta > 0 {
			weighted = int64(math.Floor(float64(delta) * multiplier))
		}
		if upDelta > 0 || downDelta > 0 || weighted > 0 {
			out.DeltaMap[userID] = Delta{Up: upDelta, Down: downDelta, Weighted: weighted}
		}
		if user.Period && weighted > 0 {
			out.PeriodUsers[userID] = true
		}

		out.Snapshots = append(out.Snapshots, snapshotRow{
			UserID: userID, NodeID: node.ID, Upload: nowUp, Download: nowDown,
		})
	}

	return out, nil
}

// directionalDeltas 复刻 TrafficSyncService::directionalDeltas()：
// 首次同步整段现值算增量；总量倒退全记 0；其余逐方向取差、负差钳 0。
func directionalDeltas(nowUp, nowDown int64, lastUp, lastDown *int64) (int64, int64) {
	if lastUp == nil || lastDown == nil {
		return nowUp, nowDown
	}
	if nowUp+nowDown < *lastUp+*lastDown {
		return 0, 0
	}
	up := nowUp - *lastUp
	if up < 0 {
		up = 0
	}
	down := nowDown - *lastDown
	if down < 0 {
		down = 0
	}
	return up, down
}

type lastSnapshot struct {
	upload   int64
	download int64
}

// latestSnapshots 对齐 `where(node_id)->whereIn(user_id)->orderByDesc('id')->get()->keyBy('user_id')`。
func (c *Collector) latestSnapshots(ctx context.Context, nodeID int64, userIDs []int64) (map[int64]lastSnapshot, error) {
	out := map[int64]lastSnapshot{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(userIDs)+1)
	args = append(args, nodeID)
	for _, id := range userIDs {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx,
		"SELECT user_id, upload, download FROM traffic_snapshots WHERE node_id = ? AND user_id IN ("+
			placeholders(len(userIDs))+") ORDER BY id DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("查询流量快照失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			uid    int64
			upload int64
			down   int64
		)
		if err := rows.Scan(&uid, &upload, &down); err != nil {
			return nil, fmt.Errorf("读取流量快照失败: %w", err)
		}
		if _, exists := out[uid]; exists {
			continue // 已取到 id 最大的那一行
		}
		out[uid] = lastSnapshot{upload: upload, download: down}
	}
	return out, rows.Err()
}

// usersWithPlan 对齐 `User::whereIn('id')->whereNotNull('plan_id')->with('plan')->get()`。
//
// LEFT JOIN 而非 INNER：plan 行缺失时 PHP 仍拿到该用户（plan=null → isPeriodPlan()=false），
// 只累加 traffic_used。INNER JOIN 会静默少算这类用户。
func (c *Collector) usersWithPlan(ctx context.Context, userIDs []int64) (map[int64]userRow, error) {
	out := map[int64]userRow{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(userIDs))
	for _, id := range userIDs {
		args = append(args, id)
	}
	rows, err := c.db.QueryContext(ctx,
		"SELECT u.id, u.enabled, u.traffic_disabled_at, u.expired_at, u.traffic_limit, u.traffic_used, "+
			"u.monthly_traffic_used, u.monthly_traffic_limit, u.plan_id, p.type "+
			"FROM users u LEFT JOIN plans p ON p.id = u.plan_id "+
			"WHERE u.plan_id IS NOT NULL AND u.id IN ("+placeholders(len(userIDs))+")", args...)
	if err != nil {
		return nil, fmt.Errorf("查询套餐用户失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			u        userRow
			enabled  int64
			planType sql.NullString
		)
		if err := rows.Scan(&u.ID, &enabled, &u.TrafficDisabledAt, &u.ExpiredAt, &u.TrafficLimit,
			&u.TrafficUsed, &u.MonthlyTrafficUsed, &u.MonthlyLimit, &u.PlanID, &planType); err != nil {
			return nil, fmt.Errorf("读取套餐用户失败: %w", err)
		}
		u.Enabled = enabled != 0
		u.Period = planType.Valid && planType.String == "period"
		out[u.ID] = u
	}
	return out, rows.Err()
}

// multiplierFor 复刻 TrafficSyncService::multiplierFor()：
// traffic_multiplier 为 NULL → 继承 site_configs.default_node_multiplier，否则用节点值。
//
// 不做进程内缓存：改倍率必须立刻生效（长驻进程缓存会让同步静默按旧值计费）。
func (c *Collector) multiplierFor(ctx context.Context, node nodeRow) float64 {
	if node.TrafficMultiplier.Valid {
		return node.TrafficMultiplier.Float64
	}
	return c.originalMultiplier(ctx)
}

// originalMultiplier 复刻 originalMultiplier()：配置行不存在 / 空串 → 1.0。
func (c *Collector) originalMultiplier(ctx context.Context) float64 {
	stored := c.siteConfigValue(ctx, originalMultiplierKey, "")
	if stored == "" {
		return 1.0
	}
	f, err := strconv.ParseFloat(stored, 64)
	if err != nil {
		return 1.0
	}
	return f
}

// upsertSnapshots 复刻 upsertSnapshots()：每 user+node 只保留一行。
//
// created_at/updated_at 用 SQL NOW()（与 PHP 逐字一致）——这两列不参与正确性，
// 但并存期两侧的运维查询要能对上。
func upsertSnapshots(ctx context.Context, tx *sql.Tx, rows []snapshotRow) error {
	for start := 0; start < len(rows); start += batchChunkSize {
		end := start + batchChunkSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		var sb strings.Builder
		sb.WriteString("INSERT INTO traffic_snapshots (user_id, node_id, upload, download, created_at, updated_at) VALUES ")
		args := make([]any, 0, len(chunk)*4)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, NOW(), NOW())")
			args = append(args, r.UserID, r.NodeID, r.Upload, r.Download)
		}
		sb.WriteString(" ON DUPLICATE KEY UPDATE upload = VALUES(upload), download = VALUES(download), updated_at = NOW()")

		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("写入流量快照失败: %w", err)
		}
	}
	return nil
}

// applyDeltas 复刻 applyDeltas()：加权值累加进 traffic_used；周期套餐同时累加 monthly_traffic_used。
func applyDeltas(ctx context.Context, tx *sql.Tx, deltaMap map[int64]Delta, periodUsers map[int64]bool) error {
	if len(deltaMap) == 0 {
		return nil
	}
	weighted := make(map[int64]int64, len(deltaMap))
	for uid, d := range deltaMap {
		if d.Weighted > 0 {
			weighted[uid] = d.Weighted
		}
	}
	if len(weighted) == 0 {
		return nil
	}

	ids := sortedKeys(weighted)
	for start := 0; start < len(ids); start += batchChunkSize {
		end := start + batchChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		if err := applyDeltaChunk(ctx, tx, "traffic_used", chunk, weighted); err != nil {
			return err
		}

		periodChunk := make([]int64, 0, len(chunk))
		for _, id := range chunk {
			if periodUsers[id] {
				periodChunk = append(periodChunk, id)
			}
		}
		if len(periodChunk) > 0 {
			if err := applyDeltaChunk(ctx, tx, "monthly_traffic_used", periodChunk, weighted); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyDeltaChunk 复刻 `UPDATE users SET col = col + CASE id WHEN ? THEN ? … END WHERE id IN (…)`。
func applyDeltaChunk(ctx context.Context, tx *sql.Tx, column string, ids []int64, weighted map[int64]int64) error {
	var sb strings.Builder
	args := make([]any, 0, len(ids)*3)
	sb.WriteString("UPDATE users SET ")
	sb.WriteString(column)
	sb.WriteString(" = ")
	sb.WriteString(column)
	sb.WriteString(" + CASE id")
	for _, id := range ids {
		sb.WriteString(" WHEN ? THEN ?")
		args = append(args, id, weighted[id])
	}
	sb.WriteString(" END WHERE id IN (")
	sb.WriteString(placeholders(len(ids)))
	sb.WriteString(")")
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("累加 %s 失败: %w", column, err)
	}
	return nil
}

// recordUserTraffic 复刻 recordUserTraffic()：小时表（UTC 小时桶）+ 天表（业务时区自然日）。
//
// 累加语义（ON DUPLICATE KEY UPDATE 列 = 列 + VALUES(列)）；幂等前提是入参增量以快照为基准算出，
// 且本函数与快照写在同一事务（回滚则三张表一起回滚）。
func recordUserTraffic(ctx context.Context, tx *sql.Tx, nodeID int64, deltaMap map[int64]Delta, now time.Time) error {
	if len(deltaMap) == 0 {
		return nil
	}
	rows := make(map[int64]Delta, len(deltaMap))
	for uid, d := range deltaMap {
		if d.Up == 0 && d.Down == 0 && d.Weighted == 0 {
			continue
		}
		rows[uid] = d
	}
	if len(rows) == 0 {
		return nil
	}

	// hour_start 存应用时区（UTC）的小时起点；date 存业务时区自然日（与 monitor_daily 同口径）。
	hourStart := now.UTC().Truncate(time.Hour)
	date := now.In(businessLocation()).Format("2006-01-02")

	if err := accumulateUserTraffic(ctx, tx, "user_traffic_hourly", "hour_start",
		hourStart.Format("2006-01-02 15:04:05"), nodeID, rows); err != nil {
		return err
	}
	return accumulateUserTraffic(ctx, tx, "user_traffic_daily", "date", date, nodeID, rows)
}

// accumulateUserTraffic 是累加型 upsert（小时表与天表共用同一段逻辑，避免两套漂移）。
func accumulateUserTraffic(ctx context.Context, tx *sql.Tx, table, bucketCol, bucket string, nodeID int64, rows map[int64]Delta) error {
	ids := sortedKeys(rows)

	for start := 0; start < len(ids); start += batchChunkSize {
		end := start + batchChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		var sb strings.Builder
		sb.WriteString("INSERT INTO " + table + " (user_id, node_id, " + bucketCol +
			", up_delta, down_delta, weighted_delta, created_at, updated_at) VALUES ")
		args := make([]any, 0, len(chunk)*6)
		for i, uid := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, NOW(), NOW())")
			d := rows[uid]
			args = append(args, uid, nodeID, bucket, d.Up, d.Down, d.Weighted)
		}
		sb.WriteString(" ON DUPLICATE KEY UPDATE up_delta = up_delta + VALUES(up_delta), " +
			"down_delta = down_delta + VALUES(down_delta), " +
			"weighted_delta = weighted_delta + VALUES(weighted_delta), updated_at = NOW()")

		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", table, err)
		}
	}
	return nil
}

// pruneExpiredHourly 复刻 pruneExpiredHourly()：小时表懒删除，每小时至多一次。
//
// 放事务外、失败只记 warning —— 清理失败绝不能连累落账回滚。
func (c *Collector) pruneExpiredHourly(ctx context.Context) {
	ok, err := c.cacheAdd(ctx, "user-traffic:hourly:prune", "1", time.Hour)
	if err != nil || !ok {
		return // 这一小时内已清过 / 缓存不可用
	}

	retainDays := 7
	if raw := strings.TrimSpace(envOr("USER_TRAFFIC_HOURLY_RETAIN_DAYS", "")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			retainDays = n
		}
	}
	if retainDays < 2 {
		retainDays = 2
	}
	cutoff := c.now().AddDate(0, 0, -retainDays).Truncate(time.Hour)
	if _, err := c.db.ExecContext(ctx,
		"DELETE FROM user_traffic_hourly WHERE hour_start < ? LIMIT 5000", dbDateTime(cutoff)); err != nil {
		c.logf_("user_traffic_hourly 过期清理失败（不影响落账）: %v", err)
	}
}

// recordDrift 复刻 recordDrift()：漂移写 operation_logs，同一 user+node 24h 内只留一条。
//
// 去重理由：同步每 5 分钟一轮，不去重会把日志页（500 条上限）刷满，把真正的管理操作挤掉。
func (c *Collector) recordDrift(ctx context.Context, userID int64, email string, node nodeRow) error {
	const action = "节点与面板状态分叉"
	path := fmt.Sprintf("node#%d/user#%d", node.ID, userID)

	var exists int
	err := c.db.QueryRowContext(ctx,
		"SELECT 1 FROM operation_logs WHERE type = 'system' AND action = ? AND path = ? AND created_at >= ? LIMIT 1",
		action, path, dbDateTime(c.now().Add(-24*time.Hour))).Scan(&exists)
	if err == nil {
		return nil // 24h 内已有告警
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("查询漂移告警失败: %w", err)
	}

	_, err = c.db.ExecContext(ctx,
		"INSERT INTO operation_logs (type, actor_name, action, method, path, status, error, created_at) "+
			"VALUES ('system', 'traffic-sync', ?, 'CLI', ?, 500, ?, ?)",
		action, path,
		fmt.Sprintf("%s 在节点 %s(#%d) 上被关闭，面板仍认为放行（可能是另一套面板在写）", email, node.Name, node.ID),
		dbDateTime(c.now()))
	if err != nil {
		return fmt.Errorf("写入漂移告警失败: %w", err)
	}
	return nil
}

// sortedKeys 返回升序 key（固定批量 SQL 的分支顺序，便于日志对拍）。
func sortedKeys[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// businessLocation 读 PANEL_BUSINESS_TIMEZONE（缺省 Asia/Shanghai）。
func businessLocation() *time.Location {
	name := strings.TrimSpace(envOr("PANEL_BUSINESS_TIMEZONE", ""))
	if name == "" {
		name = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}
