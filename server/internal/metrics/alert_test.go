package metrics

// 容量告警用例（#地区功能-21）：真连 MySQL，独立库 ch_region21_*，自建自删。
//
// 为什么必须真库：告警的每一条判据都落在 SQL 语义上 ——
//   - 节点 used 是 user_nodes 的相关子查询计数（不是内存里的数）；
//   - 地区 used 是「该地区所有节点的绑定之和」，capacity 是「该地区所有节点 capacity 之和」，
//     两个过滤条件放错位置（比如把 WHERE capacity>0 也套到 used 上）就偏；
//   - 「capacity IS NULL / capacity <= 0 不告警」是 WHERE 条件，fake store 证明不了。
//
// 变异测试锚点：把 alert.go 的 AlertCriticalRatio 从 0.95 改成 1.00，
// TestComputeAlertsNodeLevels 里「96 → critical」必红（96% 会退成 warn）。

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// alertSchema 带进程号：并行跑 go test 时各进程的库互不 DROP。
var alertSchema = fmt.Sprintf("ch_region21_alert_%d", os.Getpid())

// alertDSN 连本机 MySQL（root/空密码），可用 REGION21_MYSQL_DSN 覆盖
// （没配时沿用 metrics 其它用例的 REGION11_MYSQL_DSN，同一台机器只配一次即可）。
func alertDSN(schema string) string {
	dsn := os.Getenv("REGION21_MYSQL_DSN")
	if dsn == "" {
		dsn = os.Getenv("REGION11_MYSQL_DSN")
	}
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
	}
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	return head + schema + tail
}

