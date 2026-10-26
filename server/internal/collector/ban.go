package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"controlhub-server/internal/driver/threexui"
	"controlhub-server/internal/driver/xray"
)

// 封禁扫描：逐行对齐 BanService（M8 + 套餐适配）。
//
// 产品铁律（behavior-contract §4）：
//   - **永不封号**：users.enabled 永为 true，用户随时能登录续费；
//   - 到期 / 超量只关 3x-ui 流量（client enable=false，xray 节点投 rmu）；
//   - 「无套餐」不在这里处理（既不关流量也不发通知）。

// banReasonDisabled 是 banReason() 返回 'disable' 的 Go 形态（空串 = 无需关闭）。
const banReasonDisabled = "disable"

// banReasonAt 复刻 BanService::banReason()：返回 "disable"（需要关闭流量）或 ""（正常）。
//
// 判定顺序与 PHP 逐条一致，任一条命中即关：
//  1. 已过期（expired_at < now）；
//  2. 周期套餐当月超量（monthly_traffic_limit > 0 且已用 ≥ 限额）；
//  3. 总流量超量（traffic_limit > 0 且已用 ≥ 限额）。
//
// 永不返回「封号」：只有管理员手动封禁才写 enabled=false。
// now 显式传入（不取 time.Now），保证「同一轮扫描用同一个时钟」且测试可复现。
func banReasonAt(u userRow, now time.Time) string {
	if u.ExpiredAt.Valid && u.ExpiredAt.Time.Before(now) {
		return banReasonDisabled
	}
	if u.PlanID.Valid && u.MonthlyLimit > 0 && u.MonthlyTrafficUsed >= u.MonthlyLimit {
		return banReasonDisabled
	}
	if u.PlanID.Valid && u.TrafficLimit > 0 && u.TrafficUsed >= u.TrafficLimit {
		return banReasonDisabled
	}
	return ""
}

// recheckAfterHours 复刻 config('ban.recheck_after_hours')（默认 6；<=0 表示关闭该优化）。
func recheckAfterHours() int {
	return envInt("BAN_RECHECK_AFTER_HOURS", 6)
}

// isRecentlyDisabled 复刻 BanService::isRecentlyDisabled()：
// traffic_disabled_at 在时效内 → true，扫描器据此 0 次 HTTP 请求跳过。
func isRecentlyDisabled(u userRow, now time.Time) bool {
	if !u.TrafficDisabledAt.Valid {
		return false
	}
	hours := recheckAfterHours()
	if hours <= 0 {
		return false
	}
	return u.TrafficDisabledAt.Time.After(now.Add(-time.Duration(hours) * time.Hour))
}

// loadUser 读单个用户的封禁判定字段（不存在 → (nil, nil)）。
func (c *Collector) loadUser(ctx context.Context, userID int64) (*userRow, error) {
	var (
		u       userRow
		enabled int64
		ptype   sql.NullString
	)
	err := c.db.QueryRowContext(ctx,
		"SELECT u.id, u.enabled, u.traffic_disabled_at, u.expired_at, u.traffic_limit, u.traffic_used, "+
			"u.monthly_traffic_used, u.monthly_traffic_limit, u.plan_id, p.type "+
			"FROM users u LEFT JOIN plans p ON p.id = u.plan_id WHERE u.id = ?", userID).
		Scan(&u.ID, &enabled, &u.TrafficDisabledAt, &u.ExpiredAt, &u.TrafficLimit,
			&u.TrafficUsed, &u.MonthlyTrafficUsed, &u.MonthlyLimit, &u.PlanID, &ptype)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取用户 #%d 失败: %w", userID, err)
	}
	u.Enabled = enabled != 0
	u.Period = ptype.Valid && ptype.String == "period"
	return &u, nil
}

