package subscription

// 订阅按「节点是否归属地区」过滤（#地区功能-03）集成测试。
//
// 全部打真实 MySQL（独立库 ch_region03_*，用例内自建、结束即删），连不上就 skip ——
// 要验证的正是「region_id 是否为空 + 用户有没有绑定到已归属节点」如何改变 SQL 结果，
// 打桩只会测到自己的 if 判断，等于没测。
//
// 口径（与 nodeapi / api 同语义）：
//   - 未归属地区（region_id IS NULL）的节点 → 对所有用户全量下发；
//   - 已归属地区的节点 → 只下发给绑定到它的用户。
//
// 变异测试靶子（去掉即必须变红）：
//   - store.go 的 `n.region_id IS NULL` 改成旧的
//     `NOT EXISTS (SELECT 1 FROM user_nodes un0 WHERE un0.user_id = ?)` →
//     TestRegion03UnassignedNodesVisibleToBoundUser 红。

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
	// 入站级分组（3.1.0）：region_id 参与「有效地区 = COALESCE(入站, 节点)」过滤，
	// tag 让用例能给同节点多个入站起可读的名字（订阅用例只按 region_id 造数据）。
	// 列与 SQLRepository.XrayInbounds 的 SELECT 逐列对齐（少了任何一列读数就报 1054）。
	`CREATE TABLE xray_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		region_id bigint unsigned DEFAULT NULL,
		tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		remark varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int unsigned NOT NULL DEFAULT '443',
		listen varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '0.0.0.0',
		client_host varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		settings json DEFAULT NULL,
		stream_settings json DEFAULT NULL,
		salamander_password text COLLATE utf8mb4_unicode_ci,
		enabled tinyint(1) NOT NULL DEFAULT '1',
		sort int unsigned NOT NULL DEFAULT '0',
		PRIMARY KEY (id),
		UNIQUE KEY xray_inbounds_node_id_tag_unique (node_id, tag),
		KEY xray_inbounds_node_id_enabled_index (node_id, enabled),
		KEY idx_xray_inbounds_region (region_id)
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
	// plans 的列与 SQLRepository.PlanByID 的 SELECT 对齐（type/months/include_local/
	// third_party_sub_ids/region_ids）；端到端 Generate 用例会整行读。
	`CREATE TABLE plans (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'period',
		months int DEFAULT NULL,
		include_local tinyint(1) NOT NULL DEFAULT 1,
		third_party_sub_ids text COLLATE utf8mb4_unicode_ci,
		region_ids json DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 端到端 Generate 用例会读站点配置；空表即「全部取默认值」。
	`CREATE TABLE site_configs (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		` + "`key`" + ` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		` + "`value`" + ` text COLLATE utf8mb4_unicode_ci,
		PRIMARY KEY (id),
		UNIQUE KEY site_configs_key_unique (` + "`key`" + `)
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

// addNode 插一个节点（未归属地区）；3x-ui 节点同时补一条 node_inbounds。
func (f *region03Fixture) addNode(t *testing.T, id int64, name, status string) {
	t.Helper()
	host := fmt.Sprintf("10.0.0.%d", id)
	mustExec(t, f.db,
		"INSERT INTO nodes (id, name, host, port, driver_type, status, enabled) VALUES (?, ?, ?, 443, '3x-ui', ?, 1)",
		id, name, host, status)
	mustExec(t, f.db,
		"INSERT INTO node_inbounds (node_id, protocol, inbound_id) VALUES (?, 'vless', ?)", id, id)
}

// addAssignedNode 插一个已归属地区(region_id)的节点（+ 一条 node_inbounds）。
func (f *region03Fixture) addAssignedNode(t *testing.T, id int64, name, status string, regionID int64) {
	t.Helper()
	host := fmt.Sprintf("10.1.0.%d", id)
	mustExec(t, f.db,
		"INSERT INTO nodes (id, name, host, port, driver_type, status, enabled, region_id) VALUES (?, ?, ?, 443, '3x-ui', ?, 1, ?)",
		id, name, host, status, regionID)
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

// ─────────── 用例 1：未归属地区的节点 → 对所有用户全量下发（不看绑定） ───────────

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

// ─────────── 用例 2：已归属地区的节点，只有绑定它的用户才看得到 ───────────

func TestRegion03AssignedNodeOnlyForBoundUser(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_one")
	f.addAssignedNode(t, 1, "node-1", "online", 7)
	f.addAssignedNode(t, 2, "node-2", "online", 7)
	f.addAssignedNode(t, 3, "node-3", "online", 7)
	f.bind(t, 7, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 7)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{2})

	// 没绑定的用户：全部是三台已归属节点 → 一台都看不到（不再"无绑定即全量"）。
	nodes, err = f.repo.SubscribableNodes(context.Background(), "vless", 8)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{})
}

// ─────────── 用例 2b：未归属节点 + 已归属节点混布：未归属的对绑定用户仍全量可见 ───────────

func TestRegion03UnassignedNodesVisibleToBoundUser(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_mix")
	f.addNode(t, 1, "node-1", "online")            // 未归属
	f.addNode(t, 2, "node-2", "online")            // 未归属
	f.addAssignedNode(t, 3, "node-3", "online", 7) // 已归属，绑给用户 7
	f.addAssignedNode(t, 4, "node-4", "online", 7) // 已归属，未绑给用户 7
	f.bind(t, 7, 3)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 7)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 1/2 未归属 → 全量；3 已归属且绑定 → 可见；4 已归属未绑定 → 排除。
	equalIDs(t, nodeIDs(nodes), []int64{1, 2, 3})
}

// ─────────── 用例 3：绑定的已归属节点离线 → 该节点不出现（未归属的仍在） ───────────

func TestRegion03BoundNodeOfflineExcluded(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_offline")
	f.addNode(t, 1, "node-1", "online")             // 未归属 → 全量
	f.addAssignedNode(t, 2, "node-2", "offline", 7) // 已归属、绑给用户 7、但离线
	f.bind(t, 7, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 7)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1})
}

// ─────────── 用例 4：两个用户绑定不同已归属节点 → 各拿各的 ───────────

func TestRegion03TwoUsersGetOwnNodes(t *testing.T) {
	f := newRegion03Fixture(t, "ch_region03_two")
	f.addAssignedNode(t, 1, "node-1", "online", 7)
	f.addAssignedNode(t, 2, "node-2", "online", 7)
	f.addAssignedNode(t, 3, "node-3", "online", 7)
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
