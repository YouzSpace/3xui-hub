package task

// 监控汇总（monitor:rollup）命令体。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"fmt"
	"time"
)

// runMonitorRollup 对齐 MonitorRollupCommand::handle() → MonitorService::rollup()。
// 把超过保留期的 L0 采样折成 monitor_daily 的当天增量后删除原始样本。
func (r *commandRunner) runMonitorRollup(ctx context.Context) error {
	retainDays := r.siteConfigInt(ctx, "monitor.retain_days", 90)
	if retainDays < 1 {
		retainDays = 90
	}
	cutoff := r.now().UTC().AddDate(0, 0, -retainDays)

	tx, err := r.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 取过期样本，按 (node_id, sampled_at) 升序
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id, sampled_at, sent, recv FROM monitor_samples
		 WHERE sampled_at < ? ORDER BY node_id, sampled_at`, cutoff)
	if err != nil {
		return fmt.Errorf("查询过期样本失败: %w", err)
	}
	type sample struct {
		NodeID     int64
		SampledAt  time.Time
		Sent       int64
		Recv       int64
	}
	var samples []sample
	for rows.Next() {
		var s sample
		if err := rows.Scan(&s.NodeID, &s.SampledAt, &s.Sent, &s.Recv); err != nil {
			rows.Close()
			return err
		}
		samples = append(samples, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(samples) == 0 {
		return tx.Commit()
	}

	// 时区对齐 businessTimezone（PHP 默认 UTC）
	tz := time.UTC
	type dayKey struct {
		NodeID int64
		Date   string
	}
	groups := map[dayKey][2]int64{}
	lastSeen := map[int64]*sample{}

	for i := range samples {
		s := &samples[i]
		date := s.SampledAt.In(tz).Format("2006-01-02")
		prev := lastSeen[s.NodeID]
		lastSeen[s.NodeID] = s

		var dSent, dRecv int64
		if prev != nil {
			dSent = s.Sent - prev.Sent
			dRecv = s.Recv - prev.Recv
			if dSent < 0 {
				dSent = s.Sent
			}
			if dRecv < 0 {
				dRecv = s.Recv
			}
		} else {
			dSent = s.Sent
			dRecv = s.Recv
		}

		key := dayKey{NodeID: s.NodeID, Date: date}
		cur := groups[key]
		groups[key] = [2]int64{cur[0] + dSent, cur[1] + dRecv}
	}

	// upsert monitor_daily（覆盖写，幂等）
	for key, deltas := range groups {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO monitor_daily (node_id, date, sent_delta, recv_delta, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE sent_delta = VALUES(sent_delta), recv_delta = VALUES(recv_delta)`,
			key.NodeID, key.Date, deltas[0], deltas[1], r.now().UTC(), r.now().UTC())
		if err != nil {
			return fmt.Errorf("写入 monitor_daily 失败: %w", err)
		}
	}

	// 删除过期样本
	res, err := tx.ExecContext(ctx,
		"DELETE FROM monitor_samples WHERE sampled_at < ?", cutoff)
	if err != nil {
		return fmt.Errorf("删除过期样本失败: %w", err)
	}
	deleted, _ := res.RowsAffected()

	nodeSet := map[int64]bool{}
	for key := range groups {
		nodeSet[key.NodeID] = true
	}

	r.logf("完成：删除 %d 条过期样本，覆盖 %d 个 (节点, 日期) 分组（%d 个节点）",
		deleted, len(groups), len(nodeSet))
	return tx.Commit()
}

// ── 4. order:expire-pending ─────────────────────────────────────────
