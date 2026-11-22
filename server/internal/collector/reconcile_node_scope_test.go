package collector

// 对账「按节点取用户」的集成测试（地区功能 #06，实连 MySQL）。
//
// 验证的只有一件事：reconcileUsers(ctx, nodeID) 的下发过滤口径（按节点是否归属地区）
//   - 已归属地区的节点 A 绑了 1 个用户 → 只返回这 1 个；
//   - 未归属地区（region_id IS NULL）的节点 B 即使有旧绑定 → 返回全量（移出地区即全量）；
//   - nodeID <= 0 → 全量（不过滤）。
//
// 为什么必须有这条用例：已归属节点若按全量用户逐节点比对，绑到别的节点的人会在每台
// 都已归属节点上被判成「节点上缺人」→ 每轮补挂 → bump config_version → 内核反复重启。
//
// 刻意不用 controlhub_test：那里有真实业务数据，断言会被干扰。本用例自建
// schema（ch_region06_reconcile），跑完 DROP，不碰任何已有库。

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// reconcileScopeDDL 只建 reconcileUsers 这条查询真正用到的表与列
// （users 的全列 + plans.type + user_nodes + nodes.region_id）。
const reconcileScopeDDL = `
CREATE TABLE plans (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  type varchar(16) NOT NULL DEFAULT '',
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE users (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  uuid varchar(64) NOT NULL DEFAULT '',
  protocol varchar(16) NOT NULL DEFAULT '',
  enabled tinyint(1) NOT NULL DEFAULT 1,
  traffic_disabled_at timestamp NULL DEFAULT NULL,
  expired_at timestamp NULL DEFAULT NULL,
  traffic_limit bigint NOT NULL DEFAULT 0,
  traffic_used bigint NOT NULL DEFAULT 0,
  monthly_traffic_used bigint NOT NULL DEFAULT 0,
  monthly_traffic_limit bigint NOT NULL DEFAULT 0,
  plan_id bigint unsigned DEFAULT NULL,
  email varchar(255) NOT NULL DEFAULT '',
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

// region06DSN 连本机 MySQL（root/空密码），可用 REGION06_MYSQL_DSN 覆盖。
//
// parseTime=true 是必需的：reconcileUsers 把 expired_at / traffic_disabled_at
// 扫进 sql.NullTime，驱动不解析时间就会报 unsupported Scan。
func region06DSN() string {
	if dsn := os.Getenv("REGION06_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

// region06SchemaDSN 把 DSN 的默认库换成 schema（DSN 形如 root:@tcp(host:port)/?opt）。
func region06SchemaDSN(dsn, schema string) string {
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

// newRegion06Schema 建一个干净的测试库 + 给定 DDL，返回连到该库的连接；用完自动 DROP。
func newRegion06Schema(t *testing.T, schema, ddl string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", region06DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过按节点取用户的集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过按节点取用户的集成测试", err)
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
		admin2, err := sql.Open("mysql", region06DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region06SchemaDSN(region06DSN(), schema))
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

// TestReconcileUsersByNodeScope 是 #06 的验收用例：
// 3 个用户在库，已归属地区的节点 A 绑 1 人 → 取到 1 人；
// 未归属地区的节点 B 即使有旧绑定 → 取全量 3 人（移出地区即全量注册）。
func TestReconcileUsersByNodeScope(t *testing.T) {
	db := newRegion06Schema(t, "ch_region06_reconcile", reconcileScopeDDL)

	col, err := New(Options{DB: db, AppKey: bytes.Repeat([]byte("k"), 32)})
	if err != nil {
		t.Fatalf("构造 Collector 失败: %v", err)
	}
	ctx := context.Background()

	const (
		nodeA int64 = 101 // 已归属地区，绑了 user 1
		nodeB int64 = 102 // 未归属地区（NULL），绑了 user 1（旧绑定保留）
	)

	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("准备数据失败: %v\nSQL: %s", err, query)
		}
	}
	mustExec("INSERT INTO plans (id, type) VALUES (1, 'period')")
	for i, proto := range []string{"vless", "vless", "vmess"} {
		id := int64(i + 1)
		mustExec("INSERT INTO users (id, uuid, protocol, enabled, plan_id, email) VALUES (?, ?, ?, 1, 1, ?)",
			id, fmt.Sprintf("uuid-%d", id), proto, fmt.Sprintf("region06-u%d@example.com", id))
	}
	// 节点 A 归属地区 7；节点 B 未归属地区（NULL）。两个节点都有 user 1 的旧绑定。
	mustExec("INSERT INTO nodes (id, region_id) VALUES (?, 7)", nodeA)
	mustExec("INSERT INTO nodes (id, region_id) VALUES (?, NULL)", nodeB)
	mustExec("INSERT INTO user_nodes (user_id, node_id) VALUES (1, ?)", nodeA)
	mustExec("INSERT INTO user_nodes (user_id, node_id) VALUES (1, ?)", nodeB)

	ids := func(us []reconcileUser) []int64 {
		out := make([]int64, 0, len(us))
		for _, u := range us {
			out = append(out, u.ID)
		}
		return out
	}

	// ① 已归属地区的节点 A：只应该拿到绑给它的 1 个人。
	gotA, err := col.reconcileUsers(ctx, nodeA)
	if err != nil {
		t.Fatalf("reconcileUsers(A) 报错: %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID != 1 {
		t.Fatalf("节点 A（已归属地区、绑 1 人）期望 [1]，实际 %v", ids(gotA))
	}

	// ② 未归属地区的节点 B：即使有旧绑定，也取全量 3 人（移出地区即全量注册）。
	gotB, err := col.reconcileUsers(ctx, nodeB)
	if err != nil {
		t.Fatalf("reconcileUsers(B) 报错: %v", err)
	}
	if len(gotB) != 3 {
		t.Fatalf("节点 B 未归属地区，期望全量 3 人（旧绑定不应切片），实际 %d 人 %v", len(gotB), ids(gotB))
	}

	// ③ nodeID <= 0：明确不过滤（全局面板口径）。
	gotAll, err := col.reconcileUsers(ctx, 0)
	if err != nil {
		t.Fatalf("reconcileUsers(0) 报错: %v", err)
	}
	if len(gotAll) != 3 {
		t.Fatalf("nodeID=0 期望不过滤（3 人），实际 %d 人 %v", len(gotAll), ids(gotAll))
	}
}
