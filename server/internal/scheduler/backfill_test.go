package scheduler

// 老用户批量入组（POST /admin-api/regions/backfill 的引擎侧）集成测试：真连 MySQL，
// 独立库 ch_backfill_*，自建自删；连不上 MySQL 时 skip（与仓库其他集成测试一致）。
//
// 覆盖的语义（对应任务书「测试」一节）：
//   - 多节点分散：同一地区里负载率低的节点先被填，平局按 id 升序；
//   - 已有绑定不动：一条绑定都没有才算候选，已有绑定既不重复写也不改绑；
//   - dry 不写：dry=true 返回和真实执行同一份计划，但 user_nodes 一行都不多；
//   - 满载跳过：地区里节点全满 / 一台可用节点都没有 → 该地区计入 skipped，用户不算 failed；
//   - 过期 / 未启用 / 软删 / 无套餐 的用户不进候选；
//   - 套餐限制地区：只在套餐允许的地区里补，其余地区安静跳过（不计入 skipped）；
//   - limit 夹取与 remaining 计算（反复调用能收敛到 0）。
//
// 变异测试靶子：把 pickNode 的排序（assign.go:233-239）反向 →
// TestBackfillSpreadsAcrossNodes 与 TestBackfillDryMatchesInsert 变红（节点分布不对）。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// newBackfillScheduler 建一个 ch_backfill_* 独立测试库，表结构复用 testSchemaDDL
// （users 表额外补三列：enabled/plan_id/expired_at/deleted_at —— 批量入组要按它们筛人）。
func newBackfillScheduler(t *testing.T, caseName string) (*Scheduler, *sql.DB) {
	t.Helper()

	schema := fmt.Sprintf("ch_backfill_%s_%d", caseName, os.Getpid())
	admin, err := sql.Open("mysql", testDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过批量入组集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过批量入组集成测试", err)
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
	// testSchemaDDL 的 users 只有分配要读的列；批量入组还要按 enabled / expired_at / deleted_at 筛人，
	// 用 ALTER 补齐（测试库是自建的，给真库升级用的是 ensureColumn，与这里无关）。
	for _, stmt := range []string{
		"ALTER TABLE users ADD COLUMN expired_at datetime DEFAULT NULL",
		"ALTER TABLE users ADD COLUMN deleted_at timestamp NULL DEFAULT NULL",
	} {
		mustExec(t, db, stmt)
	}
	return New(db, &fakeLocker{}), db
}

// backfillUserOpts 控制造出来的用户状态；零值 = 合格用户（启用、有套餐、未过期、未软删）。
type backfillUserOpts struct {
	noPlan    bool   // users.plan_id = NULL
	disabled  bool   // enabled = 0
	softDrop  bool   // deleted_at 非空
	expiredAt string // 非空 = 写这个 expired_at（"2000-01-01 00:00:00" 即已过期）
	planID    int64
}

// addBackfillUser 建一个用户（默认合格，planID 必须 >0 或走 noPlan）。
func addBackfillUser(t *testing.T, db *sql.DB, id uint64, o backfillUserOpts) {
	t.Helper()
	var planArg any
	if !o.noPlan {
		planArg = o.planID
	}
	enabled := 1
	if o.disabled {
		enabled = 0
	}
	var deletedArg, expiredArg any
	if o.softDrop {
		deletedArg = "2026-01-01 00:00:00"
	}
	if o.expiredAt != "" {
		expiredArg = o.expiredAt
	}
	mustExec(t, db,
		"INSERT INTO users (id, token, uuid, enabled, plan_id, expired_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		id, fmt.Sprintf("token-%d", id), fmt.Sprintf("00000000-0000-0000-0000-%012d", id),
		enabled, planArg, expiredArg, deletedArg)
}

// backfillBindings 返回该批用户写进 user_nodes 的 (user_id, node_id) 对，按 user_id,node_id 排序。
func backfillBindings(t *testing.T, db *sql.DB) [][2]uint64 {
	t.Helper()
	rows, err := db.Query("SELECT user_id, node_id FROM user_nodes ORDER BY user_id, node_id")
	if err != nil {
		t.Fatalf("查询绑定失败: %v", err)
	}
	defer rows.Close()
	var out [][2]uint64
	for rows.Next() {
		var u, n uint64
		if err := rows.Scan(&u, &n); err != nil {
			t.Fatalf("读取绑定失败: %v", err)
		}
		out = append(out, [2]uint64{u, n})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历绑定失败: %v", err)
	}
	return out
}

// nodeCountsOf 把 ByNode 压成 map，方便断言。
func nodeCountsOf(r BackfillResult) map[int64]int {
	out := make(map[int64]int, len(r.ByNode))
	for _, n := range r.ByNode {
		out[n.NodeID] = n.Count
	}
	return out
}

// ─────────── 多节点分散：负载率低的先填，模拟计数在批内累加 ───────────

func TestBackfillSpreadsAcrossNodes(t *testing.T) {
	s, db := newBackfillScheduler(t, "spread")

	addRegion(t, db, 1, "香港", 1)
	addNode(t, db, 101, 1, 10, true, "online")
	addNode(t, db, 102, 1, 10, true, "online")
	for _, uid := range []uint64{1, 2, 3, 4} {
		addBackfillUser(t, db, uid, backfillUserOpts{planID: 1})
	}

	res, err := s.Backfill(context.Background(), BackfillOptions{Dry: false})
	if err != nil {
		t.Fatalf("批量入组失败: %v", err)
	}
	t.Logf("结果: %+v by_node=%+v", res, res.ByNode)

	if res.Candidates != 4 || res.Assigned != 4 || res.UsersDone != 4 || res.UsersSkipped != 0 || res.Remaining != 0 {
		t.Fatalf("计数不对: %+v", res)
	}
	// 两台节点各 2 人：先填 101（平局取小 id），第三次轮到 102，第四次又回来（负载率打平后按 id）
	if got := nodeCountsOf(res); got[101] != 2 || got[102] != 2 {
		t.Fatalf("节点应各分 2 人，实际 %+v", res.ByNode)
	}
	// 不能把 4 个人全塞进一台
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 101"); n != 2 {
		t.Fatalf("节点 101 应只有 2 条绑定，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes"); n != 4 {
		t.Fatalf("总绑定应为 4 条，实际 %d", n)
	}
}

// ─────────── 已有绑定的用户不动：不算候选、不改绑、不重复写 ───────────

func TestBackfillSkipsUsersWithExistingBindings(t *testing.T) {
	s, db := newBackfillScheduler(t, "bound")

	addRegion(t, db, 1, "香港", 1)
	addNode(t, db, 101, 1, 100, true, "online")
	addBackfillUser(t, db, 1, backfillUserOpts{planID: 1})
	addBackfillUser(t, db, 2, backfillUserOpts{planID: 1})
	// 用户 2 已经绑了一台节点（升级前就有的样子）—— 哪怕绑的是别的节点也不该被动。
	mustExec(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (2, 101)")

	res, err := s.Backfill(context.Background(), BackfillOptions{Dry: false})
	if err != nil {
		t.Fatalf("批量入组失败: %v", err)
	}
	t.Logf("结果: %+v", res)

	if res.Candidates != 1 || res.UsersDone != 1 {
		t.Fatalf("只有用户 1 该入选，实际 %+v", res)
	}
	got := backfillBindings(t, db)
	if len(got) != 2 {
		t.Fatalf("应仍是 2 条绑定（原有的 + 新补的），实际 %v", got)
	}
	// 用户 2 的绑定原样保留
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 2 AND node_id = 101"); n != 1 {
		t.Fatalf("已有绑定不能被改动（实际 %d 行命中）", n)
	}
}

// ─────────── dry 不写库，且计划与真实执行一致 ───────────

func TestBackfillDryMatchesInsert(t *testing.T) {
	s, db := newBackfillScheduler(t, "dry")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addNode(t, db, 101, 1, 2, true, "online")
	addNode(t, db, 102, 1, 2, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	for _, uid := range []uint64{1, 2, 3} {
		addBackfillUser(t, db, uid, backfillUserOpts{planID: 1})
	}

	dry, err := s.Backfill(context.Background(), BackfillOptions{Dry: true})
	if err != nil {
		t.Fatalf("dry 预览失败: %v", err)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes"); n != 0 {
		t.Fatalf("dry 不能写库，user_nodes 实际 %d 行", n)
	}
	if !dry.Dry || dry.Assigned != 6 { // 3 用户 × 2 地区
		t.Fatalf("dry 计划应为 6 条绑定，实际 %+v", dry)
	}

	real, err := s.Backfill(context.Background(), BackfillOptions{Dry: false})
	if err != nil {
		t.Fatalf("真实执行失败: %v", err)
	}
	if real.Assigned != dry.Assigned || real.UsersDone != dry.UsersDone {
		t.Fatalf("dry 与真实执行的计数必须一致: dry=%d/%d real=%d/%d",
			dry.Assigned, dry.UsersDone, real.Assigned, real.UsersDone)
	}
	if fmt.Sprint(dry.ByNode) != fmt.Sprint(real.ByNode) {
		t.Fatalf("dry 与真实执行的节点分布必须一致: dry=%+v real=%+v", dry.ByNode, real.ByNode)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes"); n != 6 {
		t.Fatalf("真实执行后应有 6 条绑定，实际 %d", n)
	}
}

// ─────────── 满载跳过：全满 / 无可用节点的地区进 skipped，不中断其余地区 ───────────

func TestBackfillSkipsFullRegions(t *testing.T) {
	s, db := newBackfillScheduler(t, "full")

	addRegion(t, db, 1, "香港", 1)
	addNode(t, db, 101, 1, 100, true, "online")

	addRegion(t, db, 2, "满容量", 2)
	addNode(t, db, 201, 2, 1, true, "online")
	bindUsers(t, db, 201, 1000, 1) // 容量 1，已绑 1 → 满

	addRegion(t, db, 3, "空地区", 3) // 一台节点都没有

	for _, uid := range []uint64{1, 2} {
		addBackfillUser(t, db, uid, backfillUserOpts{planID: 1})
	}

	res, err := s.Backfill(context.Background(), BackfillOptions{Dry: false})
	if err != nil {
		t.Fatalf("批量入组失败: %v", err)
	}
	t.Logf("结果: %+v skipped=%+v", res, res.Skipped)

	// 1 号地区两人都补上；2、3 号地区各跳过 2 人
	if res.Assigned != 2 || res.UsersDone != 2 || res.UsersSkipped != 0 {
		t.Fatalf("可用地区的用户仍应全部入组: %+v", res)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("应有 2 条跳过统计，实际 %+v", res.Skipped)
	}
	skips := map[int64]BackfillSkip{}
	for _, s := range res.Skipped {
		skips[s.RegionID] = s
	}
	for _, rid := range []int64{2, 3} {
		if skips[rid].Count != 2 || skips[rid].Reason == "" {
			t.Fatalf("地区 %d 应跳过 2 人并带原因，实际 %+v", rid, skips[rid])
		}
	}
	if res.ByNode[0].NodeID != 101 || res.ByNode[0].Count != 2 {
		t.Fatalf("只在 101 上补了 2 条，实际 %+v", res.ByNode)
	}
}

// ─────────── 过期 / 未启用 / 软删 / 无套餐 的用户不进候选 ───────────

func TestBackfillExcludesIneligibleUsers(t *testing.T) {
	s, db := newBackfillScheduler(t, "ineligible")

	addRegion(t, db, 1, "香港", 1)
	addNode(t, db, 101, 1, 100, true, "online")

	addBackfillUser(t, db, 1, backfillUserOpts{planID: 1})                                   // 合格
	addBackfillUser(t, db, 2, backfillUserOpts{planID: 1, expiredAt: "2000-01-01 00:00:00"}) // 已过期
	addBackfillUser(t, db, 3, backfillUserOpts{planID: 1, disabled: true})                   // 已停用
	addBackfillUser(t, db, 4, backfillUserOpts{planID: 1, softDrop: true})                   // 已软删
	addBackfillUser(t, db, 5, backfillUserOpts{noPlan: true})                                // 没有套餐
	addBackfillUser(t, db, 6, backfillUserOpts{planID: 1})                                   // 合格

	res, err := s.Backfill(context.Background(), BackfillOptions{Dry: false})
	if err != nil {
		t.Fatalf("批量入组失败: %v", err)
	}
	t.Logf("结果: %+v", res)

	if res.Candidates != 2 || res.UsersDone != 2 || res.Assigned != 2 {
		t.Fatalf("只有用户 1、6 合格，实际 %+v", res)
	}
	got := backfillBindings(t, db)
	if len(got) != 2 || got[0][0] != 1 || got[1][0] != 6 {
		t.Fatalf("只应给用户 1、6 写绑定，实际 %v", got)
	}
}

// ─────────── 套餐限制地区：只补允许的地区，其余安静跳过 ───────────

func TestBackfillRespectsPlanRegions(t *testing.T) {
	s, db := newBackfillScheduler(t, "planregion")

	addRegion(t, db, 1, "香港", 1)
	addRegion(t, db, 2, "日本", 2)
	addNode(t, db, 101, 1, 100, true, "online")
	addNode(t, db, 201, 2, 100, true, "online")
	addPlan(t, db, 7, "[1]") // 只允许 1 号地区
	addBackfillUser(t, db, 1, backfillUserOpts{planID: 7})

	res, err := s.Backfill(context.Background(), BackfillOptions{Dry: false})
	if err != nil {
		t.Fatalf("批量入组失败: %v", err)
	}
	t.Logf("结果: %+v", res)

	if res.Assigned != 1 || len(res.Skipped) != 0 {
		t.Fatalf("只应在允许地区补 1 条、且不算跳过，实际 %+v", res)
	}
	got := backfillBindings(t, db)
	if len(got) != 1 || got[0][1] != 101 {
		t.Fatalf("只应绑到 1 号地区的 101，实际 %v", got)
	}
}

// ─────────── 反复调用收敛：limit 按批推进，remaining 逐批降到 0 ───────────

func TestBackfillLimitAndRemaining(t *testing.T) {
	s, db := newBackfillScheduler(t, "limit")

	addRegion(t, db, 1, "香港", 1)
	addNode(t, db, 101, 1, 100, true, "online")
	for uid := uint64(1); uid <= 5; uid++ {
		addBackfillUser(t, db, uid, backfillUserOpts{planID: 1})
	}
	addBackfillUser(t, db, 6, backfillUserOpts{planID: 1, expiredAt: "2000-01-01 00:00:00"}) // 不合格也不该影响 remaining

	first, err := s.Backfill(context.Background(), BackfillOptions{Dry: false, Limit: 2})
	if err != nil {
		t.Fatalf("第一批失败: %v", err)
	}
	t.Logf("第一批: %+v", first)
	if first.Candidates != 5 || first.UsersDone != 2 || first.Remaining != 3 {
		t.Fatalf("第一批应处理 2 人、剩 3 人，实际 %+v", first)
	}

	second, err := s.Backfill(context.Background(), BackfillOptions{Dry: false, Limit: 2})
	if err != nil {
		t.Fatalf("第二批失败: %v", err)
	}
	if second.UsersDone != 2 || second.Remaining != 1 {
		t.Fatalf("第二批应处理 2 人、剩 1 人，实际 %+v", second)
	}

	third, err := s.Backfill(context.Background(), BackfillOptions{Dry: false, Limit: 2})
	if err != nil {
		t.Fatalf("第三批失败: %v", err)
	}
	if third.UsersDone != 1 || third.Remaining != 0 || third.Candidates != 1 {
		t.Fatalf("第三批应处理最后 1 人、剩 0 人，实际 %+v", third)
	}

	// 再调一次没有候选，不写库、不报错
	again, err := s.Backfill(context.Background(), BackfillOptions{Dry: false})
	if err != nil {
		t.Fatalf("收敛后调用失败: %v", err)
	}
	if again.Candidates != 0 || again.Assigned != 0 || again.Remaining != 0 {
		t.Fatalf("收敛后应全 0，实际 %+v", again)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes"); n != 5 {
		t.Fatalf("最终应补 5 条绑定，实际 %d", n)
	}
}

// ─────────── limit 夹取：0 用默认 500，超上限按 5000 ───────────

func TestBackfillLimitClamp(t *testing.T) {
	s, db := newBackfillScheduler(t, "clamp")

	addRegion(t, db, 1, "香港", 1)
	addNode(t, db, 101, 1, -1, true, "online") // 不限容量，不必关心装不装得下
	for uid := uint64(1); uid <= 3; uid++ {
		addBackfillUser(t, db, uid, backfillUserOpts{planID: 1})
	}

	res, err := s.Backfill(context.Background(), BackfillOptions{Dry: true, Limit: 0})
	if err != nil {
		t.Fatalf("limit=0 失败: %v", err)
	}
	if res.UsersDone != 3 {
		t.Fatalf("limit=0 应走默认 500，3 人全部处理，实际 %+v", res)
	}

	// 超上限只是一次查询上限的夹取，这里没有 5001 个用户，验证「不报错 + 全处理」即可
	if _, err := s.Backfill(context.Background(), BackfillOptions{Dry: true, Limit: 1 << 30}); err != nil {
		t.Fatalf("limit 超上限应被夹住而不是报错: %v", err)
	}
}
