package admin

// 用户列表按绑定节点筛选 + 单用户改绑（地区功能 #26）的集成测试。
//
// 实连 MySQL（root/空密码，可用 REGION10_MYSQL_DSN 覆盖），独立库 ch_region26_*，
// 自建自删。DSN 与「换库名 + 补 parseTime」两个小工具直接复用 migration_api_test.go
// 里的 region10DSN / region10WithSchema —— 同一个包、同一套约定，不必抄两遍。
//
// 覆盖任务书验收第 3 条：
//   - 3 人绑 A、2 人绑 B → 列表带 node_id=A 只出 3 条；
//   - 改绑 A→B：user_nodes 里 A 没了 B 有了、user_node_migrations 有 manual 记录、
//     users.updated_at 变了；
//   - from_node_id 为空 = 只加不减（一个可绑多节点）；
//   - 目标节点不存在 → 422（不是写出一条悬空绑定）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// region26DDL 只建接口真正读写的 7 张表（列与 cmd/server/schema.go 一致，省掉无关列）。
var region26DDL = []string{
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
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		driver_type varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '3x-ui',
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'online',
		enabled tinyint(1) NOT NULL DEFAULT 1,
		capacity int DEFAULT NULL,
		region_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_nodes_region (region_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE plans (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'total',
		months int DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 只保留列表查询会扫的列（userSelectColumns）；密码等与本任务无关，不建。
	`CREATE TABLE users (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		email varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		token varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		uuid varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		protocol varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'vless',
		plan_id bigint unsigned DEFAULT NULL,
		traffic_limit bigint NOT NULL DEFAULT 0,
		traffic_used bigint NOT NULL DEFAULT 0,
		balance bigint NOT NULL DEFAULT 0,
		monthly_traffic_used bigint NOT NULL DEFAULT 0,
		monthly_traffic_limit bigint NOT NULL DEFAULT 0,
		next_traffic_reset_at timestamp NULL DEFAULT NULL,
		expired_at timestamp NULL DEFAULT NULL,
		enabled tinyint(1) NOT NULL DEFAULT 1,
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
	`CREATE TABLE user_node_migrations (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		user_id bigint unsigned NOT NULL,
		from_node_id bigint unsigned DEFAULT NULL,
		to_node_id bigint unsigned NOT NULL,
		reason varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		operator varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		batch_id varchar(36) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_user (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newRegion26DB 建一个干净的测试库 + 7 张表，跑完自动 DROP。
// 库名带进程号：并行跑 go test 时固定库名会互相 DROP。
func newRegion26DB(t *testing.T, schema string) *sql.DB {
	t.Helper()
	schema = fmt.Sprintf("%s_%d", schema, os.Getpid())

	admin, err := sql.Open("mysql", region10DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过用户改绑集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过用户改绑集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", region10DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region10WithSchema(region10DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range region26DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// newUsersRouter 只挂用户域，并用中间件模拟 admin.auth 注入的管理员身份
// （改绑历史的 operator 取的就是它）。键名与 httpx 内部一致：httpx.admin。
func newUsersRouter(t *testing.T, db *sql.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/admin-api")
	g.Use(func(c *gin.Context) {
		c.Set("httpx.admin", &httpx.AdminIdentity{ID: 7, Username: "ops-admin"})
		c.Next()
	})
	NewUserDomain(UserDeps{
		Users:  NewAdminUserSQLStore(db),
		Logger: log.New(io.Discard, "", 0),
	}).Register(g)
	return r
}

// seedRegion26 建地区 1（香港）+ 节点 1/2（都在香港）、5 个用户：
// 1/2/3 绑节点 A(1)，4/5 绑节点 B(2)。
func seedRegion26(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec10(t, db, "INSERT INTO regions (id, name) VALUES (1, '香港')")
	mustExec10(t, db, "INSERT INTO nodes (id, name, host, region_id) VALUES (1, 'hk-01', 'a.example.com', 1), (2, 'hk-02', 'b.example.com', 1)")
	for id := 1; id <= 5; id++ {
		node := 1
		if id >= 4 {
			node = 2
		}
		mustExec10(t, db, "INSERT INTO users (id, email, token, uuid) VALUES (?, ?, ?, ?)",
			id, fmt.Sprintf("u%d@example.com", id), fmt.Sprintf("sub_%d", id), fmt.Sprintf("uuid-%d", id))
		mustExec10(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)", id, node)
	}
}

// dataList 把 data 解析成数组（列表接口的数据都在 data 里）。
func dataList(t *testing.T, resp migrationResp) []map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(resp.Data, &list); err != nil {
		t.Fatalf("解析 data 数组失败: %v（raw=%s）", err, string(resp.Data))
	}
	return list
}

// userIDsOf 从列表响应里挑 id，断言筛出来的是哪些人。
func userIDsOf(list []map[string]any) []int {
	out := make([]int, 0, len(list))
	for _, row := range list {
		if f, ok := row["id"].(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}

// ---- 验收 1：按绑定节点筛选 ----

func TestUserPageFilterByNodeID(t *testing.T) {
	db := newRegion26DB(t, "ch_region26_filter")
	seedRegion26(t, db)
	r := newUsersRouter(t, db)

	// 绑 A（节点 1）的 3 个人
	onA := dataList(t, migrationCall(t, r, http.MethodGet, "/admin-api/users?node_id=1", ""))
	if got := userIDsOf(onA); fmt.Sprint(got) != "[3 2 1]" {
		t.Fatalf("node_id=1 应返回 3 个用户 [3 2 1]（id 倒序），实际 %v", got)
	}
	for _, row := range onA {
		if fmt.Sprint(row["node_ids"]) != "[1]" {
			t.Fatalf("用户 %v 的 node_ids 应是 [1]，实际 %v", row["id"], row["node_ids"])
		}
		names, _ := row["node_names"].([]any)
		if len(names) != 1 || names[0] != "hk-01" {
			t.Fatalf("用户 %v 的 node_names 应是 [hk-01]，实际 %v", row["id"], row["node_names"])
		}
	}

	// 绑 B（节点 2）的 2 个人
	onB := dataList(t, migrationCall(t, r, http.MethodGet, "/admin-api/users?node_id=2", ""))
	if got := userIDsOf(onB); fmt.Sprint(got) != "[5 4]" {
		t.Fatalf("node_id=2 应返回 2 个用户 [5 4]，实际 %v", got)
	}
	t.Logf("node_id=1 → %v（%d 条）；node_id=2 → %v（%d 条）",
		userIDsOf(onA), len(onA), userIDsOf(onB), len(onB))

	// 不带 node_id：全部 5 个人，且没绑定的人 node_ids 是空数组（不是 null）
	all := dataList(t, migrationCall(t, r, http.MethodGet, "/admin-api/users", ""))
	if len(all) != 5 {
		t.Fatalf("不筛选应返回 5 个用户，实际 %d", len(all))
	}
	// 不在 A 上的人不该出现在 node_id=1 的结果里（EXISTS 语义，不是「随便有一行就出」）
	for _, row := range onA {
		if int(row["id"].(float64)) == 4 || int(row["id"].(float64)) == 5 {
			t.Fatalf("节点 2 上的用户 %v 不该出现在 node_id=1 的结果里", row["id"])
		}
	}
}

// ---- 验收 2：改绑 A → B ----

func TestUserNodesUpdateMovesBinding(t *testing.T) {
	db := newRegion26DB(t, "ch_region26_move")
	seedRegion26(t, db)
	r := newUsersRouter(t, db)

	// 把一个明显过去的时间写进指纹列：改绑后必须变（节点侧靠它重拉配置）。
	mustExec10(t, db, "UPDATE users SET updated_at = '2020-01-01 00:00:00' WHERE id = 1")
	before := userUpdatedAt(t, db, 1)

	resp := migrationCall(t, r, http.MethodPut, "/admin-api/users/1/nodes", `{"from_node_id":1,"to_node_id":2}`)
	if resp.Code != 0 {
		t.Fatalf("改绑失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if m := resp.dataMap(t); m["ok"] != true {
		t.Fatalf("响应 data 应是 {ok:true}，实际 %v", m)
	}

	// user_nodes：A 上没了、B 上有了
	if n := scalar26(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 1 AND node_id = 1"); n != 0 {
		t.Fatalf("改绑后用户 1 仍绑在节点 A 上（%d 行）", n)
	}
	if n := scalar26(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 1 AND node_id = 2"); n != 1 {
		t.Fatalf("改绑后用户 1 没绑到节点 B（%d 行）", n)
	}

	// 迁移历史：reason=manual、operator=当前管理员、from/to 正确
	var (
		reason, operator, fromNode sql.NullString
		toNode                     int64
	)
	if err := db.QueryRow(`SELECT reason, operator, from_node_id, to_node_id
		FROM user_node_migrations WHERE user_id = 1 ORDER BY id DESC LIMIT 1`).
		Scan(&reason, &operator, &fromNode, &toNode); err != nil {
		t.Fatalf("读迁移历史失败: %v", err)
	}
	if reason.String != "manual" || operator.String != "ops-admin" || fromNode.String != "1" || toNode != 2 {
		t.Fatalf("迁移历史不对: reason=%q operator=%q from=%q to=%d（期望 manual/ops-admin/1/2）",
			reason.String, operator.String, fromNode.String, toNode)
	}

	// 指纹变了
	after := userUpdatedAt(t, db, 1)
	if !after.After(before) {
		t.Fatalf("users.updated_at 没变（before=%s after=%s），节点侧不会重拉配置", before, after)
	}
	t.Logf("改绑后 updated_at: %s → %s，迁移历史 reason=%s operator=%s from=%s to=%d",
		before.Format(time.RFC3339), after.Format(time.RFC3339), reason.String, operator.String, fromNode.String, toNode)

	// 列表按新节点筛得到他
	onB := dataList(t, migrationCall(t, r, http.MethodGet, "/admin-api/users?node_id=2", ""))
	if got := userIDsOf(onB); fmt.Sprint(got) != "[5 4 1]" {
		t.Fatalf("改绑后 node_id=2 应返回 [5 4 1]，实际 %v", got)
	}
}

// ---- 验收 3：from_node_id 为空 → 只加不减 ----

func TestUserNodesUpdateWithoutFromOnlyAdds(t *testing.T) {
	db := newRegion26DB(t, "ch_region26_add")
	seedRegion26(t, db)
	r := newUsersRouter(t, db)

	resp := migrationCall(t, r, http.MethodPut, "/admin-api/users/1/nodes", `{"to_node_id":2}`)
	if resp.Code != 0 {
		t.Fatalf("只加不减失败: code=%d msg=%s", resp.Code, resp.Msg)
	}

	// 原绑定还在 + 新绑定加上了 = 两条
	if n := scalar26(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 1"); n != 2 {
		t.Fatalf("from 为空时应只加不减（两条绑定），实际 %d 条", n)
	}
	if n := scalar26(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 1 AND node_id = 1"); n != 1 {
		t.Fatalf("from 为空时不该动原绑定（节点 A）")
	}
	// 历史里 from_node_id 是 NULL
	var fromNode sql.NullString
	if err := db.QueryRow(`SELECT from_node_id FROM user_node_migrations WHERE user_id = 1 ORDER BY id DESC LIMIT 1`).
		Scan(&fromNode); err != nil {
		t.Fatalf("读迁移历史失败: %v", err)
	}
	if fromNode.Valid {
		t.Fatalf("from 为空时历史里的 from_node_id 应为 NULL，实际 %q", fromNode.String)
	}
	t.Logf("只加不减后用户 1 绑定节点数 = 2，历史 from_node_id = NULL")
}

// ---- 目标节点不存在 → 422，且不写悬空绑定 ----

func TestUserNodesUpdateRejectsUnknownNode(t *testing.T) {
	db := newRegion26DB(t, "ch_region26_unknown")
	seedRegion26(t, db)
	r := newUsersRouter(t, db)

	resp := migrationCall(t, r, http.MethodPut, "/admin-api/users/1/nodes", `{"from_node_id":1,"to_node_id":999}`)
	if resp.Code != 422 || resp.Msg != "目标节点不存在" {
		t.Fatalf("目标节点不存在应回 422/目标节点不存在，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	if n := scalar26(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 1"); n != 1 {
		t.Fatalf("失败的改绑不该动绑定（应仍为 1 条），实际 %d 条", n)
	}
}

// ---- 节点候选下拉 ----

func TestUserNodeOptions(t *testing.T) {
	db := newRegion26DB(t, "ch_region26_options")
	seedRegion26(t, db)
	r := newUsersRouter(t, db)

	list := dataList(t, migrationCall(t, r, http.MethodGet, "/admin-api/users/nodes-options", ""))
	if len(list) != 2 {
		t.Fatalf("节点候选应有 2 条，实际 %d：%v", len(list), list)
	}
	if list[0]["name"] != "hk-01" || list[0]["region_name"] != "香港" {
		t.Fatalf("第一条候选应是 hk-01/香港，实际 %v", list[0])
	}
	t.Logf("nodes-options → %v", list)
}

// ---- 小工具 ----

func scalar26(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询失败: %v\nSQL: %s", err, query)
	}
	return n
}

func userUpdatedAt(t *testing.T, db *sql.DB, id uint64) time.Time {
	t.Helper()
	var at time.Time
	if err := db.QueryRowContext(context.Background(),
		"SELECT updated_at FROM users WHERE id = ?", id).Scan(&at); err != nil {
		t.Fatalf("读 users.updated_at 失败: %v", err)
	}
	return at
}