// enabledUsers 读全部启用用户（对齐 User::where('enabled', true)->with('plan')，按 id 升序）。
func (c *Collector) enabledUsers(ctx context.Context) ([]userRow, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT u.id, u.enabled, u.traffic_disabled_at, u.expired_at, u.traffic_limit, u.traffic_used, "+
			"u.monthly_traffic_used, u.monthly_traffic_limit, u.plan_id, p.type "+
			"FROM users u LEFT JOIN plans p ON p.id = u.plan_id WHERE u.enabled = 1 ORDER BY u.id")
	if err != nil {
		return nil, fmt.Errorf("查询启用用户失败: %w", err)
	}
	defer rows.Close()

	var out []userRow
	for rows.Next() {
		var (
			u       userRow
			enabled int64
			ptype   sql.NullString
		)
		if err := rows.Scan(&u.ID, &enabled, &u.TrafficDisabledAt, &u.ExpiredAt, &u.TrafficLimit,
			&u.TrafficUsed, &u.MonthlyTrafficUsed, &u.MonthlyLimit, &u.PlanID, &ptype); err != nil {
			return nil, fmt.Errorf("读取启用用户失败: %w", err)
		}
		u.Enabled = enabled != 0
		u.Period = ptype.Valid && ptype.String == "period"
		out = append(out, u)
	}
	return out, rows.Err()
}

// toggleOutcome 是 toggleClient 的统计结果。
type toggleOutcome struct {
	// Confirmed 是「确认已生效」的节点数。
	Confirmed int
	// Total 是本次操作的启用节点总数。
	Total int
	// Applied 表示全部节点都确认（关闭 → 写 traffic_disabled_at；开启 → 清空标记）。
	Applied bool
}

// ToggleClient 是 toggleClient 的导出入口（对账命令 users:reconcile-enable 复用，
// 避免 task 层再写一份只会处理 3x-ui 的残缺实现）。
func (c *Collector) ToggleClient(ctx context.Context, userID int64, enable bool) (bool, error) {
	out, err := c.toggleClient(ctx, userID, enable)
	return out.Applied, err
}

// toggleClient 复刻 BanService::toggleClient()：在所有 enabled 节点上把 client 的 enable 改成 enable。
//
// 判定口径（与 PHP 逐条一致）：
//   - 关闭：要求**全部**启用节点确认才写 traffic_disabled_at —— 少关一个节点用户就能从那里继续跑；
//   - 开启：同样要求全部确认才清标记（否则「面板显示已启用、节点上仍被关」）；
//   - client 在本节点不存在 / 本节点没有该协议入站 → 视为「无需操作」= 已确认（否则该用户
//     永远差一台确认不了，扫描器每轮白跑）。
//
// 与 PHP 的差异（有意）：PHP 对 Bearer 节点批内并发、cookie/xray 串行；Go 侧统一个个来
// —— 扫描器的成本主要在 3x-ui 的 get+update 往返，P9 接入调度后再按节点池调并发
// （届时复用同一套「全部确认」判定）。语义完全一致，只是本阶段不抢 P9 的工作。
func (c *Collector) toggleClient(ctx context.Context, userID int64, enable bool) (toggleOutcome, error) {
	var out toggleOutcome

	var (
		email    string
		protocol string
	)
	err := c.db.QueryRowContext(ctx, "SELECT protocol FROM users WHERE id = ?", userID).Scan(&protocol)
	if err == sql.ErrNoRows {
		return out, fmt.Errorf("用户 #%d 不存在", userID)
	}
	if err != nil {
		return out, fmt.Errorf("读取用户 #%d 失败: %w", userID, err)
	}
	email = clientEmail(userID)

	nodes, err := c.enabledNodes(ctx)
	if err != nil {
		return out, err
	}
	out.Total = len(nodes)
	if len(nodes) == 0 {
		return out, nil // 无节点可操作：不写标记，下一轮继续尝试
	}

	for _, node := range nodes {
		ok, err := c.applyEnableOnNode(ctx, node, protocol, email, enable)
		if err != nil {
			// 单节点异常已在内部归一成「未确认」，这里的 err 只用于记录。
			c.logf_("切换 client 开关失败（node=%d user=%d enable=%v）: %v", node.ID, userID, enable, err)
			continue
		}
		if ok {
			out.Confirmed++
		}
	}

	out.Applied = out.Confirmed == out.Total
	if !out.Applied {
		// 部分（甚至全部）节点未确认：不写标记，让扫描器下一轮自动补关。
		c.logf_("toggleClient(%v) 未全部节点确认，下一轮重试（user=%d confirmed=%d total=%d）",
			enable, userID, out.Confirmed, out.Total)
		return out, nil
	}

	if enable {
		if err := c.forgetDisabledState(ctx, userID); err != nil {
			return out, err
		}
		return out, nil
	}

	// 关闭确认：写 traffic_disabled_at（+ 通知由 P10 的邮件通道负责，本层不做）。
	if _, err := c.db.ExecContext(ctx,
		"UPDATE users SET traffic_disabled_at = ?, updated_at = ? WHERE id = ?",
		dbDateTime(c.now()), dbDateTime(c.now()), userID); err != nil {
		return out, fmt.Errorf("写入 traffic_disabled_at 失败: %w", err)
	}
	return out, nil
}

