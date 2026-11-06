package admin

// admin_auth_routes.go 是管理端认证路由的 handler（login/logout/settings/改密/改用户名/2FA）。
// 逐条对齐 backend/app/Http/Controllers/Admin/AuthController.php。
//
// login 在 AdminAuth 中间件之外（登录前无需 session）；其余在组内。

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// AdminAuthRoutes 持有管理端认证 handler 的依赖。
type AdminAuthRoutes struct {
	app  *httpx.App
	db   *sql.DB
	now  func() time.Time
	auth *loginGuard
	// lookupAdmin 按用户名取登录所需凭据；查不到返回 (nil, nil)。
	// 默认实现查 admins 表，单测替换成内存实现（不连库）。
	lookupAdmin func(ctx context.Context, username string) (*adminCredentials, error)
}

// adminCredentials 是登录校验要用到的管理员凭据快照。
type adminCredentials struct {
	ID               uint64
	Username         string
	PasswordHash     string
	Google2FAEnabled bool
	Google2FASecret  sql.NullString
}

// NewAdminAuthRoutes 构造管理端认证 handler。
func NewAdminAuthRoutes(app *httpx.App, db *sql.DB) *AdminAuthRoutes {
	h := &AdminAuthRoutes{app: app, db: db, now: time.Now, auth: newLoginGuardFromEnv()}
	h.lookupAdmin = h.queryAdminByUsername
	return h
}

// queryAdminByUsername 是 lookupAdmin 的生产实现。
func (h *AdminAuthRoutes) queryAdminByUsername(ctx context.Context, username string) (*adminCredentials, error) {
	var creds adminCredentials
	err := h.db.QueryRowContext(ctx,
		"SELECT id, username, password, google2fa_enabled, google2fa_secret FROM admins WHERE username = ?",
		username).Scan(&creds.ID, &creds.Username, &creds.PasswordHash, &creds.Google2FAEnabled, &creds.Google2FASecret)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &creds, nil
}

// RegisterLogin 挂 POST /admin-api/login（无需 AdminAuth 中间件）。
func (h *AdminAuthRoutes) RegisterLogin(r gin.IRouter) {
	r.POST("/admin-api/login", h.Login)
}

// RegisterAuthed 挂需要 session 的认证路由（在 admin-api 分组内）。
func (h *AdminAuthRoutes) RegisterAuthed(r gin.IRouter) {
	r.POST("/logout", h.Logout)
	r.GET("/settings", h.Settings)
	r.POST("/change-password", h.ChangePassword)
	r.POST("/update-username", h.UpdateUsername)
	r.POST("/google2fa/generate", h.Google2faGenerate)
	r.POST("/google2fa/enable", h.Google2faEnable)
	r.POST("/google2fa/disable", h.Google2faDisable)
}

// Login 对齐 AuthController::login（外加登录失败限流与锁定，见 login_guard.go）。
func (h *AdminAuthRoutes) Login(c *gin.Context) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "参数错误", 400)
		return
	}

	ip := c.ClientIP()
	// 判锁排在查库之前：限流/锁定期内连正确密码也拒，也不做任何凭据比对
	// （爆破方拿不到「密码对了」这个信号，账号是否存在同样不泄露）。
	// 先判后记：第 N+1 次请求被判限，本次不再计入 IP 窗口（否则窗口会被自己越推越满）。
	if locked, retryAfter := h.auth.blocked(ip, body.Username); locked {
		h.failLocked(c, retryAfter)
		return
	}
	// 每次放行的登录尝试都计入 IP 的分钟窗口（成功失败都算，见 recordAttempt）。
	h.auth.recordAttempt(ip)

	creds, err := h.lookupAdmin(c.Request.Context(), body.Username)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	// 账号不存在与密码错误共用同一条文案、同一个计数（不泄露账号是否存在）。
	if creds == nil {
		h.loginFailure(c, ip, body.Username, "账号或密码错误")
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(creds.PasswordHash), []byte(body.Password)) != nil {
		h.loginFailure(c, ip, body.Username, "账号或密码错误")
		return
	}

	if creds.Google2FAEnabled && creds.Google2FASecret.Valid && creds.Google2FASecret.String != "" {
		if body.OTP == "" {
			h.loginFailure(c, ip, body.Username, "请输入验证码")
			return
		}
		if !verifyTOTP(creds.Google2FASecret.String, body.OTP, h.now()) {
			h.loginFailure(c, ip, body.Username, "验证码错误")
			return
		}
	}

	h.auth.clear(ip, body.Username)
	h.app.SetAdminSession(c, creds.ID)
	writeJSON(c, phpEncode(jo("code", 0, "msg", "登录成功", "data",
		jo("admin", jo("id", creds.ID, "username", creds.Username)))))
}

// loginFailure 记一次失败（两个维度各一次）并按原口径回 401。
// 第 N 次失败本身仍回 401；紧接的第 N+1 次才会被判锁回 429（外部验收口径）。
func (h *AdminAuthRoutes) loginFailure(c *gin.Context, ip, username, msg string) {
	h.auth.recordFailure(ip, username)
	h.fail(c, msg, 401)
}

