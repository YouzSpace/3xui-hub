package collector

// 裸内核（driver_type=xray）节点的用户全量对账。
//
// 为什么单开一层：3x-ui 节点可以直连 HTTP 读回 inbounds/list，裸内核没有那个接口 ——
// 以前 reconcile.go 遇到裸内核直接 `skip:true`，等于「面板不知道节点上到底有谁」，
// 只能等封禁扫描器一条条 rmu/adu 撞上去。这里补上读法：
//
//	入队一条 client_list 指令（复用 node_api_commands，与 firewall_hop_apply 同一条通道）
//	  → 同步等回执 → 解析出「每个入站的全部 client」→ 与面板意图逐条比对。
//
// 修法只有一种：bump 该节点的 config_version。裸内核的 agent 看到版本变了就重拉
// 全量配置（节点上的 client 集合完全由渲染结果决定），所以**任何**「节点状态与渲染
// 结果不一致」都能靠重拉治好；这也是为什么这里绝不像 3x-ui 那样 add/update client
// （裸内核没有那句 update 接口，adu/rmu 只能在运行时动单个人，修不了集合差异）。
//
// 比对基准是「重拉一次配置之后节点应该长什么样」，不是「面板希望谁可用」：
//
//	renderEligible = 账号启用 && 有套餐 && 未打流量禁用标记（traffic_disabled_at IS NULL）
//
// 这是 nodeapi.eligibleUsers() 的渲染判据（过期/超量的人仍在渲染集合里，由内核侧的
// expiryTime/totalGB 拦，封禁扫描器打到标记后才会从渲染里消失）。用「面板意图」当基准
// 会有一个致命的坏形态：过期但还没打标记的人永远「不该在节点上」，于是每轮都判不一致
// → 每 5 分钟 bump 一次 → agent 每 5 分钟重启一次内核。用「重拉后会怎样」当基准，
// 一次 bump 之后必然收敛，不会自激。
//
// 读到什么程度才算有结论（与 3x-ui 侧 UNKNOWN 口径对齐）：
//   - 回执没回来（超时/节点离线）→ UNKNOWN，本轮不动；
//   - 回执不是合法 JSON / 结构对不上 → UNKNOWN，本轮不动；
//   - 回执标了 truncated（agent 超过 39000 字符自己截短了，面板 ack 校验上限 40000）
//     → UNKNOWN，本轮不动：半份数据会把「没回的人」误判成「节点上缺人」；
//   - 单个 client 的 enable 读不到 → 这条跳过（不据此判节点有问题）。
//
// 已知限制：agent 3.1 起回执按 email 扁平化（issue #115）—— 协议通用后同一批用户会被
// 注入每个入站，旧的「入站 × client」回执体积 = 人数 × 入站数，5 个入站 103 人就会顶到
// 39000 字符被截断、这台节点的裸内核对账永久按 UNKNOWN 跳过。扁平化后体积只与人数有关，
// 但人数特别多（约 500+）时仍会被 agent 截断；要真正覆盖那种规模需要协议支持分页
// （按 email 游标分批读），属于另一层改动。
//
// totalGB / expiryTime 对不上**只报告不 bump**：协议里的 totalGB 单位没写死（面板侧是字节，
// 3x-ui 侧也叫 totalGB），拿不准单位就 bump 会把内核每轮重启一次。成员集合与 enable
// 已经能覆盖「用户连不上/不该连却在连」这两类真实故障。
// （agent 侧目前恒报 enable=true —— 内核 client 没有 enable 字段，停用靠 totalGB/expiryTime
// 表达 —— 所以 Disabled 档在真机上基本不会触发，留着是给「带 enable 的配置形态」兜底。）

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/nodeapi"
)

const (
	// bareReconcileWaitDefault 是等 client_list 回执的默认上限（秒）。
	// agent 侧是 2 秒长轮询 + 一次读 config.json，正常秒级；等不到就按 UNKNOWN 走。
	// 可用 RECONCILE_BARE_WAIT_SECONDS 覆盖（节点多、网络慢时调大）。
	bareReconcileWaitDefault = 20
)

// barePollInterval 是轮询指令终态的间隔（变量而非常量：DB 用例要把它压到毫秒级）。
var barePollInterval = 500 * time.Millisecond

// bareClientSnapshot 是节点上一次 client_list 的解析结果（entries 与 3x-ui 侧同形，
// 所以比对语义直接复用 nodeSnapshotEntry）。
type bareClientSnapshot struct {
	entries map[string]nodeSnapshotEntry
	// foreign 是非本站 email（我们不认识、也不动的 client）。
	foreign []string
	// sampleKeys 是「enable 读不到」时的样本键名，用于诊断 agent 回执字段改名。
	sampleKeys []string
}

