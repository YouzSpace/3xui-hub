package admin

// 管理端邮件（/admin-api/email…）：SMTP 配置 + 自动通知 + 批量发信 + 定时发信 + 日志。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/EmailController.php。
//
// ⚠️ Laravel mail 配置改后须刷新 mailer 缓存（PHP `forgetMailers()`）。
// Go 侧等价处理：MailTransport.Send 每次都接收**当场解密**的 SMTPSettings，
// 不在域内缓存传输层 —— 改 mail 配置后必须重建/重取配置再发，旧 mailer 不会被复用。

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	lcrypto "controlhub-server/internal/crypto"
	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// scheduleTimeRe 对齐 MailScheduleService 的时间格式（Laravel regex 规则同款）。
var scheduleTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// defaultEmailTemplate 是未配置 email_template 时的测试信正文（PHP 里的字面量）。
const defaultEmailTemplate = `<div style="padding:20px;font-family:sans-serif"><h2>注册验证码</h2><p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p><p style="color:#666">5分钟内有效，请勿泄露。</p></div>`

// Register 把邮件域路由挂到给定分组。
//
// 顺序逐条对齐 backend/routes/web.php（静态段先于参数段；本域无参数段）。
func (d *MailDomain) Register(r gin.IRouter) {
	r.GET("/email", d.mailShow)
	r.PUT("/email", d.mailSave)
	r.POST("/email/test", d.mailTest)
	r.GET("/email/notify", d.mailNotifyConfig)
	r.PUT("/email/notify", d.mailSaveNotifyConfig)
	r.POST("/email/batch-send", d.mailBatchSend)
	r.GET("/email/schedule-mail", d.mailScheduleConfig)
	r.PUT("/email/schedule-mail", d.mailSaveScheduleConfig)
	r.GET("/email/logs", d.mailLogs)
	// 批量发信进度（goroutine 队列不落库，进度由队列水位+mail_logs 聚合得出）
	r.GET("/email/batch-progress", d.mailBatchProgress)
	// 多 SMTP 账号池（列表/新增/修改/删除/状态切换）
	d.registerSMTPAccountRoutes(r)
}

// ---- 响应封装 ----

func (d *MailDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *MailDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *MailDomain) failValidation(c *gin.Context, summary string) { d.fail(c, summary, 500) }

