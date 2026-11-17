package admin

// 删节点护栏集成测试（真连 MySQL，独立库 ch_region24_*，自建自删）。
//
// 为什么必须连真库：护栏的价值全在 SQL 语义上 ——
//   - 「有绑定就拒绝」是真去 user_nodes 数一遍；
//   - 强制删除要在一个事务里解绑 + 清 4 张关联表 + 删节点行，只有真库能证明没留下孤儿行；
//   - 审计是 operation_logs 里的一行，写没写、写的内容对不对，fake store 证明不了。
//
// 测试库的表**故意不带外键**：线上老库（Laravel 时期一路 migrate 上来的）不一定还留着
// ON DELETE CASCADE，只靠级联删的写法在这里会留下孤儿行 —— 用例就是钉这一点的。
//
// 变异测试锚点：删掉 node_sqlstore.go DeleteNode 里的
//   if bound > 0 && !in.Force { return &NodeBoundUsersError{...} }
// TestRegion24StoreDeleteBlockedByBindings 与 TestRegion24APIDeleteGuard 必须变红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// region24DSN 连本机 MySQL（root/空密码），可用 REGION24_MYSQL_DSN 覆盖。
//
// parseTime=true&loc=UTC 与生产 DSN 一致：节点行有时间列（created_at 等），
// 不带 parseTime 时 scanNode 会报 unsupported Scan（[]uint8 → time.Time）。
func region24DSN() string {
	if dsn := os.Getenv("REGION24_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

// region24WithSchema 把 DSN 的默认库换成 schema。
func region24WithSchema(dsn, schema string) string {
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

// region24DDL 建本次真正读写的表。nodes 的列与 node_models.go 的 nodeColumns 对齐；
// 关联表只留 node_id + 一个业务列，够证明「按 node_id 清干净」即可。
var region24DDL = []string{
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int NOT NULL DEFAULT 443,
		scheme varchar(8) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'https',
		web_base_path varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		username varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		password text COLLATE utf8mb4_unicode_ci,
		api_key text COLLATE utf8mb4_unicode_ci,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		verify_ssl tinyint(1) NOT NULL DEFAULT 0,
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'offline',
		latency int NOT NULL DEFAULT 0,
		region_id bigint unsigned DEFAULT NULL,
		capacity int DEFAULT NULL,
		cpu decimal(5,2) DEFAULT NULL,
		mem decimal(5,2) DEFAULT NULL,
		traffic_multiplier decimal(6,2) DEFAULT NULL,
		driver_type varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'xray',
		driver_version varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		driver_config json DEFAULT NULL,
		last_check_at timestamp NULL DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE node_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		protocol varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
		inbound_id bigint NOT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY node_inbounds_uniq (node_id, protocol, inbound_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		remark varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int unsigned NOT NULL,
		listen varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '0.0.0.0',
		client_host varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		settings json DEFAULT NULL,
		stream_settings json DEFAULT NULL,
		sniffing json DEFAULT NULL,
		reality_private_key text COLLATE utf8mb4_unicode_ci,
		salamander_password text COLLATE utf8mb4_unicode_ci,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		sort int unsigned NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY xray_inbounds_node_id_tag_unique (node_id, tag)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_outbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY xray_outbounds_node_id_tag_unique (node_id, tag)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_routing_rules (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		outbound_tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE operation_logs (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		type varchar(20) COLLATE utf8mb4_unicode_ci NOT NULL,
		actor_id bigint unsigned DEFAULT NULL,
		actor_name varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		action varchar(120) COLLATE utf8mb4_unicode_ci NOT NULL,
		method varchar(10) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		path varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		status int DEFAULT NULL,
		error text COLLATE utf8mb4_unicode_ci,
		ip varchar(45) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE async_tasks (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		type varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
		subject_type varchar(32) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		subject_id bigint unsigned DEFAULT NULL,
		total int NOT NULL DEFAULT 0,
		completed int NOT NULL DEFAULT 0,
		failed int NOT NULL DEFAULT 0,
		attempts int NOT NULL DEFAULT 0,
		max_attempts int NOT NULL DEFAULT 3,
		meta json DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE async_task_items (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		task_id bigint unsigned NOT NULL,
		item_key varchar(191) COLLATE utf8mb4_unicode_ci NOT NULL,
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
		attempts int NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY async_task_items_task_id_item_key_unique (task_id, item_key)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newRegion24DB 建一个干净的测试库 + 表，跑完自动 DROP。
func newRegion24DB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", region24DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过删节点护栏集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过删节点护栏集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		again, err := sql.Open("mysql", region24DSN())
		if err != nil {
			return
		}
		defer again.Close()
		again.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region24WithSchema(region24DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range region24DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// region24SeedNode 插一台节点并返回 id（password/api_key 留 NULL，测试不碰加解密）。
func region24SeedNode(t *testing.T, db *sql.DB, name string) uint64 {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	res, err := db.Exec(
		"INSERT INTO nodes (name, host, driver_type, created_at, updated_at) VALUES (?, ?, 'xray', ?, ?)",
		name, name+".example.com", now, now)
	if err != nil {
		t.Fatalf("插节点失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取节点 id 失败: %v", err)
	}
	return uint64(id)
}

// region24SeedBindings 给节点绑 n 个用户（user_id 从 1 开始，按调用次序递增）。
func region24SeedBindings(t *testing.T, db *sql.DB, nodeID uint64, n int) {
	t.Helper()
	for uid := 1; uid <= n; uid++ {
		if _, err := db.Exec("INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)", uid, nodeID); err != nil {
			t.Fatalf("插绑定失败: %v", err)
		}
	}
}

// region24SeedRelated 给节点铺满 4 张关联表的行（强制删除要把它们全清掉）。
func region24SeedRelated(t *testing.T, db *sql.DB, nodeID uint64) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	stmts := []struct {
		q    string
		args []any
	}{
		{"INSERT INTO node_inbounds (node_id, protocol, inbound_id, created_at, updated_at) VALUES (?, 'vless', 101, ?, ?)", []any{nodeID, now, now}},
		{"INSERT INTO xray_inbounds (node_id, tag, protocol, port) VALUES (?, 'vless-443', 'vless', 443)", []any{nodeID}},
		{"INSERT INTO xray_outbounds (node_id, tag, protocol) VALUES (?, 'direct', 'freedom')", []any{nodeID}},
		{"INSERT INTO xray_routing_rules (node_id, outbound_tag) VALUES (?, 'direct')", []any{nodeID}},
	}
	for _, s := range stmts {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("铺关联数据失败: %v\nSQL: %s", err, s.q)
		}
	}
}

// region24Count 是「这行还在不在」的通用断言工具。
func region24Count(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v\nSQL: %s", err, query)
	}
	return n
}

// TestRegion24StoreDeleteBlockedByBindings：有 3 个用户绑定时 force=false 必须拒绝，
// 且节点与绑定都原样保留（这是「不把用户变成悬空」的唯一保证）。
func TestRegion24StoreDeleteBlockedByBindings(t *testing.T) {
	db := newRegion24DB(t, "ch_region24_guard")
	store := NewNodeSQLStore(db, nil)
	ctx := context.Background()

	nodeID := region24SeedNode(t, db, "HK-1")
	region24SeedBindings(t, db, nodeID, 3)

	err := store.DeleteNode(ctx, NodeDeleteInput{ID: nodeID})
	if err == nil {
		t.Fatalf("有 3 个用户绑定时应拒绝删除，实际删除成功（节点 %d 已没了）", nodeID)
	}
	var guard *NodeBoundUsersError
	if !errors.As(err, &guard) {
		t.Fatalf("错误类型应是 *NodeBoundUsersError（HTTP 层靠它回 422），实际 %T: %v", err, err)
	}
	if guard.Count != 3 {
		t.Fatalf("护栏报的绑定数 = %d，期望 3", guard.Count)
	}
	// 前端 toast 直接显示这句，必须带具体人数。
	if msg := nodeBoundUsersMessage(guard.Count); !strings.Contains(msg, "3 个用户") {
		t.Fatalf("护栏文案没带人数: %q", msg)
	}

	if n := region24Count(t, db, "SELECT COUNT(*) FROM nodes WHERE id = ?", nodeID); n != 1 {
		t.Fatalf("被拒删后节点应还在，实际 nodes 里 %d 行", n)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = ?", nodeID); n != 3 {
		t.Fatalf("被拒删后绑定应原样保留 3 条，实际 %d 条", n)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM operation_logs"); n != 0 {
		t.Fatalf("被拒删不该写强制删除审计，实际 operation_logs %d 行", n)
	}
}

// TestRegion24StoreForceDeleteUnbindsAndCleans：force=true 要删干净 ——
// 节点行、绑定、4 张关联表里的该节点数据全没，别的节点一行不碰，并留一条审计。
func TestRegion24StoreForceDeleteUnbindsAndCleans(t *testing.T) {
	db := newRegion24DB(t, "ch_region24_force")
	store := NewNodeSQLStore(db, nil)
	ctx := context.Background()

	doomed := region24SeedNode(t, db, "HK-1")
	region24SeedBindings(t, db, doomed, 3)
	region24SeedRelated(t, db, doomed)

	// 隔壁节点：证明清理是「按 node_id」而不是「清全表」。
	other := region24SeedNode(t, db, "HK-2")
	region24SeedBindings(t, db, other, 2)
	region24SeedRelated(t, db, other)

	if err := store.DeleteNode(ctx, NodeDeleteInput{ID: doomed, Force: true, Actor: "ops-admin"}); err != nil {
		t.Fatalf("强制删除应成功，实际失败: %v", err)
	}

	if n := region24Count(t, db, "SELECT COUNT(*) FROM nodes WHERE id = ?", doomed); n != 0 {
		t.Fatalf("强制删除后节点行应没了，实际还有 %d 行", n)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = ?", doomed); n != 0 {
		t.Fatalf("强制删除后该节点绑定应全没了，实际还有 %d 条", n)
	}
	for _, table := range []string{"node_inbounds", "xray_inbounds", "xray_outbounds", "xray_routing_rules"} {
		if n := region24Count(t, db, "SELECT COUNT(*) FROM "+table+" WHERE node_id = ?", doomed); n != 0 {
			t.Fatalf("强制删除后 %s 里该节点的行应清空，实际还有 %d 行", table, n)
		}
		// 隔壁节点的数据必须留着（防止写成无条件 DELETE）。
		if n := region24Count(t, db, "SELECT COUNT(*) FROM "+table+" WHERE node_id = ?", other); n != 1 {
			t.Fatalf("%s 里隔壁节点的行被误删了：期望 1 行，实际 %d 行", table, n)
		}
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = ?", other); n != 2 {
		t.Fatalf("隔壁节点的 2 条绑定被误删，实际 %d 条", n)
	}

	// 审计：一条，action 带人数，操作人就是调用方传进来的账号。
	var typ, action, actor, method, path string
	row := db.QueryRow("SELECT type, action, actor_name, method, path FROM operation_logs ORDER BY id DESC LIMIT 1")
	if err := row.Scan(&typ, &action, &actor, &method, &path); err != nil {
		t.Fatalf("强制删除应留一条审计，实际读不到: %v", err)
	}
	if want := "强制删除节点（含 3 个用户绑定）"; action != want {
		t.Fatalf("审计 action = %q，期望 %q", action, want)
	}
	if typ != "admin" || actor != "ops-admin" || method != "DELETE" {
		t.Fatalf("审计字段不对: type=%q actor=%q method=%q", typ, actor, method)
	}
	if path != "/admin-api/nodes/"+strconv.FormatUint(doomed, 10) {
		t.Fatalf("审计 path = %q，期望带上节点 id", path)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM operation_logs"); n != 1 {
		t.Fatalf("一次强制删除只该留一条审计，实际 %d 行", n)
	}
}

// TestRegion24StoreDeleteWithoutBindings：没绑定就照旧直接删（护栏不能误伤正常删除）。
func TestRegion24StoreDeleteWithoutBindings(t *testing.T) {
	db := newRegion24DB(t, "ch_region24_plain")
	store := NewNodeSQLStore(db, nil)
	ctx := context.Background()

	nodeID := region24SeedNode(t, db, "JP-1")
	region24SeedRelated(t, db, nodeID)

	if err := store.DeleteNode(ctx, NodeDeleteInput{ID: nodeID}); err != nil {
		t.Fatalf("无绑定节点应能直接删除，实际失败: %v", err)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM nodes WHERE id = ?", nodeID); n != 0 {
		t.Fatalf("节点应已删除，实际还有 %d 行", n)
	}
	for _, table := range []string{"node_inbounds", "xray_inbounds", "xray_outbounds", "xray_routing_rules"} {
		if n := region24Count(t, db, "SELECT COUNT(*) FROM "+table+" WHERE node_id = ?", nodeID); n != 0 {
			t.Fatalf("删除节点后 %s 应无残留，实际 %d 行", table, n)
		}
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM operation_logs"); n != 0 {
		t.Fatalf("非强制删除不该写强制删除审计，实际 %d 行", n)
	}
}

// region24Response 是管理端统一的 {code,msg,data} 响应。
type region24Response struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// TestRegion24APIDeleteGuard 走 HTTP：不带 force 被拒（422 + 明确文案、不留悬空清理任务），
// 带 force=1 真删并派发清理任务，无绑定节点照旧删，节点不存在回 404。
func TestRegion24APIDeleteGuard(t *testing.T) {
	db := newRegion24DB(t, "ch_region24_api")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewNodeDomain(NodeDeps{Store: NewNodeSQLStore(db, nil)}).Register(engine.Group("/admin-api"))

	call := func(method, path string) region24Response {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s HTTP 状态 = %d，管理端口径恒为 200（body=%s）", method, path, rec.Code, rec.Body.String())
		}
		var resp region24Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s %s 响应不是合法 JSON: %v（body=%s）", method, path, err, rec.Body.String())
		}
		return resp
	}

	nodeID := region24SeedNode(t, db, "HK-1")
	region24SeedBindings(t, db, nodeID, 3)
	region24SeedRelated(t, db, nodeID)

	// 1) 不带 force：拒绝，且不能先建好清理任务（否则任务跟着悬空）。
	blocked := call(http.MethodDelete, "/admin-api/nodes/"+strconv.FormatUint(nodeID, 10))
	if blocked.Code == 0 {
		t.Fatalf("有绑定时不带 force 应被拒绝，实际 code=0（msg=%s）", blocked.Msg)
	}
	if blocked.Code != 422 {
		t.Fatalf("护栏应回 422 业务错误，实际 code=%d（msg=%s）", blocked.Code, blocked.Msg)
	}
	if !strings.Contains(blocked.Msg, "3 个用户") {
		t.Fatalf("拒绝文案没带绑定人数: %q", blocked.Msg)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM nodes WHERE id = ?", nodeID); n != 1 {
		t.Fatalf("被拒后节点应还在，实际 %d 行", n)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM async_tasks"); n != 0 {
		t.Fatalf("被拒后不该留下清理任务，实际 async_tasks %d 行", n)
	}

	// 2) 带 force=1：删成功，绑定清空，清理任务派发出去（节点没了只能靠任务清远端）。
	forced := call(http.MethodDelete, "/admin-api/nodes/"+strconv.FormatUint(nodeID, 10)+"?force=1")
	if forced.Code != 0 {
		t.Fatalf("force=1 应删除成功，实际 code=%d msg=%s", forced.Code, forced.Msg)
	}
	if !strings.Contains(forced.Msg, "已强制删除") || !strings.Contains(forced.Msg, "3 个用户") {
		t.Fatalf("强制删除的成功文案应说明解绑了 3 个用户，实际 %q", forced.Msg)
	}
	var data struct {
		CleanupTaskID uint64 `json:"cleanup_task_id"`
		Message       string `json:"message"`
	}
	if err := json.Unmarshal(forced.Data, &data); err != nil {
		t.Fatalf("解析删除响应失败: %v（data=%s）", err, forced.Data)
	}
	if data.CleanupTaskID == 0 {
		t.Fatalf("强制删除后应派发清理任务，响应里 cleanup_task_id = 0")
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM nodes WHERE id = ?", nodeID); n != 0 {
		t.Fatalf("强制删除后节点应没了，实际 %d 行", n)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = ?", nodeID); n != 0 {
		t.Fatalf("强制删除后绑定应清空，实际 %d 条", n)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM async_tasks WHERE id = ?", data.CleanupTaskID); n != 1 {
		t.Fatalf("响应里的清理任务在库里不存在（task=%d）", data.CleanupTaskID)
	}

	// 3) 无绑定节点：不带 force 照旧删掉。
	plain := region24SeedNode(t, db, "JP-1")
	ok := call(http.MethodDelete, "/admin-api/nodes/"+strconv.FormatUint(plain, 10))
	if ok.Code != 0 {
		t.Fatalf("无绑定节点应能直接删，实际 code=%d msg=%s", ok.Code, ok.Msg)
	}
	if n := region24Count(t, db, "SELECT COUNT(*) FROM nodes WHERE id = ?", plain); n != 0 {
		t.Fatalf("无绑定节点应已删除，实际 %d 行", n)
	}

	// 4) 节点不存在：404（不是 500，也不是悄悄成功）。
	missing := call(http.MethodDelete, "/admin-api/nodes/99999")
	if missing.Code != 404 {
		t.Fatalf("删不存在的节点应回 404，实际 code=%d msg=%s", missing.Code, missing.Msg)
	}
}
