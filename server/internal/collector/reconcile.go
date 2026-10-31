package collector

// 节点用户全量比对与兜底修复（users:reconcile-enable 的实现层）。
//
// 为什么要有这一层：以前那版对账只会「把节点上 enable=false 的人重新打开」，而且
//   ① 候选 SQL 带 `traffic_disabled_at IS NULL`，把真正卡住的人全筛掉（线上 84 就是）；
//   ② 探测走 `GetClientTraffic`，那个端点在本版 3x-ui 上恒失败 → checked 恒 0，
//      连刷 143 轮没人发现（err != nil 直接 continue，不计数不打日志）。
// 现在改成「读节点全量状态 → 逐条按面板意图比对 → 只对差异动手」，并完整上报计数。
//
// 读法（实测定的，别想当然）：
//   - 首选一次 `inbounds/list`，解析各入站 `settings.clients[]` —— 一次请求拿到全量，
//     还附带「这个 client 挂在哪些入站」这个维度；
//   - 解析不到（本版字段缺失/结构不同）→ 退化逐用户 `GetClientStrict`，**带每轮预算 + 轮转**，
//     并明确区分「探测过且不存在」与「本轮没探测」（后者绝不能被当成"节点上没有这个人"）；
//   - 实测反例：`clients/list` 在本版节点上只回少量非本站 email（自建用户），我们自己的
//     client 一条都拿不到 → probed=0、130 秒空转。所以它不作为主路径。
//
// 报告口径：Drift 是「只看不写」的字段漂移总数，并按**方向**分三档计数 ——
// DriftSafe / DriftEarlyCut / DriftLoose（三者之和恒等于 Drift）。只有 DriftEarlyCut>0
// 表示「节点比面板更紧、有人正被提前掐」，调用方据此把日志升到 WARN。
// ProbeFailed 单独计数：它既不进 Probed 也不进 Unknown，是「本轮什么都没看到」的唯一信号。
//
// 修法（用户定稿）：
//   - 节点上没有这个人 + 面板判「该可用」→ **补注册**（AddClient，uuid 由节点生成）；
//   - 字段对不上（含残留旧记录）→ **先 UpdateClient 保留 uuid**（重建会换 uuid：
//     手工导入链接的用户当场断、订阅用户还得重拉）；复核仍不符才删除重建兜底；
//   - enable 该开/该关 → 兜底执行；标记与意图矛盾的人再走 ToggleClient（它覆盖 xray 侧
//     并写/清 users.traffic_disabled_at）；
//   - 超时 / 5xx / 登录态失效 / 字段读不全 = **UNKNOWN → 本轮不动**；
//   - **只动我们自己建的 client**：面板上自建的远程用户（其它 email 形态）只报告不删除，
//     连"孤儿"也不自动清；
//   - 每节点每轮写入上限（默认 50），超出留到下一轮。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"controlhub-server/internal/driver/threexui"
)

// ReconcileReport 是一轮全量比对的统计（日志口径，字段名固定，改动要同步调用方）。
type ReconcileReport struct {
	Nodes      int // 参与比对的启用节点数
	Users      int // 参与比对的面板用户数
	Probed     int // 有明确结论的 (节点,用户) 数
	Registered int // 节点缺人 → 补注册
	Fixed      int // 开关方向被纠正（该开被关→开 / 该关还开→关）
	Drift      int // total/expiry 漂移 → **只报告不写**（改错代价大于收益）
	// DriftSafe / DriftEarlyCut / DriftLoose 是 Drift 的方向分档（三者之和 == Drift）：
	//   Safe     = 节点不设限，或比面板更宽松（靠面板兜底，用户无感）
	//   EarlyCut = 节点配额/期限比面板更紧 → **用户会被节点提前掐**（唯一需要人工处理的方向）
	//   Loose    = 其余「节点比面板更宽松」的形态（与 Safe 的差别只在节点是否设了限）
	// 为什么单列三档：总数相同、方向不同，处置完全不同 —— 只有 EarlyCut>0 才有人受影响，
	// 混在一个 Drift 里会让「4 条漂移」看起来严重、实际全是安全方向（或反过来）。
	DriftSafe     int
	DriftEarlyCut int
	DriftLoose    int
	Rebuilt       int      // 预留：修正后仍不符时的删除重建（默认不启用，需人工确认）
	Opened        int      // 该开被关 → 打开
	Closed        int      // 该关还开 → 关闭
	// Attached 是「节点上已有这个 client，但没挂到该协议的全部入站」→ 补挂的数量
	// （节点后加了入站、或上次入站同步失败留下的缺口）。attach 幂等，重复修无副作用。
	Attached      int
	Unchanged     int      // 一致，零写入
	Unknown       int      // 未知（超时/业务错/字段读不全）→ 本轮不动
	Deferred      int      // 本轮没探测（预算外）或命中写入上限 → 下轮
	Foreign       int      // 节点上**不是我们建的** client → 只报告
	ForeignNames  []string // 上面这些 email 的样本（最多 10 个）
	SnapshotBy    string   // 本轮快照来源：inbounds / probe / mixed / none
	OfflineNodes  []string // 快照读失败的节点（不判失败，下轮再来）
	// ProbeFailed 是退化探测路径里「探测本身失败」（超时/5xx/登录态失效）的条数。
	// 它们既不进 Probed 也不进 Unknown（Unknown 记的是「探测到了但字段读不全」），
	// 所以必须单独计数：否则整轮全是探测失败时，快照看起来「干净」，实际一个人都没看到
	// （旧的 checked=0 空转 143 轮就是这么来的）。
	ProbeFailed int
	ElapsedMs   int64
}

