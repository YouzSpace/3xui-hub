package api

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 邮箱验证码 / 找回密码的缓存键（与 PHP 逐字一致，并存期共享同一批 cache 行）。
const (
	emailVerifyPrefix = "email_verify_"
	pwdResetPrefix    = "pwd_reset_"
	codeTTLSeconds    = 300 // 5 分钟
)

func emailVerifyKey(email string) string { return emailVerifyPrefix + email }
func pwdResetKey(email string) string    { return pwdResetPrefix + email }

// 文案常量与 PHP 逐字对齐。
const (
	msgCodeSent          = "验证码已发送"
	msgResetSent         = "验证码已发送" // PasswordResetController::SENT_MSG
	msgCodeInvalid       = "验证码错误"
	msgCodeInvalidOrExp  = "验证码错误或已过期" // PasswordResetController::CODE_INVALID_MSG
	msgEmailNotEnabled   = "未启用邮箱验证"
	msgSMTPNotConfigured = "SMTP未配置"
	msgPasswordReset     = "密码已重置"
)

// 找回密码的邮件模板与主题（对齐 PasswordResetController：不读 SiteConfig 的 email_template）。
const (
	resetMailSubject = "找回密码"
	resetMailBody    = `<div style="padding:20px;font-family:sans-serif"><h2>找回密码</h2>` +
		`<p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p>` +
		`<p style="color:#666">5 分钟内有效，请勿泄露。</p></div>`
)

// defaultRegisterMailBody 是注册验证码在 SiteConfig.email_template 为空时的兜底模板（逐字对齐 PHP）。
const defaultRegisterMailBody = `<div style="padding:20px;font-family:sans-serif"><h2>注册验证码</h2>` +
	`<p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p>` +
	`<p style="color:#666">5分钟内有效，请勿泄露。</p></div>`

// smtpConfigKeys 与 SimpleMailerService::CONFIG_KEYS 一致。
var smtpConfigKeys = []string{
	"smtp_host", "smtp_port", "smtp_username", "smtp_password",
	"smtp_encryption", "smtp_from_name", "email_template",
}

// mailChannelReady 报告验证码邮件通道是否可用。
//
// 口径：优先看账号池有没有可用号（验证码统一走池）；池不可用再看站点 smtp_host。
// 保持向后兼容：早期只配站点 smtp_* 的部署依然判为可用。
func (h *Handler) mailChannelReady(ctx context.Context) bool {
	if pm, ok := h.mailer.(interface{ Configured(context.Context) bool }); ok {
		return pm.Configured(ctx)
	}
	// 非 SMTPMailer 实现（测试桩等）：退回旧口径，只看站点配置。
	cfg, err := h.store.SiteConfigMany(ctx, []string{"smtp_host"})
	if err != nil {
		return false
	}
	return phpTrimSpaceOnly(cfg["smtp_host"]) != ""
}

