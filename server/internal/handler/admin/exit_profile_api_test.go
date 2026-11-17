package admin

// 管理端静态 IP 出口分类（#静态IP）集成测试：真连 MySQL，独立库 ch_region30_*。
//
// 要证明的六件事（打桩证明不了）：
//  1. 建分类（3 个前置节点）→ 列表返回 node_count=3，关联表 3 行；
//  2. 改分类（节点改成 2 个）→ 关联表旧的删掉、只剩新的 2 行；
//  3. 删除有套餐绑定的分类 → 拒绝，文案含套餐数 + 套餐名，分类与关联都还在；
//  4. 删除无绑定的分类 → 成功，关联表同步清理；
//  5. 套餐保存 exit_type=static_chain + exit_profile_id → 查询/列表都能读回；
//  6. 套餐 exit_type 非法 → 校验拦下（in:direct,static_chain,static_socks）。
//
// 每条用例自建 schema（ch_region30_*），跑完 DROP，互不影响。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// exitProfileDDL 只建本任务真正读写的表（列与 cmd/server/schema.go 对齐，省掉无关列）。
var exitProfileDDL = []string{
	`CREATE TABLE exit_profiles (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		provider_id bigint unsigned NOT NULL,
		traffic_gb int NOT NULL DEFAULT 0,
		duration_days int NOT NULL DEFAULT 30,
		max_thread_count int NOT NULL DEFAULT 0,
		socks_host varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		socks_port int NOT NULL DEFAULT 1080,
		enabled tinyint NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY exit_profiles_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE exit_profile_nodes (
		profile_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (profile_id, node_id),
		KEY idx_exit_profile_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 分类列表 JOIN 出账号名；只有 id/name 两列是必读的，其余列给「账号被删」场景留位。
	`CREATE TABLE proxy_providers (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		driver varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'webshare',
		api_token text COLLATE utf8mb4_unicode_ci NOT NULL,
		enabled tinyint(1) NOT NULL DEFAULT '1',
		last_status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending',
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int NOT NULL DEFAULT 443,
		driver_type varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '3x-ui',
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'offline',
		enabled tinyint(1) NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 套餐表带齐 exit_type/exit_profile_id（#静态IP 新增的两列）与既有的 region_ids。
	`CREATE TABLE plans (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'period',
		months int DEFAULT NULL,
		monthly_traffic bigint DEFAULT NULL,
		period_traffic bigint DEFAULT NULL,
		total_traffic bigint DEFAULT NULL,
		price decimal(10,2) NOT NULL DEFAULT 0.00,
		reset_price decimal(10,2) NOT NULL DEFAULT 0.00,
		is_active tinyint(1) NOT NULL DEFAULT 1,
		include_local tinyint(1) NOT NULL DEFAULT 1,
		third_party_sub_ids text COLLATE utf8mb4_unicode_ci,
		region_ids json DEFAULT NULL,
		exit_type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'direct',
		exit_profile_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE regions (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		sort int NOT NULL DEFAULT 0,
		PRIMARY KEY (id),
		UNIQUE KEY regions_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newExitProfile30DB 建一个干净的 ch_region30_* 测试库 + 全部表，跑完自动 DROP。
func newExitProfile30DB(t *testing.T, caseName string) *sql.DB {
	t.Helper()

	dsn := os.Getenv("REGION30_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?timeout=3s"
	}
	schema := fmt.Sprintf("ch_region30_%s_%d", caseName, os.Getpid())

	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过出口分类集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过出口分类集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	for _, q := range []string{
		"DROP DATABASE IF EXISTS `" + schema + "`",
		"CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci",
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("准备测试库失败（%q）: %v", q, err)
		}
	}
	t.Cleanup(func() {
		a, err := sql.Open("mysql", dsn)
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	slash := strings.LastIndex(dsn, "/")
	dbDSN := dsn[:slash+1] + schema + "?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=UTC&timeout=3s"
	db, err := sql.Open("mysql", dbDSN)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range exitProfileDDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// newExitProfile30Router 只挂出口分类域 + 用户/套餐域（鉴权由 routes.go 的分组负责）。
func newExitProfile30Router(t *testing.T, db *sql.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	g := engine.Group("/admin-api")
	NewExitProfileDomain(ExitProfileDeps{Store: NewExitProfileSQLStore(db)}).Register(g)
	NewUserDomain(UserDeps{
		Store: NewAdminUserSQLStore(db),
		Plans: NewPlanSQLStore(db),
	}).Register(g)
	return engine
}

// exit30Response 是统一的 {code,msg,data} 响应。
type exit30Response struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// exit30Profile 是 presentExitProfile 输出里本用例关心的字段。
type exit30Profile struct {
	ID             uint64   `json:"id"`
	Name           string   `json:"name"`
	ProviderID     uint64   `json:"provider_id"`
	ProviderName   string   `json:"provider_name"`
	TrafficGB      int64    `json:"traffic_gb"`
	DurationDays   int64    `json:"duration_days"`
	MaxThreadCount int64    `json:"max_thread_count"`
	SocksHost      string   `json:"socks_host"`
	SocksPort      int64    `json:"socks_port"`
	Enabled        bool     `json:"enabled"`
	NodeIDs        []uint64 `json:"node_ids"`
	NodeCount      int64    `json:"node_count"`
}

// exit30Plan 是 presentPlan 输出里本用例关心的字段。
type exit30Plan struct {
	ID              uint64  `json:"id"`
	Name            string  `json:"name"`
	ExitType        string  `json:"exit_type"`
	ExitProfileID   *uint64 `json:"exit_profile_id"`
	ExitProfileName string  `json:"exit_profile_name"`
}

// exit30Call 发一个 JSON 请求（管理端 HTTP 状态恒为 200）。
func exit30Call(t *testing.T, engine *gin.Engine, method, path, body string) exit30Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s HTTP 状态 = %d（管理端口径恒为 200，body=%s）", method, path, rec.Code, rec.Body.String())
	}
	var resp exit30Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s %s 响应不是合法 JSON: %v（body=%s）", method, path, err, rec.Body.String())
	}
	return resp
}

// seedExit30 造 1 个 webshare 账号 + 5 台节点（够「改成 2 个」和「3 个」两种用例）。
func seedExit30(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO proxy_providers (id, name, api_token) VALUES (1, '主账号', 'tok')"); err != nil {
		t.Fatalf("建 webshare 账号失败: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := db.Exec("INSERT INTO nodes (id, name, host, status) VALUES (?, ?, ?, 'online')",
			i, fmt.Sprintf("hk-%02d", i), fmt.Sprintf("10.0.0.%d", i)); err != nil {
			t.Fatalf("建节点失败: %v", err)
		}
	}
}

// exit30RelationRows 读关联表（按 node_id 升序）。
func exit30RelationRows(t *testing.T, db *sql.DB, profileID uint64) []uint64 {
	t.Helper()
	rows, err := db.Query("SELECT node_id FROM exit_profile_nodes WHERE profile_id = ? ORDER BY node_id", profileID)
	if err != nil {
		t.Fatalf("读关联表失败: %v", err)
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描关联行失败: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func exit30RequireIDs(t *testing.T, got []uint64, want ...uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("node_ids = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node_ids = %v，期望 %v", got, want)
		}
	}
}

// TestExit30CreateWithThreeNodes 建分类（3 个前置节点）→ 列表 node_count=3、关联表 3 行。
func TestExit30CreateWithThreeNodes(t *testing.T) {
	db := newExitProfile30DB(t, "create")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	resp := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"香港住宅","provider_id":1,"traffic_gb":50,"duration_days":30,
		  "max_thread_count":20,"socks_host":"p.webshare.io","socks_port":1080,
		  "node_ids":[1,2,3]}`)
	if resp.Code != 0 {
		t.Fatalf("建分类失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var created exit30Profile
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatalf("解析新建分类失败: %v（raw=%s）", err, resp.Data)
	}
	if created.Name != "香港住宅" || !created.Enabled {
		t.Fatalf("新建分类回显不一致: %+v", created)
	}
	exit30RequireIDs(t, created.NodeIDs, 1, 2, 3)

	// 列表：node_count / provider_name 都要算好（前端直接展示，不再自己拼）。
	list := exit30Call(t, engine, http.MethodGet, "/admin-api/exit-profiles", "")
	if list.Code != 0 {
		t.Fatalf("查列表失败: code=%d msg=%s", list.Code, list.Msg)
	}
	var profiles []exit30Profile
	if err := json.Unmarshal(list.Data, &profiles); err != nil {
		t.Fatalf("解析分类列表失败: %v（raw=%s）", err, list.Data)
	}
	if len(profiles) != 1 {
		t.Fatalf("分类数量 = %d，期望 1", len(profiles))
	}
	if profiles[0].NodeCount != 3 {
		t.Fatalf("node_count = %d，期望 3", profiles[0].NodeCount)
	}
	if profiles[0].ProviderName != "主账号" {
		t.Fatalf("provider_name = %q，期望 主账号", profiles[0].ProviderName)
	}
	if profiles[0].TrafficGB != 50 || profiles[0].DurationDays != 30 || profiles[0].MaxThreadCount != 20 {
		t.Fatalf("模板字段读回不一致: %+v", profiles[0])
	}
	if profiles[0].SocksHost != "p.webshare.io" || profiles[0].SocksPort != 1080 {
		t.Fatalf("socks 入口读回不一致: %+v", profiles[0])
	}

	// 关联表必须真有 3 行（响应里的 node_ids 是读回来的，不代表落库正确）。
	exit30RequireIDs(t, exit30RelationRows(t, db, created.ID), 1, 2, 3)
}

// TestExit30UpdateReplacesNodes 改分类（节点改成 2 个）→ 关联表旧的删掉、只剩新的 2 行。
func TestExit30UpdateReplacesNodes(t *testing.T) {
	db := newExitProfile30DB(t, "update")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	create := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"新加坡住宅","provider_id":1,"traffic_gb":10,"node_ids":[1,2,3,4,5]}`)
	if create.Code != 0 {
		t.Fatalf("建分类失败: code=%d msg=%s", create.Code, create.Msg)
	}
	var created exit30Profile
	if err := json.Unmarshal(create.Data, &created); err != nil {
		t.Fatalf("解析新建分类失败: %v", err)
	}
	exit30RequireIDs(t, exit30RelationRows(t, db, created.ID), 1, 2, 3, 4, 5)

	upd := exit30Call(t, engine, http.MethodPut, fmt.Sprintf("/admin-api/exit-profiles/%d", created.ID),
		`{"name":"新加坡住宅","provider_id":1,"traffic_gb":10,"node_ids":[4,5]}`)
	if upd.Code != 0 {
		t.Fatalf("改分类失败: code=%d msg=%s", upd.Code, upd.Msg)
	}
	var updated exit30Profile
	if err := json.Unmarshal(upd.Data, &updated); err != nil {
		t.Fatalf("解析改后分类失败: %v", err)
	}
	if updated.NodeCount != 2 || updated.TrafficGB != 10 {
		t.Fatalf("改后响应不一致: %+v", updated)
	}

	// 关键断言：关联表里只剩 4、5 —— 1/2/3 必须被删掉，不能残留。
	exit30RequireIDs(t, exit30RelationRows(t, db, created.ID), 4, 5)

	list := exit30Call(t, engine, http.MethodGet, "/admin-api/exit-profiles", "")
	var profiles []exit30Profile
	if err := json.Unmarshal(list.Data, &profiles); err != nil {
		t.Fatalf("解析分类列表失败: %v", err)
	}
	if len(profiles) != 1 || profiles[0].NodeCount != 2 {
		t.Fatalf("列表 node_count 应为 2，实际 %+v", profiles)
	}
	exit30RequireIDs(t, profiles[0].NodeIDs, 4, 5)
}

// TestExit30DeleteRefusedWhenPlanBound 删除有套餐绑定的分类 → 拒绝，文案含套餐数 + 套餐名。
func TestExit30DeleteRefusedWhenPlanBound(t *testing.T) {
	db := newExitProfile30DB(t, "delete_guard")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	create := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"被绑定的分类","provider_id":1,"node_ids":[1,2]}`)
	if create.Code != 0 {
		t.Fatalf("建分类失败: code=%d msg=%s", create.Code, create.Msg)
	}
	var profile exit30Profile
	if err := json.Unmarshal(create.Data, &profile); err != nil {
		t.Fatalf("解析新建分类失败: %v", err)
	}

	plan := exit30Call(t, engine, http.MethodPost, "/admin-api/plans",
		fmt.Sprintf(`{"name":"静态IP月付","type":"period","months":1,"price":"30.00","exit_type":"static_chain","exit_profile_id":%d}`, profile.ID))
	if plan.Code != 0 {
		t.Fatalf("建套餐失败: code=%d msg=%s", plan.Code, plan.Msg)
	}

	del := exit30Call(t, engine, http.MethodDelete, fmt.Sprintf("/admin-api/exit-profiles/%d", profile.ID), "")
	if del.Code == 0 {
		t.Fatalf("有套餐绑定时删除必须被拒绝，实际 code=0 msg=%s", del.Msg)
	}
	if !strings.Contains(del.Msg, "1 个套餐") || !strings.Contains(del.Msg, "静态IP月付") {
		t.Fatalf("拒绝文案应含套餐数与套餐名，实际 msg=%q", del.Msg)
	}

	// 拒绝 = 一行都没动：分类还在、关联表还在。
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM exit_profiles"); n != 1 {
		t.Fatalf("拒绝删除后分类应为 1 行，实际 %d", n)
	}
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM exit_profile_nodes WHERE profile_id = "+fmt.Sprint(profile.ID)); n != 2 {
		t.Fatalf("拒绝删除后关联表应为 2 行，实际 %d", n)
	}
}

