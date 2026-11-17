package metrics

// 容量告警（#地区功能-21）：节点 / 地区的「已绑 ÷ 容量」水位过线时产出一条告警。
//
// 三个设计取舍写在这里，改代码前先读：
//   - **实时算、不落库**：告警只是当前状态的函数（nodes.capacity + user_nodes 计数），
//     落一张历史表就得再配写入时机、去重、状态迁移三套逻辑，对这个面板没有收益。
//   - **两级阈值，写死常量**：>=95% critical、>=85% warn。多一个配置项只是多一个能配错
//     的地方；真要改口径就改这两个常量，测试会跟着红。
//   - **口径必须与地区总览一致**（region_models.go 顶部的「统计口径」那段）：
//     节点的 used 是 user_nodes 行数；地区的 used 是「该地区所有节点的绑定之和」，
//     地区的 capacity 是「该地区所有节点 capacity 之和」。同一份数据两处算法不一致，
//     看板数字就会互相打脸。
//
// 只对 capacity IS NOT NULL AND capacity > 0 的对象算：NULL = 不限容量，没有水位可言。

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

const (
	// AlertWarnRatio 使用率达到该比例出 warn 告警（85%）。
	AlertWarnRatio = 0.85
	// AlertCriticalRatio 使用率达到该比例出 critical 告警（95%）。
	AlertCriticalRatio = 0.95

	// AlertLevelWarn / AlertLevelCritical 是告警级别取值，前端据此判色。
	AlertLevelWarn     = "warn"
	AlertLevelCritical = "critical"

	// AlertScopeNode / AlertScopeRegion 是告警对象类型。
	AlertScopeNode   = "node"
	AlertScopeRegion = "region"
)

// Alert 是一条实时告警（不落库，每次调用现算）。
type Alert struct {
	Level    string  // AlertLevelWarn / AlertLevelCritical
	Scope    string  // AlertScopeNode / AlertScopeRegion
	ID       int64   // 节点 id 或地区 id
	Name     string  // 节点名或地区名
	Used     int64   // 已绑人数（地区取该地区所有节点的绑定之和）
	Capacity int64   // 容量（地区取该地区所有节点 capacity 之和）
	Ratio    float64 // Used / Capacity，可能 > 1（已超容量）
	Message  string  // 中文说明，直接给 UI 展示
}

// AlertCounts 是各级别告警条数。总览与告警列表共用这一份口径。
type AlertCounts struct {
	Warn     int64
	Critical int64
}

// ComputeAlerts 计算当前所有节点与地区的告警（实时算，不落库）。
//
// 返回的切片保证非 nil（没有告警时是空切片，不是 nil），排序见 sortAlerts。
func ComputeAlerts(ctx context.Context, db *sql.DB) ([]Alert, error) {
	nodeAlerts, err := computeNodeAlerts(ctx, db)
	if err != nil {
		return nil, err
	}
	regionAlerts, err := computeRegionAlerts(ctx, db)
	if err != nil {
		return nil, err
	}

	out := make([]Alert, 0, len(nodeAlerts)+len(regionAlerts))
	out = append(out, nodeAlerts...)
	out = append(out, regionAlerts...)
	sortAlerts(out)
	return out, nil
}

// CountAlerts 数出 warn / critical 各多少条。
func CountAlerts(alerts []Alert) AlertCounts {
	var c AlertCounts
	for _, a := range alerts {
		switch a.Level {
		case AlertLevelCritical:
			c.Critical++
		case AlertLevelWarn:
			c.Warn++
		}
	}
	return c
}

// computeNodeAlerts 算节点级告警：used = 该节点的绑定条数。
//
// 计数交给 SQL（user_nodes 有 idx_node(node_id)）：绑定表可能有上百万行，
// 拉回 Go 再数没有任何好处。
func computeNodeAlerts(ctx context.Context, db *sql.DB) ([]Alert, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.id, n.name, n.capacity,
		       (SELECT COUNT(*) FROM user_nodes un WHERE un.node_id = n.id) AS bound_users
		FROM nodes n
		WHERE n.capacity IS NOT NULL AND n.capacity > 0
		ORDER BY n.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询节点容量失败: %w", err)
	}
	defer rows.Close()

	out := []Alert{}
	for rows.Next() {
		var (
			id, capacity, used int64
			name               string
		)
		if err := rows.Scan(&id, &name, &capacity, &used); err != nil {
			return nil, fmt.Errorf("读取节点容量失败: %w", err)
		}
		if a, ok := newAlert(AlertScopeNode, id, name, used, capacity); ok {
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取节点容量失败: %w", err)
	}
	return out, nil
}

