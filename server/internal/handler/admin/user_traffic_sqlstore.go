package admin

// TrafficStore 的 database/sql 实现（用户×机器 流量只读）。
//
// 窗口 → 表：24h 查小时表（滑动窗口），7d/30d 查天表（业务时区自然日）。
// 三个口径：
//   - weighted：乘节点倍率后的字节，与 users.traffic_used 同源（默认排序、计费口径）；
//   - up / down：面板侧原始字节（未乘倍率）；
//   - nic_*：机器网卡口径（monitor_samples 差分合计），与用户腿并排对照。
// 没有数据的用户/节点不出现在结果里（缺数据 ≠ 0）。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NewTrafficSQLStore 用已有连接池构造流量排行的 TrafficStore。
func NewTrafficSQLStore(db *sql.DB) TrafficStore {
	return &trafficSQLStore{db: db}
}

type trafficSQLStore struct {
	db *sql.DB
}

// businessLocation 对齐 config('panel.business_timezone')（缺省 Asia/Shanghai）。
func businessLocation() *time.Location {
	name := strings.TrimSpace(getenvOr("PANEL_BUSINESS_TIMEZONE", ""))
	if name == "" {
		name = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// getenvOr 是 os.Getenv 的薄包装（便于测试替换）。
var getenvOr = func(key, def string) string {
	if v := strings.TrimSpace(osGetenv(key)); v != "" {
		return v
	}
	return def
}

// osGetenv 由 user_traffic_env.go 赋值（避免在本文件直接 import os 造成测试替换困难）。
var osGetenv func(string) string

// ResolveWindow 对齐 UserTrafficService::resolve：非法值回退 24h。
//
// 24h 走小时表：起点是「24 小时前的整点」，含当前未走完的小时（滑动窗口）。
// 7d/30d 走天表：业务时区自然日 [today-6, today] / [today-29, today]。
func (s *trafficSQLStore) ResolveWindow(window string, now time.Time) TrafficWindow {
	tz := businessLocation()
	local := now.In(tz)
	today := local.Format("2006-01-02")

	switch window {
	case "7d":
		start := local.AddDate(0, 0, -6)
		return TrafficWindow{
			Table: "user_traffic_daily", Column: "date",
			Start: start.Format("2006-01-02"), End: today, Key: "7d",
		}
	case "30d":
		start := local.AddDate(0, 0, -29)
		return TrafficWindow{
			Table: "user_traffic_daily", Column: "date",
			Start: start.Format("2006-01-02"), End: today, Key: "30d",
		}
	default:
		// 24h：now-1day 的整点 → now（字面量按 app.timezone=UTC 存）
		start := now.UTC().Add(-24 * time.Hour).Truncate(time.Hour)
		end := now.UTC()
		return TrafficWindow{
			Table: "user_traffic_hourly", Column: "hour_start",
			Start: start.Format("2006-01-02 15:04:05"),
			End:   end.Format("2006-01-02 15:04:05"),
			Key:   "24h",
		}
	}
}

// DataStart 对齐 UserTrafficService::dataStart：表里最早那个桶的业务日期。
func (s *trafficSQLStore) DataStart(ctx context.Context, w TrafficWindow, now time.Time) (*string, error) {
	q := fmt.Sprintf(`SELECT MIN(%s) FROM %s`, w.Column, w.Table)
	var earliest sql.NullString
	if err := s.db.QueryRowContext(ctx, q).Scan(&earliest); err != nil {
		return nil, err
	}
	if !earliest.Valid || earliest.String == "" {
		return nil, nil
	}
	if w.Column == "date" {
		v := earliest.String
		return &v, nil
	}
	// hour_start 是 UTC 字面量，转业务时区取日期
	t := parseDateTimeLenient(earliest.String)
	if t == nil {
		v := earliest.String
		return &v, nil
	}
	day := t.In(businessLocation()).Format("2006-01-02")
	return &day, nil
}

const trafficAggCols = `SUM(t.weighted_delta) AS weighted, SUM(t.up_delta) AS up, SUM(t.down_delta) AS down`

// UserRanking 对齐 UserTrafficService::userRanking（窗口内合计，加权降序）。
func (s *trafficSQLStore) UserRanking(ctx context.Context, w TrafficWindow, limit int64) ([]UserTrafficRow, error) {
	if limit < 1 {
		limit = 50
	}
	q := fmt.Sprintf(`SELECT t.user_id, u.email, p.type, p.name, p.months, %s
		FROM %s t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN plans p ON p.id = u.plan_id
		WHERE t.%s BETWEEN ? AND ?
		GROUP BY t.user_id, u.email, p.type, p.name, p.months
		ORDER BY weighted DESC
		LIMIT ?`, trafficAggCols, w.Table, w.Column)

	rows, err := s.db.QueryContext(ctx, q, w.Start, w.End, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserTrafficRow{}
	for rows.Next() {
		var (
			r          UserTrafficRow
			email      sql.NullString
			planType   sql.NullString
			planName   sql.NullString
			planMonths sql.NullInt64
			weighted   sql.NullInt64
			up         sql.NullInt64
			down       sql.NullInt64
		)
		if err := rows.Scan(&r.UserID, &email, &planType, &planName, &planMonths,
			&weighted, &up, &down); err != nil {
			return nil, err
		}
		r.EmailPtr = nullStrPtr(email)
		r.PlanType = nullStrPtr(planType)
		r.PlanName = nullStrPtr(planName)
		r.PlanMonths = nullIntPtr(planMonths)
		r.Weighted = weighted.Int64
		r.Up = up.Int64
		r.Down = down.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserWeightedTotal 取该窗口内**全部用户**的加权用量合计（占比分母，不受 limit 影响）。
func (s *trafficSQLStore) UserWeightedTotal(ctx context.Context, w TrafficWindow) (int64, error) {
	q := fmt.Sprintf(`SELECT COALESCE(SUM(t.weighted_delta), 0)
		FROM %s t
		WHERE t.%s BETWEEN ? AND ?`, w.Table, w.Column)
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, w.Start, w.End).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// NodeRanking 对齐 UserTrafficService::nodeRanking 的用户腿查询。
// 用户腿三列用指针：有数据的节点才出现，指针必非 nil。
func (s *trafficSQLStore) NodeRanking(ctx context.Context, w TrafficWindow) ([]UserTrafficRow, error) {
	q := fmt.Sprintf(`SELECT t.node_id, n.name, %s
		FROM %s t
		JOIN nodes n ON n.id = t.node_id
		WHERE t.%s BETWEEN ? AND ?
		GROUP BY t.node_id, n.name`, trafficAggCols, w.Table, w.Column)

	rows, err := s.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserTrafficRow{}
	for rows.Next() {
		var (
			r        UserTrafficRow
			name     string
			weighted sql.NullInt64
			up       sql.NullInt64
			down     sql.NullInt64
		)
		if err := rows.Scan(&r.NodeID, &name, &weighted, &up, &down); err != nil {
			return nil, err
		}
		r.Name = name
		wv, uv, dv := weighted.Int64, up.Int64, down.Int64
		r.WeightedPtr, r.UpPtr, r.DownPtr = &wv, &uv, &dv
		out = append(out, r)
	}
	return out, rows.Err()
}

// UserNodeBreakdown 对齐 UserTrafficService::userNodeBreakdown（按加权降序）。
func (s *trafficSQLStore) UserNodeBreakdown(ctx context.Context, w TrafficWindow, userID uint64) ([]UserTrafficRow, error) {
	q := fmt.Sprintf(`SELECT t.node_id, n.name, %s
		FROM %s t
		JOIN nodes n ON n.id = t.node_id
		WHERE t.user_id = ? AND t.%s BETWEEN ? AND ?
		GROUP BY t.node_id, n.name
		ORDER BY weighted DESC`, trafficAggCols, w.Table, w.Column)

	rows, err := s.db.QueryContext(ctx, q, userID, w.Start, w.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserTrafficRow{}
	for rows.Next() {
		var (
			r        UserTrafficRow
			name     string
			weighted sql.NullInt64
			up       sql.NullInt64
			down     sql.NullInt64
		)
		if err := rows.Scan(&r.NodeID, &name, &weighted, &up, &down); err != nil {
			return nil, err
		}
		r.Name = name
		r.Weighted = weighted.Int64
		r.Up = up.Int64
		r.Down = down.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// NodesByID 取节点名与 traffic_multiplier（含停用节点）。
func (s *trafficSQLStore) NodesByID(ctx context.Context, ids []uint64) (map[uint64]NodeTrafficMeta, error) {
	out := map[uint64]NodeTrafficMeta{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	q := fmt.Sprintf(`SELECT id, name, traffic_multiplier FROM nodes WHERE id IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   uint64
			name string
			mult sql.NullFloat64
		)
		if err := rows.Scan(&id, &name, &mult); err != nil {
			return nil, err
		}
		meta := NodeTrafficMeta{Name: name}
		if mult.Valid {
			v := mult.Float64
			meta.TrafficMultiplier = &v
		}
		out[id] = meta
	}
	return out, rows.Err()
}

// DefaultNodeMultiplier 对齐 TrafficSyncService::originalMultiplier。
func (s *trafficSQLStore) DefaultNodeMultiplier(ctx context.Context) (float64, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM site_configs WHERE `+"`key`"+` = ?`, "default_node_multiplier").Scan(&raw)
	if err == sql.ErrNoRows {
		return 1.0, nil
	}
	if err != nil {
		return 0, err
	}
	if !raw.Valid || raw.String == "" {
		return 1.0, nil
	}
	return parseFloatSafe(raw.String), nil
}

// NicSummary 对齐 MonitorService::summary 的网卡口径三列。
//
// 差分口径与 MonitorService::points 相同：窗口内每条样本相对前一条的增量，
// 窗口首条允许取窗口外最近一条；取不到按 0 起算。没有任何样本的节点不出现。
func (s *trafficSQLStore) NicSummary(ctx context.Context, w TrafficWindow) (map[uint64]NicTraffic, error) {
	// monitor 窗口是「now-N」的滑动窗口，与本服务的自然日窗口边界略有出入 ——
	// 两套口径只做量级对照（UserTrafficService::attachMachineColumns 的注释同义）。
	start, end := monitorWindowBounds(w)

	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, sent, recv FROM monitor_samples
		 WHERE sampled_at BETWEEN ? AND ? ORDER BY node_id, sampled_at`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint64]NicTraffic{}
	curNode := uint64(0)
	var prev *monitorSample
	var nodeUp, nodeDown int64
	var nodeHad bool

	flush := func() {
		if curNode != 0 && nodeHad {
			out[curNode] = NicTraffic{Up: nodeUp, Down: nodeDown, Total: nodeUp + nodeDown}
		}
	}

	for rows.Next() {
		var (
			nodeID uint64
			sent   sql.NullInt64
			recv   sql.NullInt64
		)
		if err := rows.Scan(&nodeID, &sent, &recv); err != nil {
			return nil, err
		}
		if nodeID != curNode {
			flush()
			curNode = nodeID
			nodeUp, nodeDown, nodeHad = 0, 0, false
			// 窗口首条的「前一条」：窗口外最近一条
			prev = s.previousSample(ctx, nodeID, start)
		}
		cur := monitorSample{sent: sent.Int64, recv: recv.Int64}
		if prev != nil {
			dSent := cur.sent - prev.sent
			if dSent < 0 {
				dSent = 0
			}
			dRecv := cur.recv - prev.recv
			if dRecv < 0 {
				dRecv = 0
			}
			nodeUp += dSent
			nodeDown += dRecv
			nodeHad = true
		}
		p := cur
		prev = &p
	}
	flush()
	return out, rows.Err()
}

// monitorSample 是 monitor_samples 的累计收发字节（差分基准）。
type monitorSample struct {
	sent int64
	recv int64
}

// previousSample 取窗口外最近一条（取不到 → nil）。
func (s *trafficSQLStore) previousSample(ctx context.Context, nodeID uint64, start string) *monitorSample {
	var (
		sent sql.NullInt64
		recv sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT sent, recv FROM monitor_samples
		 WHERE node_id = ? AND sampled_at < ?
		 ORDER BY sampled_at DESC LIMIT 1`, nodeID, start).Scan(&sent, &recv)
	if err != nil {
		return nil
	}
	return &monitorSample{sent: sent.Int64, recv: recv.Int64}
}

// monitorWindowBounds 把 TrafficWindow 映射到 monitor_samples 的滑动窗口边界。
//
// UserTrafficService 的注释：summary 的窗口是 now-N 滑动窗口，与自然日窗口
// 「边界那一小段本来就不可能逐字节对齐」。这里按窗口键换算：
// 24h → now-24h..now；7d → now-7d..now；30d → now-30d..now。
func monitorWindowBounds(w TrafficWindow) (string, string) {
	end := time.Now().UTC()
	var start time.Time
	switch w.Key {
	case "7d":
		start = end.Add(-7 * 24 * time.Hour)
	case "30d":
		start = end.Add(-30 * 24 * time.Hour)
	default:
		start = end.Add(-24 * time.Hour)
	}
	return start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05")
}