// alertDDL 只建告警真正读的 3 张表（列与 cmd/server/schema.go 对齐，省掉无关列）。
var alertDDL = []string{
	`CREATE TABLE regions (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		sort int NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY regions_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		capacity int DEFAULT NULL,
		region_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_nodes_region (region_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// openAlertTestDB 建一个干净的库 + 3 张表；连不上 MySQL 就 skip（打桩证明不了 SQL 口径）。
func openAlertTestDB(t *testing.T) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", alertDSN(""))
	if err != nil {
		t.Skipf("打不开 MySQL 连接（%v），跳过容量告警集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过容量告警集成测试", err)
	}
	mustExec(t, admin, "DROP DATABASE IF EXISTS "+alertSchema)
	mustExec(t, admin, "CREATE DATABASE "+alertSchema+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + alertSchema)
		admin.Close()
	})

	db, err := sql.Open("mysql", alertDSN(alertSchema))
	if err != nil {
		t.Fatalf("连测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, ddl := range alertDDL {
		mustExec(t, db, ddl)
	}
	return db
}

// alertSeedRegion 插一个地区并返回 id。
func alertSeedRegion(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	res, err := db.Exec("INSERT INTO regions (name, sort) VALUES (?, 0)", name)
	if err != nil {
		t.Fatalf("插地区失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取地区 id 失败: %v", err)
	}
	return id
}

// alertSeedNode 插一台节点并返回 id；capacity/regionID 传 nil 表示「不限容量 / 不属于地区」。
func alertSeedNode(t *testing.T, db *sql.DB, name string, capacity, regionID *int64) int64 {
	t.Helper()
	res, err := db.Exec("INSERT INTO nodes (name, host, capacity, region_id) VALUES (?, ?, ?, ?)",
		name, name+".example.com", capacity, regionID)
	if err != nil {
		t.Fatalf("插节点失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取节点 id 失败: %v", err)
	}
	return id
}

// alertSeedBindings 给节点塞 n 条绑定。主键是 (user_id,node_id)，user_id 从 1 起复用无妨。
func alertSeedBindings(t *testing.T, db *sql.DB, nodeID int64, n int) {
	t.Helper()
	for uid := 1; uid <= n; uid++ {
		if _, err := db.Exec("INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)", uid, nodeID); err != nil {
			t.Fatalf("插绑定失败: %v", err)
		}
	}
}

// alertInt 是「取指针」的小工具（capacity/region_id 是可空列）。
func alertInt(v int64) *int64 { return &v }

// alertFind 按 scope+名称找一条告警；找不到返回 nil。
func alertFind(alerts []Alert, scope, name string) *Alert {
	for i := range alerts {
		if alerts[i].Scope == scope && alerts[i].Name == name {
			return &alerts[i]
		}
	}
	return nil
}

// alertDump 统一打印实际结果，交付时贴的就是这些行。
func alertDump(t *testing.T, alerts []Alert) {
	t.Helper()
	counts := CountAlerts(alerts)
	t.Logf("实际结果：%d 条告警（warn=%d critical=%d）", len(alerts), counts.Warn, counts.Critical)
	for i, a := range alerts {
		t.Logf("  [%d] level=%s scope=%s id=%d name=%s used=%d capacity=%d ratio=%.4f message=%s",
			i, a.Level, a.Scope, a.ID, a.Name, a.Used, a.Capacity, a.Ratio, a.Message)
	}
}

// TestComputeAlertsNodeLevels 覆盖节点级四档：90% → warn、96% → critical、
// 84% → 不告警、capacity=NULL / capacity=0 → 不告警。
func TestComputeAlertsNodeLevels(t *testing.T) {
	db := openAlertTestDB(t)
	ctx := context.Background()

	warnNode := alertSeedNode(t, db, "HK-90", alertInt(100), nil)
	critNode := alertSeedNode(t, db, "HK-96", alertInt(100), nil)
	belowNode := alertSeedNode(t, db, "HK-84", alertInt(100), nil)
	nullNode := alertSeedNode(t, db, "HK-不限", nil, nil)
	zeroNode := alertSeedNode(t, db, "HK-0", alertInt(0), nil)

	alertSeedBindings(t, db, warnNode, 90)
	alertSeedBindings(t, db, critNode, 96)
	alertSeedBindings(t, db, belowNode, 84)
	alertSeedBindings(t, db, nullNode, 50)
	alertSeedBindings(t, db, zeroNode, 10)

	alerts, err := ComputeAlerts(ctx, db)
	if err != nil {
		t.Fatalf("ComputeAlerts 失败: %v", err)
	}
	alertDump(t, alerts)

	if len(alerts) != 2 {
		t.Fatalf("只有「90%%」「96%%」两条该告警，实际 %d 条: %+v", len(alerts), alerts)
	}

	// 96% → critical，排在前面（critical 优先）。
	if a := alerts[0]; a.Level != AlertLevelCritical || a.Scope != AlertScopeNode ||
		a.Name != "HK-96" || a.Used != 96 || a.Capacity != 100 {
		t.Fatalf("第一条应是 HK-96 的 critical 告警，实际 %+v", a)
	}
	if math.Abs(alerts[0].Ratio-0.96) > 1e-9 {
		t.Fatalf("HK-96 的 ratio 应为 0.96，实际 %.6f", alerts[0].Ratio)
	}
	// 90% → warn，ratio 必须正好是 0.9（前端按它画进度条）。
	if a := alerts[1]; a.Level != AlertLevelWarn || a.Name != "HK-90" || a.Used != 90 || a.Capacity != 100 {
		t.Fatalf("第二条应是 HK-90 的 warn 告警，实际 %+v", a)
	}
	if alerts[1].Ratio != 0.9 {
		t.Fatalf("HK-90 的 ratio 应为 0.9，实际 %.17f", alerts[1].Ratio)
	}

	// 84%（未到 85%）不能告警。
	if a := alertFind(alerts, AlertScopeNode, "HK-84"); a != nil {
		t.Fatalf("84/100 只有 84%%，不到 85%% 阈值，不该告警，实际产生了 %+v", *a)
	}
	// capacity = NULL（不限）不能告警。
	if a := alertFind(alerts, AlertScopeNode, "HK-不限"); a != nil {
		t.Fatalf("capacity=NULL 表示不限容量，不该告警，实际产生了 %+v", *a)
	}
	// capacity = 0（等于没设）不能告警，也不能除零出 +Inf。
	if a := alertFind(alerts, AlertScopeNode, "HK-0"); a != nil {
		t.Fatalf("capacity=0 不该告警，实际产生了 %+v", *a)
	}

	// 文案要带「谁、用了多少、什么级别」，运营看到的就这一句。
	msg := alerts[0].Message
	for _, want := range []string{"节点", "HK-96", "96/100", "96.0%"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("critical 文案缺 %q：%q", want, msg)
		}
	}
	if !strings.Contains(msg, "立即") {
		t.Fatalf("critical 文案应给行动建议（立即扩容/迁出），实际 %q", msg)
	}
	if !strings.Contains(alerts[1].Message, "提前扩容") {
		t.Fatalf("warn 文案应是「提前扩容」口径，实际 %q", alerts[1].Message)
	}

	counts := CountAlerts(alerts)
	if counts.Warn != 1 || counts.Critical != 1 {
		t.Fatalf("计数应为 warn=1 critical=1，实际 warn=%d critical=%d", counts.Warn, counts.Critical)
	}
}

// TestComputeAlertsRegionLevel 覆盖地区级：2 台节点各 100 容量、绑定共 175 →
// 地区水位 175/200 = 87.5% → warn；同时节点 96 那条 critical 排在地区 warn 之前。
func TestComputeAlertsRegionLevel(t *testing.T) {
	db := openAlertTestDB(t)
	ctx := context.Background()

	hk := alertSeedRegion(t, db, "香港")
	n1 := alertSeedNode(t, db, "HK-1", alertInt(100), alertInt(hk))
	n2 := alertSeedNode(t, db, "HK-2", alertInt(100), alertInt(hk))
	alertSeedBindings(t, db, n1, 80) // 80% → 节点级不告警
	alertSeedBindings(t, db, n2, 95) // 95% → 节点级 critical

	alerts, err := ComputeAlerts(ctx, db)
	if err != nil {
		t.Fatalf("ComputeAlerts 失败: %v", err)
	}
	alertDump(t, alerts)

	region := alertFind(alerts, AlertScopeRegion, "香港")
	if region == nil {
		t.Fatalf("地区 175/200 = 87.5%% 应出 warn 告警，实际没有（共 %d 条）", len(alerts))
	}
	if region.Level != AlertLevelWarn || region.Used != 175 || region.Capacity != 200 {
		t.Fatalf("地区告警不对：level=%s used=%d capacity=%d（期望 warn/175/200）",
			region.Level, region.Used, region.Capacity)
	}
	if math.Abs(region.Ratio-0.875) > 1e-9 {
		t.Fatalf("地区 ratio 应为 0.875，实际 %.6f", region.Ratio)
	}
	if msg := region.Message; !strings.Contains(msg, "地区") || !strings.Contains(msg, "175/200") {
		t.Fatalf("地区文案不对: %q", msg)
	}

	// 排序：节点 95% 的 critical 必须在地区 87.5% 的 warn 之前。
	if len(alerts) < 2 {
		t.Fatalf("应同时有节点与地区告警，实际只有 %d 条", len(alerts))
	}
	if alerts[0].Level != AlertLevelCritical {
		t.Fatalf("排序错了：critical 应排最前，实际第一条 = %+v", alerts[0])
	}
	if alerts[len(alerts)-1].Scope != AlertScopeRegion {
		t.Fatalf("排序错了：warn 级别的地区告警应排在 critical 之后，实际 %+v", alerts[len(alerts)-1])
	}
}

// TestComputeAlertsRegionCountsUncappedNodes 钉住地区 used 的口径：
// 该地区 capacity 为 NULL（不限）的节点上的绑定**也算进地区水位** ——
// 那些用户照样在吃这台地区声明的容量预算，漏掉他们水位就偏低、告警来得太晚。
func TestComputeAlertsRegionCountsUncappedNodes(t *testing.T) {
	db := openAlertTestDB(t)
	ctx := context.Background()

	jp := alertSeedRegion(t, db, "日本")
	capped := alertSeedNode(t, db, "JP-1", alertInt(100), alertInt(jp))
	uncapped := alertSeedNode(t, db, "JP-不限", nil, alertInt(jp))
	alertSeedBindings(t, db, capped, 10)
	alertSeedBindings(t, db, uncapped, 80) // 只有节点级不告警（不限容量），但要进地区水位

	alerts, err := ComputeAlerts(ctx, db)
	if err != nil {
		t.Fatalf("ComputeAlerts 失败: %v", err)
	}
	alertDump(t, alerts)

	region := alertFind(alerts, AlertScopeRegion, "日本")
	if region == nil {
		t.Fatalf("（10+80）/100 = 90%% 应出地区 warn 告警，实际没有（共 %d 条）", len(alerts))
	}
	if region.Used != 90 || region.Capacity != 100 {
		t.Fatalf("地区水位应把不限容量节点上的 80 人也算进 used：期望 90/100，实际 %d/%d",
			region.Used, region.Capacity)
	}
	if a := alertFind(alerts, AlertScopeNode, "JP-不限"); a != nil {
		t.Fatalf("不限容量的节点本身不该出告警，实际 %+v", *a)
	}
}

// TestComputeAlertsRegionWithoutCapacity 地区里所有节点都没设容量 → 该地区不出告警。
func TestComputeAlertsRegionWithoutCapacity(t *testing.T) {
	db := openAlertTestDB(t)
	ctx := context.Background()

	us := alertSeedRegion(t, db, "美国")
	n := alertSeedNode(t, db, "US-1", nil, alertInt(us))
	alertSeedBindings(t, db, n, 500)

	alerts, err := ComputeAlerts(ctx, db)
	if err != nil {
		t.Fatalf("ComputeAlerts 失败: %v", err)
	}
	alertDump(t, alerts)

	if len(alerts) != 0 {
		t.Fatalf("没有任何节点设容量时不该有告警（绑定再多也没有水位可比），实际 %d 条: %+v", len(alerts), alerts)
	}
}

// TestComputeAlertsSorting 钉住排序：critical 在前，同级按使用率降序。
func TestComputeAlertsSorting(t *testing.T) {
	db := openAlertTestDB(t)
	ctx := context.Background()

	for name, bound := range map[string]int{"A-96": 96, "B-99": 99, "C-90": 90} {
		node := alertSeedNode(t, db, name, alertInt(100), nil)
		alertSeedBindings(t, db, node, bound)
	}

	alerts, err := ComputeAlerts(ctx, db)
	if err != nil {
		t.Fatalf("ComputeAlerts 失败: %v", err)
	}
	alertDump(t, alerts)

	if len(alerts) != 3 {
		t.Fatalf("应 3 条告警，实际 %d 条", len(alerts))
	}
	want := []string{"B-99", "A-96", "C-90"} // 99% → 96% → 90%
	for i, w := range want {
		if alerts[i].Name != w {
			t.Fatalf("排序错了：第 %d 条应是 %s，实际 %s（期望顺序 %v）", i, w, alerts[i].Name, want)
		}
	}
	if alerts[0].Level != AlertLevelCritical || alerts[1].Level != AlertLevelCritical || alerts[2].Level != AlertLevelWarn {
		t.Fatalf("级别排序错了：%s/%s/%s", alerts[0].Level, alerts[1].Level, alerts[2].Level)
	}
}

// TestComputeAlertsEmpty 空库：返回空切片（非 nil）且不报错 —— 前端拿到的是 []，不是 null。
func TestComputeAlertsEmpty(t *testing.T) {
	db := openAlertTestDB(t)

	alerts, err := ComputeAlerts(context.Background(), db)
	if err != nil {
		t.Fatalf("空库不该报错，实际 %v", err)
	}
	if alerts == nil {
		t.Fatalf("空库应返回空切片（非 nil），前端才能直接遍历")
	}
	if len(alerts) != 0 {
		t.Fatalf("空库不该有告警，实际 %d 条", len(alerts))
	}
	if c := CountAlerts(alerts); c.Warn != 0 || c.Critical != 0 {
		t.Fatalf("空库计数应全 0，实际 warn=%d critical=%d", c.Warn, c.Critical)
	}
}
