package main

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// ───────────────────────── 地区功能建表（实连 MySQL） ─────────────────────────
//
// 这两条用例验证的是「老用户升级」这条唯一入口 ensureSchema 的两条硬约束：
//  1. 幂等 —— 重复启动/重复升级第二次不出错、表结构一字不差；
//  2. 零数据迁移 —— 升级前后 users / nodes 的原有列逐行一致，
//     region_id / capacity 全为 NULL，regions / user_nodes 保持空表。
//
// 刻意不用 controlhub_test 做主战场：那里有真实业务数据，断言会被数据干扰。
// 每条用例自建 schema（ch_region01_*），跑完 DROP，互不影响。

// baseSchemaDDL 是 ensureSchema 依赖的「升级前已存在」最小表集合：
// ensureSchema 除了建新表，还会给 xray_inbounds / users 加列、给 nodes 加列、给 plans 加
// region_ids 列、给 node_api_commands / mail_logs 加索引 —— 缺表会直接报 1146，
// 所以这里必须先把这些表按升级前的样子建出来。
const baseSchemaDDL = `
CREATE TABLE users (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  email varchar(255) NOT NULL DEFAULT '',
  plan_id bigint unsigned DEFAULT NULL,
  enabled tinyint(1) NOT NULL DEFAULT 1,
  traffic_used bigint NOT NULL DEFAULT 0,
  created_at timestamp NULL DEFAULT NULL,
  updated_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE orders (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  order_no varchar(32) NOT NULL,
  user_id bigint unsigned NOT NULL,
  order_type varchar(16) NOT NULL DEFAULT 'plan',
  plan_id bigint unsigned DEFAULT NULL,
  amount decimal(10,2) NOT NULL,
  status varchar(16) NOT NULL DEFAULT 'pending',
  trade_no varchar(64) DEFAULT NULL,
  paid_at timestamp NULL DEFAULT NULL,
  created_at timestamp NULL DEFAULT NULL,
  updated_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY orders_order_no_unique (order_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE nodes (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  name varchar(255) NOT NULL,
  host varchar(255) NOT NULL,
  port int NOT NULL DEFAULT 443,
  driver_type varchar(32) NOT NULL DEFAULT '3x-ui',
  cpu decimal(5,2) DEFAULT NULL,
  mem decimal(5,2) DEFAULT NULL,
  status varchar(16) NOT NULL DEFAULT 'offline',
  created_at timestamp NULL DEFAULT NULL,
  updated_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_inbounds (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  tag varchar(64) NOT NULL,
  enabled tinyint(1) NOT NULL DEFAULT 1,
  PRIMARY KEY (id),
  UNIQUE KEY xray_inbounds_node_id_tag_unique (node_id, tag)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE node_api_commands (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  user_id bigint unsigned DEFAULT NULL,
  action varchar(16) NOT NULL,
  status varchar(16) NOT NULL DEFAULT 'pending',
  created_at timestamp NULL DEFAULT NULL,
  updated_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE mail_logs (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  type varchar(20) NOT NULL DEFAULT 'batch',
  status varchar(20) NOT NULL DEFAULT 'sent',
  created_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE plans (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  name varchar(64) NOT NULL,
  type varchar(16) NOT NULL DEFAULT 'period',
  price decimal(10,2) NOT NULL DEFAULT 0.00,
  is_active tinyint(1) NOT NULL DEFAULT 1,
  third_party_sub_ids text,
  created_at timestamp NULL DEFAULT NULL,
  updated_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

`

// usersOriginalColumns / nodesOriginalColumns 是升级前就有的列。
// 对比升级前后数据必须按这批列比 —— ensureSchema 会新加 users.balance、
// nodes.region_id / capacity，用 SELECT * 比会把「新加列」误判成「数据变了」。
const usersOriginalColumns = "id, email, plan_id, enabled, traffic_used, created_at, updated_at"
const nodesOriginalColumns = "id, name, host, port, driver_type, cpu, mem, status, created_at, updated_at"