// handleEmailVerifySend 对齐 EmailVerifyController::sendCode。
//
// 注意它**不**做防枚举（注册验证码本来就只发给「还没注册的邮箱」这件事由注册接口兜住）：
// 未启用邮箱验证 → HTTP 400 + code 400；SMTP 未配置 → HTTP 500 + code 500；发信失败 → HTTP 500。
func (h *Handler) handleEmailVerifySend(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(input).field("email", "required", "email")
	if !v.passes() {
		respondValidation(c, v)
		return
	}
	email := input["email"].(string)

	ctx := c.Request.Context()
	ip := clientIP(c)
	limits := h.loadRateLimits(ctx)

	// 入口先过限流，再管「有没有开邮箱验证 / SMTP 配没配」这些分支。
	if blocked, err := h.codeSendBlocked(ctx, email, ip, limits); err != nil {
		h.logger.Printf("email-verify/send 限流查询失败: %v", err)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}
	// 放行即记数（与 /api/password-reset/send 共享同一套计数）。
	if err := h.recordCodeSend(ctx, email, ip, limits); err != nil {
		h.logger.Printf("email-verify/send 记数失败: %v", err)
	}

	configs, err := h.store.SiteConfigMany(ctx, append([]string{"register_email_verify"}, smtpConfigKeys...))
	if err != nil {
		h.logger.Printf("email-verify/send 读取配置失败: %v", err)
	}
	if !phpTruthy(configs["register_email_verify"]) {
		failStatus(c, msgEmailNotEnabled, 400, http.StatusBadRequest)
		return
	}
	// 邮件通道可用性：账号池有可用号即可（验证码统一走池），
	// 池不可用时才回落到站点 smtp_host 判断。见 SMTPMailer.Configured。
	if !h.mailChannelReady(ctx) {
		failStatus(c, msgSMTPNotConfigured, 500, http.StatusInternalServerError)
		return
	}

	code := randomDigits(6)
	// 存验证码（5 分钟有效）。注意 PHP 这里是 Cache::put，覆盖是正常的。
	if err := h.cache.Put(ctx, emailVerifyKey(email), code, time.Duration(codeTTLSeconds)*time.Second); err != nil {
		h.logger.Printf("email-verify/send 写验证码失败: %v", err)
		failStatus(c, "发送失败：缓存不可用", 500, http.StatusInternalServerError)
		return
	}

	fromName := configs["smtp_from_name"]
	if fromName == "" {
		fromName = "ControlHub"
	}
	template := configs["email_template"]
	if template == "" {
		template = defaultRegisterMailBody
	}
	body := strings.ReplaceAll(template, "{{code}}", code)

	if h.mailer == nil {
		failStatus(c, "发送失败：邮件服务不可用", 500, http.StatusInternalServerError)
		return
	}
	if err := h.mailer.Send(ctx, email, fromName, body); err != nil {
		h.logger.Printf("Send email verify code failed: %v", err)
		failStatus(c, "发送失败："+err.Error(), 500, http.StatusInternalServerError)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", msgCodeSent, "data", nil)))
}

// handleEmailVerifyCheck 对齐 EmailVerifyController::verifyCode。
func (h *Handler) handleEmailVerifyCheck(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(input).field("email", "required", "email").field("code", "required", "string", "size:6")
	if !v.passes() {
		respondValidation(c, v)
		return
	}
	email := input["email"].(string)
	code := input["code"].(string)

	ctx := c.Request.Context()
	ip := clientIP(c)
	limits := h.loadRateLimits(ctx)

	// 试错打满后锁定期内一律拒，正确验证码也不例外。
	if blocked, err := h.verifyBlocked(ctx, email, ip, limits); err != nil {
		h.logger.Printf("email-verify/check 限流查询失败: %v", err)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}

	cached, ok, err := h.cache.Get(ctx, emailVerifyKey(email))
	if err != nil {
		h.logger.Printf("email-verify/check 读取验证码失败: %v", err)
	}
	if !ok || cachedString(cached) != code {
		if rerr := h.recordVerifyFailure(ctx, email, ip, limits); rerr != nil {
			h.logger.Printf("email-verify/check 记失败次数失败: %v", rerr)
		}
		failStatus(c, msgCodeInvalid, 400, http.StatusBadRequest)
		return
	}

	if err := h.cache.Forget(ctx, emailVerifyKey(email)); err != nil {
		h.logger.Printf("email-verify/check 删除验证码失败: %v", err)
	}
	if err := h.clearVerifyFailures(ctx, email, ip); err != nil {
		h.logger.Printf("email-verify/check 清失败次数失败: %v", err)
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "验证成功", "data", nil)))
}

// handlePasswordResetSend 对齐 PasswordResetController::sendCode（严格防枚举）。
//
// 铁律（改这里前先读 PHP 类注释）：
//   - 校验/限流都排在「邮箱是否注册」之前；
//   - 未注册 / SMTP 未配置 / 发信失败 一律返回同一条 SENT_MSG，只记日志；
//   - 只有真正发信成功才落码。
func (h *Handler) handlePasswordResetSend(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(input).field("email", "required", "email")
	if !v.passes() {
		respondValidation(c, v)
		return
	}
	email := input["email"].(string)

	ctx := c.Request.Context()
	ip := clientIP(c)
	limits := h.loadRateLimits(ctx)

	if blocked, err := h.codeSendBlocked(ctx, email, ip, limits); err != nil {
		h.logger.Printf("password-reset/send 限流查询失败: %v", err)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}
	// 放行即记数：未注册 / SMTP 未配置 / 发信失败等分支下面早退，但账已经记上了。
	if err := h.recordCodeSend(ctx, email, ip, limits); err != nil {
		h.logger.Printf("password-reset/send 记数失败: %v", err)
	}

	exists, err := h.store.EmailExists(ctx, email)
	if err != nil {
		h.logger.Printf("password-reset/send 查询邮箱失败: %v", err)
		success(c, nil, msgResetSent) // 仍返回同一响应，不泄露内部错误
		return
	}
	if !exists {
		h.logger.Printf("password reset for unregistered email: %s", emailLogTag(email))
		success(c, nil, msgResetSent)
		return
	}

	if !h.mailChannelReady(ctx) {
		h.logger.Printf("password reset skipped: 邮件通道未配置/无可用账号: %s", emailLogTag(email))
		success(c, nil, msgResetSent)
		return
	}

	code := randomDigits(6)
	body := strings.ReplaceAll(resetMailBody, "{{code}}", code)

	if h.mailer == nil {
		h.logger.Printf("Send password reset code failed: 邮件服务不可用 %s", emailLogTag(email))
		success(c, nil, msgResetSent)
		return
	}
	if err := h.mailer.Send(ctx, email, resetMailSubject, body); err != nil {
		h.logger.Printf("Send password reset code failed: %v %s", err, emailLogTag(email))
		success(c, nil, msgResetSent)
		return
	}

	// 只有真正发信成功才落码（5 分钟有效）：发不出去的码不允许用来改密。
	if err := h.cache.Put(ctx, pwdResetKey(email), code, time.Duration(codeTTLSeconds)*time.Second); err != nil {
		h.logger.Printf("password-reset/send 写验证码失败: %v %s", err, emailLogTag(email))
		success(c, nil, msgResetSent)
		return
	}

	success(c, nil, msgResetSent)
}