// reconcileUser 是比对用的最小用户视图（一次查库取齐，避免每用户单查）。
type reconcileUser struct {
	ID       int64
	Protocol string
	// UUID 是 users.uuid：补注册（AddClient）时组协议凭据用。
	// 更新路径不用它 —— 那边的凭据由 NormalizeForUpdate 从节点原对象带回。
	UUID     string
	Intent   bool // 面板判「该可用」
	Limit    int64
	ExpiryMs int64
	FlagSet  bool // traffic_disabled_at 非空
	// PlanIDValid 区分「有套餐但过期/超量」与「压根没套餐」：后者不参与任何关闭动作
	// （旧系统从不碰无套餐的人；摘了只会造成"后台建号→被摘→分配套餐又建回来"的抖动）。
	PlanIDValid bool
}

// nodeSnapshotEntry 是节点上一条 client 的比对视图。
type nodeSnapshotEntry struct {
	Enable     bool
	TotalGB    int64
	ExpiryMs   int64
	Incomplete bool // 三个关键字段没读全 → 不许据此改写
	// Inbounds 是这条 client 在节点上**实际挂着的**入站 id（inbounds/list 按入站归属解析，
	// 同 email 跨入站取并集）。用来发现「节点后加了入站、老用户没挂上」这类漂移。
	Inbounds []int64
}

// nodeSnapshot 是一次节点读取的结果：结论集 + 本轮确实探测过的 email 集合。
//
// Probed 与 Entries 必须分开：Entries 里没有 = 「探测过、确认不存在」，
// 而没进 Probed 的 = 「本轮没看」，两者语义相反，混用会把没探测的人当成缺人而重复注册。
type nodeSnapshot struct {
	Entries map[string]nodeSnapshotEntry
	Probed  map[string]bool
	// ProbeFailed 是退化路径里「探测本身失败」的条数（既不进 Entries 也不进 Probed）。
	ProbeFailed int
	Foreign     []string
	Source      string // inbounds / probe
	// SampleKeys 是「字段读不全」样本对象的键名（每种读法最多留一条）：
	// 3x-ui 各版本 client 字段命名不一致（total/totalGB、expiry_time/expiryTime），
	// 读不全时必须把真实键名打出来，否则又会像旧的 checked=0 一样静默空转。
	SampleKeys []string
}

