package task

// 定时邮件（mail:schedule-scan）：收件人解析与逐封渲染。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// runMailScheduleScan 对齐 MailScheduleJob::handle()。
// 按月定时发信：到点且当月没发过就把配好的那封信入队。
func (r *commandRunner) runMailScheduleScan(ctx context.Context) error {
	if r.siteConfig(ctx, "schedule_mail_enabled", "") != "1" {
		return nil
	}

	cfgDay := r.siteConfigInt(ctx, "schedule_mail_day", 1)
	cfgTime := r.siteConfig(ctx, "schedule_mail_time", "09:00")
	now := r.now().UTC()

	// 计算本月发送时刻
	lastDay := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if cfgDay < 1 || cfgDay > lastDay {
		return nil // 配置的第 N 天当月不存在
	}

	var hour, minute int
	fmt.Sscanf(cfgTime, "%d:%d", &hour, &minute)
	sendAt := time.Date(now.Year(), now.Month(), cfgDay, hour, minute, 0, 0, time.UTC)

	if now.Before(sendAt) {
		return nil // 还没到点
	}

	// 开启时间晚于本月到点时刻 → 当月不补发
	armedRaw := r.siteConfig(ctx, "schedule_mail_armed_at", "")
	if armedRaw != "" {
		if armedAt, err := time.Parse("2006-01-02 15:04:05", armedRaw); err == nil {
			if armedAt.After(sendAt) {
				return nil
			}
		}
	}

	// 当月已发过 → 跳过
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	var mailLogCount int
	err := r.deps.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mail_logs WHERE type = 'schedule' AND created_at >= ? AND created_at < ?`,
		monthStart, monthEnd).Scan(&mailLogCount)
	if err != nil {
		return err
	}
	if mailLogCount > 0 {
		return nil
	}

	// 读配置
	cfgJSON := r.siteConfig(ctx, "schedule_mail_config", "")
	if cfgJSON == "" {
		r.logf("MailScheduleJob: 定时发信未配置，跳过本轮")
		return nil
	}
	var mailCfg struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(cfgJSON), &mailCfg); err != nil {
		r.logf("MailScheduleJob: 解析定时发信配置失败: %v", err)
		return nil
	}
	if mailCfg.Subject == "" || mailCfg.Body == "" {
		r.logf("MailScheduleJob: 定时发信未配置标题/正文，跳过本轮")
		return nil
	}

	// 解析收件人
	users, err := r.resolveScheduleRecipients(ctx, cfgJSON)
	if err != nil || len(users) == 0 {
		r.logf("MailScheduleJob: 没有符合条件的收件人，跳过本轮")
		return nil
	}

	// 去重标记（入队那一刻就落）
	markKey := "mail_schedule:" + now.Format("200601")
	markTTL := time.Date(now.Year(), now.Month()+1, 1, 0, 1, 0, 0, time.UTC).Sub(now)
	ok, err := r.cacheAdd(ctx, markKey, "1", markTTL)
	if err != nil || !ok {
		return err
	}

	// 入队
	batchID := now.Unix()
	bid := batchID
	now2 := now
	for i, user := range users {
		uid := uint64(user.ID)
		job := MailJob{
			ToEmail:  user.Email,
			Subject:  mailCfg.Subject,
			HTMLBody: r.renderForSchedule(ctx, mailCfg.Body, user, now2),
			Type:     "schedule",
			UserID:   &uid,
			BatchID:  &bid,
		}
		_ = i
		if r.deps.MailQueue != nil {
			if err := r.deps.MailQueue.EnqueueMail(job); err != nil {
				r.logf("MailScheduleJob: 入队失败: %v", err)
			}
		}
	}

	r.logf("MailScheduleJob: 定时发信已入队（batch=%d, total=%d）", batchID, len(users))
	return nil
}

// resolveScheduleRecipients 解析定时发信收件人（mode=all 简化）。
func (r *commandRunner) resolveScheduleRecipients(ctx context.Context, cfgJSON string) ([]notifyUser, error) {
	var cfg struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal([]byte(cfgJSON), &cfg)

	// mode=all：全部启用且有邮箱的用户
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users WHERE enabled = 1 AND email != '' ORDER BY id LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []notifyUser
	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, rows.Err()
}

// renderForSchedule 渲染定时发信正文。
func (r *commandRunner) renderForSchedule(ctx context.Context, body string, u notifyUser, now time.Time) string {
	vars := map[string]string{
		"{{email}}":     u.Email,
		"{{user_id}}":   fmt.Sprintf("%d", u.ID),
		"{{site_title}}": r.siteConfig(ctx, "site_title", "ControlHub"),
		"{{used}}":      formatBytes(u.TrafficUsed),
		"{{limit}}":     formatBytes(u.TrafficLimit),
		"{{plan_name}}": "无",
	}
	if u.PlanID.Valid {
		var name string
		if err := r.deps.DB.QueryRowContext(ctx,
			"SELECT name FROM plans WHERE id = ?", u.PlanID.Int64).Scan(&name); err == nil {
			vars["{{plan_name}}"] = name
		}
	}
	return strtr(body, vars)
}

// ── 8. third-party:fetch ───────────────────────────────────────────