// bareCompareResult 是一轮比对的结论（纯计算，无 IO）。
type bareCompareResult struct {
	Probed   int // 有明确结论的用户数
	Missing  int // 该在节点上、实际不在
	Disabled int // 该在、也在，但 enable=false
	Extra    int // 面板已不期望可用、节点上却还能连（enable=true）
	Drift    int // totalGB/expiryTime 对不上（只报告）
	Stale    int // 面板不期望、节点上还在但已禁用（只报告，无用户影响）
	// Incomplete 是「enable 读不到、没形成结论」的条数：这一轮不是完整结论，
	// 调用方不得据此推进指纹基线（否则下一轮会拿没核对完的结果短路掉对账）。
	Incomplete int
	Foreign    []string
}

// needBump 是「本轮要不要 bump config_version」的唯一判据。
//
// 只认会真实影响用户的三类：缺人、该开被关、不该能连却能连。
// Stale（已禁用的残留）与 Drift（字段不一致）不触发 —— 用户无感，重启内核的代价更大。
func (r bareCompareResult) needBump() bool {
	return r.Missing > 0 || r.Disabled > 0 || r.Extra > 0
}

// reconcileBareOnNode 处理单台裸内核节点：读 client_list → 比对 → 不一致就 bump 版本。
//
// 名单指纹短路（issue #115 3.5，对齐 Xboard 的 ETag 思路）：agent 每轮心跳带
// clients_fp（配置里 client 集合的 md5）。如果本轮指纹与「上轮成功对账时记下的指纹」
// 相同，就说明节点上的名单与上轮一字不差 —— 上轮已经比对过且没有触发重拉，本轮直接
// 判 Unchanged，不再发 client_list。
//
// 能短路的前提写死在 bareFingerprintShortcut 里（五条缺一不可）：
//   - 本轮心跳带了指纹（老 agent → 走现状，每轮全量读）；
//   - 上轮记下过指纹基线（首轮/面板重启/旧 agent 升级后第一轮 → 全量读）；
//   - 上轮对账有结论（成功且没有 enable 读不出来的条目）—— UNKNOWN / 失败 / 重拉 / 不完整的
//     轮次绝不推进基线；
//   - 指纹一字不差（sum + n），且基线之后节点没有被重拉（config_version 未变）；
//   - **面板侧**预期集合也没变（users_sum）—— 删号/停用/改套餐只动面板侧，
//     只比节点指纹会漏掉「面板已不该让他连、节点上还开着」这类漂移。
//
// 基线推进只发生在「全量读 + 比对成功」之后（短路轮指纹本来就相同，不写库）；
// 读回失败/截断/解析失败/重拉失败/确实重拉，都会把旧基线**删掉** —— 下一轮必须全量读，
// 不让一次失败被后续「指纹恰好相同」永久短路。
//
// 并发安全：不碰 ReconcileReport、不碰共享 map，全部写进返回值；日志走 c.logf_（自带锁）。
func (c *Collector) reconcileBareOnNode(ctx context.Context, node nodeRow, users []reconcileUser, dry bool) (nodeOutcome, error) {
	var out nodeOutcome
	out.Source = "client_list"

	// 1) 本轮心跳指纹（没有 = 老 agent / 配置读不到）、上轮基线与当前版本。
	//    面板侧预期集合也参与短路（见 bareFingerprintShortcut 条件 5）。
	current, baseline, nodeVersion := readBareNodeState(node)
	usersSum := expectedUsersSum(users)

	if bareFingerprintShortcut(current, nodeVersion, usersSum, baseline) {
		// 名单没变（节点 + 面板两侧都没变）：信任上轮结果。Probed / Unchanged 按用户数记
		// —— 报告里「有一致结论」不能因为短路而看起来像「没对账」。
		out.Probed = len(users)
		out.Unchanged = len(users)
		c.logf_("[reconcile] 裸内核名单指纹未变，跳过 client_list node=%s fp=%s n=%d",
			node.Name, current.sum, current.n)
		return out, nil
	}

	snap, err := c.bareClientSnapshot(ctx, node)
	if err != nil {
		// 读不到就整轮判 UNKNOWN（对齐 3x-ui 侧：绝不当成「节点上一个人都没有」）。
		// 同时作废旧基线：下一轮必须重新全量读，不能拿「上上轮的成功结论」短路掉。
		if !dry {
			c.invalidateBareClientsFPBaseline(ctx, node)
		}
		return out, err
	}

	cmp := compareBareClients(snap.entries, users)
	out.Probed = cmp.Probed
	out.BareMissing = cmp.Missing
	out.BareDisabled = cmp.Disabled
	out.BareExtra = cmp.Extra
	out.BareDrift = cmp.Drift
	out.BareStale = cmp.Stale
	out.BareMismatch = cmp.Missing + cmp.Disabled + cmp.Extra
	out.SampleKeys = snap.sampleKeys
	for _, email := range snap.foreign {
		out.Foreign++
		if len(out.ForeignNames) < 10 {
			out.ForeignNames = append(out.ForeignNames, email)
		}
	}

	if !cmp.needBump() {
		// 读回成功且没有可修的差异：这是唯一「可以信任本轮快照」的结论。
		// 有 enable 读不出来的 client（cmp.Incomplete>0）说明这一轮不是完整结论，
		// 不能据此推进基线（否则下一轮会拿「没核对完」的结果短路掉对账）。
		if !dry && current.ok() && cmp.Incomplete == 0 {
			if err := c.saveBareClientsFPBaseline(ctx, node.ID, current, nodeVersion, usersSum); err != nil {
				// 基线写失败只是下轮多重读一次，不影响本轮结论。
				c.logf_("[reconcile] 名单指纹基线写入失败 node=%s: %v", node.Name, err)
			}
		}
		return out, nil
	}
	if dry {
		// dry 只报告：把「本来会 bump」记在 Mismatch 里，不写库。
		return out, nil
	}

	version, err := c.bumpConfigVersion(ctx, node.ID)
	if err != nil {
		c.logf_("[reconcile] 裸内核重拉失败 node=%s: %v", node.Name, err)
		// 修不了 = 这一轮不算修好：既不算 Probed 也不算 Unchanged，交下一轮。
		out.Unknown += cmp.Missing + cmp.Disabled + cmp.Extra
		// 本轮没有结论 → 旧基线作废，下一轮必须全量读（否则指纹若恰好没变，
		// 会把这次「修失败」永久短路掉）。
		c.invalidateBareClientsFPBaseline(ctx, node)
		return out, nil
	}
	out.BareBumped = 1
	// 本轮触发了重拉：节点会在下一次拉取后变成新名单，旧基线的结论不能再用。
	c.invalidateBareClientsFPBaseline(ctx, node)
	c.logf_("[reconcile] 裸内核不一致已触发重拉 node=%s version=%d 缺人=%d 该开被关=%d 不该连却在=%d",
		node.Name, version, cmp.Missing, cmp.Disabled, cmp.Extra)
	return out, nil
}