func (d *MailDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// ---- 配置读写 ----

// decryptSmtpPassword 对齐 EmailController 里的 Crypt::decryptString + 失败置空。
func (d *MailDomain) decryptSmtpPassword(encrypted string) string {
	if encrypted == "" {
		return ""
	}
	plain, err := lcrypto.DecryptString(encrypted, d.appKey)
	if err != nil {
		return ""
	}
	return plain
}

// buildSMTPSettings 从站点配置拼一次发信用的 SMTP 参数（密码当场解密）。
func (d *MailDomain) buildSMTPSettings(cfg map[string]string) SMTPSettings {
	port := 587
	if n, err := strconv.Atoi(cfg["smtp_port"]); err == nil && n > 0 {
		port = n
	}
	return SMTPSettings{
		Host:       cfg["smtp_host"],
		Port:       port,
		Username:   cfg["smtp_username"],
		Password:   d.decryptSmtpPassword(cfg["smtp_password"]),
		Encryption: cfg["smtp_encryption"],
		FromName:   cfg["smtp_from_name"],
	}
}

// ---- handlers ----

// mailShow 对齐 GET /admin-api/email（smtp_password 解密回显）。
func (d *MailDomain) mailShow(c *gin.Context) {
	cfg, err := d.store.MailConfigMany(ctxOf(c), emailConfigKeys)
	if err != nil {
		d.failServer(c, "读取邮件配置", err)
		return
	}
	out := &jsObj{}
	for _, k := range emailConfigKeys {
		v := cfg[k]
		if k == "smtp_password" && v != "" {
			v = d.decryptSmtpPassword(v)
		}
		out.put(k, v)
	}
	d.success(c, out)
}

// mailSave 对齐 PUT /admin-api/email。
func (d *MailDomain) mailSave(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	v.field("smtp_host", "nullable", "string", "max:255")
	v.field("smtp_port", "nullable", "integer", "min:1", "max:65535")
	v.field("smtp_username", "nullable", "string", "max:255")
	v.field("smtp_password", "nullable", "string", "max:255")
	v.field("smtp_encryption", "nullable", "in:tls,ssl,none")
	v.field("smtp_from_name", "nullable", "string", "max:100")
	v.field("email_template", "nullable", "string", "max:65535")
	v.field("register_email_verify", "nullable", "boolean")
	v.field("mail_rate_limit_enabled", "nullable", "boolean")
	v.field("mail_rate_per_minute", "nullable", "integer", "min:1", "max:600")
	v.field("mail_send_interval_ms", "nullable", "integer", "min:0")
	v.field("rate_code_interval", "nullable", "integer", "min:0")
	v.field("rate_code_per_day", "nullable", "integer", "min:0")
	v.field("rate_code_ip_hourly", "nullable", "integer", "min:0")
	v.field("rate_verify_max_attempts", "nullable", "integer", "min:0")
	v.field("rate_verify_lock_seconds", "nullable", "integer", "min:0")
	v.field("rate_login_max_attempts", "nullable", "integer", "min:0")
	v.field("rate_login_lock_seconds", "nullable", "integer", "min:0")
	v.field("rate_register_ip_hourly", "nullable", "integer", "min:0")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验邮件配置", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}

	updates := map[string]string{}
	for _, k := range emailConfigKeys {
		val, ok := data[k]
		if !ok {
			continue // 未提交的键不写（对应 Laravel validated() 只含出现过的字段）
		}
		s := mailConfigStoreValue(k, val)
		if k == "smtp_password" && s != "" {
			enc, err := lcrypto.EncryptString(s, d.appKey)
			if err != nil {
				d.failServer(c, "加密 SMTP 密码", err)
				return
			}
			s = enc
		}
		updates[k] = s
	}

	// 空值保护：SMTP 三件套一旦被空串写进去，面板就再也发不出邮件。
	// 语义：只提交非空值即视为「保持原样」；要真正清空某项请在数据库侧操作。
	protected := []string{"smtp_host", "smtp_username", "smtp_password"}
	if len(protected) > 0 {
		var need []string
		for _, k := range protected {
			if _, ok := updates[k]; ok {
				need = append(need, k)
			}
		}
		if len(need) > 0 {
			current, err := d.store.MailConfigMany(ctxOf(c), need)
			if err != nil {
				d.failServer(c, "读取邮件配置", err)
				return
			}
			for _, k := range need {
				if updates[k] == "" && current[k] != "" {
					delete(updates, k)
				}
			}
		}
	}

	if err := d.store.MailConfigSetMany(ctxOf(c), updates); err != nil {
		d.failServer(c, "保存邮件配置", err)
		return
	}
	d.success(c, nil, "保存成功")
}

// mailConfigStoreValue 对齐 `$updates[$key] = $value ?? ''`（布尔按 PDO prepareBindings 落 1/0）。
func mailConfigStoreValue(key string, val any) string {
	if val == nil {
		return ""
	}
	switch t := val.(type) {
	case bool:
		if t {
			return "1"
		}
		return "0"
	case string:
		return t
	default:
		if s, ok := inputString(val); ok {
			return s
		}
		return fmt.Sprint(val)
	}
}

// mailTest 对齐 POST /admin-api/email/test。
func (d *MailDomain) mailTest(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	v.field("to", "required")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验测试邮件", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}
	to, ok := inputString(data["to"])
	to = strings.TrimSpace(to)
	if !ok || !emailValid(to) {
		d.failValidation(c, "The to field must be a valid email address.")
		return
	}

	cfg, err := d.store.MailConfigMany(ctxOf(c), emailConfigKeys)
	if err != nil {
		d.failServer(c, "读取邮件配置", err)
		return
	}
	if cfg["smtp_host"] == "" {
		d.fail(c, "请先配置 SMTP 服务器地址", 400)
		return
	}

	settings := d.buildSMTPSettings(cfg)
	fromAddress := cfg["smtp_username"]
	fromName := cfg["smtp_from_name"]

	template := cfg["email_template"]
	if template == "" {
		template = defaultEmailTemplate
	}
	body := strings.ReplaceAll(template, "{{code}}", "TEST1234")
	subject := "ControlHub"
	if fromName != "" {
		subject = fromName
	}

	sendErr := d.transport.Send(settings, to, subject, body)
	logRow := MailLogRow{
		Type:     mailLogTypeTest,
		ToEmail:  &to,
		Subject:  &subject,
		CreatedAt: timePtr(d.now().UTC()),
	}
	if sendErr != nil {
		d.logger.Printf("admin Email test failed: %v", sendErr)
		failed := mailLogStatusFailed
		logRow.Status = failed
		em := sendErr.Error()
		logRow.Error = &em
		if err := d.store.CreateMailLog(ctxOf(c), logRow); err != nil {
			d.logger.Printf("admin 写测试邮件日志失败: %v", err)
		}
		d.fail(c, "发送失败："+sendErr.Error(), 500)
		return
	}
	sent := mailLogStatusSent
	logRow.Status = sent
	if err := d.store.CreateMailLog(ctxOf(c), logRow); err != nil {
		d.logger.Printf("admin 写测试邮件日志失败: %v", err)
	}
	_ = fromAddress
	d.success(c, nil, "测试邮件已发送")
}

// ---- 自动通知 ----

// notifyAllConfig 对齐 MailNotifyService::allConfig()（扁平键，enabled 存 '1'/''）。
func (d *MailDomain) notifyAllConfig(ctx context.Context) (*jsObj, error) {
	keys := []string{"notify_traffic_almost_threshold", "notify_expiring_days", "notify_admin_email"}
	for _, scene := range mailNotifyScenes {
		keys = append(keys,
			"notify_"+scene+"_enabled",
			"notify_"+scene+"_title",
			"notify_"+scene+"_body",
			"notify_"+scene+"_to",
		)
	}
	cfg, err := d.store.MailConfigMany(ctx, keys)
	if err != nil {
		return nil, err
	}

	out := &jsObj{}
	for _, scene := range mailNotifyScenes {
		def := mailNotifySceneDefaults[scene]
		enabled := cfg["notify_"+scene+"_enabled"] == "1"
		title := cfg["notify_"+scene+"_title"]
		if title == "" {
			title = def.Title
		}
		body := cfg["notify_"+scene+"_body"]
		if body == "" {
			body = def.Body
		}
		to := cfg["notify_"+scene+"_to"]
		if to == "" {
			to = def.To
		}
		out.put("notify_"+scene+"_enabled", boolAsFlag(enabled))
		out.put("notify_"+scene+"_title", title)
		out.put("notify_"+scene+"_body", body)
		out.put("notify_"+scene+"_to", to)
	}

	threshold := 90
	if s := cfg["notify_traffic_almost_threshold"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			threshold = n
		}
	}
	days := 3
	if s := cfg["notify_expiring_days"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			days = n
		}
	}
	out.put("notify_traffic_almost_threshold", threshold)
	out.put("notify_expiring_days", days)
	out.put("notify_admin_email", cfg["notify_admin_email"])
	return out, nil
}

