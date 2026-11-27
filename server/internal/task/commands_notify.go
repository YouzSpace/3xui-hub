package task

// 邮件通知扫描与渲染（mail:notify-scan）：流量/到期/过期三类场景的取数、
// 模板渲染、去重与入队。从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// runMailNotifyScan 对齐 MailNotifyScanJob::handle()。
// 扫描流量即将用尽 / 套餐即将到期 / 已到期。
func (r *commandRunner) runMailNotifyScan(ctx context.Context) error {
	if err := r.scanTrafficAlmost(ctx); err != nil {
		r.logf("扫描流量即将用尽失败: %v", err)
	}
	if err := r.scanExpiring(ctx); err != nil {
		r.logf("扫描套餐即将到期失败: %v", err)
	}
	if err := r.scanExpired(ctx); err != nil {
		r.logf("扫描已到期失败: %v", err)
	}
	return nil
}

// notifyEnabled 检查场景开关。
func (r *commandRunner) notifyEnabled(ctx context.Context, scene string) bool {
	return r.siteConfig(ctx, "notify_"+scene+"_enabled", "") == "1"
}

// scanTrafficAlmost 流量即将用尽（默认 90%）。
func (r *commandRunner) scanTrafficAlmost(ctx context.Context) error {
	if !r.notifyEnabled(ctx, "traffic_almost") {
		return nil
	}
	threshold := r.siteConfigInt(ctx, "notify_traffic_almost_threshold", 90)
	if threshold <= 0 || threshold > 100 {
		threshold = 90
	}
	ratio := float64(threshold) / 100

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users
		 WHERE enabled = 1 AND traffic_limit > 0
		   AND traffic_used >= FLOOR(traffic_limit * ?)
		   AND traffic_used < traffic_limit
		 ORDER BY id LIMIT 10000`, ratio)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return err
		}
		if err := r.dispatchNotifyOnce(ctx, "traffic_almost", u); err != nil {
			r.logf("发送 traffic_almost 通知失败（用户 %d）: %v", u.ID, err)
		}
	}
	return rows.Err()
}

// scanExpiring 套餐即将到期（默认提前 3 天）。
func (r *commandRunner) scanExpiring(ctx context.Context) error {
	if !r.notifyEnabled(ctx, "expiring") {
		return nil
	}
	days := r.siteConfigInt(ctx, "notify_expiring_days", 3)
	now := r.now().UTC()
	end := now.AddDate(0, 0, days)

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users
		 WHERE enabled = 1 AND expired_at IS NOT NULL
		   AND expired_at > ? AND expired_at <= ?
		 ORDER BY id LIMIT 10000`, now, end)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return err
		}
		if err := r.dispatchNotifyOnce(ctx, "expiring", u); err != nil {
			r.logf("发送 expiring 通知失败（用户 %d）: %v", u.ID, err)
		}
	}
	return rows.Err()
}