// ReconcileNodes 做一轮「节点全量比对 + 兜底修复」。dry=true 只报告不动手。
func (c *Collector) ReconcileNodes(ctx context.Context, dry bool, maxWritesPerNode int) (ReconcileReport, error) {
	start := time.Now()
	var rep ReconcileReport
	if maxWritesPerNode <= 0 {
		maxWritesPerNode = 50
	}
	const probeBudgetPerNode = 60 // 退化路径下每节点每轮最多探测多少个用户

	users, err := c.reconcileUsers(ctx)
	if err != nil {
		return rep, err
	}
	rep.Users = len(users)

	// 补挂/补建一轮要能补齐整台节点：节点上新加入站时，缺的是「全部有套餐用户」，
	// 按 50 上限会被切到下一轮，用户看到的就是「入站只补了一半人」。
	// 用户定稿：一轮就补齐，不设人为上限。
	if len(users) > maxWritesPerNode {
		maxWritesPerNode = len(users)
	}

	// 我们自己建的 client 只认 `ch_user_{id}` 且该 id 在 users 表里存在；
	// 节点上其它 email（自建的远程用户、历史孤儿）一律不碰，只报告。
	ourEmails := make(map[string]bool, len(users))
	for _, u := range users {
		ourEmails[clientEmail(u.ID)] = true
	}

	nodes, err := c.enabledNodes(ctx)
	if err != nil {
		return rep, err
	}
	rep.Nodes = len(nodes)

	needOpen := map[int64]bool{}
	needClose := map[int64]bool{}

	// 只有「标记与面板意图矛盾」的人才需要 ToggleClient（它覆盖 xray 侧并写/清标记）；
	// 标记本来一致的人，3x-ui 侧直接修即可，xray 侧由 agent 周期拉取自行对齐。
	markIfContradictory := func(u reconcileUser) {
		if u.Intent && u.FlagSet {
			needOpen[u.ID] = true
		}
		// 无套餐的人**不参与关闭**（旧系统也从不碰）：摘了只会造成"建号→被摘→分配套餐又建回来"的抖动。
		if u.PlanIDValid && !u.Intent && !u.FlagSet {
			needClose[u.ID] = true
		}
	}

	sources := map[string]bool{}
	round := time.Now().Unix() / 300 // 轮转偏移：退化路径每轮换一批人，几轮内覆盖全体

	// ── 并发取：节点之间并行（上限 4），单节点内部仍串行 ──
	// 用户定稿：四节点并发取；**超时/掉线只兜底重试一次**，再失败就直接放弃这台
	// （本轮记 OfflineNodes + Unknown，绝不把失败当「节点上没有这个人」）。
	type nodeResult struct {
		node nodeRow
		out  nodeOutcome
		err  error
		skip bool
	}
	results := make([]nodeResult, len(nodes))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, node := range nodes {
		if node.isXray() {
			// 裸核节点没有可读回的 client 快照：只登记标记/意图矛盾，开/关走 ToggleClient。
			results[i] = nodeResult{node: node, skip: true}
			continue
		}
		if node.DriverType != "" && node.DriverType != "3x-ui" {
			results[i] = nodeResult{node: node, skip: true}
			continue
		}
		// 健康检查已判离线的节点：**直接放弃**，不发任何请求（用户定稿：ping 不到就别试）。
		if strings.EqualFold(node.Status, "offline") {
			results[i] = nodeResult{node: node, err: fmt.Errorf("节点离线（health-check 判定）")}
			continue
		}
		wg.Add(1)
		go func(i int, node nodeRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := c.reconcileOnNode(ctx, node, users, ourEmails, dry,
				maxWritesPerNode, probeBudgetPerNode, int(round))
			results[i] = nodeResult{node: node, out: out, err: err}
		}(i, node)
	}
	wg.Wait()

	// 合并各节点结果（单 goroutine 合并，避免锁竞争）
	for _, r := range results {
		if r.skip {
			if r.node.isXray() {
				for _, u := range users {
					markIfContradictory(u)
				}
			}
			continue
		}
		if r.err != nil {
			rep.OfflineNodes = append(rep.OfflineNodes, fmt.Sprintf("%s(%v)", r.node.Name, r.err))
			rep.Unknown += len(users)
			continue
		}
		out := r.out
		if out.Source != "" {
			sources[out.Source] = true
		}
		if len(out.SampleKeys) > 0 {
			c.logf_("[reconcile] %s 快照字段读不全（%s），样本键名=[%s] —— 需要按它补字段映射",
				r.node.Name, out.Source, strings.Join(out.SampleKeys, ","))
		}
		rep.Probed += out.Probed
		rep.Registered += out.Registered
		rep.Fixed += out.Fixed
		rep.Drift += out.Drift
		rep.DriftSafe += out.DriftSafe
		rep.DriftEarlyCut += out.DriftEarlyCut
		rep.DriftLoose += out.DriftLoose
		rep.ProbeFailed += out.ProbeFailed
		rep.Opened += out.Opened
		rep.Closed += out.Closed
		rep.Attached += out.Attached
		rep.Unchanged += out.Unchanged
		rep.Unknown += out.Unknown
		rep.Deferred += out.Deferred
		rep.Foreign += out.Foreign
		for _, email := range out.ForeignNames {
			if len(rep.ForeignNames) < 10 {
				rep.ForeignNames = append(rep.ForeignNames, r.node.Name+":"+email)
			}
		}
		for _, id := range out.NeedOpen {
			needOpen[id] = true
		}
		for _, id := range out.NeedClose {
			needClose[id] = true
		}
	}

	switch {
	case len(sources) == 0:
		rep.SnapshotBy = "none"
	case len(sources) == 1:
		for k := range sources {
			rep.SnapshotBy = k
		}
	default:
		rep.SnapshotBy = "mixed"
	}

	// 本轮结论都在 ReconcileReport 里（含方向分档与探测失败数）。
	// 「日志升级 WARN」由调用方按报告判定并只打一次（task.runUsersReconcileEnable）——
	// 这里再喊一遍会让同一件事每 5 分钟刷两条 WARN。

	// xray 侧 + users.traffic_disabled_at 的记账：统一交给 ToggleClient（全部节点确认才算）。
	if !dry {
		for id := range needOpen {
			if ok, err := c.ToggleClient(ctx, id, true); err != nil || !ok {
				c.logf_("[reconcile] 打开未全部确认 user=%d err=%v applied=%v", id, err, ok)
			}
		}
		for id := range needClose {
			if ok, err := c.ToggleClient(ctx, id, false); err != nil || !ok {
				c.logf_("[reconcile] 关闭未全部确认 user=%d err=%v applied=%v", id, err, ok)
			}
		}
	}

	// ToggleClient 在全部 enabled 节点上重写 enable（含本轮没在节点循环里动过的节点）：
	// 只要本轮到过这一支，缓存里任何节点的 enable 都可能过期 → 整体作废，绝不顺着旧快照
	// 再判下一轮。
	if !dry && (len(needOpen) > 0 || len(needClose) > 0) {
		c.clearInboundSnapshots()
	}

	rep.ElapsedMs = time.Since(start).Milliseconds()
	return rep, nil
}

// nodeOutcome 是单台节点一轮比对的产出（并发执行，各节点独立统计，最后由主 goroutine 合并）。
type nodeOutcome struct {
	Source        string
	SampleKeys    []string
	Probed        int
	ProbeFailed   int
	Registered    int
	Fixed         int
	Drift         int
	DriftSafe     int
	DriftEarlyCut int
	DriftLoose    int
	Opened        int
	Closed        int
	Attached      int
	Unchanged     int
	Unknown       int
	Deferred      int
	Foreign       int
	ForeignNames  []string
	NeedOpen      []int64
	NeedClose     []int64
}

// driftClass 是 total/expiry 漂移的方向分档（只影响计数与日志级别，不影响读法/修法）。
type driftClass int