// TestExit30DeleteCascadesRelation 删除无绑定的分类 → 成功，关联表同步清理。
func TestExit30DeleteCascadesRelation(t *testing.T) {
	db := newExitProfile30DB(t, "delete_ok")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	create := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"没人用","provider_id":1,"node_ids":[1,2,3]}`)
	if create.Code != 0 {
		t.Fatalf("建分类失败: code=%d msg=%s", create.Code, create.Msg)
	}
	var profile exit30Profile
	if err := json.Unmarshal(create.Data, &profile); err != nil {
		t.Fatalf("解析新建分类失败: %v", err)
	}

	del := exit30Call(t, engine, http.MethodDelete, fmt.Sprintf("/admin-api/exit-profiles/%d", profile.ID), "")
	if del.Code != 0 {
		t.Fatalf("无绑定的分类应能删除，实际 code=%d msg=%s", del.Code, del.Msg)
	}
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM exit_profiles"); n != 0 {
		t.Fatalf("删除后分类应为 0 行，实际 %d", n)
	}
	// 关联表不能留孤儿行（否则同一个 id 被复用时会凭空多出节点）。
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM exit_profile_nodes"); n != 0 {
		t.Fatalf("删除后关联表应为 0 行，实际 %d", n)
	}

	// 再删一次 → 404（不是 500，也不是「删除成功」）。
	again := exit30Call(t, engine, http.MethodDelete, fmt.Sprintf("/admin-api/exit-profiles/%d", profile.ID), "")
	if again.Code != 404 {
		t.Fatalf("重复删除应回 404，实际 code=%d msg=%s", again.Code, again.Msg)
	}
}

func exit30Count(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("计数失败（%s）: %v", query, err)
	}
	return n
}

// TestExit30PlanSavesExitTypeAndProfile 套餐保存 exit_type=static_chain + exit_profile_id → 能读回。
func TestExit30PlanSavesExitTypeAndProfile(t *testing.T) {
	db := newExitProfile30DB(t, "plan_save")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	create := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"链式出口","provider_id":1,"node_ids":[1]}`)
	if create.Code != 0 {
		t.Fatalf("建分类失败: code=%d msg=%s", create.Code, create.Msg)
	}
	var profile exit30Profile
	if err := json.Unmarshal(create.Data, &profile); err != nil {
		t.Fatalf("解析新建分类失败: %v", err)
	}

	resp := exit30Call(t, engine, http.MethodPost, "/admin-api/plans",
		fmt.Sprintf(`{"name":"静态IP套餐","type":"period","months":3,"price":"99.00","exit_type":"static_chain","exit_profile_id":%d}`, profile.ID))
	if resp.Code != 0 {
		t.Fatalf("建套餐失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var created exit30Plan
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatalf("解析新建套餐失败: %v（raw=%s）", err, resp.Data)
	}
	if created.ExitType != "static_chain" {
		t.Fatalf("回显 exit_type = %q，期望 static_chain", created.ExitType)
	}
	if created.ExitProfileID == nil || *created.ExitProfileID != profile.ID {
		t.Fatalf("回显 exit_profile_id = %v，期望 %d", created.ExitProfileID, profile.ID)
	}
	if created.ExitProfileName != "链式出口" {
		t.Fatalf("回显 exit_profile_name = %q，期望 链式出口", created.ExitProfileName)
	}

	// 库里两列真的落了（不是只在响应里拼出来的）。
	var (
		exitType string
		profID   sql.NullInt64
	)
	if err := db.QueryRow("SELECT exit_type, exit_profile_id FROM plans WHERE id = ?", created.ID).
		Scan(&exitType, &profID); err != nil {
		t.Fatalf("读 plans 失败: %v", err)
	}
	if exitType != "static_chain" || !profID.Valid || uint64(profID.Int64) != profile.ID {
		t.Fatalf("库里 exit_type=%q exit_profile_id=%v（valid=%v），期望 static_chain / %d",
			exitType, profID.Int64, profID.Valid, profile.ID)
	}

	// 列表接口同样要带上这三个字段（读路径 JOIN 了 exit_profiles）。
	list := exit30Call(t, engine, http.MethodGet, "/admin-api/plans?all=1", "")
	if list.Code != 0 {
		t.Fatalf("查套餐列表失败: code=%d msg=%s", list.Code, list.Msg)
	}
	var plans []exit30Plan
	if err := json.Unmarshal(list.Data, &plans); err != nil {
		t.Fatalf("解析套餐列表失败: %v（raw=%s）", err, list.Data)
	}
	if len(plans) != 1 {
		t.Fatalf("套餐数量 = %d，期望 1", len(plans))
	}
	if plans[0].ExitType != "static_chain" || plans[0].ExitProfileID == nil || *plans[0].ExitProfileID != profile.ID {
		t.Fatalf("列表出口字段不一致: %+v", plans[0])
	}
	if plans[0].ExitProfileName != "链式出口" {
		t.Fatalf("列表 exit_profile_name = %q，期望 链式出口", plans[0].ExitProfileName)
	}

	// PUT 把出口类型切回直连 → 分类必须被清空（不能留着「direct + 绑着分类」的脏组合）。
	upd := exit30Call(t, engine, http.MethodPut, fmt.Sprintf("/admin-api/plans/%d", created.ID),
		`{"name":"静态IP套餐","type":"period","months":3,"price":"99.00","exit_type":"direct"}`)
	if upd.Code != 0 {
		t.Fatalf("改套餐失败: code=%d msg=%s", upd.Code, upd.Msg)
	}
	var updated exit30Plan
	if err := json.Unmarshal(upd.Data, &updated); err != nil {
		t.Fatalf("解析改后套餐失败: %v", err)
	}
	if updated.ExitType != "direct" || updated.ExitProfileID != nil {
		t.Fatalf("切回直连后 exit_type=%q exit_profile_id=%v，期望 direct / null",
			updated.ExitType, updated.ExitProfileID)
	}
}