// boolAsFlag 对齐 `$cfg['enabled'] ? '1' : ''`（前端用 === '1' 严格判断）。
func boolAsFlag(v bool) string {
	if v {
		return "1"
	}
	return ""
}

func (d *MailDomain) mailNotifyConfig(c *gin.Context) {
	out, err := d.notifyAllConfig(ctxOf(c))
	if err != nil {
		d.failServer(c, "读取通知配置", err)
		return
	}
	d.success(c, out)
}

// mailSaveNotifyConfig 对齐 PUT /admin-api/email/notify。
func (d *MailDomain) mailSaveNotifyConfig(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	for _, scene := range mailNotifyScenes {
		v.field("notify_"+scene+"_enabled", "nullable", "boolean")
		v.field("notify_"+scene+"_title", "nullable", "string", "max:255")
		v.field("notify_"+scene+"_body", "nullable", "string", "max:65535")
		v.field("notify_"+scene+"_to", "nullable", "in:user,admin")
	}
	v.field("notify_traffic_almost_threshold", "nullable", "integer", "min:1", "max:100")
	v.field("notify_expiring_days", "nullable", "integer", "min:0", "max:365")
	v.field("notify_admin_email", "nullable")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验通知配置", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}
	// email 规则单独校验（共享校验器没有 email 规则）
	if val, ok := data["notify_admin_email"]; ok && val != nil {
		if s, ok2 := inputString(val); ok2 && s != "" && !emailValid(s) {
			d.failValidation(c, "The notify admin email field must be a valid email address.")
			return
		}
	}

	updates := map[string]string{}
	for k, val := range data {
		if !containsString(emailNotifySaveKeys(), k) {
			continue
		}
		if k == "notify_admin_email" {
			if val == nil {
				updates[k] = ""
			} else {
				s, _ := inputString(val)
				updates[k] = s
			}
			continue
		}
		if strings.HasSuffix(k, "_enabled") {
			// checkbox 传的是 0/1，存成 '1' / ''（未配置 = 关闭）
			updates[k] = boolAsFlag(inputBool(val))
			continue
		}
		if val == nil {
			updates[k] = ""
		} else {
			s, _ := inputString(val)
			updates[k] = s
		}
	}

	if err := d.store.MailConfigSetMany(ctxOf(c), updates); err != nil {
		d.failServer(c, "保存通知配置", err)
		return
	}
	out, err := d.notifyAllConfig(ctxOf(c))
	if err != nil {
		d.failServer(c, "读取通知配置", err)
		return
	}
	d.success(c, out, "通知配置已保存")
}

