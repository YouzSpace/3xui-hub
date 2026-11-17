// Package metrics 采集节点运行指标，并据此推算节点的「建议容量」。
//
// 本包只做两件事：
//   - Record：把调用方（3x-ui 驱动 / agent 上报）已经取到的数值写进 node_metrics；
//   - Recommend：用最近 7 天的历史样本反推该节点能承载多少用户。
//
// 取数不在本包范围内：调用方负责从驱动取内存/CPU/连接数，填好 Snapshot 再交给 Record。
//
// 时间口径与全仓一致：datetime 列一律按 UTC 字面量读写（DSN parseTime=true&loc=UTC），
// 所以窗口比较用 UTC 时间参数，不写服务器本地的 NOW() —— 数据库服务器时区与 UTC 可能
// 相差若干小时（本机是 +8），混用会让 7 天窗口和 30 天清理都偏。
package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	// DefaultPerUserKB 样本不足时使用的每用户内存占用默认值（KB）。
	DefaultPerUserKB = 5.0
	// MemCeilingRatio 建议容量按「内存用满该比例」推算，留出系统余量。
	MemCeilingRatio = 0.80
	// ConfidenceHigh/Medium/Low 推荐结果的置信度取值。
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"

	// BasisAbsolute 样本带真实内存绝对值（MB）：每用户 KB 斜率可算。
	BasisAbsolute = "absolute"
	// BasisPercent 样本只有内存百分比（3x-ui 面板 / agent 心跳都只给百分比）：
	// 「每用户 KB」不适用，改按「占用百分比随用户数的变化率」推算。
	BasisPercent = "percent"
	// BasisDefault 样本不足或口径不明：按默认 5 KB/用户估算。
	BasisDefault = "default"

	// percentMemTotal 是「百分比口径」的标记值。
	//
	// 采集器的口径：只有百分比时写 mem_total = 0（明确标记「无绝对值」）；而升级前的旧数据
	// 是把百分比按「总量 100」归一化写进 mem_used/mem_total 的。两者都是百分比口径，
	// 所以 mem_total <= 100 一律按百分比解释 —— 真实机器内存总量不会只有 100MB，
	// 采集器也不会再写 100，撞车风险可忽略。
	percentMemTotal = 100

	// windowDays 推荐使用的历史窗口（天）。
	windowDays = 7
	// retainDays 指标保留天数。
	retainDays = 30
	// cleanupEvery 每写多少次顺手清一次过期数据（不必精确）。
	cleanupEvery = 100
	// minSamples 参与拟合所需的最少样本数。
	minSamples = 10
	// minUserSpread 参与拟合所需的用户数最小跨度。
	minUserSpread = 1000
	// suggestStep Suggest 向上取整的粒度。
	suggestStep = 1000.0
)

// Snapshot 表示一次采样（调用方负责从各驱动取数后填好）。
type Snapshot struct {
	NodeID   int64
	Users    int     // 该节点当前绑定的用户数（user_nodes 计数）
	MemUsed  int     // 内存已用：MemTotal > 100 时是 MB；MemTotal = 0 时是实测百分比（1-100）
	MemTotal int     // 内存总量 MB；0 = 该驱动只有百分比、拿不到绝对值
	CPU      float64 // 百分比
	Conns    int
}

// Recommendation 是一次容量推荐的结果。
type Recommendation struct {
	NodeID     int64
	Suggest    int     // 建议用户数上限
	PerUserKB  float64 // 每用户内存 KB（估算）；百分比口径节点固定 0（不适用）
	Basis      string  // "absolute" / "percent" / "default"，说明 Suggest 按哪个口径算出来
	Confidence string  // "high" / "medium" / "low"
	Samples    int     // 参与计算的数据点数
	Reason     string  // 中文说明，直接给 UI 展示
}

// Store 负责写 node_metrics 并基于历史样本推荐容量。
// 只依赖 *sql.DB，可安全并发调用（写入计数受互斥量保护）。
type Store struct {
	db  *sql.DB
	now func() time.Time

	mu     sync.Mutex
	writes int
}

// NewStore 用给定连接池构造 Store。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// Record 写入一条 node_metrics，并每 cleanupEvery 次写顺手清理 retainDays 天前的旧数据。
// 清理在写入事务之外执行，失败不影响本次写入。
func (s *Store) Record(ctx context.Context, snap Snapshot) error {
	now := s.now().UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO node_metrics (node_id, ts, users, mem_used, mem_total, cpu, conns) VALUES (?, ?, ?, ?, ?, ?, ?)",
		snap.NodeID, now, snap.Users, snap.MemUsed, snap.MemTotal, snap.CPU, snap.Conns)
	if err != nil {
		return fmt.Errorf("写入节点指标失败: %w", err)
	}

	s.mu.Lock()
	s.writes++
	due := s.writes%cleanupEvery == 0
	s.mu.Unlock()

	if due {
		// 清理失败不影响调用方：采样已经落库，下个周期会再清。
		_, _ = s.db.ExecContext(ctx, "DELETE FROM node_metrics WHERE ts < ?", now.AddDate(0, 0, -retainDays))
	}
	return nil
}

