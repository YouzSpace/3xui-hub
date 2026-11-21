package main

import (
	"fmt"
	"os"
	"testing"
)

// TestEnsureSchemaAddsLegacyUserColumns 复现真实客户库：极老版本建的 users 表缺
// deleted_at 等列，升级到新版后「批量入组」直接 SQL 1054 Unknown column。
//
// 修法是把这些列加进 ensureSchema 的幂等补列清单；本用例用升级前的最小表结构
// （baseSchemaDDL 的 users 只有 7 列）跑两遍 ensureSchema：
//  1. 第一遍必须把缺的列补上；
//  2. 第二遍必须不报错（幂等）。
func TestEnsureSchemaAddsLegacyUserColumns(t *testing.T) {
	db := newRegionTestSchema(t, fmt.Sprintf("ch_legacy_users_%d", os.Getpid()))

	if err := ensureSchema(db); err != nil {
		t.Fatalf("第一遍 ensureSchema 失败: %v", err)
	}
	if err := ensureSchema(db); err != nil {
		t.Fatalf("第二遍 ensureSchema 失败（幂等被破坏）: %v", err)
	}

	want := []string{
		"deleted_at",            // 客户报的 1054 就是它
		"traffic_disabled_at",   // 到期/超量关流量
		"used_invite_code_id",   // 邀请码锁死
		"invite_code_used_at",   //
		"next_traffic_reset_at", // 月度重置
		"monthly_traffic_used",  //
		"monthly_traffic_limit", //
	}
	for _, col := range want {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = 'users' AND column_name = ?`, col).Scan(&n)
		if err != nil {
			t.Fatalf("查 users.%s 失败: %v", col, err)
		}
		if n != 1 {
			t.Fatalf("ensureSchema 没补上 users.%s（老库升级后会 1054）", col)
		}
	}
}
