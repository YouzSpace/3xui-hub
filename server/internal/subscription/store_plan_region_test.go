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
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
)

// newPlanRegionSubFixture 建一个 ch_region15_* 独立库（自建自删），表结构复用 region03SchemaDDL。
func newPlanRegionSubFixture(t *testing.T, caseName string) *region03Fixture {
	t.Helper()
	return newRegion03Fixture(t, fmt.Sprintf("ch_region15_%s_%d", caseName, os.Getpid()))
}

// addNodeInRegion 插一个 xray 可订阅节点并把它划进 regionID（regionID=0 表示 region_id = NULL）。
func addNodeInRegion(t *testing.T, f *region03Fixture, id int64, regionID int64) {
	t.Helper()
	f.addNode(t, id, fmt.Sprintf("node-%d", id), "online")
	// 入站级分组（3.1.0）对 xray 节点按「有效地区」过滤入站；本组用例造的是 xray 节点，
	// 入站一律 region_id = NULL（跟随节点），这样节点地区的过滤结果与升级前一致。
	mustExec(t, f.db, "UPDATE nodes SET driver_type = 'xray' WHERE id = ?", id)
	mustExec(t, f.db,
		"INSERT INTO xray_inbounds (node_id, region_id, tag, protocol, enabled) VALUES (?, NULL, ?, 'vless', 1)",
		id, fmt.Sprintf("inbound-%d", id))
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

// ─────────── 入站级分组（3.1.0）：套餐地区过滤下沉到入站 ───────────
//
// 这三条用例钉住「有效地区 = COALESCE(入站地区, 节点地区)」：
//   1. 入站自己归属的地区命中套餐 → 输出；不命中 → 不输出；
//   2. 入站 region_id=NULL 时跟随节点地区（老数据行为不变）；
//   3. 一台节点上没有任何命中入站 → 整台节点不出现在订阅里。
//
// 变异测试靶子：把 SubscribableNodes 里的 planregion.InboundNodeFilter 换回 NodeFilter
// （按节点过滤）→ 用例 1/3 变红：整台节点按节点地区进/出，入站级归属被无视。

// addInbound 给节点插一个启用入站，regionID=0 表示 region_id = NULL（跟随节点）。
// 协议固定 vless、端口随机：过滤只读 region_id，不读协议；服务层逐入站用例也只断言 tag。
func addInbound(t *testing.T, f *region03Fixture, nodeID int64, tag string, regionID int64) {
	t.Helper()
	var raw any
	if regionID > 0 {
		raw = regionID
	}
	mustExec(t, f.db,
		"INSERT INTO xray_inbounds (node_id, region_id, tag, protocol, enabled) VALUES (?, ?, ?, 'vless', 1)",
		nodeID, raw, tag)
}

func TestRegion15InboundRegionHitsPlanRegion(t *testing.T) {
	f := newPlanRegionSubFixture(t, "inbound_hit")
	addNodeInRegion(t, f, 1, 1) // 节点属于 1 号地区
	// addNodeInRegion 会补一个「跟随节点」的入站；本用例只留下面两条明确归属的，
	// 免得跟随入站（有效地区 = 1）掩盖「按入站过滤」的效果。
	mustExec(t, f.db, "DELETE FROM xray_inbounds WHERE node_id = ?", 1)
	// 同节点两个入站：一个归 2 号地区（套餐不允许），一个归 1 号（允许）。
	addInbound(t, f, 1, "vless-2", 2)
	addInbound(t, f, 1, "vless-1", 1)
	addSubPlan(t, f, 11, "[1]")
	addSubUser(t, f, 7, 11)
	f.bind(t, 7, 1)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 7)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	equalIDs(t, nodeIDs(nodes), []int64{1})

	// 逐入站的过滤（服务层）：只输出有效地区命中套餐的那个入站。
	inbounds, err := f.repo.XrayInbounds(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询入站失败: %v", err)
	}
	plan := &Plan{ID: 11, RegionIDs: []int64{1}, RegionRestricted: true}
	node := nodes[0]
	var got []string
	for _, ib := range inbounds {
		if inboundRegionAllowed(ib, node, plan) {
			got = append(got, ib.Tag)
		}
	}
	if len(got) != 1 || got[0] != "vless-1" {
		t.Fatalf("应只输出归属 1 号地区的入站 vless-1，实际 %v", got)
	}
}