// forgetDisabledState 复刻 BanService::forgetDisabledState()。
func (c *Collector) forgetDisabledState(ctx context.Context, userID int64) error {
	_, err := c.db.ExecContext(ctx,
		"UPDATE users SET traffic_disabled_at = NULL, updated_at = ? WHERE id = ? AND traffic_disabled_at IS NOT NULL",
		dbDateTime(c.now()), userID)
	if err != nil {
		return fmt.Errorf("清除 traffic_disabled_at 失败: %w", err)
	}
	return nil
}

// applyEnableOnNode 复刻 applyEnableOnNode()（同步版）：
// 先 getClient 拿完整对象（3x-ui 的 update 是整条替换语义），再逐入站 update + 兜底 update。
//
// 返回值：该节点是否【确认】已生效。
//   - 3x-ui：getClient 取到 → 逐入站 update + 兜底 update 全成功才算确认；
//     client 不存在 → 已确认（本节点没有东西可关）；传输层失败 → 未确认（必须重试，
//     绝不能把网络故障当「关好了」）。
//   - xray：指令通道（rmu/adu）入队即算确认 —— 与 PHP 的 enqueueRemove/enqueueAdd 同口径
//     （入队失败才是未确认）。
func (c *Collector) applyEnableOnNode(ctx context.Context, node nodeRow, protocol, email string, enable bool) (bool, error) {
	if node.isXray() {
		driver := c.xrayDriver(node)
		if err := driver.ToggleClient(ctx, email, enable); err != nil {
			return false, err
		}
		return true, nil
	}
	if node.DriverType != "" && node.DriverType != "3x-ui" {
		return false, errUnsupportedDriver(node.DriverType)
	}

	client, err := c.threeXUiClient(node)
	if err != nil {
		return false, err
	}

	inboundIDs, err := c.inboundIDsFor(ctx, node.ID, protocol)
	if err != nil {
		return false, err
	}

	resp, err := client.GetClient(ctx, email)
	if err != nil {
		var terr *threexui.TransportError
		if errors.As(err, &terr) {
			return false, err // 网络/鉴权失败：未确认，交给下一轮
		}
		return len(inboundIDs) == 0, err
	}
	if resp == nil {
		// client 不存在或请求失败（requestNullable 折叠了两者）：
		// 本节点没有该协议的入站 → 无需操作；否则保守按未确认（下一轮重试）。
		return len(inboundIDs) == 0, nil
	}

	clientData := normalizeForUpdate(resp)
	clientData["enable"] = enable

	ok := true
	for _, inboundID := range inboundIDs {
		id := inboundID
		if err := client.UpdateClient(ctx, email, clientData, &id); err != nil {
			ok = false
		}
	}
	// 兜底不带 inboundId 再更新一次（与 PHP 顺序一致）。
	if err := client.UpdateClient(ctx, email, clientData, nil); err != nil {
		ok = false
	}
	return ok, nil
}