// regionTestDSN 连本机 MySQL（root/空密码），可用 REGION01_MYSQL_DSN 覆盖。
func regionTestDSN() string {
	if dsn := os.Getenv("REGION01_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// withSchemaName 把 DSN 的默认库换成 schema（DSN 形如 root:@tcp(host:port)/?timeout=3s）。
func withSchemaName(dsn, schema string) string {
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

// newRegionTestSchema 建一个干净的测试库 + 升级前的基础表，返回连到该库的连接。
// 用完自动 DROP —— 不污染本机任何已有数据库。
func newRegionTestSchema(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", regionTestDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过地区建表集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过地区建表集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", regionTestDSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", withSchemaName(regionTestDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range strings.Split(baseSchemaDDL, ";\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建升级前基础表失败: %v\nSQL: %s", err, stmt)
		}
	}
	return db
}

// schemaFingerprint 把地区相关结构压成一个可比字符串：
// 4 张新表的 SHOW CREATE TABLE + nodes 的新列/新索引。
var autoIncrementRe = regexp.MustCompile(`\s*AUTO_INCREMENT=\d+`)

func schemaFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()

	var b strings.Builder
	for _, table := range []string{"regions", "user_nodes", "user_node_migrations", "node_metrics"} {
		var name, create string
		if err := db.QueryRow("SHOW CREATE TABLE `"+table+"`").Scan(&name, &create); err != nil {
			t.Fatalf("取 %s 建表语句失败: %v", table, err)
		}
		b.WriteString("-- " + table + "\n" + autoIncrementRe.ReplaceAllString(create, "") + "\n")
	}

	for _, column := range []string{"region_id", "capacity"} {
		var dataType, nullable string
		err := db.QueryRow(
			"SELECT COLUMN_TYPE, IS_NULLABLE FROM information_schema.COLUMNS "+
				"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='nodes' AND COLUMN_NAME=?", column).Scan(&dataType, &nullable)
		if err != nil {
			t.Fatalf("取 nodes.%s 失败: %v", column, err)
		}
		b.WriteString(fmt.Sprintf("-- nodes.%s %s null=%s\n", column, dataType, nullable))
	}

	var indexCols string
	err := db.QueryRow(
		"SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS " +
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='nodes' AND INDEX_NAME='idx_nodes_region'").Scan(&indexCols)
	if err != nil {
		t.Fatalf("取 idx_nodes_region 失败: %v", err)
	}
	b.WriteString("-- nodes.idx_nodes_region " + indexCols + "\n")
	return b.String()
}

// tableRows 按列顺序把整表导成字符串切片，用于「升级前后逐行一致」的对比。
func tableRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()

	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("导出数据失败（%s）: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("取列名失败: %v", err)
	}

	out := []string{strings.Join(cols, "|")}
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("扫描行失败: %v", err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "<NULL>"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历数据失败: %v", err)
	}
	return out
}

func countOf(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("计数失败（%s）: %v", query, err)
	}
	return n
}

// 幂等：连跑两次 ensureSchema，第二次不报错，且 4 张新表 + nodes 两列一索引的结构一字不差。
func TestEnsureSchemaRegionsIdempotent(t *testing.T) {
	db := newRegionTestSchema(t, "ch_region01_idem")

	if err := ensureSchema(db); err != nil {
		t.Fatalf("第一次 ensureSchema 失败: %v", err)
	}
	first := schemaFingerprint(t, db)

	if err := ensureSchema(db); err != nil {
		t.Fatalf("第二次 ensureSchema 失败（不幂等）: %v", err)
	}
	second := schemaFingerprint(t, db)

	if first != second {
		t.Fatalf("第二次执行后结构变了：\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", first, second)
	}

	// 新表必须是空表：升级不塞任何默认地区 / 用户绑定。
	for _, table := range []string{"regions", "user_nodes", "user_node_migrations", "node_metrics"} {
		if n := countOf(t, db, "SELECT COUNT(*) FROM `"+table+"`"); n != 0 {
			t.Fatalf("%s 迁移后应为空表，实际 %d 行", table, n)
		}
	}
	// 新列必须可空：region_id 为 NULL 就是「不参与地区过滤」的升级默认状态。
	for _, column := range []string{"region_id", "capacity"} {
		var nullable string
		if err := db.QueryRow(
			"SELECT IS_NULLABLE FROM information_schema.COLUMNS "+
				"WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='nodes' AND COLUMN_NAME=?", column).Scan(&nullable); err != nil {
			t.Fatalf("查 nodes.%s 失败: %v", column, err)
		}
		if nullable != "YES" {
			t.Fatalf("nodes.%s 必须可空，实际 IS_NULLABLE=%s", column, nullable)
		}
	}
	// 入站级分组（3.1.0）：xray_inbounds.region_id 必须被加上且可空 ——
	// 老数据全 NULL = 跟随节点地区，行为与升级前一致。
	var inboundRegionType, inboundRegionNullable string
	if err := db.QueryRow(
		"SELECT COLUMN_TYPE, IS_NULLABLE FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='xray_inbounds' AND COLUMN_NAME='region_id'").
		Scan(&inboundRegionType, &inboundRegionNullable); err != nil {
		t.Fatalf("ensureSchema 没给 xray_inbounds 加 region_id 列: %v", err)
	}
	if inboundRegionType != "bigint unsigned" {
		t.Fatalf("xray_inbounds.region_id 类型 = %s，期望 bigint unsigned", inboundRegionType)
	}
	if inboundRegionNullable != "YES" {
		t.Fatalf("xray_inbounds.region_id 必须可空（NULL=跟随节点地区），实际 IS_NULLABLE=%s", inboundRegionNullable)
	}
	var inboundIdx string
	if err := db.QueryRow(
		"SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS " +
			"WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='xray_inbounds' AND INDEX_NAME='idx_xray_inbounds_region'").
		Scan(&inboundIdx); err != nil {
		t.Fatalf("取 idx_xray_inbounds_region 失败: %v", err)
	}
	if inboundIdx != "region_id" {
		t.Fatalf("idx_xray_inbounds_region 的列 = %q，期望 region_id", inboundIdx)
	}

	// 套餐地区（#地区功能-15）：plans.region_ids 必须被加上，JSON 且可空 —— 老套餐就是 NULL（不限地区）。
	var regionIDsType, regionIDsNullable string
	if err := db.QueryRow(
		"SELECT COLUMN_TYPE, IS_NULLABLE FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='plans' AND COLUMN_NAME='region_ids'").
		Scan(&regionIDsType, &regionIDsNullable); err != nil {
		t.Fatalf("ensureSchema 没给 plans 加 region_ids 列: %v", err)
	}
	if regionIDsType != "json" {
		t.Fatalf("plans.region_ids 类型 = %s，期望 json", regionIDsType)
	}
	if regionIDsNullable != "YES" {
		t.Fatalf("plans.region_ids 必须可空（NULL=不限地区），实际 IS_NULLABLE=%s", regionIDsNullable)
	}

	// 独占 socks 池已随代理商城下线移除，这里不再校验 proxy_socks 的列与索引。
}