// TestExit30PlanRejectsInvalidExitType 套餐 exit_type 非法 → 校验拦下，不落库。
func TestExit30PlanRejectsInvalidExitType(t *testing.T) {
	db := newExitProfile30DB(t, "plan_bad_type")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	resp := exit30Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"脏套餐","type":"period","months":1,"price":"9.00","exit_type":"wireguard"}`)
	if resp.Code == 0 {
		t.Fatalf("非法 exit_type 应被拦下，实际 code=0 msg=%s", resp.Msg)
	}
	if !strings.Contains(resp.Msg, "exit type") && !strings.Contains(resp.Msg, "exit_type") {
		t.Fatalf("校验文案应指向 exit type 字段，实际 msg=%q", resp.Msg)
	}
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM plans"); n != 0 {
		t.Fatalf("校验失败不该落库，实际 plans 有 %d 行", n)
	}

	// PUT 同样要拦（不能只在新建时校验）。
	ok := exit30Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"正常套餐","type":"period","months":1,"price":"9.00"}`)
	if ok.Code != 0 {
		t.Fatalf("建正常套餐失败: code=%d msg=%s", ok.Code, ok.Msg)
	}
	var created exit30Plan
	if err := json.Unmarshal(ok.Data, &created); err != nil {
		t.Fatalf("解析新建套餐失败: %v", err)
	}
	if created.ExitType != "direct" {
		t.Fatalf("未传 exit_type 时应默认 direct，实际 %q", created.ExitType)
	}

	bad := exit30Call(t, engine, http.MethodPut, fmt.Sprintf("/admin-api/plans/%d", created.ID),
		`{"name":"正常套餐","type":"period","months":1,"price":"9.00","exit_type":"socks5"}`)
	if bad.Code == 0 {
		t.Fatalf("PUT 非法 exit_type 应被拦下，实际 code=0 msg=%s", bad.Msg)
	}
}

