package task

// 定时邮件（mail:schedule-scan）：收件人解析与逐封渲染。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// resolveScheduleRecipients 解析定时发信收件人。
//
// 与「批量发信」共用同一套筛选口径（config 就是 batch-send 的入参）：
//   - mode=single → 按邮箱 / 用户 ID 精确取一人；
//   - mode=all    → 全部已启用且有邮箱的用户（不看套餐/状态）；
//   - mode=filter → user_status（all/normal/depleted）+ plan_ids 多选 + include_no_plan。
//
// 恒定硬条件（与 admin 域 MailRecipients 一致）：
//   - enabled = 1 —— 被封禁的账号不发；
//   - email 非空   —— users.email 可空，空邮箱发不出去。
//
// 以前这里写死 `WHERE enabled = 1` 全量取人，页面上选的套餐/状态被完全忽略，
// 定时任务会按全量发出去 —— 与界面承诺不符，这里一并收口。
func (r *commandRunner) resolveScheduleRecipients(ctx context.Context, cfgJSON string) ([]notifyUser, error) {
	var cfg scheduleMailConfig
	_ = json.Unmarshal([]byte(cfgJSON), &cfg)

	if cfg.Mode == "single" {
		q := strings.TrimSpace(cfg.SingleQuery)
		if q == "" {
			return nil, nil
		}
		// 纯数字按用户 ID 查，否则按邮箱精确查（与 admin 域 MailUserByLogin 同口径）。
		field, arg := "u.email", any(q)
		if id, err := strconv.ParseInt(q, 10, 64); err == nil {
			field, arg = "u.id", id
		}
		row := r.deps.DB.QueryRowContext(ctx,
			`SELECT `+scheduleRecipientColumns+`
			 FROM users u WHERE `+field+` = ? AND u.enabled = 1 AND u.email IS NOT NULL AND u.email <> '' LIMIT 1`, arg)
		u, err := scanNotifyUserRow(row)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				r.logf("MailScheduleJob: 单发对象不存在或已被封禁，跳过本轮")
				return nil, nil
			}
			return nil, err
		}
		return []notifyUser{*u}, nil
	}

	where, args := scheduleRecipientWhere(cfg, r.now().UTC())
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT `+scheduleRecipientColumns+`
		 FROM users u WHERE `+where+` ORDER BY u.id LIMIT 10000`, args...)
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

// scheduleRecipientColumns 是定时发信取人的列（顺序须与 scanNotifyUser 一致）。
const scheduleRecipientColumns = `u.id, u.email, u.token, u.traffic_limit, u.traffic_used,
	u.expired_at, u.plan_id, u.monthly_traffic_used, u.monthly_traffic_limit`

// scheduleMailConfig 是 schedule_mail_config 的 JSON 结构（与批量发信同一套字段）。
type scheduleMailConfig struct {
	Mode          string  `json:"mode"`
	UserStatus    string  `json:"user_status"`
	PlanID        *int64  `json:"plan_id"`  // 旧配置的单值套餐（兼容）
	PlanIDs       []int64 `json:"plan_ids"` // 套餐多选
	IncludeNoPlan bool    `json:"include_no_plan"`
	SingleQuery   string  `json:"single_query"`
}

// scheduleRecipientWhere 把定时发信配置编译成 WHERE 子句与参数（纯函数，便于单测）。
//
// 口径与 admin 域 mailRecipientWhere 完全一致，避免「即时群发」和「定时发信」两处漂移：
//   - enabled = 1 与邮箱非空是所有分支的硬条件（封禁不发、空邮箱发不出去）；
//   - mode=all 忽略套餐/状态（全量就是全量）；
//   - 「未购买过套餐」= plan_id IS NULL，可与套餐多选并存（并集）。
func scheduleRecipientWhere(cfg scheduleMailConfig, now time.Time) (string, []any) {
	where := []string{"u.enabled = 1", "u.email IS NOT NULL", "u.email <> ''"}
	args := []any{}
	if cfg.Mode == "all" {
		return strings.Join(where, " AND "), args
	}

	planIDs := cfg.PlanIDs
	if len(planIDs) == 0 && cfg.PlanID != nil {
		planIDs = []int64{*cfg.PlanID} // 兼容旧的单值 plan_id
	}
	ph := make([]string, 0, len(planIDs))
	for _, id := range planIDs {
		ph = append(ph, "?")
		args = append(args, id)
	}
	switch {
	case len(ph) > 0 && cfg.IncludeNoPlan:
		where = append(where, "(u.plan_id IN ("+strings.Join(ph, ",")+") OR u.plan_id IS NULL)")
	case len(ph) > 0:
		where = append(where, "u.plan_id IN ("+strings.Join(ph, ",")+")")
	case cfg.IncludeNoPlan:
		where = append(where, "u.plan_id IS NULL")
	}

	switch cfg.UserStatus {
	case "all":
		// 不加状态条件
	case "depleted":
		where = append(where, "(",
			"(u.expired_at IS NOT NULL AND u.expired_at < ?)",
			"OR (u.traffic_limit > 0 AND u.traffic_used >= u.traffic_limit)",
			"OR (u.monthly_traffic_limit > 0 AND u.monthly_traffic_used >= u.monthly_traffic_limit))")
		args = append(args, now)
	default: // normal（含空值）
		where = append(where,
			"(u.expired_at IS NULL OR u.expired_at > ?)",
			"(u.traffic_limit <= 0 OR u.traffic_used < u.traffic_limit)",
			"(u.monthly_traffic_limit <= 0 OR u.monthly_traffic_used < u.monthly_traffic_limit)")
		args = append(args, now)
	}
	return strings.Join(where, " AND "), args
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