func TestRegion15InboundRegionNullFollowsNode(t *testing.T) {
	f := newPlanRegionSubFixture(t, "inbound_follow")
	addNodeInRegion(t, f, 1, 1) // 节点属于 1 号地区
	addNodeInRegion(t, f, 2, 2) // 节点属于 2 号地区（套餐不允许）
	// 两台节点都只有一个「跟随节点」的入站（region_id = NULL，升级前的老数据形态）。
	// addNodeInRegion 补的那条就是，删掉重插是为了让 tag 可读。
	for _, nodeID := range []int64{1, 2} {
		mustExec(t, f.db, "DELETE FROM xray_inbounds WHERE node_id = ?", nodeID)
	}
	addInbound(t, f, 1, "vless-follow-1", 0)
	addInbound(t, f, 2, "vless-follow-2", 0)
	addSubPlan(t, f, 12, "[1]")
	addSubUser(t, f, 8, 12)
	f.bind(t, 8, 1)
	f.bind(t, 8, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 8)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 1 号节点命中（跟随 → 1 号地区）；2 号节点不命中（跟随 → 2 号地区）。
	equalIDs(t, nodeIDs(nodes), []int64{1})

	inbounds, err := f.repo.XrayInbounds(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询入站失败: %v", err)
	}
	if len(inbounds) != 1 {
		t.Fatalf("节点 1 应有 1 个入站，实际 %d", len(inbounds))
	}
	plan := &Plan{ID: 12, RegionIDs: []int64{1}, RegionRestricted: true}
	if !inboundRegionAllowed(inbounds[0], nodes[0], plan) {
		t.Fatal("region_id=NULL 的入站应跟随节点地区（节点在套餐内 → 允许）")
	}
}

func TestRegion15NodeWithoutMatchingInboundDropped(t *testing.T) {
	f := newPlanRegionSubFixture(t, "inbound_none")
	// 节点 1 属于 1 号地区，但两个入站分别归 2、3 号地区 —— 没有命中套餐 [1] 的入站。
	addNodeInRegion(t, f, 1, 1)
	// addNodeInRegion 会补一个「跟随节点」的入站；本用例要的是「一个命中入站都没有」，
	// 先把它删掉再按需造。
	mustExec(t, f.db, "DELETE FROM xray_inbounds WHERE node_id = ?", 1)
	addInbound(t, f, 1, "vless-2", 2)
	addInbound(t, f, 1, "vless-3", 3)
	// 节点 2 属于 1 号地区且有一个跟随入站 → 命中（addNodeInRegion 补的那条就是跟随）。
	addNodeInRegion(t, f, 2, 1)
	addSubPlan(t, f, 13, "[1]")
	addSubUser(t, f, 9, 13)
	f.bind(t, 9, 1)
	f.bind(t, 9, 2)

	nodes, err := f.repo.SubscribableNodes(context.Background(), "vless", 9)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 节点 1 一个命中入站都没有 → 整台不输出；节点 2 命中 → 输出。
	equalIDs(t, nodeIDs(nodes), []int64{2})
}

// 端到端：走完整条 Generate 链路，断言订阅正文里出现/不出现哪个入站的 tag。
// 用户抱怨的形态永远是「订阅里少了/多了节点」，只验节点 id 不够；
// 同节点两个入站分属不同地区时，必须只出现命中套餐的那个。
func TestRegion15GenerateFiltersInboundLevel(t *testing.T) {
	f := newPlanRegionSubFixture(t, "inbound_generate")
	addNodeInRegion(t, f, 1, 1) // xray 节点，属于 1 号地区
	mustExec(t, f.db, "DELETE FROM xray_inbounds WHERE node_id = ?", 1)
	// 两个 vless 入站：tag 会进订阅链接的 #备注段，便于直接断言。
	// 复刻最小可用的 vless 入站参数（无 TLS/Reality 时链接仍会生成）。
	for _, it := range []struct {
		tag      string
		regionID int64
		port     int64
	}{{"hit-inbound", 1, 8443}, {"miss-inbound", 2, 8444}} {
		var raw any
		if it.regionID > 0 {
			raw = it.regionID
		}
		mustExec(t, f.db,
			"INSERT INTO xray_inbounds (node_id, region_id, tag, protocol, port, enabled, stream_settings) "+
				"VALUES (?, ?, ?, 'vless', ?, 1, '{\"network\":\"tcp\",\"security\":\"none\"}')",
			1, raw, it.tag, it.port)
	}
	addSubPlan(t, f, 21, "[1]")
	addSubUser(t, f, 31, 21)
	f.bind(t, 31, 1)
	// 用户要能生成链接：UUID 必填、账号可用（未过期/未超量/已启用）。
	// users 表在本测试 DDL 里只有 id/plan_id（其余订阅字段由 User 结构体直接给），无需改库。
	mustExec(t, f.db, "UPDATE nodes SET host = 'hk.example.com' WHERE id = 1")

	svc := NewService(f.repo)
	user := &User{
		ID: 31, UUID: "11111111-2222-3333-4444-555555555555", Protocol: "vless",
		PlanID: ptrInt64(21), Enabled: true,
	}
	body, err := svc.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("生成订阅失败: %v", err)
	}
	text := decodeBase64OrRaw(body)
	if !strings.Contains(text, "hit-inbound") {
		t.Fatalf("订阅里应包含归属套餐地区的入站 hit-inbound，实际:\n%s", text)
	}
	if strings.Contains(text, "miss-inbound") {
		t.Fatalf("订阅里不该出现归属别处的入站 miss-inbound，实际:\n%s", text)
	}
}

// decodeBase64OrRaw 把 base64 订阅正文解回明文（不是 base64 就原样返回）。
func decodeBase64OrRaw(body string) string {
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body)); err == nil && len(raw) > 0 {
		return string(raw)
	}
	return body
}

// ptrInt64 取 int64 的地址（PlanID 是 *int64）。
func ptrInt64(v int64) *int64 { return &v }