// TestExit30PlanRejectsProfileOnNonStaticType direct 套餐不能绑出口分类（脏组合会在出站生成时误导）。
func TestExit30PlanRejectsProfileOnNonStaticType(t *testing.T) {
	db := newExitProfile30DB(t, "plan_bad_combo")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	create := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"有分类","provider_id":1,"node_ids":[1]}`)
	if create.Code != 0 {
		t.Fatalf("建分类失败: code=%d msg=%s", create.Code, create.Msg)
	}
	var profile exit30Profile
	if err := json.Unmarshal(create.Data, &profile); err != nil {
		t.Fatalf("解析新建分类失败: %v", err)
	}

	// 新建：exit_type=direct（默认）+ 绑分类 → 422，不落库。
	bad := exit30Call(t, engine, http.MethodPost, "/admin-api/plans",
		fmt.Sprintf(`{"name":"脏组合","type":"period","months":1,"price":"9.00","exit_profile_id":%d}`, profile.ID))
	if bad.Code == 0 {
		t.Fatalf("direct 套餐绑分类应被拒绝，实际 code=0 msg=%s", bad.Msg)
	}
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM plans"); n != 0 {
		t.Fatalf("校验失败不该落库，实际 plans 有 %d 行", n)
	}

	// 修改：已经是 direct 的套餐，PUT 只带 exit_profile_id → 也要拒绝。
	ok := exit30Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"直连套餐","type":"period","months":1,"price":"9.00"}`)
	if ok.Code != 0 {
		t.Fatalf("建直连套餐失败: code=%d msg=%s", ok.Code, ok.Msg)
	}
	var plan exit30Plan
	if err := json.Unmarshal(ok.Data, &plan); err != nil {
		t.Fatalf("解析新建套餐失败: %v", err)
	}
	badUpd := exit30Call(t, engine, http.MethodPut, fmt.Sprintf("/admin-api/plans/%d", plan.ID),
		fmt.Sprintf(`{"name":"直连套餐","type":"period","months":1,"price":"9.00","exit_profile_id":%d}`, profile.ID))
	if badUpd.Code == 0 {
		t.Fatalf("PUT 给 direct 套餐绑分类应被拒绝，实际 code=0 msg=%s", badUpd.Msg)
	}
}