// computeRegionAlerts 算地区级告警。
//
// capacity 只累加「非 NULL 且 >0」的节点（INNER JOIN + WHERE 已经把不限容量的节点挡在外面，
// SUM(capacity) 也不会把 NULL 当 0）；used 则数该地区**所有**节点的绑定 —— capacity 为 NULL
// 的节点上的用户照样在吃这个地区声明的容量预算，漏掉他们会让地区水位偏低、告警来得太晚。
//
// 一个地区如果没有任何一个节点设了容量，就没有水位可比，不出告警（GROUP BY 里不会出现）。
func computeRegionAlerts(ctx context.Context, db *sql.DB) ([]Alert, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.id, r.name, SUM(n.capacity) AS capacity,
		       (SELECT COUNT(*) FROM user_nodes un
		        INNER JOIN nodes nd ON nd.id = un.node_id
		        WHERE nd.region_id = r.id) AS bound_users
		FROM regions r
		INNER JOIN nodes n ON n.region_id = r.id
		WHERE n.capacity IS NOT NULL AND n.capacity > 0
		GROUP BY r.id, r.name
		ORDER BY r.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询地区容量失败: %w", err)
	}
	defer rows.Close()

	out := []Alert{}
	for rows.Next() {
		var (
			id, capacity, used int64
			name               string
		)
		if err := rows.Scan(&id, &name, &capacity, &used); err != nil {
			return nil, fmt.Errorf("读取地区容量失败: %w", err)
		}
		if a, ok := newAlert(AlertScopeRegion, id, name, used, capacity); ok {
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取地区容量失败: %w", err)
	}
	return out, nil
}

// newAlert 按水位定级并拼文案；没过 warn 线返回 ok=false（不产生告警）。
// 调用方保证 capacity > 0。
func newAlert(scope string, id int64, name string, used, capacity int64) (Alert, bool) {
	ratio := float64(used) / float64(capacity)
	level := alertLevel(ratio)
	if level == "" {
		return Alert{}, false
	}
	return Alert{
		Level:    level,
		Scope:    scope,
		ID:       id,
		Name:     name,
		Used:     used,
		Capacity: capacity,
		Ratio:    ratio,
		Message:  alertMessage(scope, name, used, capacity, ratio, level),
	}, true
}

// alertLevel 按水位定级：>=95% critical，>=85% warn，其余返回空串（不告警）。
//
// 注意这里是「水位到达即降级更严」的顺序判断：96% 只出 critical 一条，不会再出 warn。
func alertLevel(ratio float64) string {
	switch {
	case ratio >= AlertCriticalRatio:
		return AlertLevelCritical
	case ratio >= AlertWarnRatio:
		return AlertLevelWarn
	}
	return ""
}

// alertMessage 拼中文说明，管理端直接显示（运营看的是「谁快满了、满了多少」，不是比例数字）。
func alertMessage(scope, name string, used, capacity int64, ratio float64, level string) string {
	hint := "接近上限，建议提前扩容"
	switch {
	case used > capacity:
		hint = "已超过容量上限，请立即扩容或迁出用户"
	case level == AlertLevelCritical:
		hint = "已超警戒线，请立即扩容或迁出用户"
	}
	return fmt.Sprintf("%s %s 容量已用 %d/%d（%.1f%%），%s",
		alertScopeLabel(scope), name, used, capacity, ratio*100, hint)
}

// alertScopeLabel 把 scope 翻成中文（文案里不能出现英文 node/region）。
func alertScopeLabel(scope string) string {
	if scope == AlertScopeRegion {
		return "地区"
	}
	return "节点"
}

// sortAlerts 排序：critical 在前，同级按使用率降序；再按 scope/id 兜底。
//
// 兜底是为了「同样的数据每次顺序一致」—— 前端列表不跟着数据库返回顺序抖动。
func sortAlerts(alerts []Alert) {
	sort.SliceStable(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if a.Level != b.Level {
			return alertLevelRank(a.Level) < alertLevelRank(b.Level)
		}
		if a.Ratio != b.Ratio {
			return a.Ratio > b.Ratio
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.ID < b.ID
	})
}

// alertLevelRank 给排序用：critical = 0（排前面），其余 = 1。
func alertLevelRank(level string) int {
	if level == AlertLevelCritical {
		return 0
	}
	return 1
}