// ── 名单指纹短路（issue #115 3.5） ───────────────────────────────

// bareClientsFP 是名单指纹（心跳上报的 clients_fp / 基线里记的同一份）。
type bareClientsFP struct {
	n   int
	sum string
}

// ok 表示「这是一份可用的指纹」（sum 非空 = 确实报了一份名字集合）。
func (f bareClientsFP) ok() bool { return strings.TrimSpace(f.sum) != "" }

// bareFPBaseline 是上轮「成功对账」时记下的快照（driver_config.bare_fp）。
type bareFPBaseline struct {
	fp bareClientsFP
	// configVersion 是那一轮对账时节点的 config_version：基线之后 bump 过
	// （管理员改入站/出站、订阅刷新等）就说明「名单可能变了，但节点还没重拉完」，
	// 这时候指纹相同也不能短路 —— 那是「改了但没生效」，不是「没变」。
	configVersion int64
	// usersSum 是上轮对账时**面板侧**预期集合（users 表）的摘要。
	//
	// 只比节点指纹还不够：删号/停用/改套餐只改面板侧，节点配置可能还没被下发
	// （或由 rmu 在运行时移除、config.json 不变）。只拿节点指纹短路会把这批
	// 「面板已经不该让他连、节点上还开着」的漂移永久跳过 —— 这是本功能最危险的
	// 坏形态（用户能白连）。所以面板侧预期一变，基线即失效，本轮必须全量读。
	usersSum string
	// executedAt 为空 = 那一轮没有结论（UNKNOWN / 失败 / 只报告），基线无效。
	executedAt string
}

