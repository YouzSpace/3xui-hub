package subscription

// 订阅按「套餐地区」过滤（#地区功能-15）集成测试：真连 MySQL，独立库 ch_region15_*，自建自删。
//
// 要验证的是 SQL 语义，打桩只能证明参数传下去了：
//   - 套餐 region_ids=[1] → 订阅里只看得到 1 号地区的节点 + region_id 为 NULL 的老节点；
//   - 套餐 region_ids=NULL → 老行为，全部绑定的节点都在（升级前后一致）；
//   - 用户没套餐 → 同上。
//
// 变异测试靶子（删掉即必须变红）：
//   planregion.NodeFilter 生成的 `n.region_id IS NULL` 兼容分支
//   → TestRegion15SubUnassignedRegionNodeStillVisible 红（老节点被套餐地区挡掉）。

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// newPlanRegionSubFixture 建一个 ch_region15_* 独立库（自建自删），表结构复用 region03SchemaDDL。
func newPlanRegionSubFixture(t *testing.T, caseName string) *region03Fixture {
	t.Helper()
	return newRegion03Fixture(t, fmt.Sprintf("ch_region15_%s_%d", caseName, os.Getpid()))
}

// addNodeInRegion 插一个可订阅节点并把它划进 regionID（regionID=0 表示 region_id = NULL 的老节点）。
func addNodeInRegion(t *testing.T, f *region03Fixture, id int64, regionID int64) {
	t.Helper()
	f.addNode(t, id, fmt.Sprintf("node-%d", id), "online")
	if regionID > 0 {
		mustExec(t, f.db, "UPDATE nodes SET region_id = ? WHERE id = ?", regionID, id)
	}
}

// addSubPlan 建一个套餐；regionIDs 传 "" 表示 region_ids = NULL（不限地区）。
func addSubPlan(t *testing.T, f *region03Fixture, id int64, regionIDs string) {
	t.Helper()
	var raw any
	if regionIDs != "" {
		raw = regionIDs
	}
	mustExec(t, f.db, "INSERT INTO plans (id, name, region_ids) VALUES (?, ?, ?)",
		id, fmt.Sprintf("plan-%d", id), raw)
}

// addSubUser 建一个用户并挂上套餐（planID=0 表示没套餐，users.plan_id = NULL）。
func addSubUser(t *testing.T, f *region03Fixture, userID int64, planID int64) {
	t.Helper()
	var raw any
	if planID > 0 {
		raw = planID
	}
	mustExec(t, f.db, "INSERT INTO users (id, plan_id) VALUES (?, ?)", userID, raw)
}

// ─────────── 套餐 [1]：只看到 1 号地区的节点 + 未归属地区的老节点 ───────────

func TestRegion15SubPlanRegionFiltersNodes(t *testing.T) {
	f := newPlanRegionSubFixture(t, "filter")
	addNodeInRegion(t, f, 1, 1)
	addNodeInRegion(t, f, 2, 2)
	addNodeInRegion(t, f, 3, 0) // 老节点：region_id = NULL
	addSubPlan(t, f, 11, "[1]")
	addSubUser(t, f, 7, 11)
	// 三条绑定都写上：留下哪几条由「套餐地区」决定，不是由 user_nodes 决定。
	f.bind(t, 7, 1)
	f.bind(t, 7, 2)
	f.bind(t, 7, 3)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 7)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1, 3})
}

// ─────────── 老节点兼容：region_id 为 NULL 的节点必须仍然可见（变异测试靶子） ───────────

func TestRegion15SubUnassignedRegionNodeStillVisible(t *testing.T) {
	f := newPlanRegionSubFixture(t, "oldnode")
	addNodeInRegion(t, f, 1, 0) // 升级前就存在的节点，从没归属过地区
	addNodeInRegion(t, f, 2, 2) // 明确属于 2 号地区：套餐不允许
	addSubPlan(t, f, 12, "[1]")
	addSubUser(t, f, 10, 12)
	f.bind(t, 10, 1)
	f.bind(t, 10, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1})
}

// ─────────── 套餐不限地区（NULL）：老行为，绑定的节点全都在 ───────────

func TestRegion15SubUnlimitedPlanKeepsAllNodes(t *testing.T) {
	f := newPlanRegionSubFixture(t, "unlimited")
	addNodeInRegion(t, f, 1, 1)
	addNodeInRegion(t, f, 2, 2)
	addSubPlan(t, f, 13, "")
	addSubUser(t, f, 8, 13)
	f.bind(t, 8, 1)
	f.bind(t, 8, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 8)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1, 2})
}

// ─────────── 用户没有套餐：同样不限地区 ───────────

func TestRegion15SubUserWithoutPlanKeepsAllNodes(t *testing.T) {
	f := newPlanRegionSubFixture(t, "noplan")
	addNodeInRegion(t, f, 1, 1)
	addNodeInRegion(t, f, 2, 2)
	addSubUser(t, f, 9, 0)
	f.bind(t, 9, 1)
	f.bind(t, 9, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 9)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1, 2})
}
