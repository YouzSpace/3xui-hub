package api

// 用户端节点列表按「套餐地区」过滤（#地区功能-15）集成测试：真连 MySQL，独立库 ch_region15_*。
//
// 口径必须与 subscription.SubscribableNodes 一致（同一份 planregion.NodeFilter）：
//   - 套餐 region_ids=[1] → 只返回 1 号地区的节点 + region_id 为 NULL 的老节点；
//   - 套餐 region_ids=NULL → 老行为，绑定的节点全都在。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// planRegionSchema 拼独立的测试库名（ch_region15_<用例>_<pid>，自建自删）。
func planRegionSchema(caseName string) string {
	return fmt.Sprintf("ch_region15_%s_%d", caseName, os.Getpid())
}

// addOnlineNode 插一个在线且有 vless 入站的节点；regionID=0 表示 region_id = NULL（老节点）。
func addOnlineNode(t *testing.T, db *sql.DB, id, regionID uint64) {
	t.Helper()
	var region any
	if regionID > 0 {
		region = regionID
	}
	if _, err := db.Exec(
		`INSERT INTO nodes (id, name, host, port, enabled, status, driver_type, latency, region_id)
		 VALUES (?, ?, ?, 443, 1, 'online', '3x-ui', 20, ?)`,
		id, fmt.Sprintf("节点%02d", id), fmt.Sprintf("10.0.0.%d", id), region,
	); err != nil {
		t.Fatalf("插入节点 %d 失败: %v", id, err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_inbounds (node_id, protocol, inbound_id) VALUES (?, 'vless', ?)`,
		id, 1000+id,
	); err != nil {
		t.Fatalf("插入节点 %d 入站失败: %v", id, err)
	}
}

// addPlanUser 建套餐（regionIDs 传 "" 表示 NULL=不限地区）+ 用户（planID=0 表示没套餐）+ 绑定。
func addPlanUser(t *testing.T, db *sql.DB, planID int64, regionIDs string, userID int64, planForUser int64, nodeIDs ...uint64) {
	t.Helper()
	if planID > 0 {
		var raw any
		if regionIDs != "" {
			raw = regionIDs
		}
		if _, err := db.Exec(`INSERT INTO plans (id, name, region_ids) VALUES (?, ?, ?)`,
			planID, fmt.Sprintf("plan-%d", planID), raw); err != nil {
			t.Fatalf("插入套餐 %d 失败: %v", planID, err)
		}
	}
	var planArg any
	if planForUser > 0 {
		planArg = planForUser
	}
	if _, err := db.Exec(`INSERT INTO users (id, plan_id) VALUES (?, ?)`, userID, planArg); err != nil {
		t.Fatalf("插入用户 %d 失败: %v", userID, err)
	}
	for _, nodeID := range nodeIDs {
		if _, err := db.Exec(`INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)`, userID, nodeID); err != nil {
			t.Fatalf("插入绑定 (%d→%d) 失败: %v", userID, nodeID, err)
		}
	}
}

// TestOnlineNodesPlanRegionFilters 套餐 [1]：2 号地区的节点不可见，老节点（region_id NULL）仍然可见。
func TestOnlineNodesPlanRegionFilters(t *testing.T) {
	db := newNodesBindingDB(t, planRegionSchema("planregion"))
	addOnlineNode(t, db, 1, 1)
	addOnlineNode(t, db, 2, 2)
	addOnlineNode(t, db, 3, 0)
	addPlanUser(t, db, 11, "[1]", 201, 11, 1, 2, 3)
	store := NewSQLStore(db)

	nodes, err := store.OnlineNodesForProtocol(context.Background(), "vless", 201)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	assertNodeIDs(t, nodes, []uint64{1, 3})
}

// TestOnlineNodesUnlimitedPlanKeepsAll 套餐不限地区（NULL）：老行为，绑定的节点全都在。
func TestOnlineNodesUnlimitedPlanKeepsAll(t *testing.T) {
	db := newNodesBindingDB(t, planRegionSchema("unlimited"))
	addOnlineNode(t, db, 1, 1)
	addOnlineNode(t, db, 2, 2)
	addPlanUser(t, db, 12, "", 202, 12, 1, 2)
	store := NewSQLStore(db)

	nodes, err := store.OnlineNodesForProtocol(context.Background(), "vless", 202)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	assertNodeIDs(t, nodes, []uint64{1, 2})
}
