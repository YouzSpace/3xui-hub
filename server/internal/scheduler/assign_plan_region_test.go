package scheduler

// 套餐绑定地区（#地区功能-15）集成测试：真连 MySQL，独立库 ch_region15_*，自建自删。
//
// 覆盖的语义：
//   - 套餐 region_ids=NULL（老套餐）→ 每个地区都分（与升级前完全一致）；
//   - region_ids=[1] / [1,2] → 只在这些地区里分；
//   - 换套餐 [1,2] → [1]：PrunePlanRegions 只解除 2 号地区的绑定，1 号保留；
//   - 老节点（region_id IS NULL）不受套餐地区限制，清理时也不碰；
//   - 不限地区的套餐调 PrunePlanRegions 一条都不删。
//
// 变异测试靶子（去掉即必须变红）：assign.go 里 unboundRegions 的 `if restricted { ... }`
// 分支去掉 → TestRegion15PlanRegionSingleRegion 会分到全部 3 个地区。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// newPlanRegionScheduler 建一个 ch_region15_* 独立测试库，表结构复用 testSchemaDDL。
func newPlanRegionScheduler(t *testing.T, caseName string) (*Scheduler, *sql.DB) {
	t.Helper()

	schema := fmt.Sprintf("ch_region15_%s_%d", caseName, os.Getpid())
	admin, err := sql.Open("mysql", testDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过套餐地区集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过套餐地区集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	mustExec(t, admin, "DROP DATABASE IF EXISTS `"+schema+"`")
	mustExec(t, admin, "CREATE DATABASE `"+schema+"` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci")
	t.Cleanup(func() {
		a, err := sql.Open("mysql", testDSN())
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", withSchema(testDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range testSchemaDDL {
		mustExec(t, db, stmt)
	}
	return New(db, &fakeLocker{}), db
}

// addPlan 建一个套餐；regionIDs 传 "" 表示 region_ids = NULL（不限地区）。
func addPlan(t *testing.T, db *sql.DB, id int64, regionIDs string) {
	t.Helper()
	var raw any
	if regionIDs != "" {
		raw = regionIDs
	}
	mustExec(t, db, "INSERT INTO plans (id, name, region_ids) VALUES (?, ?, ?)",
		id, fmt.Sprintf("plan-%d", id), raw)
}

// addUserWithPlan 建一个用户并挂上套餐；planID=0 表示没有套餐（users.plan_id = NULL）。
func addUserWithPlan(t *testing.T, db *sql.DB, userID uint64, planID int64) {
	t.Helper()
	var raw any
	if planID > 0 {
		raw = planID
	}
	mustExec(t, db, "INSERT INTO users (id, token, uuid, plan_id) VALUES (?, ?, ?, ?)",
		userID, fmt.Sprintf("token-%d", userID), fmt.Sprintf("00000000-0000-0000-0000-%012d", userID), raw)
}

// assignedRegions 把分配结果压成地区 id 序列（顺序即分配顺序）。
func assignedRegions(as []Assignment) []int64 {
	out := make([]int64, 0, len(as))
	for _, a := range as {
		out = append(out, a.RegionID)
	}
	return out
}

// equalInt64s 断言两个 id 序列完全相同。
func equalInt64s(t *testing.T, got, want []int64, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：期望 %v，实际 %v", label, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s：期望 %v，实际 %v", label, want, got)
		}
	}
}

// boundRegions 返回该用户当前绑定的地区 id 序列（按地区 id 排序；region_id NULL 的绑定不计）。
func boundRegions(t *testing.T, db *sql.DB, userID uint64) []int64 {
	t.Helper()
	rows, err := db.Query(`
		SELECT DISTINCT n.region_id
		FROM user_nodes un
		JOIN nodes n ON n.id = un.node_id
		WHERE un.user_id = ? AND n.region_id IS NOT NULL
		ORDER BY n.region_id`, userID)
	if err != nil {
		t.Fatalf("查询绑定地区失败: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("读取绑定地区失败: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历绑定地区失败: %v", err)
	}
	return out
}

// ─────────── 用例 1：套餐不限地区（region_ids=NULL）→ 3 个地区都分（老行为） ───────────

func TestRegion15UnlimitedPlanAssignsEveryRegion(t *testing.T) {
	s, db := newPlanRegionScheduler(t, "unlimited")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addRegion(t, db, 3, "美国", 3)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addNode(t, db, 301, 3, 100, true, "online")
	addPlan(t, db, 11, "") // region_ids = NULL
	addUserWithPlan(t, db, 900, 11)

	assigned, err := s.Assign(context.Background(), 900)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	equalInt64s(t, assignedRegions(assigned), []int64{1, 2, 3}, "不限地区的套餐应分到全部地区")
	equalInt64s(t, boundRegions(t, db, 900), []int64{1, 2, 3}, "绑定地区")
}

// ─────────── 用户根本没有套餐（users.plan_id = NULL）→ 同样不限地区 ───────────

func TestRegion15UserWithoutPlanAssignsEveryRegion(t *testing.T) {
	s, db := newPlanRegionScheduler(t, "noplan")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addUserWithPlan(t, db, 901, 0) // 没套餐

	assigned, err := s.Assign(context.Background(), 901)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	equalInt64s(t, assignedRegions(assigned), []int64{1, 2}, "没套餐的用户应分到全部地区")
}

// ─────────── 用例 2：套餐 region_ids=[1] → 只分到 1 号地区 ───────────

func TestRegion15PlanRegionSingleRegion(t *testing.T) {
	s, db := newPlanRegionScheduler(t, "single")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addRegion(t, db, 3, "美国", 3)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addNode(t, db, 301, 3, 100, true, "online")
	addPlan(t, db, 12, "[1]")
	addUserWithPlan(t, db, 902, 12)

	assigned, err := s.Assign(context.Background(), 902)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if len(assigned) != 1 {
		t.Fatalf("只允许 1 号地区时应只分 1 个，实际分配了 %d 个: %v", len(assigned), assigned)
	}
	if assigned[0].RegionID != 1 || assigned[0].NodeID != 101 {
		t.Fatalf("应分到地区 1 的节点 101，实际 %+v", assigned[0])
	}
	equalInt64s(t, boundRegions(t, db, 902), []int64{1}, "绑定地区（2、3 号地区不许绑）")
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 902"); n != 1 {
		t.Fatalf("user_nodes 应该只有 1 行，实际 %d 行", n)
	}
}

