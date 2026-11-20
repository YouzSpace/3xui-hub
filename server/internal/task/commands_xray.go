package task

// xray 相关命令体：WARP 换 IP、出站订阅刷新、geo 数据更新。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// runXrayWarpRotate 对齐 XrayWarpRotate::handle()。
// 到点条件：已启用 + last_rotate_at（或 updated_at）距今 ≥ auto_rotate_hours。
func (r *commandRunner) runXrayWarpRotate(ctx context.Context) error {
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT wa.id, wa.node_id, wa.auto_rotate_hours, wa.last_rotate_at, wa.updated_at,
		        n.name, n.enabled
		 FROM node_warp_accounts wa
		 JOIN nodes n ON n.id = wa.node_id
		 WHERE wa.auto_rotate_hours > 0 AND wa.enabled = 1 AND n.enabled = 1
		 ORDER BY wa.id`)
	if err != nil {
		return fmt.Errorf("查询 WARP 账户失败: %w", err)
	}
	type warpRow struct {
		ID              int64
		NodeID          int64
		AutoRotateHours int64
		LastRotateAt    sql.NullTime
		UpdatedAt       time.Time
		NodeName        string
	}
	var accounts []warpRow
	for rows.Next() {
		var a warpRow
		if err := rows.Scan(&a.ID, &a.NodeID, &a.AutoRotateHours, &a.LastRotateAt,
			&a.UpdatedAt, &a.NodeName, new(sql.NullInt64)); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, a)
	}
	rows.Close()

	now := r.now().UTC()
	rotated := 0
	for _, a := range accounts {
		base := a.UpdatedAt
		if a.LastRotateAt.Valid {
			base = a.LastRotateAt.Time
		}
		due := base.Add(time.Duration(a.AutoRotateHours) * time.Hour)
		if now.Before(due) {
			continue
		}

		// 换 IP：重新注册设备 → 重组 wireguard 出站 → bump config（PHP WarpService::rotate）。
		// 失败也推进 last_rotate_at，否则每 5 分钟重试一次、把 CF 接口打到限流。
		if r.deps.RotateWarp == nil {
			r.logf("RotateWarp 未配置，跳过 node=%d", a.NodeID)
			continue
		}
		if err := r.deps.RotateWarp(ctx, uint64(a.NodeID)); err != nil {
			r.logf("node#%d WARP 换 IP 失败（%s）: %v", a.NodeID, a.NodeName, err)
		} else {
			r.logf("node#%d WARP 已换 IP（%s）", a.NodeID, a.NodeName)
			rotated++
		}

		// 更新 last_rotate_at（成功失败都推进，对齐 PHP 的失败也写）
		_, _ = r.deps.DB.ExecContext(ctx,
			"UPDATE node_warp_accounts SET last_rotate_at = ?, updated_at = ? WHERE id = ?",
			now, now, a.ID)
	}

	r.logf("完成：%d/%d 个账户已换 IP", rotated, len(accounts))
	return nil
}

// ── 10. xray:refresh-outbound-subs ─────────────────────────────────

// runXrayRefreshOutboundSubs 对齐 RefreshOutboundSubscriptions::handle()。
// 逐条检查到期项 → 拉取 → 解析 → 缓存；内容变化则 bump 节点 config_version。
func (r *commandRunner) runXrayRefreshOutboundSubs(ctx context.Context) error {
	var enabled int
	if err := r.deps.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM xray_outbound_subscriptions WHERE enabled = 1").Scan(&enabled); err != nil {
		return err
	}
	if enabled == 0 {
		r.logf("没有启用的出站订阅，跳过")
		return nil
	}

	r.logf("%s 检查 %d 条出站订阅...", r.now().Format("2006-01-02 15:04:05"), enabled)

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, node_id, url, update_interval, last_fetched_at, content_hash, link_identities, tag_prefix
		 FROM xray_outbound_subscriptions WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return err
	}
	type subRow struct {
		ID             int64
		NodeID         int64
		URL            string
		UpdateInterval int64
		LastFetchedAt  sql.NullTime
		ContentHash    sql.NullString
		LinkIDentities sql.NullString
		TagPrefix      sql.NullString
	}
	var subs []subRow
	for rows.Next() {
		var s subRow
		if err := rows.Scan(&s.ID, &s.NodeID, &s.URL, &s.UpdateInterval,
			&s.LastFetchedAt, &s.ContentHash, &s.LinkIDentities, &s.TagPrefix); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, s)
	}
	rows.Close()

	now := r.now().UTC()
	refreshed := 0

	for _, sub := range subs {
		// 检查到期
		if sub.LastFetchedAt.Valid {
			due := sub.LastFetchedAt.Time.Add(time.Duration(sub.UpdateInterval) * time.Minute)
			if now.Before(due) {
				continue
			}
		}

		// 拉取并解析
		outbounds, fetchErr := r.fetchOutboundSub(ctx, sub.URL)
		if fetchErr != nil || len(outbounds) == 0 {
			msg := ""
			if fetchErr != nil {
				msg = fetchErr.Error()
			} else {
				msg = "拉取成功但未解析到任何可用出站"
			}
			_, _ = r.deps.DB.ExecContext(ctx,
				`UPDATE xray_outbound_subscriptions
				 SET last_status = 'failed', last_error = ?, last_fetched_at = ?, updated_at = ?
				 WHERE id = ?`, truncateStr(msg, 500), now, now, sub.ID)
			continue
		}

		// 序列化并计算哈希
		fetchedJSON, _ := json.Marshal(outbounds)
		hash := simpleSHA1(string(fetchedJSON))

		// 保存
		_, err := r.deps.DB.ExecContext(ctx,
			`UPDATE xray_outbound_subscriptions
			 SET last_status = 'success', last_error = NULL,
			     fetched = ?, content_hash = ?, node_count = ?,
			     last_fetched_at = ?, updated_at = ?
			 WHERE id = ?`, string(fetchedJSON), hash, len(outbounds), now, now, sub.ID)
		if err != nil {
			r.logf("保存出站订阅 #%d 失败: %v", sub.ID, err)
			continue
		}

		// 内容变化才 bump config_version（复用 NodeStore.BumpConfigVersion：
		// 行锁内递增 driver_config JSON 的 config_version 键 + 写 config_changed_at）。
		if !sub.ContentHash.Valid || sub.ContentHash.String != hash {
			if r.deps.BumpConfigVersion != nil {
				if _, berr := r.deps.BumpConfigVersion(ctx, uint64(sub.NodeID)); berr != nil {
					r.logf("bump config_version 失败 node=%d: %v", sub.NodeID, berr)
				}
			} else {
				r.logf("BumpConfigVersion 未配置，跳过 node=%d", sub.NodeID)
			}
		}
		refreshed++
	}

	r.logf("完成：已刷新 %d 条", refreshed)
	return nil
}

// fetchOutboundSub 拉取出站订阅并解析出站列表。
func (r *commandRunner) fetchOutboundSub(ctx context.Context, url string) ([]map[string]any, error) {
	body, err := httpGet(ctx, url, 30*time.Second)
	if err != nil {
		return nil, err
	}

	decoded := tryBase64Decode(string(body))
	if decoded == "" {
		decoded = string(body)
	}

	var outbounds []map[string]any
	for _, line := range strings.Split(decoded, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 简化解析：每行一个 URI，包装为 {tag, uri}
		if strings.Contains(line, "://") {
			outbounds = append(outbounds, map[string]any{
				"tag": fmt.Sprintf("out-%d", len(outbounds)),
				"uri": line,
			})
		}
	}
	return outbounds, nil
}

// ── 11. geodata:update ─────────────────────────────────────────────

// runGeodataUpdate 对齐 GeodataUpdateCommand::handle()。
//
// 输出口径与对账命令一致：末尾必有一行固定键名的结构化计数 ——
// 「无更新」「所有节点都在途」这类正常空转也要看得见规模，而不是只打印一句「无更新」。
func (r *commandRunner) runGeodataUpdate(ctx context.Context, args map[string]string) error {
	start := time.Now()
	if r.deps.Geodata == nil {
		r.logf("[WARN] geodata:update 跳过：Geodata 服务未配置 nodes=0 updated=0 failed=0 queued=0 skipped=0 ms=0")
		return nil
	}

	results, err := r.deps.Geodata.Update(ctx)
	if err != nil {
		return fmt.Errorf("geo 数据更新失败: %w", err)
	}

	updated, failed := 0, 0
	for name, res := range results {
		detail := res.Message
		if detail == "" {
			detail = fmt.Sprintf("%d 字节", res.Size)
		}
		switch res.Status {
		case "updated":
			updated++
			r.logf("%s：已更新（%s）", name, detail)
		case "failed":
			// 失败必须单独计数并 WARN：文件级失败会让节点上的 geo 数据悄悄过期。
			failed++
			r.logf("[WARN] %s：更新失败（%s）", name, detail)
		default:
			r.logf("%s：%s（%s）", name, res.Status, detail)
		}
	}

	forceDispatch := args["--force-dispatch"] == "1"
	shouldDispatch := forceDispatch || (args["--dispatch"] == "1" && updated > 0)

	queued, skipped, missingDispatch := 0, 0, false
	if shouldDispatch {
		var derr error
		queued, skipped, missingDispatch, derr = r.deps.Geodata.DispatchToNodes(ctx)
		if derr != nil {
			return fmt.Errorf("分发 geo 数据失败: %w", derr)
		}
		if missingDispatch {
			r.logf("面板上没有 geo 文件，跳过分发。")
		} else {
			r.logf("已分发到 %d 个节点（跳过 %d 个已有在途任务的节点）。", queued, skipped)
		}
	} else if updated > 0 {
		r.logf("（未分发；如需推送节点请加 --dispatch）")
	} else {
		r.logf("无更新。")
	}

	// files 是 geo 文件总数（updated/unchanged/failed 之和）；nodes 是分发目标（queued+skipped）。
	r.logf("nodes=%d files=%d updated=%d unchanged=%d failed=%d queued=%d skipped=%d missing=%v ms=%d",
		queued+skipped, len(results), updated, len(results)-updated-failed, failed,
		queued, skipped, missingDispatch, time.Since(start).Milliseconds())

	// 一条文件都没更新成功、却又有失败项 → 最高风险组合，喊出来。
	if failed > 0 && updated == 0 {
		r.logf("[WARN] geodata:update 本轮无任何文件更新成功（failed=%d），节点 geo 数据保持旧版本", failed)
	}
	return nil
}

// ── 12. users:reconcile-enable ─────────────────────────────────────