// TestExit30RejectsUnknownNode 勾了不存在的节点 → 422 且点名是哪个，不落库。
func TestExit30RejectsUnknownNode(t *testing.T) {
	db := newExitProfile30DB(t, "bad_node")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	resp := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"脏节点","provider_id":1,"node_ids":[1,999]}`)
	if resp.Code == 0 {
		t.Fatalf("不存在的节点应被拦下，实际 code=0 msg=%s", resp.Msg)
	}
	if !strings.Contains(resp.Msg, "#999") {
		t.Fatalf("文案应点出不存在的节点，实际 msg=%q", resp.Msg)
	}
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM exit_profiles"); n != 0 {
		t.Fatalf("校验失败不该落库，实际 exit_profiles 有 %d 行", n)
	}
}

// TestExit30RejectsUnknownProvider 选了一个不存在的 webshare 账号 → 422，不落库。
func TestExit30RejectsUnknownProvider(t *testing.T) {
	db := newExitProfile30DB(t, "bad_provider")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	resp := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"野账号","provider_id":99,"node_ids":[1]}`)
	if resp.Code == 0 {
		t.Fatalf("不存在的 webshare 账号应被拦下，实际 code=0 msg=%s", resp.Msg)
	}
	if !strings.Contains(resp.Msg, "账号不存在") {
		t.Fatalf("文案应说明账号不存在，实际 msg=%q", resp.Msg)
	}
	if n := exit30Count(t, db, "SELECT COUNT(*) FROM exit_profiles"); n != 0 {
		t.Fatalf("校验失败不该落库，实际 exit_profiles 有 %d 行", n)
	}
}

