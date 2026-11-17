package admin

// 容量告警接口用例（#地区功能-21）：真连 MySQL，独立库 ch_region21_api_*，自建自删。
//
// 覆盖两条 HTTP 契约（前端按这两份 JSON 写页面，字段名就是契约）：
//   - GET /admin-api/regions/alerts   → {alerts:[...], counts:{warn,critical}}，critical 在前；
//   - GET /admin-api/regions/overview → 响应里新增 counts，与告警接口同一份算法。
//
// 为什么走真库：告警值全是聚合出来的（节点计数 + 地区求和），fake store 只能证明 handler
// 传参，证明不了「地区 used 把不限容量节点上的绑定也算进去」这类口径。
//
// 变异测试锚点：
//   - 删掉 regionOverview 里的 "counts" 那一行 → TestRegion21AlertsOverviewCounts 红；
//   - 把 alertSource 的类型断言改成永远 false → 两个用例都红（counts 全 0、alerts 为空）。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// region21Schema 带进程号：并行跑 go test 时各进程的库互不 DROP。
func region21Schema() string { return fmt.Sprintf("ch_region21_api_%d", os.Getpid()) }

// region21Counts 是 counts 的 JSON 形状。
type region21Counts struct {
	Warn     int64 `json:"warn"`
	Critical int64 `json:"critical"`
}

// region21Alert 是告警一条的 JSON 形状。
type region21Alert struct {
	Level    string  `json:"level"`
	Scope    string  `json:"scope"`
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Used     int64   `json:"used"`
	Capacity int64   `json:"capacity"`
	Ratio    float64 `json:"ratio"`
	Message  string  `json:"message"`
}

// region21SeedNode 插一台节点并返回 id；capacity 传 nil 表示不限容量，regionID 传 0 表示不属于地区。
func region21SeedNode(t *testing.T, db *sql.DB, name string, capacity *int, regionID int64) int64 {
	t.Helper()
	var region any
	if regionID > 0 {
		region = regionID
	}
	res, err := db.Exec(
		"INSERT INTO nodes (name, host, status, capacity, region_id) VALUES (?, ?, 'online', ?, ?)",
		name, name+".example.com", capacity, region)
	if err != nil {
		t.Fatalf("插节点失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取节点 id 失败: %v", err)
	}
	return id
}

// region21SeedBindings 给节点塞 n 条绑定（主键是 (user_id,node_id)，user_id 可跨节点复用）。
func region21SeedBindings(t *testing.T, db *sql.DB, nodeID int64, n int) {
	t.Helper()
	for uid := 1; uid <= n; uid++ {
		if _, err := db.Exec("INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)", uid, nodeID); err != nil {
			t.Fatalf("插绑定失败: %v", err)
		}
	}
}

// region21Int 是取指针的小工具。
func region21Int(v int) *int { return &v }

// TestRegion21AlertsRouteRegistered 钉住路由：路径写错前端直接 404，编译期不会提醒。
func TestRegion21AlertsRouteRegistered(t *testing.T) {
	db := newRegion09DB(t, region21Schema())
	engine := newRegion09Router(t, db)

	for _, r := range engine.Routes() {
		if r.Method == http.MethodGet && r.Path == "/admin-api/regions/alerts" {
			return
		}
	}
	t.Fatalf("路由 GET /admin-api/regions/alerts 没注册（已注册 %d 条）", len(engine.Routes()))
}