const (
	// driftSafe：节点不设限（total==0）—— 节点放得比面板开，靠面板封禁兜底，用户无感。
	driftSafe driftClass = iota
	// driftEarlyCut：节点配额/期限比面板更紧 —— **用户会被节点提前掐**，必须人工确认。
	driftEarlyCut
	// driftLoose：节点设了限但比面板更宽松（安全方向）。
	driftLoose
)

// label 是日志里的中文口径（与旧版逐字一致，便于对线上历史日志）。
func (d driftClass) label() string {
	switch d {
	case driftEarlyCut:
		return "节点<面板(会提前掐人,需人工确认)"
	case driftLoose:
		return "节点更宽松(安全)"
	default:
		return "节点不限(靠面板兜底,安全)"
	}
}

// classifyDrift 判定一条漂移的方向分档（纯函数，只读三个字段）。
//
// 3x-ui 语义（实测）：total==0 = **不限流量**（不是"限额 0"）；expiry==0 = 不过期。
// 判定顺序（期限优先 —— 期限提前到期是最硬的"提前掐人"）：
//  1. 两边都有期限、节点更早 → EarlyCut；
//  2. 面板有限额、节点限额更小 → EarlyCut（0 < 节点 < 面板）；
//  3. 面板有限额、节点不设限 → Safe；
//  4. 其余 → Loose（节点比面板宽松）。
func classifyDrift(entry nodeSnapshotEntry, u reconcileUser) driftClass {
	if u.ExpiryMs > 0 && entry.ExpiryMs > 0 && entry.ExpiryMs < u.ExpiryMs {
		return driftEarlyCut
	}
	if entry.TotalGB > 0 && entry.TotalGB < u.Limit {
		return driftEarlyCut
	}
	if entry.TotalGB == 0 && u.Limit > 0 {
		return driftSafe
	}
	return driftLoose
}

// recordDrift 记一条漂移并累加对应方向分档（三档之和恒等于 Drift）。
func (o *nodeOutcome) recordDrift(d driftClass) {
	o.Drift++
	switch d {
	case driftEarlyCut:
		o.DriftEarlyCut++
	case driftLoose:
		o.DriftLoose++
	default:
		o.DriftSafe++
	}
}

