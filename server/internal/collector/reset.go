package collector

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// 月重置：逐行对齐 MonthlyResetTrafficCommand（traffic:monthly-reset，每日 00:00）。
//
// 铁律（behavior-contract §3）：
//   - **只清已用流量**（monthly_traffic_used），next_traffic_reset_at += 30 天；
//   - 不动套餐时长 / 额度：traffic_limit、expired_at 一律不碰；
//   - 仅「绑定 period 套餐 + next_traffic_reset_at 非空且 ≤ now」的用户参与；
//   - 重置后重置 3x-ui 计数并重新打开流量（用户从新周期开始即可用）。
//
// 「管理员手动重置」「用户购买重置」是另外两条入口（P8a/支付），口径同样是「只清零已用」，
// 但都不改 next_traffic_reset_at —— 本命令是唯一推进下次重置日期的定时任务。

// MonthlyResetResult 是月重置的统计。
type MonthlyResetResult struct {
	// Candidates 是命中的用户数。
	Candidates int
	// Reset 是成功重置（写库 + 节点动作）的用户数。
	Reset int
	// Errors 是单用户失败的条数（不中断整轮）。
	Errors int
	// UserIDs 是本次重置的用户（日志/对账用）。
	UserIDs []int64
}

// resetCandidate 是一个待重置的周期套餐用户。
type resetCandidate struct {
	ID                 int64
	NextTrafficResetAt sql.NullTime
}

// MonthlyReset 复刻 MonthlyResetTrafficCommand::handle()。
//
// 每个用户的动作顺序与 PHP 一致：
//  1. 清零 monthly_traffic_used + next_traffic_reset_at += 30 天（单条 UPDATE，原子）；
//  2. resetClientOnNodes（各启用节点重置该 email 的流量计数）；
//  3. toggleClient(true) 打开流量（3x-ui 只是顺手打开 client；xray 走 adu 重加，两类节点都幂等）。
//
// 与 PHP 的差异：PHP 在内存里 Carbon::parse(...)->addDays(30) 再 save，Go 侧用
// `next_traffic_reset_at + INTERVAL 30 DAY` 在同一条 UPDATE 里算 —— 结果完全相同，
// 但避免了「读出来 → 算 → 写回」之间被并发改动覆盖的窗口。
func (c *Collector) MonthlyReset(ctx context.Context) (MonthlyResetResult, error) {
	var out MonthlyResetResult

	now := c.now()
	rows, err := c.db.QueryContext(ctx,
		"SELECT u.id, u.next_traffic_reset_at FROM users u JOIN plans p ON p.id = u.plan_id "+
			"WHERE u.plan_id IS NOT NULL AND u.next_traffic_reset_at IS NOT NULL "+
			"AND u.next_traffic_reset_at <= ? AND p.type = 'period' ORDER BY u.id",
		dbDateTime(now))
	if err != nil {
		return out, fmt.Errorf("查询待重置用户失败: %w", err)
	}

	var candidates []resetCandidate
	for rows.Next() {
		var cand resetCandidate
		if err := rows.Scan(&cand.ID, &cand.NextTrafficResetAt); err != nil {
			rows.Close()
			return out, fmt.Errorf("读取待重置用户失败: %w", err)
		}
		candidates = append(candidates, cand)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, fmt.Errorf("读取待重置用户失败: %w", err)
	}
	rows.Close()
	out.Candidates = len(candidates)

	for _, cand := range candidates {
		if err := c.resetMonthly(ctx, cand.ID, now); err != nil {
			out.Errors++
			c.logf_("月流量重置失败（user=%d）: %v", cand.ID, err)
			continue
		}
		out.Reset++
		out.UserIDs = append(out.UserIDs, cand.ID)
	}
	return out, nil
}

// resetMonthly 重置单个用户的当月流量并恢复 3x-ui 连接。
func (c *Collector) resetMonthly(ctx context.Context, userID int64, now time.Time) error {
	// 1. 只清已用流量 + 推进下次重置日期（同一条语句，原子）。
	//
	// 条件里重复 next_traffic_reset_at <= now：并发的两条命令（比如手工补跑）
	// 只有一条能命中，另一条 RowsAffected=0 → 直接跳过，不会把日期 +60 天。
	res, err := c.db.ExecContext(ctx,
		"UPDATE users SET monthly_traffic_used = 0, "+
			"next_traffic_reset_at = DATE_ADD(next_traffic_reset_at, INTERVAL 30 DAY), updated_at = ? "+
			"WHERE id = ? AND next_traffic_reset_at IS NOT NULL AND next_traffic_reset_at <= ?",
		dbDateTime(now), userID, dbDateTime(now))
	if err != nil {
		return fmt.Errorf("重置月流量失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // 已被另一条命令重置（并发/重复执行）
	}

	// 2. 重置各启用节点上该 email 的流量计数。
	if err := c.resetClientOnNodes(ctx, userID); err != nil {
		// 与 PHP 一致：单节点失败只 report，不阻断后续「打开流量」。
		c.logf_("重置节点流量计数失败（user=%d）: %v", userID, err)
	}

	// 3. 打开 3x-ui 流量（周期套餐月超被关的用户在新周期恢复）。
	//    计费动作 → 流量级（bookkeep=true）：全部节点确认后清 traffic_disabled_at。
	if _, err := c.toggleClient(ctx, userID, true, true); err != nil {
		return fmt.Errorf("恢复用户流量失败: %w", err)
	}
	return nil
}

// resetClientOnNodes 复刻 UserAdminService::resetClientOnNodes()：
// 在各 enabled 节点上重置该 email 的流量计数；单节点失败不阻断其它节点。
func (c *Collector) resetClientOnNodes(ctx context.Context, userID int64) error {
	email := clientEmail(userID)

	nodes, err := c.enabledNodes(ctx)
	if err != nil {
		return err
	}

	var firstErr error
	for _, node := range nodes {
		if err := c.resetClientOnNode(ctx, node, email); err != nil {
			c.logf_("重置节点流量失败（node=%d user=%d）: %v", node.ID, userID, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// resetClientOnNode 在单个节点上重置该 email 的流量计数。
//
// xray 驱动的 ResetClientTraffic 是 no-op（「绝对值快照 + 增量落账」口径下，
// 清零 users 计数后下一轮 delta 会继续正常累计），与 PHP 一致。
func (c *Collector) resetClientOnNode(ctx context.Context, node nodeRow, email string) error {
	switch {
	case node.isXray():
		return c.xrayDriver(node).ResetClientTraffic(ctx, email)
	case node.DriverType == "" || node.DriverType == "3x-ui":
		client, err := c.threeXUiClient(node)
		if err != nil {
			return err
		}
		return client.ResetClientTraffic(ctx, email)
	default:
		return errUnsupportedDriver(node.DriverType)
	}
}
