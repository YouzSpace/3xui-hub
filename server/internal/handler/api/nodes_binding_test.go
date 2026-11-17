package api

// /api/nodes 按 user_nodes 绑定过滤的集成测试（真连 MySQL，独立库 ch_region04_*，自建自删）。
//
// 口径（与 nodeapi / subscription 同语义）：
//   - 用户没有任何绑定 → 全量（老用户升级后看到的节点与升级前完全一致）；
//   - 有绑定 → 只返回绑定给他的节点；
//   - 绑定的节点离线/禁用 → 该用户拿到空列表（不允许回退成全量）。
//
// 为什么必须连真库：这段过滤是 SQL 语义（NOT EXISTS / EXISTS 组合 + 参数顺序），
// 用 fake store 只能证明「参数传下去了」，证明不了「SQL 写对了」。
//
// 变异测试锚点：删掉 `NOT EXISTS (SELECT 1 FROM user_nodes un0 WHERE un0.user_id = ?)`
// 那一行，「无绑定 → 全量」用例必须变红（只剩绑定分支时无绑定用户拿到 0 条）。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// region04DSN 连本机 MySQL（root/空密码），可用 REGION04_MYSQL_DSN 覆盖。
func region04DSN() string {
	if dsn := os.Getenv("REGION04_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// region04Schema 拼测试库名（形如 ch_region04_bound_12345）。
//
// 必须带 pid：本仓库多张卡并行跑在同一台机器上，谁跑了 `go test ./...` 就会顺手
// 跑别人的集成测试；库名写死时两个进程会互相 DROP / 撞 Error 1007 database exists
// （region-01 / region-08 的用例现在就这么红）。带 pid 后各跑各的，互不干扰。
func region04Schema(caseName string) string {
	return fmt.Sprintf("ch_region04_%s_%d", caseName, os.Getpid())
}

// region04WithSchema 把 DSN 的默认库换成 schema（DSN 形如 root:@tcp(host:port)/?timeout=3s）。
func region04WithSchema(dsn, schema string) string {
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

// nodesBindingDDL 只建本次查询真正读到的 4 张表的**最小列集**：
// nodes（nodeColumns 全部字段）、node_inbounds、xray_inbounds（loadNodeInbounds 用到）、
// user_nodes（本次新增的绑定表，列与 schema.go 完全一致）。
var nodesBindingDDL = []string{
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) NOT NULL,
		host varchar(255) NOT NULL,
		port int NOT NULL DEFAULT 443,
		scheme varchar(8) NOT NULL DEFAULT 'https',
		web_base_path varchar(255) NOT NULL DEFAULT '',
		username varchar(255) NOT NULL DEFAULT '',
		password text,
		api_key text,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		driver_type varchar(32) NOT NULL DEFAULT '3x-ui',
		driver_config json DEFAULT NULL,
		verify_ssl tinyint(1) NOT NULL DEFAULT 0,
		status varchar(16) NOT NULL DEFAULT 'offline',
		traffic_multiplier decimal(6,2) DEFAULT NULL,
		latency int NOT NULL DEFAULT 0,
		region_id bigint unsigned DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE node_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		protocol varchar(16) NOT NULL,
		driver_type varchar(32) NOT NULL DEFAULT '3x-ui',
		inbound_id bigint NOT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		tag varchar(64) NOT NULL,
		protocol varchar(32) NOT NULL,
		port int unsigned NOT NULL,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// 套餐地区过滤（#地区功能-15）会按 users.plan_id JOIN plans 读 region_ids；
	// plans.region_ids 为 NULL = 不限地区（老套餐）。
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

// newNodesBindingDB 建一个干净的测试库 + 最小表结构，用完自动 DROP。
func newNodesBindingDB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", region04DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过节点绑定过滤集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过节点绑定过滤集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", region04DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region04WithSchema(region04DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("测试库 ping 失败: %v", err)
	}

	for _, ddl := range nodesBindingDDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v（DDL: %s）", err, ddl)
		}
	}
	return db
}