// reconcileOnNode 处理单台 3x-ui 节点：取快照 → 逐用户按面板意图比对 → 只修开关方向。
//
// 并发安全：不碰 ReconcileReport、不碰共享 map，全部写进返回值；日志用 c.logf_（内部自带锁）。
func (c *Collector) reconcileOnNode(ctx context.Context, node nodeRow, users []reconcileUser,
	ourEmails map[string]bool, dry bool, maxWritesPerNode, probeBudget, round int) (nodeOutcome, error) {

	var out nodeOutcome
	client, err := c.threeXUiClient(node)
	if err != nil {
		return out, fmt.Errorf("构造失败: %w", err)
	}

	snap, err := c.nodeSnapshot(ctx, node.ID, client, users, probeBudget, round)
	if err != nil {
		return out, err
	}
	// 该节点各协议的入站集合：用来判「这个人有没有挂到本协议的全部入站」。
	// 一次查询覆盖全部用户（按协议分组），避免逐用户查库。
	protoInbounds, err := c.nodeInboundIDsByProtocol(ctx, node.ID)
	if err != nil {
		return out, err
	}
	out.Source = snap.Source
	out.SampleKeys = snap.SampleKeys
	out.ProbeFailed = snap.ProbeFailed
	for _, email := range snap.Foreign {
		out.Foreign++
		if len(out.ForeignNames) < 10 {
			out.ForeignNames = append(out.ForeignNames, email)
		}
	}

	// 分批写入：maxWritesPerNode 是「单批上限」，不是「本轮总量上限」。
	// 节点新加入站时要补的人可能上百个，卡在单批会被切到下一轮（用户看到「只补了一半」）。
	// 所以写满一批就歇一下继续下一批，直到这轮该补的都补完。
	writes := 0
	batchCap := maxWritesPerNode
	if batchCap <= 0 {
		batchCap = 100
	}
	batchWrites := 0
	for _, u := range users {
		email := clientEmail(u.ID)
		if !snap.Probed[email] {
			out.Deferred++ // 本轮没探测（预算外/轮转未轮到）
			continue
		}
		entry, exists := snap.Entries[email]

		if !exists {
			// 确认不存在：只有「该可用」才补注册；「该关」的人没有 = 已关。
			if !u.Intent {
				out.Unchanged++
				continue
			}
			out.Probed++
			if !dry && batchWrites >= batchCap {
				// 一批写完，歇一下再继续下一批（避免瞬间打满节点）
				select {
				case <-ctx.Done():
					return out, ctx.Err()
				case <-time.After(300 * time.Millisecond):
				}
				batchWrites = 0
			}
			if dry {
				out.Registered++
				out.Opened++
				writes++
				batchWrites++
				continue
			}
			if err := c.addClientOnNode(ctx, client, node, u, true); err != nil {
				c.logf_("[reconcile] 补注册失败 node=%s user=%d: %v", node.Name, u.ID, err)
				out.Unknown++
				continue
			}
			out.Registered++
			out.Opened++
			out.NeedOpen = append(out.NeedOpen, u.ID)
			writes++
			batchWrites++
			continue
		}

		out.Probed++
		if entry.Incomplete {
			out.Unknown++ // 字段读不全 → 不许据此改写
			continue
		}

		// 入站维度：节点后加了入站、或远程把 client 丢了 → 这个人缺挂载/缺人，
		// 新入站上连不上（3x-ui 的 update 只改 client 定义，不会自动补挂）。
		// want 取该节点**全部**勾选的入站，不按用户协议过滤：用户全协议通用，
		// 节点上新加的 hy2 入站也要给所有老用户补上（否则那条入站永远 0 客户端）。
		want := allInboundIDs(protoInbounds)
		if len(want) > 0 && len(entry.Inbounds) > 0 && u.PlanIDValid {
			if missing := missingInbounds(want, entry.Inbounds); len(missing) > 0 {
				if !dry && batchWrites >= batchCap {
					select {
					case <-ctx.Done():
						return out, ctx.Err()
					case <-time.After(300 * time.Millisecond):
					}
					batchWrites = 0
				}
				writes++
				batchWrites++
				if dry {
					out.Attached++
				} else if err := client.AttachClient(ctx, email, missing); err != nil {
					c.logf_("[reconcile] 补挂入站失败 node=%s user=%d missing=%v: %v",
						node.Name, u.ID, missing, err)
					out.Unknown++
					continue
				} else {
					out.Attached++
					if out.Attached <= 3 {
						c.logf_("[reconcile] 补挂入站 node=%s user=%d inbounds=%v", node.Name, u.ID, missing)
					}
				}
			}
		}

		// 只对**开关方向**动手：该开被关→开、该关还开→关。
		// total/expiryTime 漂移只报告（用户无感，写错代价大；实测 117/117 本来一致）。
		// 无套餐的人一律不碰（没有"该关"这回事）。
		if entry.Enable != u.Intent && !u.PlanIDValid {
			out.Unchanged++
			continue
		}
		if entry.Enable != u.Intent {
			if writes >= maxWritesPerNode {
				out.Deferred++
				continue
			}
			if dry {
				if u.Intent {
					out.Opened++
				} else {
					out.Closed++
				}
				writes++
				continue
			}
			ok, err := c.applyEnableOnNode(ctx, node, u.Protocol, email, u.Intent)
			if err != nil || !ok {
				c.logf_("[reconcile] 开关未确认 node=%s user=%d enable=%v err=%v", node.Name, u.ID, u.Intent, err)
				out.Unknown++
				continue
			}
			out.Fixed++
			if u.Intent {
				out.Opened++
				out.NeedOpen = append(out.NeedOpen, u.ID)
			} else {
				out.Closed++
				out.NeedClose = append(out.NeedClose, u.ID)
			}
			writes++
			continue
		}

		// enable 一致、只有 total/expiry 对不上 → 只报告，带方向分档（classifyDrift）。
		// 3x-ui 的语义：total==0 = **不限流量**（不是"限额 0"），所以
		//   node_total==0          → 节点不设限、靠面板封禁兜底（安全方向）
		//   0 < node_total < panel → 节点会比面板更早掐人（唯一需人工处理的方向）
		//   node_total > panel     → 节点更宽松（安全方向）
		if entry.TotalGB != u.Limit || entry.ExpiryMs != u.ExpiryMs {
			dir := classifyDrift(entry, u)
			out.recordDrift(dir)
			if out.Drift <= 3 {
				c.logf_("[reconcile] 字段漂移（只报告）node=%s user=%d total %d/%d expiry %d/%d %s",
					node.Name, u.ID, entry.TotalGB, u.Limit, entry.ExpiryMs, u.ExpiryMs, dir.label())
			}
			continue
		}
		out.Unchanged++
	}

	// 写入后作废该节点的共享快照：本轮读到的 entry 已是「写入前」的，
	// 若留着 TTL，紧接着跑的 traffic-sync 会把刚修好的人误报成漂移（drift 记录）。
	if !dry && writes > 0 {
		c.invalidateInboundSnapshot(node.ID)
	}
	return out, nil
}

// ── 读快照 ───────────────────────────────────────────────────

// nodeInboundIDsByProtocol 一次查出该节点各协议的入站 id（升序去重）。
//
// 入站维度对账用：判「用户的 client 有没有挂到本协议的全部入站」。一次查询覆盖整轮，
// 不逐用户查库。
func (c *Collector) nodeInboundIDsByProtocol(ctx context.Context, nodeID int64) (map[string][]int64, error) {
	if c.db == nil {
		// 无库场景（纯渲染/单测）：没有入站维度可对账，按「不知道」处理（不补挂）。
		return map[string][]int64{}, nil
	}
	rows, err := c.db.QueryContext(ctx,
		"SELECT protocol, inbound_id FROM node_inbounds WHERE node_id = ?", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询节点入站失败: %w", err)
	}
	defer rows.Close()

	out := map[string][]int64{}
	for rows.Next() {
		var proto string
		var id int64
		if err := rows.Scan(&proto, &id); err != nil {
			return nil, fmt.Errorf("读取节点入站失败: %w", err)
		}
		out[proto] = appendUniqueID(out[proto], id)
	}
	return out, rows.Err()
}

