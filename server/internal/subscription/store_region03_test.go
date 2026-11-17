package subscription

// 订阅按用户绑定过滤（#地区功能-03）集成测试。
//
// 全部打真实 MySQL（独立库 ch_region03_*，用例内自建、结束即删），连不上就 skip ——
// 要验证的正是「user_nodes 里有没有该用户的行」如何改变 SQL 结果，
// 打桩只会测到自己的 if 判断，等于没测。
//
// 变异测试靶子（去掉即必须变红）：
//   - store.go 的 `NOT EXISTS (SELECT 1 FROM user_nodes un0 WHERE un0.user_id = ?)`
//     兼容分支改成恒假 → TestRegion03UnboundUserGetsAllNodes 红。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// region03SchemaDDL 与生产 schema/schema.sql + cmd/server/schema.go 逐列对齐
// （只保留 SubscribableNodes 的 SQL 会碰到、以及扫行时要读的列）。
var region03SchemaDDL = []string{
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int NOT NULL DEFAULT '443',
		scheme varchar(8) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'https',
		web_base_path varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		username varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		password text COLLATE utf8mb4_unicode_ci,
		api_key text COLLATE utf8mb4_unicode_ci,
		enabled tinyint(1) NOT NULL DEFAULT '1',
		driver_type varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '3x-ui',
		driver_config json DEFAULT NULL,
		verify_ssl tinyint(1) NOT NULL DEFAULT '0',
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'offline',
		traffic_multiplier decimal(6,2) DEFAULT NULL,
		region_id bigint unsigned DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE node_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		protocol varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
		inbound_id bigint NOT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY node_inbounds_node_id_protocol_inbound_id_unique (node_id, protocol, inbound_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
		enabled tinyint(1) NOT NULL DEFAULT '1',
		PRIMARY KEY (id),
		KEY xray_inbounds_node_id_enabled_index (node_id, enabled)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 与 cmd/server/schema.go 的 user_nodes 完全一致（含 idx_node）。
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 套餐地区过滤（#地区功能-15）会按 users.plan_id JOIN plans 读 region_ids。
	`CREATE TABLE users (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		plan_id bigint unsigned DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE plans (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		region_ids json DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// region03Fixture 是一个独立的测试库 + 打真实 MySQL 的仓储。
type region03Fixture struct {
	repo *SQLRepository
	db   *sql.DB
}

// newRegion03Fixture 建库建表；t.Cleanup 负责删库。
func newRegion03Fixture(t *testing.T, schemaName string) *region03Fixture {
	t.Helper()

	admin := openTestMySQL(t)
	mustExec(t, admin, "DROP DATABASE IF EXISTS "+schemaName)
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + schemaName) })
	mustExec(t, admin, "CREATE DATABASE "+schemaName+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")

	db := openTestMySQLDSN(t, mysqlDSN(schemaName))
	for _, ddl := range region03SchemaDDL {
		mustExec(t, db, ddl)
	}
	return &region03Fixture{repo: NewSQLRepository(db, nil), db: db}
}

// addNode 插一个节点；3x-ui 节点同时补一条 node_inbounds（否则它在原查询里也不可订阅）。
func (f *region03Fixture) addNode(t *testing.T, id int64, name, status string) {
	t.Helper()
	host := fmt.Sprintf("10.0.0.%d", id)
	mustExec(t, f.db,
		"INSERT INTO nodes (id, name, host, port, driver_type, status, enabled) VALUES (?, ?, ?, 443, '3x-ui', ?, 1)",
		id, name, host, status)
	mustExec(t, f.db,
		"INSERT INTO node_inbounds (node_id, protocol, inbound_id) VALUES (?, 'vless', ?)", id, id)
}

// bind 写一条 user_nodes 绑定。
func (f *region03Fixture) bind(t *testing.T, userID, nodeID int64) {
	t.Helper()
	mustExec(t, f.db, "INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)", userID, nodeID)
}

// ids 把节点列表压成 id 序列，便于整段比较。
func nodeIDs(nodes []Node) []int64 {
	out := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// equalIDs 断言两个 id 序列完全相等（顺序也算 —— 订阅顺序敏感）。
func equalIDs(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("节点 id 数量不符：期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("节点 id 不符：期望 %v，实际 %v", want, got)
		}
	}
}

// ─────────── 用例 1：用户无任何绑定 → 返回全部在线可订阅节点（老行为） ───────────

func TestRegion03UnboundUserGetsAllNodes(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_all")
	f.addNode(t, 1, "node-1", "online")
	f.addNode(t, 2, "node-2", "online")
	f.addNode(t, 3, "node-3", "online")

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 99)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1, 2, 3})
}

// ─────────── 用例 2：用户绑定 1 个节点（共 3 个在线）→ 只返回那 1 个 ───────────

func TestRegion03BoundUserGetsOnlyBoundNode(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_one")
	f.addNode(t, 1, "node-1", "online")
	f.addNode(t, 2, "node-2", "online")
	f.addNode(t, 3, "node-3", "online")
	f.bind(t, 7, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 7)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{2})

	// 同一份数据、没绑定的用户仍拿全部 —— 证明过滤是按用户走的，不是全局收敛
	nodes, err = f.repo.SubscribableNodes(context.Background(), "vless", 8)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1, 2, 3})
}

// ─────────── 用例 3：用户绑定的节点离线 → 返回空（不是返回全部） ───────────

func TestRegion03BoundNodeOfflineReturnsEmpty(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_offline")
	f.addNode(t, 1, "node-1", "online")
	f.addNode(t, 2, "node-2", "offline") // 绑给他的是这个，但离线
	f.addNode(t, 3, "node-3", "online")
	f.bind(t, 7, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 7)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("绑定节点离线时必须返回空，实际返回 %v", nodeIDs(nodes))
	}
}

// ─────────── 用例 4：两个用户绑定不同节点 → 各拿各的 ───────────

func TestRegion03TwoUsersGetOwnNodes(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_two")
	f.addNode(t, 1, "node-1", "online")
	f.addNode(t, 2, "node-2", "online")
	f.addNode(t, 3, "node-3", "online")
	f.bind(t, 11, 1)
	f.bind(t, 12, 3)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 11)
	if err != nil {
		t.Fatalf("用户 11 查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1})

	nodes, err = f.repo.SubscribableNodes(context.Background(), "vless", 12)
	if err != nil {
		t.Fatalf("用户 12 查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{3})
}