// normalizeForUpdate 复刻 BanService::normalizeForUpdate()：整条替换前的类型规范化。
//
//   - id：3x-ui v3.7.0 返回的 id 是数据库自增行号，uuid 才是 Xray client id。
//     全量替换时把行号写回 id 会把 uuid 覆盖成数字、导致用户断连 → 优先用 uuid；
//     旧版（id 即 uuid、无 uuid 键）回退仅做 string 强转；
//   - allowedIPs / tunnelAllowedIPs：Go 侧按 []string 反序列化，空串会报
//     "cannot unmarshal string into Go struct field" → 空串转空数组。
func normalizeForUpdate(clientData map[string]any) map[string]any {
	out := make(map[string]any, len(clientData)+2)
	for k, v := range clientData {
		out[k] = v
	}
	if uuid, ok := out["uuid"].(string); ok && uuid != "" {
		out["id"] = uuid
	} else if id, ok := out["id"]; ok && id != nil {
		out["id"] = fmt.Sprintf("%v", id)
	}
	for _, field := range []string{"allowedIPs", "tunnelAllowedIPs"} {
		if v, ok := out[field]; ok && v == "" {
			out[field] = []any{}
		}
	}
	return out
}

// inboundIDsFor 对齐 Node::inboundIdsFor($protocol)：node_inbounds 里该协议的全部 inbound_id。
func (c *Collector) inboundIDsFor(ctx context.Context, nodeID int64, protocol string) ([]int64, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT inbound_id FROM node_inbounds WHERE node_id = ? AND protocol = ?", nodeID, protocol)
	if err != nil {
		return nil, fmt.Errorf("查询节点入站失败: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取节点入站失败: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// checkAfterSync 复刻 BanService::checkAfterSync()：流量同步后内联检查。
//
// 与 BanCheckJob / SyncTrafficCommand 同一规则：已确认关闭且未超时效 → 早退（0 次 HTTP）。
func (c *Collector) checkAfterSync(ctx context.Context, userID int64) error {
	user, err := c.loadUser(ctx, userID)
	if err != nil || user == nil {
		return err
	}
	if isRecentlyDisabled(*user, c.now()) {
		return nil
	}
	if banReasonAt(*user, c.now()) == "" {
		return nil
	}
	_, err = c.toggleClient(ctx, userID, false)
	return err
}

// needsDisable 复刻 BanService::needsDisable()：命中关闭条件且未处于「已确认关闭」时效内。
func (c *Collector) needsDisable(u userRow) bool {
	return banReasonAt(u, c.now()) != "" && !isRecentlyDisabled(u, c.now())
}

// clientEmail 复刻 User::clientEmail()：'ch_user_{id}'（data-contract §1）。
func clientEmail(userID int64) string {
	return xray.ClientEmail(userID)
}

// BanScanResult 是封禁扫描的统计。
type BanScanResult struct {
	// Scanned 是本轮遍历的启用用户数。
	Scanned int
	// Disabled 是实际发出关闭动作的用户数。
	Disabled int
	// Errors 是单用户失败的条数（不中断整轮）。
	Errors int
}

// ScanBans 复刻 BanCheckJob：遍历启用用户，命中条件且未在时效内 → 关闭 3x-ui 流量。
//
// 不封号：全程不写 users.enabled（product 铁律）。
func (c *Collector) ScanBans(ctx context.Context) (BanScanResult, error) {
	var out BanScanResult

	users, err := c.enabledUsers(ctx)
	if err != nil {
		return out, err
	}
	out.Scanned = len(users)

	for _, user := range users {
		if !c.needsDisable(user) {
			continue
		}
		if _, err := c.toggleClient(ctx, user.ID, false); err != nil {
			out.Errors++
			c.logf_("关闭用户流量失败（user=%d）: %v", user.ID, err)
			continue
		}
		out.Disabled++
	}
	return out, nil
}