// emailNotifySaveKeys 是 saveNotifyConfig 会写入的键（校验白名单）。
func emailNotifySaveKeys() []string {
	keys := []string{"notify_traffic_almost_threshold", "notify_expiring_days", "notify_admin_email"}
	for _, scene := range mailNotifyScenes {
		keys = append(keys,
			"notify_"+scene+"_enabled",
			"notify_"+scene+"_title",
			"notify_"+scene+"_body",
			"notify_"+scene+"_to",
		)
	}
	return keys
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// ---- 批量发信 ----

// mailBatchSend 对齐 POST /admin-api/email/batch-send。
func (d *MailDomain) mailBatchSend(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	v.field("subject", "required", "string", "max:255")
	v.field("body", "required", "string", "max:65535")
	v.field("mode", "required", "in:all,filter,single")
	v.field("plan_id", "nullable", "integer")
	v.field("user_status", "nullable", "in:normal,over,expired")
	v.field("single_query", "nullable", "string", "max:255")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验批量发信", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}

	cfg, err := d.store.MailConfigMany(ctxOf(c), []string{"smtp_host"})
	if err != nil {
		d.failServer(c, "读取邮件配置", err)
		return
	}
	if cfg["smtp_host"] == "" {
		d.fail(c, "请先配置 SMTP 服务器地址", 400)
		return
	}

	mode, _ := inputString(data["mode"])
	subject, _ := inputString(data["subject"])
	bodyText, _ := inputString(data["body"])

	recipients, resolveErr := d.resolveRecipients(ctxOf(c), data, mode)
	if resolveErr != nil {
		if mode == "single" {
			q := ""
			if s, ok := inputString(data["single_query"]); ok {
				q = strings.TrimSpace(s)
			}
			if q == "" {
				d.fail(c, "请填写要发给谁（邮箱或用户 ID）", 400)
			} else {
				d.fail(c, "没找到该用户", 404)
			}
			return
		}
		d.fail(c, "没有符合条件的用户", 400)
		return
	}

	now := d.now().UTC()
	batchID := now.Year()*10000000000 + int(now.Month())*100000000 + now.Day()*1000000 +
		now.Hour()*10000 + now.Minute()*100 + now.Second()
	batchIDInt := int64(batchID)

	rateEnabled, ratePerMinute := d.rateLimitConfig(ctxOf(c))
	total := 0
	for i, user := range recipients {
		delay := 0
		if rateEnabled {
			delay = (i / ratePerMinute) * 60
		}
		uid := user.ID
		job := MailJob{
			ToEmail:      user.Email,
			Subject:      subject,
			HTMLBody:     d.renderForUser(ctxOf(c), bodyText, user, now),
			Type:         mailLogTypeBatch,
			UserID:       &uid,
			BatchID:      &batchIDInt,
			DelaySeconds: delay,
		}
		if err := d.queue.Enqueue(job); err != nil {
			d.failServer(c, "批量发信入队", err)
			return
		}
		total++
	}

	rateLimited := any(nil)
	if rateEnabled {
		rateLimited = ratePerMinute
	}
	// 预计总时长：限速开启时按 ratePerMinute 推，否则按队列间隔（mail_send_interval_ms）推。
	// 给前端「预计 X 分钟发完」用；实际以 batch-progress 为准。
	intervalMs := int64(d.mailSendInterval() / time.Millisecond)
	var estimateSeconds int64
	if rateEnabled && ratePerMinute > 0 {
		estimateSeconds = int64(math.Ceil(float64(total) / float64(ratePerMinute) * 60))
	} else {
		estimateSeconds = int64(total) * intervalMs / 1000
	}
	d.success(c, jo(
		"batch_id", batchIDInt,
		"total", total,
		"rate_limited", rateLimited,
		"send_interval_ms", intervalMs,
		"estimate_seconds", estimateSeconds,
	), fmt.Sprintf("已加入发送队列，共 %d 封", total))
}

// mailBatchProgress 对齐 GET /admin-api/email/batch-progress?batch_id=xxx。
//
// goroutine 队列不落库，所以「还剩几封」只能由队列内存水位 + 库里的既成事实拼出来：
// sent/failed 数库，queued/sending 读队列。重启进程后 queued 归零、sent/failed 保留。
func (d *MailDomain) mailBatchProgress(c *gin.Context) {
	raw := ""
	if s := queryString(c, "batch_id"); s != nil {
		raw = strings.TrimSpace(*s)
	}
	batchID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || batchID <= 0 {
		d.failValidation(c, "batch_id 无效")
		return
	}

	p, err := d.store.MailBatchProgress(ctxOf(c), batchID)
	if err != nil {
		d.failServer(c, "查询批次进度", err)
		return
	}

	var queued, sending, qfailed int64
	if stater, ok := d.queue.(MailQueueStater); ok {
		st := stater.Stats()
		queued, sending, qfailed = st.Pending, st.Running, st.Failed
	}

	// total 取「库里已发 + 队列里还排着的」：两者相加就是本批还没发完的量，
	// 比复算用户表更贴近实际（用户状态会变）。done 到位时 total == sent+failed。
	total := p.Sent + p.Failed + queued + sending
	d.success(c, jo(
		"batch_id", p.BatchID,
		"total", total,
		"sent", p.Sent,
		"failed", p.Failed,
		"remaining", queued+sending,
		"queued", queued,
		"sending", sending,
		"queue_failed", qfailed,
		"done", queued == 0 && sending == 0,
		"first_at", nullableEloq(p.FirstAt),
		"last_at", nullableEloq(p.LastAt),
	))
}

// resolveRecipients 对齐 MailBatchService::resolveRecipients（找不到人返回 error）。
func (d *MailDomain) resolveRecipients(ctx context.Context, data map[string]any, mode string) ([]MailUser, error) {
	if mode == "single" {
		q := ""
		if s, ok := inputString(data["single_query"]); ok {
			q = strings.TrimSpace(s)
		}
		if q == "" {
			return nil, errNoMailRecipient
		}
		user, err := d.store.MailUserByLogin(ctx, q)
		if err != nil || user == nil {
			return nil, errNoMailRecipient
		}
		return []MailUser{*user}, nil
	}

	filter := MailRecipientFilter{UserStatus: "normal"}
	if s, ok := inputString(data["user_status"]); ok && s != "" {
		filter.UserStatus = s
	}
	if val, ok := data["plan_id"]; ok && val != nil {
		id := inputInt(val)
		filter.PlanID = &id
	}
	users, err := d.store.MailRecipients(ctx, filter, d.now().UTC())
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, errNoMailRecipient
	}
	return users, nil
}