// nodeSnapshot 读一次节点：先试 inbounds/list 全量解析，拿不到就退化逐用户探测（带预算）。
//
// 用户定稿的兜底口径：
//   - **ping 不到（连不上/拒绝/无路由/DNS 失败）→ 直接放弃**，不重试（重试也是白等）；
//   - **超时 → 兜底重试一次**（多数是瞬时抖动），再超时才放弃；
//   - 放弃的节点本轮记 OfflineNodes + Unknown，绝不当成「节点上没有这个人」而误注册/误清标记。
//
// 重试语义与缓存的关系：失败的拉取不进缓存（见 cachedInbounds），所以「超时再试一次」
// 仍然是一次真实的新请求，不会被上一轮的错误结果短路掉。
func (c *Collector) nodeSnapshot(ctx context.Context, nodeID int64, client *threexui.Client, users []reconcileUser,
	budget, round int) (nodeSnapshot, error) {

	snap, err := c.nodeSnapshotOnce(ctx, nodeID, client, users, budget, round)
	if err == nil || ctx.Err() != nil || !isTimeoutErr(err) {
		return snap, err
	}
	c.logf_("[reconcile] 拉取超时，兜底重试一次: %v", err)
	return c.nodeSnapshotOnce(ctx, nodeID, client, users, budget, round)
}

// isTimeoutErr 判定是否「超时类」错误（只有这类才值得重试一次）。
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

// nodeSnapshotOnce 是单次拉取（不含重试）。
func (c *Collector) nodeSnapshotOnce(ctx context.Context, nodeID int64, client *threexui.Client, users []reconcileUser,
	budget, round int) (nodeSnapshot, error) {

	if entries, keysSample, err := c.snapshotFromInbounds(ctx, nodeID, client); err == nil && len(entries) > 0 {
		snap := nodeSnapshot{Entries: entries, Probed: map[string]bool{}, Source: "inbounds", SampleKeys: keysSample}
		ourSet := make(map[string]bool, len(users))
		for _, u := range users {
			ourSet[clientEmail(u.ID)] = true
		}
		for email := range entries {
			if ourSet[email] {
				snap.Probed[email] = true
			} else {
				snap.Foreign = append(snap.Foreign, email)
			}
		}
		// inbounds 快照里没有的我们的人 = 确认不存在（本轮确实看到了全量）
		for _, u := range users {
			email := clientEmail(u.ID)
			if !snap.Probed[email] {
				snap.Probed[email] = true
			}
		}
		return snap, nil
	}

	// 退化路径：逐用户探测，带预算 + 轮转（覆盖会跨轮补齐，不在单轮里把节点打满）
	return c.snapshotFromProbe(ctx, client, users, budget, round)
}

// snapshotFromInbounds 一次 inbounds/list，解析每个入站的 client 列表。
//
// 本版 3x-ui 的实测口径（与仓库里确定能用的 clientStatsFromInbounds 对齐）：
//   - 入站对象里带 **`clientStats`** 数组（= client_traffics 行），字段是 **snake_case**：
//     `email` / `enable` / `total`（限额字节）/ `expiry_time`（毫秒）；
//   - `settings` 可能是 JSON 字符串，也可能已经是解析好的对象，里面 `clients[]` 用 camelCase
//     （`totalGB` / `expiryTime`）。两种形态都要吃，取不到的字段标 Incomplete 并留键名样本。
//
// 读的是节点级共享快照（TTL + singleflight，见 inbound_cache.go）：与 traffic-sync 的
// Stats 出自同一次 inbounds/list，两个任务不再各拉一遍。解析逻辑在 parseInboundSnapshot。
func (c *Collector) snapshotFromInbounds(ctx context.Context, nodeID int64, client *threexui.Client) (map[string]nodeSnapshotEntry, []string, error) {
	snap, err := c.cachedInbounds(ctx, nodeID, client)
	if err != nil {
		return nil, nil, err
	}
	return snap.Entries, snap.KeysSample, nil
}