// readBareNodeState 从节点行读出（本轮心跳指纹, 上轮基线, 当前 config_version）。
//
// 只解析一次 driver_config（比分开读三遍省事，也保证三个值来自同一份快照）。
// 缺键 / null / 形状不对一律按「没有」处理 —— 与旧 agent 同一条路径：
// 照旧每轮发 client_list，绝不拿坏数据短路。
func readBareNodeState(node nodeRow) (bareClientsFP, bareFPBaseline, int64) {
	cfg, ok := bareDriverConfigMap(node.DriverConfig)
	if !ok {
		return bareClientsFP{}, bareFPBaseline{}, 0
	}
	version, _ := asInt64(cfg["config_version"])

	var current bareClientsFP
	if agent, isObj := cfg["agent"].(map[string]any); isObj {
		if fp, isObj := agent["clients_fp"].(map[string]any); isObj {
			n64, _ := asInt64(fp["n"])
			current = bareClientsFP{n: int(n64), sum: strings.TrimSpace(stringOf(fp["sum"]))}
		}
	}

	var baseline bareFPBaseline
	if raw, isObj := cfg["bare_fp"].(map[string]any); isObj {
		n64, _ := asInt64(raw["n"])
		v64, _ := asInt64(raw["config_version"])
		baseline = bareFPBaseline{
			fp:            bareClientsFP{n: int(n64), sum: strings.TrimSpace(stringOf(raw["sum"]))},
			configVersion: v64,
			usersSum:      strings.TrimSpace(stringOf(raw["users_sum"])),
			executedAt:    strings.TrimSpace(stringOf(raw["executed_at"])),
		}
	}
	return current, baseline, version
}

// expectedUsersSum 是面板侧「重拉配置后节点应该长什么样」的摘要（纯函数）。
//
// 只覆盖 compareBareClients 判据用到的字段：email（由 ID 推出）、enable、totalGB、expiry，
// 以及「是否在渲染集合里」（renderEligible）。排序后拼接再取 md5，与 users 的查询顺序无关。
//
// 为什么必须参与短路：删号/停用/改套餐只动面板侧。只比节点指纹的话，这批差异
// 会一直「指纹没变」而被跳过 —— 那是「面板已不该让他连、节点上还开着」的白连漏洞。
func expectedUsersSum(users []reconcileUser) string {
	lines := make([]string, 0, len(users))
	for _, u := range users {
		eligible := "0"
		if u.renderEligible() {
			eligible = "1"
		}
		lines = append(lines, strings.Join([]string{
			clientEmail(u.ID),
			eligible,
			strconv.FormatInt(u.Limit, 10),
			strconv.FormatInt(u.ExpiryMs, 10),
		}, "|"))
	}
	sort.Strings(lines)

	h := md5.New()
	for _, line := range lines {
		_, _ = h.Write([]byte(line))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// bareDriverConfigMap 把 driver_config 原始 JSON 解成 map（UseNumber，保大整数原文）。
func bareDriverConfigMap(raw sql.NullString) (map[string]any, bool) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(raw.String))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, false
	}
	obj, ok := parsed.(map[string]any)
	return obj, ok
}

// bareFingerprintShortcut 判定本轮能否跳过 client_list（纯函数，便于穷举边界）。
//
// 返回 true 才能跳过。五条硬条件，缺一不可（缺任何一条都可能「漏掉节点漂移」）：
//  1. 本轮心跳带了指纹（老 agent 不带 → 不可短路）；
//  2. 上轮记过基线，且那一轮对账**有结论**（executedAt 非空）；
//  3. 指纹一字不差：sum 相同、n 相同（n 是去重后的 client 数，与 sum 互补校验）；
//  4. 基线以来节点没有被重拉（config_version 未变）—— 否则是「改过还没生效」；
//  5. 面板侧预期集合也没变（usersSum 相同）—— 否则是「删号/停用/改套餐，节点还没跟上」。
//
// 第 5 条是本设计里最容易被忽略的一条：节点指纹只描述节点，不描述面板意图。
// usersSum 为空（旧版本写的基线）一律不短路：宁可多读一轮，不能拿不完整基线放行。
func bareFingerprintShortcut(current bareClientsFP, currentVersion int64, currentUsersSum string, baseline bareFPBaseline) bool {
	if !current.ok() || !baseline.fp.ok() {
		return false
	}
	if baseline.executedAt == "" || baseline.usersSum == "" {
		return false
	}
	if current.sum != baseline.fp.sum || current.n != baseline.fp.n {
		return false
	}
	if currentVersion != baseline.configVersion {
		return false
	}
	return currentUsersSum == baseline.usersSum
}