// errNoMailRecipient 对齐 MailBatchService::ERR_NO_RECIPIENT。
var errNoMailRecipient = fmt.Errorf("no_recipient")

// rateLimitConfig 对齐 MailBatchService::rateLimitConfig（「限制」页全局配置）。
func (d *MailDomain) rateLimitConfig(ctx context.Context) (bool, int) {
	cfg, err := d.store.MailConfigMany(ctx, []string{"mail_rate_limit_enabled", "mail_rate_per_minute"})
	if err != nil {
		return false, 30
	}
	perMinute := 30
	if s := cfg["mail_rate_per_minute"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 {
			perMinute = n
		}
	}
	return cfg["mail_rate_limit_enabled"] == "1", perMinute
}

// mailSendInterval 读 mail_send_interval_ms（毫秒）——串行 worker 两封之间的间隔。
//
// 缺失键/坏值/负数一律按 0（不等），保持既有行为；配置在每封发完后重读，
// 改配置后下一封立即生效。
func (d *MailDomain) mailSendInterval() time.Duration {
	cfg, err := d.store.MailConfigMany(context.Background(), []string{"mail_send_interval_ms"})
	if err != nil {
		d.logger.Printf("admin 读发信间隔配置失败: %v", err)
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(cfg["mail_send_interval_ms"]))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// renderForUser 对齐 MailBatchService::renderForUser。
func (d *MailDomain) renderForUser(ctx context.Context, body string, user MailUser, now time.Time) string {
	limit := user.TrafficLimit
	used := user.TrafficUsed
	percent := "0"
	if limit > 0 {
		percent = formatPHPPercent(used, limit)
	}
	expireDate := "—"
	daysLeft := "0"
	if user.ExpiredAt != nil {
		expireDate = user.ExpiredAt.UTC().Format("2006-01-02")
		daysLeft = strconv.Itoa(daysLeftFrom(now, *user.ExpiredAt))
	}
	planName := "无"
	if user.PlanName != nil && *user.PlanName != "" {
		planName = *user.PlanName
	}
	subURL := ""
	if user.Token != "" {
		subURL = "/api/sub/" + user.Token
	}
	siteTitle := d.siteTitle(ctx)
	vars := map[string]string{
		"{{email}}":         user.Email,
		"{{user_id}}":       strconv.FormatUint(user.ID, 10),
		"{{site_title}}":    siteTitle,
		"{{used}}":          formatBytesPHP(used),
		"{{limit}}":         formatBytesPHP(limit),
		"{{percent}}":       percent,
		"{{plan_name}}":     planName,
		"{{expire_date}}":   expireDate,
		"{{days_left}}":     daysLeft,
		"{{subscribe_url}}": subURL,
	}
	out := body
	for k, val := range vars {
		out = strings.ReplaceAll(out, k, val)
	}
	return out
}

func (d *MailDomain) siteTitle(ctx context.Context) string {
	cfg, err := d.store.MailConfigMany(ctx, []string{"site_title"})
	if err != nil {
		return "ControlHub"
	}
	if cfg["site_title"] == "" {
		return "ControlHub"
	}
	return cfg["site_title"]
}

// formatPHPPercent 对齐 `round($used / $limit * 100, 1)` 的字符串形态。
func formatPHPPercent(used, limit int64) string {
	p := math.Round(float64(used)/float64(limit)*100*10) / 10
	return strconv.FormatFloat(p, 'f', -1, 64)
}

// daysLeftFrom 对齐 `(int) ceil(now()->diffInDays($user->expired_at, false))`。
func daysLeftFrom(now, expired time.Time) int {
	days := expired.Sub(now).Hours() / 24
	return int(math.Ceil(days))
}

// formatBytesPHP 对齐 MailBatchService::formatBytes。
func formatBytesPHP(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	n := float64(bytes)
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	r := math.Round(n*100) / 100
	return strconv.FormatFloat(r, 'f', -1, 64) + " " + units[i]
}

// ---- 按月定时发信 ----

var scheduleMailKeys = []string{"schedule_mail_enabled", "schedule_mail_day", "schedule_mail_time", "schedule_mail_config"}

// mailScheduleConfig 对齐 GET /admin-api/email/schedule-mail（MailScheduleService::config）。
func (d *MailDomain) mailScheduleConfig(c *gin.Context) {
	cfg, err := d.store.MailConfigMany(ctxOf(c), scheduleMailKeys)
	if err != nil {
		d.failServer(c, "读取定时发信配置", err)
		return
	}
	d.success(c, d.scheduleConfigView(cfg))
}

func (d *MailDomain) scheduleConfigView(cfg map[string]string) *jsObj {
	day := 1
	if s := cfg["schedule_mail_day"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			day = n
		}
	}
	t := cfg["schedule_mail_time"]
	if t == "" {
		t = "09:00"
	}
	out := &jsObj{}
	out.put("enabled", cfg["schedule_mail_enabled"] == "1")
	out.put("day", day)
	out.put("time", t)
	out.put("config", decodeScheduleConfig(cfg["schedule_mail_config"]))
	return out
}