// handlePasswordResetReset 对齐 PasswordResetController::reset。
func (h *Handler) handlePasswordResetReset(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(input).
		field("email", "required", "email").
		field("code", "required", "string", "size:6").
		field("password", "required", "string", "min:6")
	if !v.passes() {
		respondValidation(c, v)
		return
	}
	email := input["email"].(string)
	code := input["code"].(string)
	password := input["password"].(string)

	ctx := c.Request.Context()
	ip := clientIP(c)
	limits := h.loadRateLimits(ctx)

	// 试错次数打满后，锁定期内连正确验证码也一律拒（防在线穷举 6 位码）。
	if blocked, err := h.verifyBlocked(ctx, email, ip, limits); err != nil {
		h.logger.Printf("password-reset/reset 限流查询失败: %v", err)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}

	cached, ok, err := h.cache.Get(ctx, pwdResetKey(email))
	if err != nil {
		h.logger.Printf("password-reset/reset 读取验证码失败: %v", err)
	}
	if !ok || cachedString(cached) != code {
		if rerr := h.recordVerifyFailure(ctx, email, ip, limits); rerr != nil {
			h.logger.Printf("password-reset/reset 记失败次数失败: %v", rerr)
		}
		fail(c, msgCodeInvalidOrExp, 400)
		return
	}

	user, err := h.store.UserByEmail(ctx, email)
	if err != nil && !IsNoRows(err) {
		h.logger.Printf("password-reset/reset 查询 user 失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	// 防枚举：与上面「验证码错误或已过期」同一文案同一 code，不暴露注册状态。
	if user == nil {
		fail(c, msgCodeInvalidOrExp, 400)
		return
	}

	hash, err := hashPassword(password, h.bcryptCost)
	if err != nil {
		h.logger.Printf("password-reset/reset 生成密码哈希失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	if err := h.store.UpdateUserPassword(ctx, user.ID, hash); err != nil {
		h.logger.Printf("password-reset/reset 改密失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	// 验证码一次性。
	if err := h.cache.Forget(ctx, pwdResetKey(email)); err != nil {
		h.logger.Printf("password-reset/reset 删除验证码失败: %v", err)
	}
	// 旧登录态全部作废。
	if err := h.store.DeleteAccessTokensByUser(ctx, user.ID); err != nil {
		h.logger.Printf("password-reset/reset 清 access_token 失败: %v", err)
	}
	if err := h.clearVerifyFailures(ctx, email, ip); err != nil {
		h.logger.Printf("password-reset/reset 清失败次数失败: %v", err)
	}

	success(c, nil, msgPasswordReset)
}

// randomDigits 对齐 str_pad(random_int(0, 999999), 6, '0', STR_PAD_LEFT)：6 位数字，允许前导 0。
func randomDigits(n int) string {
	b := randomBytes(4)
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	mod := uint32(1)
	for i := 0; i < n; i++ {
		mod *= 10
	}
	v %= mod
	s := fmt.Sprintf("%0*d", n, v)
	return s
}

// cachedString 把缓存值转成字符串（PHP 比较用 !==，两边都是字符串才算相等；
// 这里统一转字符串再比，与「写入方是字符串」的前提一致）。
func cachedString(v any) string {
	if s, ok := phpStringValue(v); ok {
		return s
	}
	return ""
}

// emailLogTag 对齐 PasswordResetController::emailLogContext：掩码 + sha1，不留完整明文。
func emailLogTag(email string) string {
	at := strings.IndexByte(email, '@')
	masked := "***"
	if at >= 0 && at > 0 {
		masked = email[:1] + "***" + email[at:]
	}
	sum := sha1.Sum([]byte(email))
	return "email_masked=" + masked + " email_sha1=" + hex.EncodeToString(sum[:])
}