// ─────────── 用例 3：套餐 region_ids=[1,2] → 1、2（不含 3） ───────────

func TestRegion15PlanRegionMultipleRegions(t *testing.T) {
	s, db := newPlanRegionScheduler(t, "multi")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addRegion(t, db, 3, "美国", 3)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addNode(t, db, 301, 3, 100, true, "online")
	addPlan(t, db, 13, "[1,2]")
	addUserWithPlan(t, db, 903, 13)

	assigned, err := s.Assign(context.Background(), 903)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	equalInt64s(t, assignedRegions(assigned), []int64{1, 2}, "只允许 1、2 号地区")
	equalInt64s(t, boundRegions(t, db, 903), []int64{1, 2}, "绑定地区（3 号地区不许绑）")
}

// ─────────── 用例 5：换套餐 [1,2] → [1] 只解除 2 号地区的绑定 ───────────

func TestRegion15PlanDowngradePrunesOnlyDisallowedRegion(t *testing.T) {
	s, db := newPlanRegionScheduler(t, "downgrade")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addPlan(t, db, 21, "[1,2]")
	addPlan(t, db, 22, "[1]")
	addUserWithPlan(t, db, 904, 21)

	// 先用老套餐分配：1、2 号地区各一条绑定。
	if _, err := s.Assign(context.Background(), 904); err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	equalInt64s(t, boundRegions(t, db, 904), []int64{1, 2}, "换套餐前的绑定")

	// 换到只允许 1 号地区的套餐，然后清理。
	mustExec(t, db, "UPDATE users SET plan_id = ? WHERE id = ?", 22, 904)
	pruned, err := s.PrunePlanRegions(context.Background(), 904)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("应只解除 1 条（2 号地区），实际解除 %d 条", pruned)
	}
	equalInt64s(t, boundRegions(t, db, 904), []int64{1}, "2 号地区绑定必须被删、1 号地区必须保留")

	// 再分配一次不会把 2 号地区补回来（新套餐不允许），也不会有新绑定。
	assigned, err := s.Assign(context.Background(), 904)
	if err != nil {
		t.Fatalf("再次分配失败: %v", err)
	}
	if len(assigned) != 0 {
		t.Fatalf("1 号地区已绑定、2 号地区不允许，不应再分配，实际 %v", assigned)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 904"); n != 1 {
		t.Fatalf("user_nodes 应该只有 1 行，实际 %d 行", n)
	}
}

// ─────────── 老节点兼容：region_id IS NULL 的绑定不受套餐地区限制，清理时也不碰 ───────────

func TestRegion15PruneKeepsUnassignedRegionBindings(t *testing.T) {
	s, db := newPlanRegionScheduler(t, "oldnode")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addNode(t, db, 301, 0, 100, true, "online") // region_id = NULL 的老节点
	addPlan(t, db, 31, "[1]")
	addUserWithPlan(t, db, 905, 31)
	// 直接造三条绑定：1 号地区、2 号地区、老节点（升级前就绑好的样子）。
	mustExec(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?), (?, ?), (?, ?)",
		905, 101, 905, 201, 905, 301)

	pruned, err := s.PrunePlanRegions(context.Background(), 905)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("应只解除 2 号地区那 1 条，实际 %d 条", pruned)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 905 AND node_id = 301"); n != 1 {
		t.Fatalf("region_id 为 NULL 的老节点绑定不能被删（命中 %d 行）", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 905 AND node_id = 101"); n != 1 {
		t.Fatalf("1 号地区（套餐允许）的绑定不能被删（命中 %d 行）", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 905 AND node_id = 201"); n != 0 {
		t.Fatalf("2 号地区（套餐不允许）的绑定必须被删（命中 %d 行）", n)
	}
}

// ─────────── 不限地区的套餐清理时一条都不删 ───────────

func TestRegion15PruneDoesNothingForUnlimitedPlan(t *testing.T) {
	s, db := newPlanRegionScheduler(t, "pruneunlimited")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addPlan(t, db, 41, "") // region_ids = NULL
	addUserWithPlan(t, db, 906, 41)
	mustExec(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?), (?, ?)", 906, 101, 906, 201)

	pruned, err := s.PrunePlanRegions(context.Background(), 906)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("不限地区的套餐不应删除任何绑定，实际删了 %d 条", pruned)
	}
	equalInt64s(t, boundRegions(t, db, 906), []int64{1, 2}, "绑定必须原样保留")
}
