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
// 已知限制：client 极多（约 500+）的节点，回执必然被 agent 截断，于是这台节点的
// 裸内核对账永远按 UNKNOWN 跳过 —— **不会误删人，但也修不了人**。要真正覆盖这类
// 节点需要协议支持分页（按入站 tag 分批读），属于 agent 侧的改动。
//
// totalGB / expiryTime 对不上**只报告不 bump**：协议里的 totalGB 单位没写死（面板侧是字节，
// 3x-ui 侧也叫 totalGB），拿不准单位就 bump 会把内核每轮重启一次。成员集合与 enable
// 已经能覆盖「用户连不上/不该连却在连」这两类真实故障。
// （agent 侧目前恒报 enable=true —— 内核 client 没有 enable 字段，停用靠 totalGB/expiryTime
// 表达 —— 所以 Disabled 档在真机上基本不会触发，留着是给「带 enable 的配置形态」兜底。）

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	Foreign  []string
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
// 并发安全：不碰 ReconcileReport、不碰共享 map，全部写进返回值；日志走 c.logf_（自带锁）。
func (c *Collector) reconcileBareOnNode(ctx context.Context, node nodeRow, users []reconcileUser, dry bool) (nodeOutcome, error) {
	var out nodeOutcome
	out.Source = "client_list"

	snap, err := c.bareClientSnapshot(ctx, node)
	if err != nil {
		// 读不到就整轮判 UNKNOWN（对齐 3x-ui 侧：绝不当成「节点上一个人都没有」）。
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
		return out, nil
	}
	out.BareBumped = 1
	c.logf_("[reconcile] 裸内核不一致已触发重拉 node=%s version=%d 缺人=%d 该开被关=%d 不该连却在=%d",
		node.Name, version, cmp.Missing, cmp.Disabled, cmp.Extra)
	return out, nil
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

// parseBareClientList 解析 client_list 回执：
//
//	{"inbounds":[{"inbound_id":0,"tag":"vless-443","port":443,"protocol":"vless",
//	              "clients":[{"email":"ch_user_12","enable":true,"totalGB":0,"expiryTime":0}]}],
//	 "truncated":true,"reason":"..."}   ← truncated 可缺省，出现就本轮不动
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

	inbounds, ok := obj["inbounds"].([]any)
	if !ok {
		// 空数组是合法回执（节点上一个入站都没有），但要与「结构不对」区分开：
		// 真没有 inbounds 键 = 回执形态不符，判解析失败。
		if _, present := obj["inbounds"]; present && obj["inbounds"] == nil {
			return bareClientParse{entries: map[string]nodeSnapshotEntry{}, truncated: truncated,
				reason: stringOf(obj["reason"])}, true
		}
		return bareClientParse{}, false
	}

	out := bareClientParse{
		entries:   map[string]nodeSnapshotEntry{},
		truncated: truncated,
		reason:    stringOf(obj["reason"]),
	}
	seenForeign := map[string]bool{}
	for _, item := range inbounds {
		ib, ok := item.(map[string]any)
		if !ok {
			continue
		}
		clients, _ := ib["clients"].([]any)
		for _, c := range clients {
			co, ok := c.(map[string]any)
			if !ok {
				continue
			}
			email := strings.TrimSpace(stringOf(co["email"]))
			if email == "" {
				continue
			}
			enable, hasEnable := truthyField(co, "enable")
			if _, ours := parseClientEmail(email); !ours {
				// 非本站 client（中转凭据 / 手工加的用户）：只报告，绝不据此 bump。
				if !seenForeign[email] {
					seenForeign[email] = true
					out.foreign = append(out.foreign, email)
				}
				continue
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
			// 同一个 email 挂在多个入站：任一处 enable=false 就算「被关」
			// （用户会以为某个端口能用、实际连不上）。
			if prev, exists := out.entries[email]; exists {
				entry.Enable = prev.Enable && entry.Enable
				entry.Incomplete = prev.Incomplete || entry.Incomplete
				entry.TotalGB = prev.TotalGB
				entry.ExpiryMs = prev.ExpiryMs
			}
			out.entries[email] = entry
		}
	}
	return out, true
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