// scanExpired 已到期（扫描窗口 7 天）。
func (r *commandRunner) scanExpired(ctx context.Context) error {
	if !r.notifyEnabled(ctx, "expired") {
		return nil
	}
	now := r.now().UTC()
	start := now.AddDate(0, 0, -7)

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users
		 WHERE enabled = 1 AND expired_at IS NOT NULL
		   AND expired_at <= ? AND expired_at >= ?
		 ORDER BY id LIMIT 10000`, now, start)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return err
		}
		if err := r.dispatchNotifyOnce(ctx, "expired", u); err != nil {
			r.logf("发送 expired 通知失败（用户 %d）: %v", u.ID, err)
		}
	}
	return rows.Err()
}

// notifyUser 是通知扫描的用户行。
type notifyUser struct {
	ID                  int64
	Email               string
	Token               string
	TrafficLimit        int64
	TrafficUsed         int64
	ExpiredAt           *time.Time
	PlanID              sql.NullInt64
	MonthlyTrafficUsed  int64
	MonthlyTrafficLimit int64
	PlanName            string
}

// scanNotifyUser 从查询结果扫一行用户。
func scanNotifyUser(rows *sql.Rows) (*notifyUser, error) {
	return scanNotifyUserRow(rows)
}

// scanNotifyUserRow 扫一行用户（*sql.Rows 与 *sql.Row 都满足 Scan 接口，
// 定时发信的单发分支用 QueryRow 复用同一套列序）。
func scanNotifyUserRow(row interface{ Scan(...any) error }) (*notifyUser, error) {
	var u notifyUser
	var expiredAt sql.NullTime
	if err := row.Scan(&u.ID, &u.Email, &u.Token, &u.TrafficLimit, &u.TrafficUsed,
		&expiredAt, &u.PlanID, &u.MonthlyTrafficUsed, &u.MonthlyTrafficLimit); err != nil {
		return nil, err
	}
	if expiredAt.Valid {
		t := expiredAt.Time
		u.ExpiredAt = &t
	}
	return &u, nil
}

// dispatchNotifyOnce 对齐 MailNotifyScanJob::dispatchOnce()。
func (r *commandRunner) dispatchNotifyOnce(ctx context.Context, scene string, u *notifyUser) error {
	if u.Email == "" {
		return nil
	}

	// 渲染
	rendered := r.renderNotify(ctx, scene, u)
	if rendered == nil {
		return nil
	}

	// 解析收件人
	to := rendered.ToEmail
	if rendered.ToType == "admin" {
		to = r.siteConfig(ctx, "notify_admin_email", "")
	} else {
		to = u.Email
	}
	if to == "" {
		return nil
	}

	// 防重标记
	claimKey := r.mailClaimKey(scene, rendered.ToType, to, u.ID)
	ok, err := r.cacheAdd(ctx, claimKey, "1", 365*24*time.Hour)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	// 入队
	uid := uint64(u.ID)
	job := MailJob{
		ToEmail:  to,
		Subject:  rendered.Subject,
		HTMLBody: rendered.Body,
		Type:     "notify",
		UserID:   &uid,
		Scene:    &scene,
	}
	if r.deps.MailQueue != nil {
		return r.deps.MailQueue.EnqueueMail(job)
	}
	r.logf("[mail:notify-scan] MailQueue 未配置，跳过 %s → %s", scene, to)
	return nil
}

// notifyRendered 是渲染结果。
type notifyRendered struct {
	Subject string
	Body    string
	ToType  string
	ToEmail string
}

// renderNotify 对齐 MailNotifyService::render()。
func (r *commandRunner) renderNotify(ctx context.Context, scene string, u *notifyUser) *notifyRendered {
	def := mailNotifySceneDefaults[scene]
	if def.Title == "" {
		return nil
	}

	title := r.siteConfig(ctx, "notify_"+scene+"_title", def.Title)
	if title == "" {
		title = def.Title
	}
	body := r.siteConfig(ctx, "notify_"+scene+"_body", def.Body)
	if body == "" {
		body = def.Body
	}
	toType := r.siteConfig(ctx, "notify_"+scene+"_to", def.To)
	if toType == "" {
		toType = def.To
	}

	// 渲染变量
	limit := u.TrafficLimit
	used := u.TrafficUsed
	var percent float64
	if limit > 0 {
		percent = math.Round(float64(used)/float64(limit)*100*10) / 10
	}

	expireDate := "—"
	daysLeft := 0
	if u.ExpiredAt != nil {
		expireDate = u.ExpiredAt.Format("2006-01-02")
		daysLeft = int(math.Ceil(u.ExpiredAt.Sub(r.now().UTC()).Hours() / 24))
	}

	siteTitle := r.siteConfig(ctx, "site_title", "ControlHub")
	subURL := ""
	if u.Token != "" {
		subURL = "/api/sub/" + u.Token
	}

	planName := "无"
	if u.PlanID.Valid {
		var name string
		err := r.deps.DB.QueryRowContext(ctx,
			"SELECT name FROM plans WHERE id = ?", u.PlanID.Int64).Scan(&name)
		if err == nil {
			planName = name
		}
	}

	vars := map[string]string{
		"{{email}}":         u.Email,
		"{{user_id}}":       fmt.Sprintf("%d", u.ID),
		"{{site_title}}":    siteTitle,
		"{{used}}":          formatBytes(used),
		"{{limit}}":         formatBytes(limit),
		"{{percent}}":       fmt.Sprintf("%g", percent),
		"{{plan_name}}":     planName,
		"{{expire_date}}":   expireDate,
		"{{days_left}}":     fmt.Sprintf("%d", daysLeft),
		"{{subscribe_url}}": subURL,
	}

	return &notifyRendered{
		Subject: strtr(title, vars),
		Body:    strtr(body, vars),
		ToType:  toType,
	}
}

// mailClaimKey 对齐 MailNotifyService::claimKey()。
func (r *commandRunner) mailClaimKey(scene, toType, to string, userID int64) string {
	var who string
	if toType == "admin" {
		who = fmt.Sprintf("admin:%d", userID)
	} else {
		who = "email:" + strings.ToLower(strings.TrimSpace(to))
	}
	return fmt.Sprintf("mail_notify:%s:%s", scene, who)
}

// ── 7. mail:schedule-scan ──────────────────────────────────────────

// mailNotifySceneDefaults 是每个场景的出厂标题/正文/收件人。
var mailNotifySceneDefaults = map[string]struct {
	Title string
	Body  string
	To    string
}{
	"traffic_exhausted": {
		Title: "您的流量已用尽",
		Body:  "<p>您好，</p><p>您的流量已用尽，流量已被暂停。请续费或重置流量后继续使用。</p>",
		To:    "user",
	},
	"traffic_almost": {
		Title: "您的流量即将用尽",
		Body:  "<p>您好，</p><p>您的流量已使用 {{percent}}%，请及时续费，避免服务中断。</p>",
		To:    "user",
	},
	"expiring": {
		Title: "您的套餐即将到期",
		Body:  "<p>您好，</p><p>您的套餐将于 {{expire_date}} 到期（剩余 {{days_left}} 天），请及时续费。</p>",
		To:    "user",
	},
	"expired": {
		Title: "您的套餐已到期",
		Body:  "<p>您好，</p><p>您的套餐已于 {{expire_date}} 到期，流量已关闭。续费后可继续使用。</p>",
		To:    "user",
	},
}

// formatBytes 字节转可读文本。
func formatBytes(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := int(math.Log(float64(bytes)) / math.Log(1024))
	if i < 0 {
		i = 0
	}
	if i >= len(units) {
		i = len(units) - 1
	}
	return fmt.Sprintf("%.2f %s", float64(bytes)/math.Pow(1024, float64(i)), units[i])
}

// strtr 替换模板变量。
func strtr(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// truncateStr 截断字符串。
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// clientEmail 生成客户端 email 标识。
func clientEmail(userID int64) string {
	return fmt.Sprintf("user_%d", userID)
}