// TestRegion21AlertsAPI 走 HTTP 验告警列表：字段、排序、计数、空态。
//
// 数据：香港 2 台各 100 容量（绑 90 / 96）→ 节点 warn 90%、节点 critical 96%、
// 地区 186/200 = 93% warn；日本 1 台不限容量（绑 5）→ 节点与地区都不出告警。
func TestRegion21AlertsAPI(t *testing.T) {
	db := newRegion09DB(t, region21Schema())
	engine := newRegion09Router(t, db)

	if _, err := db.Exec("INSERT INTO regions (name, sort) VALUES ('香港', 1), ('日本', 2)"); err != nil {
		t.Fatalf("造地区失败: %v", err)
	}
	hk1 := region21SeedNode(t, db, "hk-1", region21Int(100), 1)
	hk2 := region21SeedNode(t, db, "hk-2", region21Int(100), 1)
	jp1 := region21SeedNode(t, db, "jp-1", nil, 2)
	region21SeedBindings(t, db, hk1, 90)
	region21SeedBindings(t, db, hk2, 96)
	region21SeedBindings(t, db, jp1, 5)

	resp := region09Call(t, engine, http.MethodGet, "/admin-api/regions/alerts", "")
	if resp.Code != 0 {
		t.Fatalf("取告警失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var data struct {
		Alerts []region21Alert `json:"alerts"`
		Counts region21Counts  `json:"counts"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("解析告警响应失败: %v（data=%s）", err, resp.Data)
	}
	t.Logf("实际结果：alerts=%+v counts=%+v", data.Alerts, data.Counts)

	if len(data.Alerts) != 3 {
		t.Fatalf("应有 3 条告警（节点 critical/warn + 地区 warn），实际 %d 条: %+v", len(data.Alerts), data.Alerts)
	}
	// 顺序：critical 96% → 地区 warn 93% → 节点 warn 90%。
	if a := data.Alerts[0]; a.Level != "critical" || a.Scope != "node" || a.Name != "hk-2" ||
		a.Used != 96 || a.Capacity != 100 || a.Ratio != 0.96 {
		t.Fatalf("第一条应是 hk-2 的 critical：%+v", a)
	}
	if a := data.Alerts[1]; a.Level != "warn" || a.Scope != "region" || a.Name != "香港" ||
		a.Used != 186 || a.Capacity != 200 || a.Ratio != 0.93 {
		t.Fatalf("第二条应是香港地区的 warn（186/200）：%+v", a)
	}
	if a := data.Alerts[2]; a.Level != "warn" || a.Scope != "node" || a.Name != "hk-1" || a.Ratio != 0.9 {
		t.Fatalf("第三条应是 hk-1 的 warn（90%%）：%+v", a)
	}
	if data.Alerts[0].ID != hk2 {
		t.Fatalf("告警里的 id 应是节点 id（%d），实际 %d", hk2, data.Alerts[0].ID)
	}
	if !strings.Contains(data.Alerts[0].Message, "96/100") {
		t.Fatalf("告警文案应带用量: %q", data.Alerts[0].Message)
	}
	if data.Counts.Warn != 2 || data.Counts.Critical != 1 {
		t.Fatalf("counts 应为 warn=2 critical=1，实际 warn=%d critical=%d", data.Counts.Warn, data.Counts.Critical)
	}

	// 比例必须是 PHP 口径的短浮点（0.96 / 0.93 / 0.9），不是 0.960000。
	for _, want := range []string{`"ratio":0.96`, `"ratio":0.93`, `"ratio":0.9`} {
		if !strings.Contains(string(resp.Data), want) {
			t.Fatalf("响应里应有 %s（PHP 浮点口径），实际 data=%s", want, resp.Data)
		}
	}

	// 空态：清空数据后 alerts 必须是 []（不是 null），counts 全 0。
	for _, stmt := range []string{"DELETE FROM user_nodes", "DELETE FROM nodes", "DELETE FROM regions"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("清数据失败（%s）: %v", stmt, err)
		}
	}
	empty := region09Call(t, engine, http.MethodGet, "/admin-api/regions/alerts", "")
	if !strings.Contains(string(empty.Data), `"alerts":[]`) {
		t.Fatalf("没有告警时 alerts 应是空数组 []，实际 data=%s", empty.Data)
	}
	if !strings.Contains(string(empty.Data), `"counts":{"warn":0,"critical":0}`) {
		t.Fatalf("没有告警时 counts 应全 0，实际 data=%s", empty.Data)
	}
}

// TestRegion21AlertsOverviewCounts 钉住「总览里带 counts」：同一份数据、
// 同一套算法，数字必须与 /regions/alerts 完全一致（两处不一致前端角标会说谎）。
func TestRegion21AlertsOverviewCounts(t *testing.T) {
	db := newRegion09DB(t, region21Schema())
	engine := newRegion09Router(t, db)

	if _, err := db.Exec("INSERT INTO regions (name, sort) VALUES ('香港', 1)"); err != nil {
		t.Fatalf("造地区失败: %v", err)
	}
	warnNode := region21SeedNode(t, db, "hk-1", region21Int(100), 1)
	critNode := region21SeedNode(t, db, "hk-2", region21Int(100), 1)
	region21SeedBindings(t, db, warnNode, 85) // 85% → warn（恰好压线）
	region21SeedBindings(t, db, critNode, 96) // 96% → critical
	// 地区：181/200 = 90.5% → warn；所以总览应是 warn=2 critical=1。

	resp := region09Call(t, engine, http.MethodGet, "/admin-api/regions/overview", "")
	if resp.Code != 0 {
		t.Fatalf("取总览失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var data struct {
		NodeTotal int64          `json:"node_total"`
		Counts    region21Counts `json:"counts"`
		Regions   []any          `json:"regions"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("解析总览失败: %v（data=%s）", err, resp.Data)
	}
	t.Logf("实际结果：node_total=%d counts=%+v（data=%s）", data.NodeTotal, data.Counts, resp.Data)

	if data.NodeTotal != 2 {
		t.Fatalf("总览原有字段不能丢：node_total 期望 2，实际 %d", data.NodeTotal)
	}
	if len(data.Regions) != 1 {
		t.Fatalf("总览原有字段不能丢：regions 期望 1 个，实际 %d 个", len(data.Regions))
	}
	if !strings.Contains(string(resp.Data), `"counts":{`) {
		t.Fatalf("总览响应里必须有 counts 字段，实际 data=%s", resp.Data)
	}
	if data.Counts.Warn != 2 || data.Counts.Critical != 1 {
		t.Fatalf("总览 counts 应为 warn=2 critical=1（告警接口同口径），实际 warn=%d critical=%d",
			data.Counts.Warn, data.Counts.Critical)
	}

	// 与告警接口对拍：两份 counts 必须一模一样。
	alertsResp := region09Call(t, engine, http.MethodGet, "/admin-api/regions/alerts", "")
	var alertsData struct {
		Alerts []region21Alert `json:"alerts"`
		Counts region21Counts  `json:"counts"`
	}
	if err := json.Unmarshal(alertsResp.Data, &alertsData); err != nil {
		t.Fatalf("解析告警响应失败: %v（data=%s）", err, alertsResp.Data)
	}
	if alertsData.Counts != data.Counts {
		t.Fatalf("总览 counts(%+v) 与告警接口 counts(%+v) 不一致 —— 两处口径必须同源",
			data.Counts, alertsData.Counts)
	}
	if int64(len(alertsData.Alerts)) != alertsData.Counts.Warn+alertsData.Counts.Critical {
		t.Fatalf("counts 之和应等于告警条数：counts=%+v 条数=%d", alertsData.Counts, len(alertsData.Alerts))
	}
	for _, a := range alertsData.Alerts {
		if a.Level != "warn" && a.Level != "critical" {
			t.Fatalf("告警级别只能是 warn/critical，实际 %q", a.Level)
		}
		if a.Scope != "node" && a.Scope != "region" {
			t.Fatalf("告警对象只能是 node/region，实际 %q", a.Scope)
		}
	}
}