// invalidateBareClientsFPBaseline 删除指纹基线（driver_config.bare_fp）。
//
// 用在「本轮没有得出结论」的所有路径上：读回失败/截断、重拉失败、确实触发了重拉。
// 不删的话，下一轮若指纹恰好与旧基线相同，就会把这一轮的失败永久短路掉。
//
// 失败只打日志：基线残留的最坏后果是下一轮多重读一次（短路条件里还有
// config_version / executed_at / users_sum 三道闸），绝不至于漏掉漂移。
func (c *Collector) invalidateBareClientsFPBaseline(ctx context.Context, node nodeRow) {
	if _, ok := jsonObjectField([]byte(node.DriverConfig.String), "bare_fp"); !ok {
		return // 本来就没有，不必开事务。
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		c.logf_("[reconcile] 清理指纹基线失败 node=%s: %v", node.Name, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT driver_config FROM nodes WHERE id = ? FOR UPDATE", node.ID).Scan(&raw); err != nil {
		c.logf_("[reconcile] 清理指纹基线失败 node=%s: %v", node.Name, err)
		return
	}
	cfg, ok := bareDriverConfigMap(raw)
	if !ok {
		return // 库里的 JSON 已损坏/为空：没有基线可删。
	}
	if _, exists := cfg["bare_fp"]; !exists {
		return
	}
	delete(cfg, "bare_fp")

	body, err := json.Marshal(cfg)
	if err != nil {
		c.logf_("[reconcile] 清理指纹基线失败 node=%s: %v", node.Name, err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE nodes SET driver_config = ?, updated_at = ? WHERE id = ?",
		string(body), dbDateTime(c.now()), node.ID); err != nil {
		c.logf_("[reconcile] 清理指纹基线失败 node=%s: %v", node.Name, err)
		return
	}
	if err := tx.Commit(); err != nil {
		c.logf_("[reconcile] 清理指纹基线失败 node=%s: %v", node.Name, err)
	}
}

// saveBareClientsFPBaseline 把「本轮全量读 + 比对成功」的结论写成指纹基线。
//
// config_version 取**本轮读回时看到的那个版本**（调用方从同一次查询的快照传进来），
// 不是写库当下重读的值：对账与写基线之间若发生并发 bump，说明「名单可能已变、节点
// 还没重拉」，这时候把新版本记进基线等于宣布「新配置已生效」—— 下一轮指纹若恰好
// 相同（例如 bump 的是出站而不是用户），就会拿一份没核对过的结论短路掉对账。
// 记旧版本则下一轮看到版本不一致 → 全量读，正确且只多读一轮。
//
// 与 bumpConfigVersion 同一把行锁（SELECT ... FOR UPDATE）：并发 bump 的
// config_version 绝不能被这里覆盖回旧值（这里根本不改它）。
func (c *Collector) saveBareClientsFPBaseline(ctx context.Context, nodeID int64, fp bareClientsFP, configVersion int64, usersSum string) error {
	if !fp.ok() || usersSum == "" {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT driver_config FROM nodes WHERE id = ? FOR UPDATE", nodeID).Scan(&raw); err != nil {
		return err
	}
	cfg, ok := bareDriverConfigMap(raw)
	if !ok {
		cfg = map[string]any{}
	}

	cfg["bare_fp"] = map[string]any{
		"n":              fp.n,
		"sum":            fp.sum,
		"config_version": configVersion,
		"users_sum":      usersSum,
		"executed_at":    c.now().UTC().Format("2006-01-02T15:04:05+00:00"),
	}

	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE nodes SET driver_config = ?, updated_at = ? WHERE id = ?",
		string(body), dbDateTime(c.now()), nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

// compareBareClients 把节点读回的 client 与面板意图逐条比对（纯函数，无 IO）。
//
// 判据见文件头：基准是「重拉一次配置后节点应该长什么样」（renderEligible）。
func compareBareClients(entries map[string]nodeSnapshotEntry, users []reconcileUser) bareCompareResult {
	var res bareCompareResult
	known := make(map[string]bool, len(users))
	for _, u := range users {
		email := clientEmail(u.ID)
		known[email] = true
		res.Probed++

		entry, exists := entries[email]
		if !exists {
			// 渲染集合里的人在节点上必须存在（裸内核没有「有这个 client 但没挂入站」的中间态）。
			if u.renderEligible() {
				res.Missing++
			}
			continue
		}
		if entry.Incomplete {
			// enable 读不到：不据此判节点有问题（宁可漏报，也不能误判成不一致去重启内核）。
			res.Probed--
			res.Incomplete++
			continue
		}
		wantPresent := u.renderEligible()
		switch {
		case wantPresent && !entry.Enable:
			res.Disabled++
		case wantPresent:
			// 成员与开关都对：只剩字段漂移（只报告）。
			if entry.TotalGB != u.Limit || entry.ExpiryMs != u.ExpiryMs {
				res.Drift++
			}
		case entry.Enable:
			// 面板已经不该让他连了，节点上还开着 —— 这是唯一有安全含义的方向。
			res.Extra++
		default:
			res.Stale++
		}
	}
	// 节点上挂着、但 users 表里已经没有的 ch_user_*（删号残留）：渲染集合里永远不包含它们，
	// 所以「重拉一次」必然把它们拿掉 —— 能连（enable=true）的算 Extra（真的能白连），
	// 已经禁用的算 Stale（只报告）。不认识的 email（中转凭据等）已在解析时归到 foreign。
	for email, entry := range entries {
		if known[email] || entry.Incomplete {
			continue
		}
		if entry.Enable {
			res.Extra++
		} else {
			res.Stale++
		}
	}
	return res
}

// renderEligible 复刻 nodeapi.eligibleUsers() 的渲染判据：
// 账号启用 + 有套餐 + 没打流量禁用标记 → 这个人会被渲染进节点配置。
//
// ⚠️ 与 Intent 的区别（别合并）：Intent 还要求「未过期/未超量」，那是封禁扫描器的
// 判据；渲染判据只看标记。拿 Intent 当比对基准会每轮都判「过期用户不该在节点上」，
// 而重拉配置并不会把他们拿掉（标记没打之前他们仍在渲染集合里）→ 每轮 bump → 内核每 5
// 分钟重启一次。
func (u reconcileUser) renderEligible() bool {
	return u.AccountEnabled && u.PlanIDValid && !u.FlagSet
}

// ── 读：client_list ───────────────────────────────────────────────

// bareClientSnapshot 通过指令通道读回节点上的全量 client。
func (c *Collector) bareClientSnapshot(ctx context.Context, node nodeRow) (bareClientSnapshot, error) {
	var out bareClientSnapshot

	// 上一轮的命令还没终结（agent 没认领：老版本不认这个 action，或者节点掉线）：
	// 本轮不再入队。否则每 5 分钟堆一条 pending，节点回来时一次涌上来一堆同样的读请求。
	if inflight, err := c.bareClientListInFlight(ctx, node.ID); err == nil && inflight {
		return out, fmt.Errorf("上一轮 client_list 仍未回执（agent 未认领，可能版本过旧或节点掉线）")
	}

	id, err := c.enqueueNodeCommand(ctx, node.ID, nodeapi.ActionClientList, nil, nil)
	if err != nil {
		return out, fmt.Errorf("入队 client_list 失败: %w", err)
	}
	status, result, err := c.waitNodeCommand(ctx, id, c.bareReconcileWait())
	if err != nil {
		return out, fmt.Errorf("读取 client_list 回执失败: %w", err)
	}
	switch status {
	case nodeapi.StatusSuccess:
	case nodeapi.StatusFailed:
		// agent 明说失败（老版本不认这个 action 时也是这条）→ 本轮不动。
		return out, fmt.Errorf("节点执行 client_list 失败: %s", truncateForLog(result, 200))
	default:
		return out, fmt.Errorf("client_list 未在 %d 秒内回执（节点可能离线）", c.bareReconcileWaitSeconds())
	}

	parsed, ok := parseBareClientList(result)
	if !ok {
		return out, fmt.Errorf("client_list 回执解析失败: %s", truncateForLog(result, 200))
	}
	if parsed.truncated {
		// agent 超过 39000 字符会把回执截短并标 truncated（面板 ack 校验上限 40000，
		// 超了整条回执会被拒）。半份数据绝不能拿来对账 —— 会把没回的人当「节点上缺人」。
		return out, fmt.Errorf("client_list 回执被截断，本轮按 UNKNOWN 处理: %s", parsed.reason)
	}
	out.entries = parsed.entries
	out.foreign = parsed.foreign
	out.sampleKeys = parsed.sampleKeys
	return out, nil
}

// bareClientParse 是 client_list 回执的解析结果。
type bareClientParse struct {
	entries    map[string]nodeSnapshotEntry
	foreign    []string
	sampleKeys []string
	// truncated = agent 明确说「这份回执不全」（超字符上限只回了前若干个 client）。
	// 调用方必须按 UNKNOWN 处理：拿半份数据当全量会把没回的人误判成「节点上缺人」。
	truncated bool
	reason    string
}

// parseBareClientList 解析 client_list 回执，新旧两种形状都认：
//
//	新（agent 3.1+，扁平化 issue #115）：
//	  {"clients":[{"email":"ch_user_12","enable":true,"totalGB":0,"expiryTime":0,
//	               "inbounds":["vless-443","hy2"]}],"total":103,"truncated":true,"reason":"..."}
//	旧（agent ≤3.0）：
//	  {"inbounds":[{"inbound_id":0,"tag":"vless-443","port":443,"protocol":"vless",
//	                "clients":[{"email":"ch_user_12","enable":true,"totalGB":0,"expiryTime":0}]}],
//	   "truncated":true,"reason":"..."}   ← truncated 可缺省，出现就本轮不动
//
// 分流看根级键：有 clients 走新解析（节点侧已按 email 去重，一层数组直接落表），
// 有 inbounds 走旧解析（跨入站按 email 合并）。clients 里的 inbounds 数组只是节点侧
// 的诊断信息，比对只认 email，这里忽略。
//
// 用 map + UseNumber 而不是结构体：agent 各版本数值字段可能给 json.Number 或字符串，
// 结构体一遇类型不符整条回执就废了（整轮 UNKNOWN）。取值复用 3x-ui 侧同一套
// truthyField / firstIntField，口径一致；expiryTime 额外认 ISO8601（agent 原样透传内核
// 配置里的字符串形态，不归一）。
//
// second 返回值 false = 回执形态不符（判 UNKNOWN）。
func parseBareClientList(raw string) (bareClientParse, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return bareClientParse{}, false
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return bareClientParse{}, false
	}
	truncated, _ := truthyField(obj, "truncated")

	out := bareClientParse{
		entries:   map[string]nodeSnapshotEntry{},
		truncated: truncated,
		reason:    stringOf(obj["reason"]),
	}
	seenForeign := map[string]bool{}

	// 新形状：根级 clients 数组。
	if _, present := obj["clients"]; present {
		clients, ok := obj["clients"].([]any)
		if !ok {
			// clients:null 与空数组一样是「节点上一个人都没有」的合法空快照；
			// 其它类型才是回执形态不符。
			if obj["clients"] == nil {
				return out, true
			}
			return bareClientParse{}, false
		}
		for _, c := range clients {
			if co, ok := c.(map[string]any); ok {
				mergeBareClient(co, &out, seenForeign)
			}
		}
		return out, true
	}

	inbounds, ok := obj["inbounds"].([]any)
	if !ok {
		// 空数组是合法回执（节点上一个入站都没有），但要与「结构不对」区分开：
		// 真没有 inbounds 键 = 回执形态不符，判解析失败。
		if _, present := obj["inbounds"]; present && obj["inbounds"] == nil {
			return out, true
		}
		return bareClientParse{}, false
	}

	for _, item := range inbounds {
		ib, ok := item.(map[string]any)
		if !ok {
			continue
		}
		clients, _ := ib["clients"].([]any)
		for _, c := range clients {
			if co, ok := c.(map[string]any); ok {
				mergeBareClient(co, &out, seenForeign)
			}
		}
	}
	return out, true
}

// mergeBareClient 把一条 client 落进 out.entries。
//
// 同一 email 出现在多条里时合并（旧形状一个人本来就挂在每个入站上；新形状理论上已去重，
// 这里照样兜住重复）：
//   - 任一处 enable=false 就算「被关」（用户会以为某个端口能用、实际连不上）；
//   - totalGB / expiryTime 取第一次出现的值（与新 agent 侧的去重口径一致）。
func mergeBareClient(co map[string]any, out *bareClientParse, seenForeign map[string]bool) {
	email := strings.TrimSpace(stringOf(co["email"]))
	if email == "" {
		return
	}
	enable, hasEnable := truthyField(co, "enable")
	if _, ours := parseClientEmail(email); !ours {
		// 非本站 client（中转凭据 / 手工加的用户）：只报告，绝不据此 bump。
		if !seenForeign[email] {
			seenForeign[email] = true
			out.foreign = append(out.foreign, email)
		}
		return
	}
	entry := nodeSnapshotEntry{Enable: enable, Incomplete: !hasEnable}
	if hasEnable {
		if total, ok := firstIntField(co, "totalGB", "total_gb", "total"); ok {
			entry.TotalGB = total
		}
		entry.ExpiryMs = bareExpiryMs(co["expiryTime"])
	} else if len(out.sampleKeys) == 0 {
		out.sampleKeys = objKeyNames(co)
	}
	if prev, exists := out.entries[email]; exists {
		entry.Enable = prev.Enable && entry.Enable
		entry.Incomplete = prev.Incomplete || entry.Incomplete
		entry.TotalGB = prev.TotalGB
		entry.ExpiryMs = prev.ExpiryMs
	}
	out.entries[email] = entry
}

// bareExpiryMs 把回执里的 expiryTime 归一成毫秒：数字（内核/3x-ui 口径）或 ISO8601 字符串。
//
// agent 是「原样透传」内核配置里的 expiryTime（节点侧刻意不归一），所以两种形态都会出现。
// 读不出来按 0 = 不过期（与 3x-ui 侧同口径）；这一项只影响「只报告」的字段漂移计数，
// 不会触发重拉，所以宁可保守取 0。
func bareExpiryMs(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.UnixMilli()
		}
		return 0
	}
	ms, _ := asInt64(v)
	// 秒级时间戳（10 位）与毫秒级（13 位）都出现过：小于 1e12 的按秒解释。
	if ms > 0 && ms < 1_000_000_000_000 {
		ms *= 1000
	}
	return ms
}