// decodeScheduleConfig 对齐 `json_decode($raw, true) ?: []`（坏 JSON / 空串 → 空对象）。
func decodeScheduleConfig(raw string) *jsObj {
	out := &jsObj{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	dec := newJSONDecoder(raw)
	var generic map[string]any
	if err := dec.Decode(&generic); err != nil {
		return out
	}
	// 保序：PHP json_decode 保键序，这里按字典序稳定输出（前端按字段名取值）
	for _, k := range sortedKeys(generic) {
		out.put(k, normalizeJSONValue(generic[k]))
	}
	return out
}

func normalizeJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := &jsObj{}
		for _, k := range sortedKeys(t) {
			out.put(k, normalizeJSONValue(t[k]))
		}
		return out
	case []any:
		out := make(jsArr, 0, len(t))
		for _, item := range t {
			out = append(out, normalizeJSONValue(item))
		}
		return out
	default:
		return v
	}
}

// mailSaveScheduleConfig 对齐 PUT /admin-api/email/schedule-mail（MailScheduleService::save）。
func (d *MailDomain) mailSaveScheduleConfig(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	v.field("enabled", "nullable", "boolean")
	v.field("day", "nullable", "integer", "min:1")
	v.field("time", "nullable", "string", "regex:/^([01]\\d|2[0-3]):[0-5]\\d$/")
	v.field("config", "nullable", "array")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验定时发信配置", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}

	// config.* 子字段：共享校验器按字面键查值，这里手工展开校验（与 Laravel 点号规则同语义）
	var subErrs []string
	sub, _ := data["config"].(map[string]any)
	checkSub := func(name, rule string, allow func(any) (bool, string)) {
		val, ok := sub[name]
		if !ok || val == nil {
			return
		}
		ok2, msg := allow(val)
		if !ok2 {
			subErrs = append(subErrs, msg)
		}
	}
	checkSub("subject", "", func(val any) (bool, string) {
		s, ok := inputString(val)
		if !ok {
			return false, "The config.subject field must be a string."
		}
		if len([]rune(s)) > 255 {
			return false, "The config.subject field must not be greater than 255 characters."
		}
		return true, ""
	})
	checkSub("body", "", func(val any) (bool, string) {
		s, ok := inputString(val)
		if !ok {
			return false, "The config.body field must be a string."
		}
		if len([]rune(s)) > 65535 {
			return false, "The config.body field must not be greater than 65535 characters."
		}
		return true, ""
	})
	checkSub("mode", "", func(val any) (bool, string) {
		s, ok := inputString(val)
		if !ok || !containsString([]string{"all", "filter", "single"}, s) {
			return false, "The selected config.mode is invalid."
		}
		return true, ""
	})
	checkSub("plan_id", "", func(val any) (bool, string) {
		if !phpIntegerPasses(val) {
			return false, "The config.plan_id field must be an integer."
		}
		return true, ""
	})
	checkSub("user_status", "", func(val any) (bool, string) {
		s, ok := inputString(val)
		if !ok || !containsString([]string{"normal", "over", "expired"}, s) {
			return false, "The selected config.user_status is invalid."
		}
		return true, ""
	})
	checkSub("single_query", "", func(val any) (bool, string) {
		s, ok := inputString(val)
		if !ok {
			return false, "The config.single_query field must be a string."
		}
		if len([]rune(s)) > 255 {
			return false, "The config.single_query field must not be greater than 255 characters."
		}
		return true, ""
	})
	if len(subErrs) > 0 {
		d.failValidation(c, validateSummary(subErrs))
		return
	}

	wasEnabled := false
	{
		cur, err := d.store.MailConfigMany(ctxOf(c), []string{"schedule_mail_enabled"})
		if err != nil {
			d.failServer(c, "读取定时发信配置", err)
			return
		}
		wasEnabled = cur["schedule_mail_enabled"] == "1"
	}

	updates := map[string]string{}
	enabledVal := false
	if val, ok := data["enabled"]; ok {
		enabledVal = inputBool(val)
	}
	updates["schedule_mail_enabled"] = boolAsFlag(enabledVal)

	day := 1
	if val, ok := data["day"]; ok && val != nil {
		day = int(inputInt(val))
	}
	if day < 1 {
		day = 1
	} else if day > 31 {
		day = 31
	}
	updates["schedule_mail_day"] = strconv.Itoa(day)

	t := "09:00"
	if val, ok := data["time"]; ok && val != nil {
		if s, ok2 := inputString(val); ok2 && s != "" {
			if scheduleTimeRe.MatchString(s) {
				t = s
			}
		}
	}
	updates["schedule_mail_time"] = t

	cfgJSON := "{}"
	if raw, ok := data["config"]; ok && raw != nil {
		cfgJSON = encodeScheduleConfig(raw)
	}
	updates["schedule_mail_config"] = cfgJSON

	// 关→开转变：记下开启时刻（DateTimeString 口径）
	if !wasEnabled && enabledVal {
		updates["schedule_mail_armed_at"] = d.now().UTC().Format("2006-01-02 15:04:05")
	}

	if err := d.store.MailConfigSetMany(ctxOf(c), updates); err != nil {
		d.failServer(c, "保存定时发信配置", err)
		return
	}

	cfg, err := d.store.MailConfigMany(ctxOf(c), scheduleMailKeys)
	if err != nil {
		d.failServer(c, "读取定时发信配置", err)
		return
	}
	d.success(c, d.scheduleConfigView(cfg), "定时发信配置已保存")
}

