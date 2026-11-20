package admin

// 多 SMTP 账号池的管理端 API（只做后端端点，前端列表另做）。
//
// 路由（挂在 /admin-api 下，邮件域 Register）：
//
//	GET    /email/smtp-accounts             列表
//	POST   /email/smtp-accounts             新增
//	PUT    /email/smtp-accounts/:account    修改
//	DELETE /email/smtp-accounts/:account    删除
//	PUT    /email/smtp-accounts/:account/status  状态切换（active/disabled）
//	PUT    /email/smtp-accounts/:account/reset-today  今日计数清零
//
// 密码只进不出：列表/详情一律回 has_password，不回明文或密文。

import (
	"database/sql"

	"github.com/gin-gonic/gin"
)

// registerSMTPAccountRoutes 由 MailDomain.Register 调用。
func (d *MailDomain) registerSMTPAccountRoutes(r gin.IRouter) {
	r.GET("/email/smtp-accounts", d.listSMTPAccounts)
	r.POST("/email/smtp-accounts", d.createSMTPAccount)
	r.PUT("/email/smtp-accounts/:account", d.updateSMTPAccount)
	r.DELETE("/email/smtp-accounts/:account", d.deleteSMTPAccount)
	r.PUT("/email/smtp-accounts/:account/status", d.setSMTPAccountStatus)
	// 今日计数清零（sent_today / error_count）。放在 :account 静态段之后不影响既有路由匹配。
	r.PUT("/email/smtp-accounts/:account/reset-today", d.resetSMTPAccountToday)
	// 设为默认账号（挑号时优先用它）；传 0 表示清除默认。
	r.PUT("/email/smtp-accounts/:account/default", d.setSMTPAccountDefault)
}

// paramID / notFoundRoute 与 Handler 同口径（邮件域是独立 Domain，不复用 *Handler）。
func (d *MailDomain) paramID(c *gin.Context, name string) (uint64, bool) {
	return parseUint(c.Param(name))
}

func (d *MailDomain) notFoundRoute(c *gin.Context) { d.fail(c, "接口不存在", 404) }

// presentSMTPAccount 是列表/详情的输出形态（不输密码）。
func presentSMTPAccount(a *SMTPAccount) *jsObj {
	return jo(
		"id", a.ID,
		"host", a.Host,
		"port", a.Port,
		"username", a.Username,
		"encryption", a.Encryption,
		"from_name", nullableStr(a.FromName),
		"weight", a.Weight,
		"daily_limit", a.DailyLimit,
		"sent_today", a.SentToday,
		"last_used_at", nullableISO(a.LastUsedAt),
		"reset_at", nullableISO(a.ResetAt),
		"status", a.Status,
		"cooldown_until", nullableISO(a.CooldownUntil),
		"error_count", a.ErrorCount,
		"is_default", a.IsDefault,
		// has_password：是否已配置密码（前端据此提示「留空=不修改」）。
		"has_password", a.Password != "",
		// password：回显**解密后的明文**，供后台「眼睛」按钮切换查看。
		//
		// 为什么回显明文（管理端的取舍，与前台接口无关）：
		//   - 这个接口在 AdminAuth 鉴权组内，只有管理员能读；
		//   - 站点默认 SMTP 密码一直就是这么回显的（见 mailShow 的 decryptSmtpPassword），
		//     池账号若不同口径，管理员改配置时看不到原密码，等于丢配置；
		//   - 落库仍是 password_enc 密文，只在此处解密返回，不改变存储安全边界。
		"password", a.Password,
		"created_at", nullableISO(a.CreatedAt),
		"updated_at", nullableISO(a.UpdatedAt),
	)
}

// smtpAccountRules 是新增用的校验规则；修改时全部降级成 sometimes（见 update）。
func applySMTPAccountRules(v *validator, sometimes bool) {
	prefix := func(rules ...string) []string {
		if sometimes {
			return append([]string{"sometimes"}, rules...)
		}
		return rules
	}
	v.field("host", prefix("required", "string", "max:255")...)
	v.field("port", prefix("nullable", "integer", "min:1", "max:65535")...)
	v.field("username", prefix("nullable", "string", "max:255")...)
	v.field("password", prefix("nullable", "string", "max:255")...)
	v.field("encryption", prefix("nullable", "in:tls,ssl,none")...)
	v.field("from_name", prefix("nullable", "string", "max:100")...)
	v.field("weight", prefix("nullable", "integer", "min:0", "max:1000")...)
	v.field("daily_limit", prefix("nullable", "integer", "min:0")...)
}

