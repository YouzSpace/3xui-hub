package main

// 配置模板（#地区功能-19）的建表/加列用例：验证「老用户升级」这条唯一入口。
//
// ensureSchema 是唯一建表入口（install.sh 只在全新安装时跑 schema.sql），
// 所以三张 xray_template_* 表与 nodes.is_custom_config 必须在这里补上，
// 且重复执行不报错、不重建、不塞数据 —— 升级后库里就是三张空表 + 一列默认 0。

import (
	"database/sql"
	"testing"
)

// templateSchema19Tables 是本功能新增的三张表。
var templateSchema19Tables = []string{"xray_template_inbounds", "xray_template_outbounds", "xray_template_routing"}

// TestEnsureSchemaXrayTemplatesIdempotent 连跑两次 ensureSchema：
// 三张表建出来、结构不变、保持空表；nodes.is_custom_config 存在且默认 0。
func TestEnsureSchemaXrayTemplatesIdempotent(t *testing.T) {
	db := newRegionTestSchema(t, "ch_region19_schema")

	for i := 1; i <= 2; i++ {
		if err := ensureSchema(db); err != nil {
			t.Fatalf("第 %d 次 ensureSchema 失败: %v", i, err)
		}
	}

	// 三张表必须存在，且列与任务书一致（逐表核对关键列）。
	wantColumns := map[string][]string{
		"xray_template_inbounds":  {"id", "name", "sort", "tag", "remark", "protocol", "port", "listen", "settings", "stream_settings", "sniffing", "enabled", "created_at", "updated_at"},
		"xray_template_outbounds": {"id", "name", "sort", "tag", "protocol", "settings", "stream_settings", "enabled", "created_at", "updated_at"},
		"xray_template_routing":   {"id", "name", "sort", "domains", "ips", "port", "network", "protocol", "inbound_tag", "outbound_tag", "remark", "enabled", "created_at", "updated_at"},
	}
	for _, table := range templateSchema19Tables {
		got := tableColumns(t, db, table)
		for _, column := range wantColumns[table] {
			if !got[column] {
				t.Fatalf("%s 缺列 %s（实际列: %v）", table, column, got)
			}
		}
		// 升级不塞数据：模板表保持为空，不套用的节点行为完全不变。
		if n := countOf(t, db, "SELECT COUNT(*) FROM `"+table+"`"); n != 0 {
			t.Fatalf("%s 升级后应为空表，实际 %d 行", table, n)
		}
	}

	// xray_template_inbounds 刻意不含 reality_private_key：私钥必须每节点单独生成。
	if tableColumns(t, db, "xray_template_inbounds")["reality_private_key"] {
		t.Fatalf("xray_template_inbounds 不该有 reality_private_key 列（私钥必须每节点单独生成）")
	}

	// nodes.is_custom_config：NOT NULL DEFAULT 0 —— 老节点升级后就是「跟随模板」。
	var columnType, nullable, columnDefault string
	if err := db.QueryRow(
		"SELECT COLUMN_TYPE, IS_NULLABLE, IFNULL(COLUMN_DEFAULT, '') FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='nodes' AND COLUMN_NAME='is_custom_config'").
		Scan(&columnType, &nullable, &columnDefault); err != nil {
		t.Fatalf("ensureSchema 没给 nodes 加 is_custom_config 列: %v", err)
	}
	if columnType != "tinyint" {
		t.Fatalf("nodes.is_custom_config 类型 = %s，期望 tinyint", columnType)
	}
	if nullable != "NO" {
		t.Fatalf("nodes.is_custom_config 必须 NOT NULL，实际 IS_NULLABLE=%s", nullable)
	}
	if columnDefault != "0" {
		t.Fatalf("nodes.is_custom_config 默认值 = %q，期望 0（升级后老节点跟随模板）", columnDefault)
	}
	// 老数据默认值必须是 0：升级不改已有节点的行为。
	if _, err := db.Exec("INSERT INTO nodes (name, host) VALUES ('legacy', 'legacy.example.com')"); err != nil {
		t.Fatalf("造升级后节点失败: %v", err)
	}
	if n := countOf(t, db, "SELECT COUNT(*) FROM nodes WHERE is_custom_config <> 0"); n != 0 {
		t.Fatalf("新插入节点的 is_custom_config 应为 0，实际有 %d 行非 0", n)
	}
}

// tableColumns 取一张表的列名集合（表不存在时直接失败）。
func tableColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(
		"SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", table)
	if err != nil {
		t.Fatalf("查 %s 的列失败: %v", table, err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫列名失败: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列名失败: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("表 %s 不存在（ensureSchema 没建）", table)
	}
	return out
}
