package main

// 静态 IP 出口分类（#静态IP）的建表/加列用例：验证「老用户升级」这条唯一入口。
//
// ensureSchema 是唯一建表入口（install.sh 只在全新安装时跑 schema.sql），
// 所以 exit_profiles / exit_profile_nodes 两张表与 plans 的两个新列必须在这里补上，
// 且重复执行不报错、不重建、不塞数据 —— 升级后就是两张空表 + 老套餐自动是 direct。

import (
	"testing"
)

// TestEnsureSchemaExitProfilesIdempotent 连跑两次 ensureSchema：
// 两张表建出来、结构不变、保持空表；plans.exit_type/exit_profile_id 存在且默认 direct/NULL。
func TestEnsureSchemaExitProfilesIdempotent(t *testing.T) {
	db := newRegionTestSchema(t, "ch_region30_schema")

	for i := 1; i <= 2; i++ {
		if err := ensureSchema(db); err != nil {
			t.Fatalf("第 %d 次 ensureSchema 失败: %v", i, err)
		}
	}

	// 分类表：列与任务书一致（逐列核对，少一列读写都会 Unknown column）。
	wantProfileColumns := []string{
		"id", "name", "provider_id", "traffic_gb", "duration_days", "max_thread_count",
		"socks_host", "socks_port", "enabled", "created_at", "updated_at",
	}
	profileCols := tableColumns(t, db, "exit_profiles")
	for _, column := range wantProfileColumns {
		if !profileCols[column] {
			t.Fatalf("exit_profiles 缺列 %s（实际列: %v）", column, profileCols)
		}
	}

	// 关联表：复合主键 (profile_id,node_id) + node_id 索引（按节点反查分类要用）。
	nodeCols := tableColumns(t, db, "exit_profile_nodes")
	for _, column := range []string{"profile_id", "node_id"} {
		if !nodeCols[column] {
			t.Fatalf("exit_profile_nodes 缺列 %s（实际列: %v）", column, nodeCols)
		}
	}
	var pkCols string
	if err := db.QueryRow(
		"SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS " +
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='exit_profile_nodes' AND INDEX_NAME='PRIMARY'").
		Scan(&pkCols); err != nil {
		t.Fatalf("查 exit_profile_nodes 主键失败: %v", err)
	}
	if pkCols != "profile_id,node_id" {
		t.Fatalf("exit_profile_nodes 主键应为 (profile_id,node_id)，实际 %q", pkCols)
	}

	// 升级不塞数据：两表都保持为空，老套餐行为完全不变。
	for _, table := range []string{"exit_profiles", "exit_profile_nodes"} {
		if n := countOf(t, db, "SELECT COUNT(*) FROM `"+table+"`"); n != 0 {
			t.Fatalf("%s 升级后应为空表，实际 %d 行", table, n)
		}
	}

	// plans.exit_type：NOT NULL DEFAULT 'direct' —— 老套餐升级后自动是直连。
	var (
		exitTypeType, exitTypeNullable, exitTypeDefault string
	)
	if err := db.QueryRow(
		"SELECT COLUMN_TYPE, IS_NULLABLE, IFNULL(COLUMN_DEFAULT, '') FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='plans' AND COLUMN_NAME='exit_type'").
		Scan(&exitTypeType, &exitTypeNullable, &exitTypeDefault); err != nil {
		t.Fatalf("ensureSchema 没给 plans 加 exit_type 列: %v", err)
	}
	if exitTypeNullable != "NO" {
		t.Fatalf("plans.exit_type 必须 NOT NULL，实际 IS_NULLABLE=%s", exitTypeNullable)
	}
	if exitTypeDefault != "direct" {
		t.Fatalf("plans.exit_type 默认值 = %q，期望 direct（升级后老套餐直连）", exitTypeDefault)
	}

	// plans.exit_profile_id：可空（direct/static_socks 套餐不绑分类）。
	var exitProfNullable string
	if err := db.QueryRow(
		"SELECT IS_NULLABLE FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='plans' AND COLUMN_NAME='exit_profile_id'").
		Scan(&exitProfNullable); err != nil {
		t.Fatalf("ensureSchema 没给 plans 加 exit_profile_id 列: %v", err)
	}
	if exitProfNullable != "YES" {
		t.Fatalf("plans.exit_profile_id 必须可空（direct 套餐不绑分类），实际 IS_NULLABLE=%s", exitProfNullable)
	}

	// 老套餐插入后 exit_type 必须是 direct（不是空串）：读侧按它决定出站类型。
	if _, err := db.Exec("INSERT INTO plans (name, type) VALUES ('老套餐', 'period')"); err != nil {
		t.Fatalf("造升级后套餐失败: %v", err)
	}
	var exitType string
	if err := db.QueryRow("SELECT exit_type FROM plans WHERE name = '老套餐'").Scan(&exitType); err != nil {
		t.Fatalf("读新套餐 exit_type 失败: %v", err)
	}
	if exitType != "direct" {
		t.Fatalf("新套餐默认 exit_type = %q，期望 direct", exitType)
	}

	// 绑分类外键只建了普通索引（不建外键约束：老库升级加不了），
	// 但删除守卫要按 exit_profile_id 数套餐，索引必须在。
	var indexCols string
	if err := db.QueryRow(
		"SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS " +
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='plans' AND INDEX_NAME='idx_plans_exit_profile'").
		Scan(&indexCols); err != nil {
		t.Fatalf("查询 idx_plans_exit_profile 失败: %v", err)
	}
	if indexCols != "exit_profile_id" {
		t.Fatalf("idx_plans_exit_profile 列应为 exit_profile_id，实际 %q", indexCols)
	}
}