// ── 指令通道（node_api_commands） ─────────────────────────────────

// enqueueNodeCommand 入队一条节点指令并返回指令 id。
//
// 与 xray 驱动的 enqueueAdd/enqueueRemove 走同一张表、同一条 WS/长轮询通道 ——
// 不另开队列（协议上 agent 只认这一种下发方式）。
func (c *Collector) enqueueNodeCommand(ctx context.Context, nodeID int64, action string, payload []byte, userID *int64) (int64, error) {
	if len(payload) == 0 {
		payload = []byte("null")
	}
	now := dbDateTime(c.now())
	res, err := c.db.ExecContext(ctx,
		"INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 'pending', ?, ?)",
		nodeID, userID, action, string(payload), now, now)
	if err != nil {
		return 0, fmt.Errorf("写入节点指令失败: %w", err)
	}
	return res.LastInsertId()
}

// bareInFlightWindow 是「在途」的判定窗口：超过这个时间还没终结的 client_list 一律不再等。
//
// 没有这个窗口，一条卡死 acknowledged 的指令（agent 认领后没写回执）会把这台节点的
// 对账永久钉死在「上一轮未回执」，谁都发现不了。代价是多一条同样的读指令（每窗口最多一次）。
const bareInFlightWindow = 10 * time.Minute