// sample 是内存里的一条历史样本。
type sample struct {
	users    int
	memUsed  int
	memTotal int
}

// isPercent 判断一条样本是「只有内存百分比」还是「带真实 MB 绝对值」。
//
//   - mem_used <= 0           → 没有内存数据，两种口径都不算；
//   - mem_total <= 0          → 新采集器：明确标记「无绝对值」，mem_used 里是百分比；
//   - mem_total <= 100        → 旧采集器：百分比按总量 100 归一化写入；
//   - 其余（mem_total > 100） → 真实 MB（原生 xray 驱动暴露字节数后才会出现）。
func (sm sample) isPercent() bool {
	if sm.memUsed <= 0 {
		return false
	}
	return sm.memTotal <= percentMemTotal
}

// Recommend 根据最近 7 天数据推荐容量上限。
//
// 两个驱动目前都只给**内存百分比**（3x-ui 的 /server/status、agent 心跳的 nodes.mem），
// 只有百分比时「每用户 MB 斜率」算出来的数看着像真的、其实是错的（旧实现会给出
// 0.17 KB/用户 这种值），所以按样本口径分三档：
//
//	absolute（mem_total > 100 且 mem_used > 0）：
//	  1. 每用户内存 = (mem_high-mem_low)/(users_high-users_low)（MB/人 → ×1024 转 KB）；
//	  2. 剩余可用内存 = mem_total*80% - 当前 mem_used，
//	     Suggest = 当前 users + 剩余(MB)*1024/每用户内存，PerUserKB 返回该斜率；
//	percent（只有百分比：mem_total = 0，或旧数据的 mem_total = 100）：
//	  1. pctPerUser = (pct_high-pct_low)/(users_high-users_low)（百分比/人）；
//	  2. 剩余百分比 = 80 - 当前 pct，Suggest = 当前 users + 剩余百分比/pctPerUser；
//	  3. PerUserKB 返回 0（口径不适用），Basis = "percent"；
//	default（样本 < 10 条 / 用户数跨度 < 1000 / 内存没随用户数增长）：
//	  Basis = "default"，PerUserKB = 5.0；有绝对值时按 mem_total*80%/5KB 估，
//	  只有百分比时按当前占用比例线性外推到 80%。
//
// 口径按**最新一条样本**判定，再把窗口内同口径的样本挑出来参与计算：升级采集器的当天，
// 窗口里会同时有 mem_total=100（旧）和 mem_total=0（新）的数据，两者都是百分比口径，
// 不会被切成两段；真正带 MB 的样本才会走 absolute。
//
// Suggest 向上取整到 1000 的倍数；已经超出建议容量时照样返回，Reason 里写明。
func (s *Store) Recommend(ctx context.Context, nodeID int64) (Recommendation, error) {
	rec := Recommendation{NodeID: nodeID, PerUserKB: DefaultPerUserKB, Basis: BasisDefault, Confidence: ConfidenceLow}

	since := s.now().UTC().AddDate(0, 0, -windowDays)
	rows, err := s.db.QueryContext(ctx,
		"SELECT users, mem_used, mem_total FROM node_metrics WHERE node_id = ? AND ts >= ? ORDER BY ts ASC, id ASC",
		nodeID, since)
	if err != nil {
		return rec, fmt.Errorf("查询节点指标失败: %w", err)
	}
	defer rows.Close()

	var samples []sample
	for rows.Next() {
		var sm sample
		if err := rows.Scan(&sm.users, &sm.memUsed, &sm.memTotal); err != nil {
			return rec, fmt.Errorf("读取节点指标失败: %w", err)
		}
		samples = append(samples, sm)
	}
	if err := rows.Err(); err != nil {
		return rec, fmt.Errorf("读取节点指标失败: %w", err)
	}
	if len(samples) == 0 {
		rec.Reason = "最近 7 天没有采样数据，无法推算建议容量"
		return rec, nil
	}

	// 口径看最新一条样本；没有内存数据的样本（mem_used = 0）不参与拟合。
	percent := samples[len(samples)-1].isPercent()
	usable := make([]sample, 0, len(samples))
	for _, sm := range samples {
		if sm.memUsed > 0 && sm.isPercent() == percent {
			usable = append(usable, sm)
		}
	}
	rec.Samples = len(usable)
	if rec.Samples == 0 {
		rec.Reason = "最近 7 天没有采到内存数据（节点未上报内存占用），无法推算建议容量"
		return rec, nil
	}

	low, high := usable[0], usable[0]
	for _, sm := range usable {
		if sm.users < low.users {
			low = sm // 同值时保留更早的一条
		}
		if sm.users >= high.users {
			high = sm // 同值时取更晚的一条
		}
	}
	cur := usable[len(usable)-1]

	spreadUsers := high.users - low.users
	spreadRatio := 0.0
	if low.users > 0 {
		spreadRatio = float64(spreadUsers) / float64(low.users)
	}
	fittable := rec.Samples >= minSamples && spreadUsers >= minUserSpread

	var (
		fitted     bool
		pctPerUser float64 // percent 口径的斜率（百分比/人），给 Reason 用
		suggest    float64
	)

	if percent {
		rec.Basis = BasisPercent
		// 百分比口径下「每用户 KB」没有意义，显式返回 0，免得上游把估算数当真。
		rec.PerUserKB = 0
		if fittable {
			pctPerUser = float64(high.memUsed-low.memUsed) / float64(spreadUsers)
		}
		if pctPerUser > 0 {
			fitted = true
			suggest = float64(cur.users) + (MemCeilingRatio*100-float64(cur.memUsed))/pctPerUser
		} else {
			// 数据不足：百分比本身是实测值，按当前占用比例线性外推到 80% 上限。
			rec.Basis = BasisDefault
			rec.PerUserKB = DefaultPerUserKB
			suggest = float64(cur.users)
			if cur.memUsed > 0 {
				suggest = float64(cur.users) * MemCeilingRatio * 100 / float64(cur.memUsed)
			}
		}
	} else {
		rec.Basis = BasisAbsolute
		var perUserKB float64
		if fittable {
			perUserKB = float64(high.memUsed-low.memUsed) / float64(spreadUsers) * 1024
		}
		remainingMB := float64(cur.memTotal)*MemCeilingRatio - float64(cur.memUsed)
		if perUserKB > 0 {
			fitted = true
			rec.PerUserKB = perUserKB
			suggest = float64(cur.users) + remainingMB*1024/perUserKB
		} else {
			rec.Basis = BasisDefault
			rec.PerUserKB = DefaultPerUserKB
			suggest = float64(cur.users) + remainingMB*1024/rec.PerUserKB
		}
	}

	switch {
	case !fitted:
		rec.Confidence = ConfidenceLow
	case rec.Samples >= 20 && spreadRatio >= 0.30:
		rec.Confidence = ConfidenceHigh
	case spreadRatio >= 0.10:
		rec.Confidence = ConfidenceMedium
	default:
		rec.Confidence = ConfidenceLow
	}

	if suggest < 0 {
		suggest = 0
	}
	rec.Suggest = int(math.Ceil(suggest/suggestStep-1e-9) * suggestStep)

	rec.Reason = recommendReason(rec, low, high, cur, spreadUsers, pctPerUser, fitted)
	return rec, nil
}

