package admin

// 管理端「协议分布」按节点统计的集成测试（地区功能 #06，实连 MySQL）。
//
// 验证 EligibleProtocolCounts(ctx, nodeID) 的下发过滤口径（按节点是否归属地区）：
//   - 已归属地区的节点 A 绑了 1 个 vless 用户 → A = vless:1（按绑定切片）；
//   - nodeID = 0 → 不过滤（全局面板口径）→ vless:3；
//   - 未归属地区（region_id=NULL）的节点，即使有旧绑定 → 全量（移出地区即全量，保留绑定）。
//
// 自建 schema（ch_region06_counts）、跑完 DROP，不碰任何已有库。

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// countsScopeDDL 只建这条统计 SQL 用到的表与列（users 的筛选列 + user_nodes + nodes 的 region_id）。
const countsScopeDDL = `
CREATE TABLE users (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  protocol varchar(16) NOT NULL DEFAULT '',
  enabled tinyint(1) NOT NULL DEFAULT 1,
  traffic_disabled_at timestamp NULL DEFAULT NULL,
  plan_id bigint unsigned DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE user_nodes (
  user_id bigint unsigned NOT NULL,
  node_id bigint unsigned NOT NULL,
  PRIMARY KEY (user_id, node_id),
  KEY idx_node (node_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE nodes (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  region_id bigint unsigned DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

// countsTestDSN 连本机 MySQL（root/空密码），可用 REGION06_MYSQL_DSN 覆盖。
func countsTestDSN() string {
	if dsn := os.Getenv("REGION06_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

// countsSchemaDSN 把 DSN 的默认库换成 schema（DSN 形如 root:@tcp(host:port)/?opt）。
func countsSchemaDSN(dsn, schema string) string {
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

// newCountsSchema 建一个干净的测试库 + 给定 DDL，返回连到该库的连接；用完自动 DROP。
func newCountsSchema(t *testing.T, schema, ddl string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", countsTestDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过协议分布集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过协议分布集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	// 本机可能有多个 agent 同时跑 go test（共用一个 MySQL），建库/建表偶发瞬时错误
	// （锁等待、连接被挤掉）。只重试一次，并把首次错误写进失败信息，避免用例假红。
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		time.Sleep(300 * time.Millisecond)
		if _, err2 := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err2 != nil {
			t.Fatalf("建测试库失败: %v（重试后: %v）", err, err2)
		}
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", countsTestDSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", countsSchemaDSN(countsTestDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range strings.Split(ddl, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			time.Sleep(300 * time.Millisecond)
			if _, err2 := db.Exec(stmt); err2 != nil {
				t.Fatalf("建表失败: %v（重试后: %v）\nSQL: %s", err, err2, stmt)
			}
		}
	}
	return db
}

// TestEligibleProtocolCountsByNodeScope 是 #06 的验收用例：
// 节点 A（归属地区）绑 1 个 vless 用户 → A = vless:1，nodeID=0 = vless:3；
// 节点 B 未归属地区（region=NULL）但有旧绑定 → 全量 vless:3（移出地区即全量）。
func TestEligibleProtocolCountsByNodeScope(t *testing.T) {
	db := newCountsSchema(t, "ch_region06_counts", countsScopeDDL)
	store := NewNodeSQLStore(db, nil)
	ctx := context.Background()

	const (
		nodeA     int64 = 201 // 已归属地区，绑了 user 1
		nodeB     int64 = 202 // 未归属地区（NULL），绑了 user 1（旧绑定保留）
		regionOfA int64 = 7
	)

	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("准备数据失败: %v\nSQL: %s", err, query)
		}
	}
	for id := 1; id <= 3; id++ {
		mustExec("INSERT INTO users (id, protocol, enabled, plan_id) VALUES (?, 'vless', 1, 9)", id)
	}
	// 节点 A 归属地区 7；节点 B 未归属地区（NULL）。
	mustExec("INSERT INTO nodes (id, region_id) VALUES (?, ?)", nodeA, regionOfA)
	mustExec("INSERT INTO nodes (id, region_id) VALUES (?, NULL)", nodeB)
	// 两个节点都有 user 1 的绑定（快照旧数据：B 移出地区后旧绑定仍在）。
	mustExec("INSERT INTO user_nodes (user_id, node_id) VALUES (1, ?)", nodeA)
	mustExec("INSERT INTO user_nodes (user_id, node_id) VALUES (1, ?)", nodeB)

	// ① 节点 A（已归属地区）：只算绑给它的 1 个 vless。
	gotA, err := store.EligibleProtocolCounts(ctx, nodeA)
	if err != nil {
		t.Fatalf("EligibleProtocolCounts(A) 报错: %v", err)
	}
	if gotA["vless"] != 1 || len(gotA) != 1 {
		t.Fatalf("节点 A 期望 vless:1 且只有 vless，实际 %v", gotA)
	}

	// ② nodeID = 0：不过滤（全局面板口径）。
	gotAll, err := store.EligibleProtocolCounts(ctx, 0)
	if err != nil {
		t.Fatalf("EligibleProtocolCounts(0) 报错: %v", err)
	}
	if gotAll["vless"] != 3 || len(gotAll) != 1 {
		t.Fatalf("nodeID=0 期望 vless:3 且只有 vless，实际 %v", gotAll)
	}

	// ③ 节点 B（未归属地区，有旧绑定）：全量 vless:3 —— 移出地区即全量，不因旧绑定被切片。
	gotB, err := store.EligibleProtocolCounts(ctx, nodeB)
	if err != nil {
		t.Fatalf("EligibleProtocolCounts(B) 报错: %v", err)
	}
	if gotB["vless"] != 3 || len(gotB) != 1 {
		t.Fatalf("节点 B 未归属地区，期望全量 vless:3（旧绑定不应切片），实际 %v", gotB)
	}
}