// nodesFilterFixture 铺 6 个节点 + 3 条绑定：
//
//	1、2、3 = 在线且有 node_inbounds（合格的 3 个节点，也就是「全量」应为 [1 2 3]）
//	4       = 在线但没有入站（基础筛选的第 2 条，任何用户都不该看到）
//	5       = 有入站但 enabled=0（第 1 条）
//	6       = 有入站但 status=offline（第 1 条；case C 绑定的就是它）
//
// 绑定：user 102 → 节点 2；user 103 → 节点 6（离线）；user 101 无绑定。
func nodesFilterFixture(t *testing.T, db *sql.DB) {
	t.Helper()

	for id, row := range []struct {
		status  string
		enabled int
		inbound bool
	}{
		{"online", 1, true},  // 1
		{"online", 1, true},  // 2
		{"online", 1, true},  // 3
		{"online", 1, false}, // 4 无入站
		{"online", 0, true},  // 5 禁用
		{"offline", 1, true}, // 6 离线
	} {
		nodeID := id + 1
		if _, err := db.Exec(
			`INSERT INTO nodes (id, name, host, port, enabled, status, driver_type, latency)
			 VALUES (?, ?, ?, 443, ?, ?, '3x-ui', 20)`,
			nodeID, fmt.Sprintf("节点%02d", nodeID), fmt.Sprintf("10.0.0.%d", nodeID), row.enabled, row.status,
		); err != nil {
			t.Fatalf("插入节点 %d 失败: %v", nodeID, err)
		}
		if row.inbound {
			if _, err := db.Exec(
				`INSERT INTO node_inbounds (node_id, protocol, inbound_id) VALUES (?, 'vless', ?)`,
				nodeID, 1000+nodeID,
			); err != nil {
				t.Fatalf("插入节点 %d 入站失败: %v", nodeID, err)
			}
		}
	}

	for _, bind := range []struct{ user, node uint64 }{{102, 2}, {103, 6}} {
		if _, err := db.Exec(`INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)`, bind.user, bind.node); err != nil {
			t.Fatalf("插入绑定 (%d→%d) 失败: %v", bind.user, bind.node, err)
		}
	}
}

// nodeIDsOf 取节点 id 列表，用于断言顺序与集合。
func nodeIDsOf(nodes []Node) []uint64 {
	out := make([]uint64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func assertNodeIDs(t *testing.T, got []Node, want []uint64) {
	t.Helper()
	ids := nodeIDsOf(got)
	if len(ids) != len(want) {
		t.Fatalf("返回节点 id = %v, 期望 %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("返回节点 id = %v, 期望 %v", ids, want)
		}
	}
}

// TestOnlineNodesForProtocolNoBindingReturnsAll：无绑定的用户拿到全部在线节点（老行为）。
func TestOnlineNodesForProtocolNoBindingReturnsAll(t *testing.T) {
	db := newNodesBindingDB(t, region04Schema("nobind"))
	nodesFilterFixture(t, db)
	store := NewSQLStore(db)

	nodes, err := store.OnlineNodesForProtocol(context.Background(), "vless", 101)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	assertNodeIDs(t, nodes, []uint64{1, 2, 3})
}

// TestOnlineNodesForProtocolBoundReturnsOnlyBound：有绑定 → 只返回绑定的那个。
func TestOnlineNodesForProtocolBoundReturnsOnlyBound(t *testing.T) {
	db := newNodesBindingDB(t, region04Schema("bound"))
	nodesFilterFixture(t, db)
	store := NewSQLStore(db)

	nodes, err := store.OnlineNodesForProtocol(context.Background(), "vless", 102)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	assertNodeIDs(t, nodes, []uint64{2})
}

// TestOnlineNodesForProtocolBoundOfflineReturnsEmpty：绑定的节点离线 → 空列表，
// 不能回退成全量（那是「绑了个寂寞」的漏洞）。
func TestOnlineNodesForProtocolBoundOfflineReturnsEmpty(t *testing.T) {
	db := newNodesBindingDB(t, region04Schema("offline"))
	nodesFilterFixture(t, db)
	store := NewSQLStore(db)

	nodes, err := store.OnlineNodesForProtocol(context.Background(), "vless", 103)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("返回节点 id = %v, 期望空列表（绑定节点已离线）", nodeIDsOf(nodes))
	}
}