// bareClientListInFlight 判断该节点是否还有「最近窗口内、未终结」的 client_list。
//
// 「未认领」与「认领了没回」都算在途：两种情况都不该再压一条同样的读请求上去。
func (c *Collector) bareClientListInFlight(ctx context.Context, nodeID int64) (bool, error) {
	var n int
	err := c.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM node_api_commands WHERE node_id = ? AND action = ? "+
			"AND status IN ('pending','acknowledged') AND created_at >= ?",
		nodeID, nodeapi.ActionClientList, dbDateTime(c.now().Add(-bareInFlightWindow))).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// waitNodeCommand 轮询一条指令直到终态，返回 (status, result)。
//
// 超时返回 ("pending", "") —— 调用方按 UNKNOWN 处理（绝不当成「节点上什么都没有」）。
func (c *Collector) waitNodeCommand(ctx context.Context, id int64, wait time.Duration) (string, string, error) {
	deadline := time.Now().Add(wait)
	for {
		var (
			status string
			result sql.NullString
		)
		err := c.db.QueryRowContext(ctx,
			"SELECT status, result FROM node_api_commands WHERE id = ?", id).Scan(&status, &result)
		if err != nil {
			return "", "", err
		}
		switch status {
		case nodeapi.StatusSuccess, nodeapi.StatusFailed:
			return status, result.String, nil
		}
		if time.Now().After(deadline) {
			return status, result.String, nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(barePollInterval):
		}
	}
}