// failLocked 输出锁定期的 429：body code 与 HTTP status 都是 429
// （外部扫描器/网关按 http status 判定，前端仍可读 body code）。
func (h *AdminAuthRoutes) failLocked(c *gin.Context, retryAfter time.Duration) {
	secs := int(retryAfter/time.Second) + 1 // 向上取整，至少 1 秒
	if secs < 1 {
		secs = 1
	}
	c.Data(http.StatusTooManyRequests, "application/json; charset=utf-8",
		phpEncode(jo("code", 429, "msg", fmt.Sprintf("登录失败次数过多，请 %d 秒后再试", secs), "data", nil)))
}

// Logout 对齐 AuthController::logout。
func (h *AdminAuthRoutes) Logout(c *gin.Context) {
	h.app.ClearAdminSession(c)
	writeJSON(c, phpEncode(jo("code", 0, "msg", "已退出", "data", nil)))
}

// Settings 对齐 AuthController::settings。
func (h *AdminAuthRoutes) Settings(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var google2faEnabled bool
	_ = h.db.QueryRowContext(c.Request.Context(),
		"SELECT google2fa_enabled FROM admins WHERE id = ?", admin.ID).Scan(&google2faEnabled)

	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data",
		jo("id", admin.ID, "username", admin.Username, "google2fa_enabled", google2faEnabled))))
}

// ChangePassword 对齐 AuthController::changePassword。
func (h *AdminAuthRoutes) ChangePassword(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "old_password 字段不能为空。,new_password 字段不能为空。", 500)
		return
	}
	if len(body.NewPassword) < 6 || len(body.NewPassword) > 64 {
		h.fail(c, "new_password 长度必须介于 6 - 64 个字符之间。", 500)
		return
	}

	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT password FROM admins WHERE id = ?", admin.ID).Scan(&passwordHash)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.OldPassword)) != nil {
		h.fail(c, "原密码错误", 400)
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET password = ? WHERE id = ?", string(newHash), admin.ID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "密码已修改", "data", nil)))
}

// UpdateUsername 对齐 AuthController::updateUsername。
func (h *AdminAuthRoutes) UpdateUsername(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "username 字段不能为空。,password 字段不能为空。", 500)
		return
	}
	if len(body.Username) < 3 || len(body.Username) > 32 {
		h.fail(c, "username 长度必须介于 3 - 32 个字符之间。", 500)
		return
	}

	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT password FROM admins WHERE id = ?", admin.ID).Scan(&passwordHash)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password)) != nil {
		h.fail(c, "密码错误", 400)
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET username = ? WHERE id = ?", body.Username, admin.ID)
	if err != nil {
		h.fail(c, "用户名已被使用", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "用户名已修改", "data", nil)))
}

// Google2faGenerate 对齐 AuthController::google2faGenerate。
func (h *AdminAuthRoutes) Google2faGenerate(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	secret := generateBase32Secret()
	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", jo("secret", secret))))
}

// Google2faEnable 对齐 AuthController::google2faEnable。
func (h *AdminAuthRoutes) Google2faEnable(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		Secret string `json:"secret"`
		OTP    string `json:"otp"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "otp 字段不能为空。", 500)
		return
	}

	if body.Secret == "" {
		h.fail(c, "请先生成密钥", 400)
		return
	}

	if !verifyTOTP(body.Secret, body.OTP, h.now()) {
		h.fail(c, "验证码错误", 400)
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET google2fa_enabled = 1, google2fa_secret = ? WHERE id = ?",
		body.Secret, admin.ID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "两步验证已启用", "data", nil)))
}

// Google2faDisable 对齐 AuthController::google2faDisable。
func (h *AdminAuthRoutes) Google2faDisable(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "password 字段不能为空。", 500)
		return
	}

	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT password FROM admins WHERE id = ?", admin.ID).Scan(&passwordHash)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password)) != nil {
		h.fail(c, "密码错误", 400)
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET google2fa_enabled = 0, google2fa_secret = NULL WHERE id = ?", admin.ID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "两步验证已关闭", "data", nil)))
}

// fail 输出 PHP ApiResponse::error 口径。
func (h *AdminAuthRoutes) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// ---- TOTP 工具 ----

// verifyTOTP 验证 TOTP 验证码（RFC 6238，30 秒窗口，±1 容差）。
func verifyTOTP(secret, code string, now time.Time) bool {
	if len(code) != 6 {
		return false
	}
	secret = strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return false
	}

	timestamp := now.Unix()
	for _, offset := range []int64{-1, 0, 1} {
		counter := (timestamp + offset*30) / 30
		if totpCode(key, counter) == code {
			return true
		}
	}
	return false
}

// totpCode 生成 TOTP 验证码（HMAC-SHA1，6 位）。
func totpCode(key []byte, counter int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0F
	code := (int(sum[offset])&0x7F)<<24 |
		(int(sum[offset+1])&0xFF)<<16 |
		(int(sum[offset+2])&0xFF)<<8 |
		(int(sum[offset+3]) & 0xFF)
	return fmt.Sprintf("%06d", code%1000000)
}

// generateBase32Secret 生成 20 字节的 base32 密钥。
func generateBase32Secret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