// clientsOfInbound 从入站对象里取出它的 client 列表（clientStats 优先，settings 兜底）。
func clientsOfInbound(ib map[string]any) []map[string]any {
	var out []map[string]any

	// ① clientStats（本版实测存在，snake_case）
	if arr, ok := ib["clientStats"].([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}

	// ② settings.clients（老形态；字符串或已解析对象都可能）
	var settings map[string]any
	switch s := ib["settings"].(type) {
	case string:
		if strings.TrimSpace(s) != "" {
			var parsed map[string]any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				settings = parsed
			}
		}
	case map[string]any:
		settings = s
	}
	if settings != nil {
		if arr, ok := settings["clients"].([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// snapshotFromProbe 退化路径：逐用户 GetClientStrict，带预算与轮转偏移。
func (c *Collector) snapshotFromProbe(ctx context.Context, client *threexui.Client, users []reconcileUser,
	budget, round int) (nodeSnapshot, error) {

	snap := nodeSnapshot{Entries: map[string]nodeSnapshotEntry{}, Probed: map[string]bool{}, Source: "probe"}
	if budget <= 0 {
		budget = 60
	}
	if len(users) == 0 {
		return snap, nil
	}
	offset := 0
	if round > 0 {
		offset = (round * budget) % len(users)
	}
	done := 0
	for i := 0; i < len(users) && done < budget; i++ {
		u := users[(offset+i)%len(users)]
		email := clientEmail(u.ID)
		if snap.Probed[email] {
			continue
		}
		obj, err := client.GetClientStrict(ctx, email)
		if err != nil {
			if threexui.IsClientNotFound(err) {
				snap.Probed[email] = true // 确认不存在
				done++
				continue
			}
			// 探测本身失败（超时/5xx/登录态失效）：不算探测过，下轮再来 ——
			// 但必须单独计数，否则整轮全失败时快照看起来「干净」而实际一个人都没看到。
			snap.ProbeFailed++
			continue
		}
		if obj == nil {
			snap.Probed[email] = true
			done++
			continue
		}
		snap.Entries[email] = entryFromClientObject(obj)
		if snap.Entries[email].Incomplete && snap.SampleKeys == nil {
			snap.SampleKeys = objKeyNames(obj)
		}
		snap.Probed[email] = true
		done++
	}
	return snap, nil
}

// entryFromClientObject 从 client 对象里取三个比对字段。
//
// 线上实测的键名（JP1/US1/US3/MYKL1 一致）：
//
//	enable / expiryTime / total  ← clientStats 形态（限额叫 total，不是 totalGB）
//	enable / expiryTime / totalGB ← settings.clients 形态（老的 camelCase）
//
// 判定：**只有 enable 读不到才算读不全**；`total`/`expiryTime` 为 null 或缺失一律按 0
// （3x-ui 里 0 = 不限流量 / 不过期，是合法值而不是"没读到"）。
// 早期版本把 total=null 当成读不全 → 整轮 unknown、一个人都没修（线上实测过）。
func entryFromClientObject(obj map[string]any) nodeSnapshotEntry {
	if inner, ok := obj["client"].(map[string]any); ok {
		obj = inner
	}
	var e nodeSnapshotEntry
	enable, hasEnable := truthyField(obj, "enable")
	if !hasEnable {
		e.Incomplete = true
		return e
	}
	totalGB, _ := firstIntField(obj, "totalGB", "total")
	expiryMs, _ := firstIntField(obj, "expiryTime", "expiry_time")
	e.Enable, e.TotalGB, e.ExpiryMs = enable, totalGB, expiryMs
	return e
}

// truthyField 取布尔/数值/字符串的可真值字段。
//
// ⚠️ 必须吃 json.Number：驱动层的 Decode 用 UseNumber（大整数保原文），
// 节点回来的 `enable: true` 是 bool、但 `enable: 1` 会是 json.Number("1")。
// 只认 float64/int64 会让真实节点上的 enable 全部「读不到」→ 整条判 Incomplete → 全轮 Unknown。
func truthyField(obj map[string]any, key string) (bool, bool) {
	v, ok := obj[key]
	if !ok || v == nil {
		return false, false
	}
	switch t := v.(type) {
	case bool:
		return t, true
	case float64:
		return t != 0, true
	case int64:
		return t != 0, true
	case int:
		return t != 0, true
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f != 0, true
		}
		return false, false
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		return s == "true" || s == "1", true
	default:
		return false, false
	}
}

// firstIntField 依次尝试多个键名取整数值（兼容 camelCase / snake_case）。
func firstIntField(obj map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		if v, ok := asInt64(obj[k]); ok {
			return v, true
		}
	}
	return 0, false
}

// ── 写：补注册 / 修正 / 重建 ─────────────────────────────────

// allInboundIDs 把「协议 → 入站 id」的 map 拍平成一串去重升序的入站 id。
//
// 用户全协议通用：补挂/补建时要用该节点**全部**勾选的入站，不按用户协议过滤。
func allInboundIDs(byProto map[string][]int64) []int64 {
	var out []int64
	for _, ids := range byProto {
		for _, id := range ids {
			out = appendUniqueID(out, id)
		}
	}
	return out
}

// addClientOnNode 在单台 3x-ui 节点上补注册（按面板意图组负载）。
//
// 这是**新建**路径（补注册 / 重建都是这一条），所以要带协议凭据：
// vless/vmess→id、trojan/shadowsocks→password、hysteria→auth（取 users.uuid）。
// 不带的话 hysteria 的 client 会被 3x-ui 拒成 `empty client ID`，补注册等于白跑。
// 更新路径（updateClientOnNode）相反，见那边的说明。
func (c *Collector) addClientOnNode(ctx context.Context, client *threexui.Client, node nodeRow, u reconcileUser, enable bool) error {
	inboundIDs, err := c.inboundIDsFor(ctx, node.ID, u.Protocol)
	if err != nil {
		return err
	}
	if len(inboundIDs) == 0 {
		return nil // 该节点没有这个协议的端口：不必注册（等同不存在）
	}
	payload := threexui.ClientPayload(clientEmail(u.ID), enable, u.Limit, u.ExpiryMs)
	_, err = client.AddClient(ctx, payload, inboundIDs)
	return err
}

// updateClientOnNode 修正字段但**保留 uuid**：get 全量 → NormalizeForUpdate → 覆盖三个字段。
//
// 刻意**不传协议凭据**：凭据（id/password/auth）已经由 NormalizeForUpdate 从节点上的
// 完整对象原样带回，节点 uuid 保持不变。若在这里改成显式写 users.uuid，存量用户的 uuid
// 会被改写 → 订阅链接与客户端配置当场失效。新建（含重建）才该带凭据，见 addClientOnNode。
//
// 逐入站 + 兜底各更新一次，与 applyEnableOnNode 同序（3x-ui 的 update 是整条替换语义，
// 且入站维度也有 client 副本，只更一次可能漏掉某条入站）。
func (c *Collector) updateClientOnNode(ctx context.Context, client *threexui.Client, node nodeRow, u reconcileUser) error {
	email := clientEmail(u.ID)
	full, err := client.GetClientStrict(ctx, email)
	if err != nil {
		return err
	}
	if full == nil {
		return fmt.Errorf("client 不存在，无法更新")
	}
	payload := NormalizeForUpdate(full)
	payload["email"] = email
	payload["enable"] = u.Intent
	payload["totalGB"] = u.Limit
	payload["expiryTime"] = u.ExpiryMs
	// 刻意不动 limitIp / flow / subId：只改我们负责的三个字段，其它保持节点现状。

	inboundIDs, err := c.inboundIDsFor(ctx, node.ID, u.Protocol)
	if err != nil {
		return err
	}
	var firstErr error
	for _, inboundID := range inboundIDs {
		id := inboundID
		if err := client.UpdateClient(ctx, email, payload, &id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := client.UpdateClient(ctx, email, payload, nil); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// rebuildClientOnNode 删除重建：DeleteClient(keepTraffic=1) 保住流量统计基线 → 重新 AddClient。
//
// ⚠️ 重建会走 addClientOnNode，也就是带上 users.uuid 作凭据：uuid 不再由面板随机生成，
// 重建后与面板侧 users.uuid 一致（订阅复算得回来）。但重建前节点上的 uuid 若本来就是
// 面板早期随机生成的、与 users.uuid 不同，这一步仍会换号 —— 手工导入链接的用户要改配置。
// 因此只在「字段修正后仍对不上」时才用它，并在日志里标出来。
func (c *Collector) rebuildClientOnNode(ctx context.Context, client *threexui.Client, node nodeRow, u reconcileUser) error {
	email := clientEmail(u.ID)
	if err := client.DeleteClient(ctx, email, true); err != nil {
		return fmt.Errorf("删除失败: %w", err)
	}
	return c.addClientOnNode(ctx, client, node, u, u.Intent)
}

// clientMatches 复核节点上的 client 是否已与面板意图一致（修正后的验收）。
func (c *Collector) clientMatches(ctx context.Context, client *threexui.Client, u reconcileUser) bool {
	obj, err := client.GetClientStrict(ctx, clientEmail(u.ID))
	if err != nil || obj == nil {
		return false
	}
	e := entryFromClientObject(obj)
	if e.Incomplete {
		return false // 读不全 → 不算通过
	}
	return e.Enable == u.Intent && e.TotalGB == u.Limit && e.ExpiryMs == u.ExpiryMs
}

// reconcileUsers 一次取齐参与比对的面板用户（意图在 Go 侧算，不再用 SQL 排除带标记的人）。
func (c *Collector) reconcileUsers(ctx context.Context) ([]reconcileUser, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT u.id, u.uuid, u.protocol, u.enabled, u.traffic_disabled_at, u.expired_at, u.traffic_limit, "+
			"u.traffic_used, u.monthly_traffic_used, u.monthly_traffic_limit, u.plan_id, p.type "+
			"FROM users u LEFT JOIN plans p ON p.id = u.plan_id ORDER BY u.id")
	if err != nil {
		return nil, fmt.Errorf("查询对账用户失败: %w", err)
	}
	defer rows.Close()

	now := c.now()
	var out []reconcileUser
	for rows.Next() {
		var (
			u        reconcileUser
			enabled  int64
			flagAt   sql.NullTime
			expired  sql.NullTime
			planID   sql.NullInt64
			planType sql.NullString
			used     int64
			monthly  int64
			monthLim int64
		)
		if err := rows.Scan(&u.ID, &u.UUID, &u.Protocol, &enabled, &flagAt, &expired, &u.Limit,
			&used, &monthly, &monthLim, &planID, &planType); err != nil {
			return nil, fmt.Errorf("读取对账用户失败: %w", err)
		}
		row := userRow{
			ID:                 u.ID,
			Enabled:            enabled != 0,
			ExpiredAt:          expired,
			TrafficLimit:       u.Limit,
			TrafficUsed:        used,
			MonthlyTrafficUsed: monthly,
			MonthlyLimit:       monthLim,
			PlanID:             planID,
		}
		row.Period = planType.Valid && planType.String == "period"
		if expired.Valid {
			u.ExpiryMs = expired.Time.UnixMilli()
		}
		// 面板意图：账号启用 + 有套餐 + 未到期 + 未超量（与封禁扫描器同一判据 banReasonAt）。
		u.Intent = row.Enabled && row.PlanID.Valid && banReasonAt(row, now) == ""
		u.PlanIDValid = row.PlanID.Valid
		u.FlagSet = flagAt.Valid
		out = append(out, u)
	}
	return out, rows.Err()
}

// ── 小工具 ───────────────────────────────────────────────────

// asInt64 兼容 JSON 数字与字符串两种形态。
//
// ⚠️ json.Number 是主路径（驱动用 UseNumber 解码，保大整数原文）：
// 流量限额/期限是 int64 量级，走 float64 中转会把大数截断成相邻可表示值；
// 原先只认 float64 时，真实节点上的 total/expiryTime 一律读成 0 → 字段漂移全部漏报。
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, false
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		// 面板偶尔把整数编码成 "9.0"/1e3 形态：退一步按浮点截断。
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// stringOf 宽松取字符串。
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// objKeyNames 返回对象的键名（排序后），用于诊断「字段读不全」时把真实命名打出来。
func objKeyNames(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 24 {
		keys = keys[:24]
	}
	return keys
}