// bareReconcileWaitSeconds 是等回执的秒数（RECONCILE_BARE_WAIT_SECONDS 可覆盖）。
func (c *Collector) bareReconcileWaitSeconds() int {
	if n := envInt("RECONCILE_BARE_WAIT_SECONDS", bareReconcileWaitDefault); n > 0 {
		return n
	}
	return bareReconcileWaitDefault
}

func (c *Collector) bareReconcileWait() time.Duration {
	return time.Duration(c.bareReconcileWaitSeconds()) * time.Second
}

// bumpConfigVersion 行锁内递增 nodes.driver_config.config_version，并写 config_changed_at。
//
// 与 admin 侧 NodeStore.BumpConfigVersion 同一套语义（节点页按 config_changed_at 显示
// 「配置滞后 N 秒」）。差异只在键序：这里用 encoding/json + UseNumber 就地改一个键，
// 不重排、不丢精度；键序对读取方没有语义（两侧都是按 key 解析），不必引入保序编码器。
func (c *Collector) bumpConfigVersion(ctx context.Context, nodeID int64) (int64, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT driver_config FROM nodes WHERE id = ? FOR UPDATE", nodeID).Scan(&raw); err != nil {
		return 0, err
	}

	cfg := map[string]any{}
	if raw.Valid && strings.TrimSpace(raw.String) != "" {
		dec := json.NewDecoder(strings.NewReader(raw.String))
		dec.UseNumber()
		var parsed any
		if err := dec.Decode(&parsed); err == nil {
			if obj, ok := parsed.(map[string]any); ok {
				cfg = obj
			}
		}
	}

	version := int64(1)
	if cur, ok := asInt64(cfg["config_version"]); ok {
		version = cur + 1
	}
	cfg["config_version"] = version
	cfg["config_changed_at"] = c.now().UTC().Format("2006-01-02T15:04:05+00:00")

	body, err := json.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE nodes SET driver_config = ?, updated_at = ? WHERE id = ?",
		string(body), dbDateTime(c.now()), nodeID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return version, nil
}

// truncateForLog 截断日志里的回执片段（runes，避免把半个 UTF-8 也打出来）。
func truncateForLog(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= limit {
		return string(r)
	}
	return string(r[:limit]) + "…"
}