// 零数据迁移：升级前后 users / nodes 的原有列逐行一致，region_id / capacity 全 NULL。
func TestEnsureSchemaRegionsNoDataMigration(t *testing.T) {
	db := newRegionTestSchema(t, "ch_region01_zero")

	// 升级前的老数据：3 个有套餐的在用用户 + 1 个没套餐的 + 2 个老节点。
	fixtures := []string{
		"INSERT INTO users (email, plan_id, enabled, traffic_used) VALUES ('a@example.com', 1, 1, 100)",
		"INSERT INTO users (email, plan_id, enabled, traffic_used) VALUES ('b@example.com', 2, 1, 200)",
		"INSERT INTO users (email, plan_id, enabled, traffic_used) VALUES ('c@example.com', 1, 1, 300)",
		"INSERT INTO users (email, plan_id, enabled, traffic_used) VALUES ('d@example.com', NULL, 1, 0)",
		"INSERT INTO users (email, plan_id, enabled, traffic_used) VALUES ('e@example.com', 3, 0, 500)",
		"INSERT INTO nodes (name, host, status) VALUES ('hk-1', '1.1.1.1', 'online')",
		"INSERT INTO nodes (name, host, status) VALUES ('jp-1', '2.2.2.2', 'online')",
	}
	for _, stmt := range fixtures {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("造老数据失败: %v\nSQL: %s", err, stmt)
		}
	}

	usersBefore := tableRows(t, db, "SELECT "+usersOriginalColumns+" FROM users ORDER BY id")
	nodesBefore := tableRows(t, db, "SELECT "+nodesOriginalColumns+" FROM nodes ORDER BY id")

	if err := ensureSchema(db); err != nil {
		t.Fatalf("第一次 ensureSchema 失败: %v", err)
	}
	if err := ensureSchema(db); err != nil {
		t.Fatalf("第二次 ensureSchema 失败: %v", err)
	}

	usersAfter := tableRows(t, db, "SELECT "+usersOriginalColumns+" FROM users ORDER BY id")
	nodesAfter := tableRows(t, db, "SELECT "+nodesOriginalColumns+" FROM nodes ORDER BY id")
	if strings.Join(usersBefore, "\n") != strings.Join(usersAfter, "\n") {
		t.Fatalf("升级改动了 users 老数据：\n--- 升级前 ---\n%s\n--- 升级后 ---\n%s",
			strings.Join(usersBefore, "\n"), strings.Join(usersAfter, "\n"))
	}
	if strings.Join(nodesBefore, "\n") != strings.Join(nodesAfter, "\n") {
		t.Fatalf("升级改动了 nodes 老数据：\n--- 升级前 ---\n%s\n--- 升级后 ---\n%s",
			strings.Join(nodesBefore, "\n"), strings.Join(nodesAfter, "\n"))
	}

	// 新列全 NULL：老节点不落到任何地区、不限容量。
	if n := countOf(t, db, "SELECT COUNT(*) FROM nodes WHERE region_id IS NOT NULL OR capacity IS NOT NULL"); n != 0 {
		t.Fatalf("升级后节点的 region_id/capacity 应全为 NULL，实际有 %d 行非 NULL", n)
	}
	// 零迁移：不插默认地区、不插用户节点绑定。
	if n := countOf(t, db, "SELECT COUNT(*) FROM regions"); n != 0 {
		t.Fatalf("升级不应插入默认地区，regions 实际 %d 行", n)
	}
	if n := countOf(t, db, "SELECT COUNT(*) FROM user_nodes"); n != 0 {
		t.Fatalf("升级不应插入 user_nodes 绑定，实际 %d 行", n)
	}
}