// recommendReason 拼中文说明，直接给 UI 展示。三种口径各给一句能自证的话。
func recommendReason(rec Recommendation, low, high, cur sample, spreadUsers int, pctPerUser float64, fitted bool) string {
	var reason string
	switch {
	case rec.Basis == BasisPercent:
		reason = fmt.Sprintf("最近 7 天 %d 个数据点，内存占用每万用户约涨 %.1f%%，当前已用 %.0f%%，按 %.0f%% 上限推算",
			rec.Samples, pctPerUser*10000, float64(cur.memUsed), MemCeilingRatio*100)
	case rec.Basis == BasisAbsolute:
		reason = fmt.Sprintf("最近 7 天 %d 个数据点，每用户约 %.1f KB，内存已用 %d/%d MB，按 %.0f%% 内存上限推算",
			rec.Samples, rec.PerUserKB, cur.memUsed, cur.memTotal, MemCeilingRatio*100)
	case rec.Samples < minSamples:
		reason = fmt.Sprintf("最近 7 天仅 %d 个数据点，数据不足，按默认 %.1f KB/用户估算（3-7 天后更准）",
			rec.Samples, DefaultPerUserKB)
	case spreadUsers < minUserSpread:
		reason = fmt.Sprintf("最近 7 天 %d 个数据点，但用户数只从 %d 变到 %d（跨度 %d 人，数据不足：至少需要 %d 人），按默认 %.1f KB/用户估算（3-7 天后更准）",
			rec.Samples, low.users, high.users, spreadUsers, minUserSpread, DefaultPerUserKB)
	case cur.isPercent():
		reason = fmt.Sprintf("最近 7 天 %d 个数据点，但内存占用没有随用户数增长，数据不足，按当前占用比例外推（3-7 天后更准）",
			rec.Samples)
	default:
		reason = fmt.Sprintf("最近 7 天 %d 个数据点，但内存占用没有随用户数增长，数据不足，按默认 %.1f KB/用户估算（3-7 天后更准）",
			rec.Samples, DefaultPerUserKB)
	}

	if fitted && rec.Suggest <= cur.users {
		reason += fmt.Sprintf("；当前 %d 人已超建议容量 %d，建议扩容内存或增加节点", cur.users, rec.Suggest)
	}
	return reason
}