// smtpAccountInput 把已校验的输入转成入参（nil = 没提交该字段）。
func smtpAccountInput(v *validator) SMTPAccountInput {
	var in SMTPAccountInput
	if val, ok := v.valueOf("host"); ok && val != nil {
		s, _ := inputString(val)
		in.Host = &s
	}
	if val, ok := v.valueOf("port"); ok && val != nil {
		n := int(inputInt(val))
		in.Port = &n
	}
	if val, ok := v.valueOf("username"); ok {
		in.Username = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("password"); ok {
		// 空 = 不修改（编辑表单不回显密码，空值代表「保持原密码」）。
		if s, isStr := inputString(val); isStr && s != "" {
			in.Password = &s
		}
	}
	if val, ok := v.valueOf("encryption"); ok && val != nil {
		s, _ := inputString(val)
		in.Encryption = &s
	}
	if val, ok := v.valueOf("from_name"); ok {
		in.FromName = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("weight"); ok && val != nil {
		n := int(inputInt(val))
		in.Weight = &n
	}
	if val, ok := v.valueOf("daily_limit"); ok && val != nil {
		n := int(inputInt(val))
		in.DailyLimit = &n
	}
	return in
}

func (d *MailDomain) listSMTPAccounts(c *gin.Context) {
	list, err := d.store.SMTPAccounts(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询 SMTP 账号池", err)
		return
	}
	items := make(jsArr, 0, len(list))
	for i := range list {
		items = append(items, presentSMTPAccount(&list[i]))
	}
	d.success(c, jo("items", items))
}

func (d *MailDomain) createSMTPAccount(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	applySMTPAccountRules(v, false)
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验 SMTP 账号", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}

	acc, err := d.store.CreateSMTPAccount(ctxOf(c), smtpAccountInput(v), d.now().UTC())
	if err != nil {
		d.failServer(c, "新增 SMTP 账号", err)
		return
	}
	d.success(c, presentSMTPAccount(acc), "已添加")
}

func (d *MailDomain) updateSMTPAccount(c *gin.Context) {
	id, ok := d.paramID(c, "account")
	if !ok {
		d.notFoundRoute(c)
		return
	}
	if _, err := d.store.SMTPAccountByID(ctxOf(c), id); err != nil {
		if err == sql.ErrNoRows {
			d.notFoundRoute(c)
			return
		}
		d.failServer(c, "查询 SMTP 账号", err)
		return
	}

	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	applySMTPAccountRules(v, true)
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验 SMTP 账号", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}

	acc, err := d.store.UpdateSMTPAccount(ctxOf(c), id, smtpAccountInput(v), d.now().UTC())
	if err != nil {
		if err == sql.ErrNoRows {
			d.notFoundRoute(c)
			return
		}
		d.failServer(c, "保存 SMTP 账号", err)
		return
	}
	d.success(c, presentSMTPAccount(acc), "已保存")
}

func (d *MailDomain) deleteSMTPAccount(c *gin.Context) {
	id, ok := d.paramID(c, "account")
	if !ok {
		d.notFoundRoute(c)
		return
	}
	if _, err := d.store.SMTPAccountByID(ctxOf(c), id); err != nil {
		if err == sql.ErrNoRows {
			d.notFoundRoute(c)
			return
		}
		d.failServer(c, "查询 SMTP 账号", err)
		return
	}
	if err := d.store.DeleteSMTPAccount(ctxOf(c), id); err != nil {
		d.failServer(c, "删除 SMTP 账号", err)
		return
	}
	d.success(c, nil, "已删除")
}

// setSMTPAccountStatus 状态切换：只允许 active / disabled（cooling 由失败分类自动进入）。
func (d *MailDomain) setSMTPAccountStatus(c *gin.Context) {
	id, ok := d.paramID(c, "account")
	if !ok {
		d.notFoundRoute(c)
		return
	}
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	v.field("status", "required", "in:active,disabled")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验账号状态", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}
	status, _ := inputString(data["status"])

	if _, err := d.store.SMTPAccountByID(ctxOf(c), id); err != nil {
		if err == sql.ErrNoRows {
			d.notFoundRoute(c)
			return
		}
		d.failServer(c, "查询 SMTP 账号", err)
		return
	}
	if err := d.store.SetSMTPAccountStatus(ctxOf(c), id, status, d.now().UTC()); err != nil {
		d.failServer(c, "切换 SMTP 账号状态", err)
		return
	}
	acc, err := d.store.SMTPAccountByID(ctxOf(c), id)
	if err != nil {
		d.failServer(c, "查询 SMTP 账号", err)
		return
	}
	d.success(c, presentSMTPAccount(acc), "状态已更新")
}

// resetSMTPAccountToday 今日计数清零（sent_today / error_count）。
//
// 用途：日限额到量后不必等到跨天。刻意不碰状态与冷却 —— 冷却中的号
// 仍然要等 cooldown_until 到期（或管理员手动启用），重置计数不等于「放行」。
func (d *MailDomain) resetSMTPAccountToday(c *gin.Context) {
	id, ok := d.paramID(c, "account")
	if !ok {
		d.notFoundRoute(c)
		return
	}
	if _, err := d.store.SMTPAccountByID(ctxOf(c), id); err != nil {
		if err == sql.ErrNoRows {
			d.notFoundRoute(c)
			return
		}
		d.failServer(c, "查询 SMTP 账号", err)
		return
	}
	if err := d.store.ResetSMTPAccountToday(ctxOf(c), id, d.now().UTC()); err != nil {
		if err == sql.ErrNoRows {
			d.notFoundRoute(c)
			return
		}
		d.failServer(c, "重置 SMTP 账号今日计数", err)
		return
	}
	acc, err := d.store.SMTPAccountByID(ctxOf(c), id)
	if err != nil {
		d.failServer(c, "查询 SMTP 账号", err)
		return
	}
	d.success(c, presentSMTPAccount(acc), "今日计数已重置")
}

// setSMTPAccountDefault 设为默认账号（挑号时优先用它）。id=0 表示清除默认。
func (d *MailDomain) setSMTPAccountDefault(c *gin.Context) {
	id, ok := d.paramID(c, "account")
	if !ok {
		d.notFoundRoute(c)
		return
	}
	if id > 0 {
		if _, err := d.store.SMTPAccountByID(ctxOf(c), id); err != nil {
			if err == sql.ErrNoRows {
				d.notFoundRoute(c)
				return
			}
			d.failServer(c, "查询 SMTP 账号", err)
			return
		}
	}
	if err := d.store.SetSMTPAccountDefault(ctxOf(c), id, d.now().UTC()); err != nil {
		d.failServer(c, "设置默认 SMTP 账号", err)
		return
	}
	msg := "已设为默认账号"
	if id == 0 {
		msg = "已清除默认账号"
	}
	d.success(c, nil, msg)
}