// encodeScheduleConfig 对齐 `json_encode($cfg, JSON_UNESCAPED_UNICODE)`。
func encodeScheduleConfig(raw any) string {
	switch t := raw.(type) {
	case map[string]any:
		obj := &jsObj{}
		// PHP 数组/对象键序 = 提交顺序；Go map 无序，按字典序稳定
		for _, k := range sortedKeys(t) {
			obj.put(k, t[k])
		}
		return string(phpEncode(obj))
	default:
		return string(phpEncode(raw))
	}
}

// ---- 发信日志 ----

// mailLogs 对齐 GET /admin-api/email/logs（手写 pagination，页码**不**夹回最后一页）。
func (d *MailDomain) mailLogs(c *gin.Context) {
	typ := ""
	if s := queryString(c, "type"); s != nil {
		typ = *s
	}
	status := ""
	if s := queryString(c, "status"); s != nil {
		status = *s
	}
	page := int64(1)
	if s := queryString(c, "page"); s != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(*s)); err == nil && n >= 1 {
			page = int64(n)
		}
	}

	// 校验（对齐 $request->validate 的 query 规则）
	if typ != "" && !containsString([]string{"notify", "batch", "test", "schedule"}, typ) {
		d.failValidation(c, "The selected type is invalid.")
		return
	}
	if status != "" && !containsString([]string{"sent", "failed"}, status) {
		d.failValidation(c, "The selected status is invalid.")
		return
	}

	filter := MailLogFilter{Type: typ, Status: status}
	rows, total, err := d.store.MailLogPage(ctxOf(c), filter, page, 50)
	if err != nil {
		d.failServer(c, "查询发信日志", err)
		return
	}

	items := make(jsArr, 0, len(rows))
	for i := range rows {
		items = append(items, presentMailLog(&rows[i]))
	}
	lastPage := (total + 50 - 1) / 50
	if lastPage < 1 {
		lastPage = 1
	}
	writeJSON(c, phpEncode(jo(
		"code", 0,
		"msg", "ok",
		"data", items,
		"pagination", jo(
			"current_page", page,
			"last_page", lastPage,
			"total", total,
			"per_page", 50,
		),
	)))
}

// presentMailLog 对齐 EmailController::mailLogs 的 map。
func presentMailLog(l *MailLogRow) *jsObj {
	toUserID := any(nil)
	if l.ToUserID != nil {
		toUserID = *l.ToUserID
	}
	batchID := any(nil)
	if l.BatchID != nil {
		batchID = *l.BatchID
	}
	return jo(
		"id", l.ID,
		"type", l.Type,
		"status", l.Status,
		"to_email", nullableStr(l.ToEmail),
		"to_user_id", toUserID,
		"subject", nullableStr(l.Subject),
		"scene", nullableStr(l.Scene),
		"batch_id", batchID,
		"error", nullableStr(l.Error),
		"created_at", nullableISO(l.CreatedAt),
	)
}

// timePtr 便于测试构造。
func timePtr(t time.Time) *time.Time { return &t }