// TestExit30DetailReturnsNodes 详情接口带前置节点明细（名称/状态），不只是 id。
func TestExit30DetailReturnsNodes(t *testing.T) {
	db := newExitProfile30DB(t, "detail")
	seedExit30(t, db)
	engine := newExitProfile30Router(t, db)

	create := exit30Call(t, engine, http.MethodPost, "/admin-api/exit-profiles",
		`{"name":"详情分类","provider_id":1,"node_ids":[2,3]}`)
	if create.Code != 0 {
		t.Fatalf("建分类失败: code=%d msg=%s", create.Code, create.Msg)
	}
	var profile exit30Profile
	if err := json.Unmarshal(create.Data, &profile); err != nil {
		t.Fatalf("解析新建分类失败: %v", err)
	}

	show := exit30Call(t, engine, http.MethodGet, fmt.Sprintf("/admin-api/exit-profiles/%d", profile.ID), "")
	if show.Code != 0 {
		t.Fatalf("查详情失败: code=%d msg=%s", show.Code, show.Msg)
	}
	var detail struct {
		exit30Profile
		Nodes []struct {
			ID     uint64 `json:"id"`
			Name   string `json:"name"`
			Host   string `json:"host"`
			Status string `json:"status"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(show.Data, &detail); err != nil {
		t.Fatalf("解析详情失败: %v（raw=%s）", err, show.Data)
	}
	if len(detail.Nodes) != 2 {
		t.Fatalf("详情节点数 = %d，期望 2", len(detail.Nodes))
	}
	if detail.Nodes[0].Name != "hk-02" || detail.Nodes[1].Name != "hk-03" {
		t.Fatalf("详情节点名不一致: %+v", detail.Nodes)
	}
}
