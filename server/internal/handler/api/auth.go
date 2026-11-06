package api

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// ctxUserKey 是中间件注入用户时用的 gin.Context 键。
const ctxUserKey = "api.user"

// AuthMiddleware 对齐 backend/app/Http/Middleware/ApiAuth.php：
//
//	Authorization: Bearer {token} → 查 access_tokens → expires_at 未过期 → 查 user → enabled
//	→ 更新 last_used_at → 注入 user
//
// 失败文案与 code（HTTP 一律 200，前端只看 code）：
//   - 没有 / 不是 Bearer 前缀 ………「未认证」401
//   - token 查不到 或 已过期 ………「未认证或 token 已过期」401
//   - 用户不存在 或 enabled=0 ……「账号不可用」401
func (h *Handler) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			fail(c, "未认证", 401)
			c.Abort()
			return
		}

		ctx := c.Request.Context()
		access, err := h.store.AccessTokenByToken(ctx, token)
		if err != nil && !IsNoRows(err) {
			h.logger.Printf("api.auth 查询 access_token 失败: %v", err)
			fail(c, "服务器错误", 500)
			c.Abort()
			return
		}
		now := h.now()
		if access == nil || access.Expired(now) {
			fail(c, "未认证或 token 已过期", 401)
			c.Abort()
			return
		}

		user, uerr := h.store.UserByID(ctx, access.UserID)
		if uerr != nil && !IsNoRows(uerr) {
			h.logger.Printf("api.auth 查询 user 失败: %v", uerr)
			fail(c, "服务器错误", 500)
			c.Abort()
			return
		}
		if user == nil || !user.Enabled {
			fail(c, "账号不可用", 401)
			c.Abort()
			return
		}

		// 与 PHP 一样：无论请求是否成功都刷新 last_used_at（失败不阻断请求 ——
		// 这个字段只用于「最近活跃」展示，写失败不该让用户掉线）。
		if err := h.store.TouchAccessToken(ctx, access.ID, now); err != nil {
			h.logger.Printf("api.auth 更新 last_used_at 失败: %v", err)
		}

		c.Set(ctxUserKey, user)
		c.Next()
	}
}

// handleLogin 对齐 AuthController::login：users.token 换 access_token。
//
//	{"token": "..."} → {"access_token": "..."}
//	缺 token → 「token 必填」400；查不到或 disabled → 「token 无效」401
func (h *Handler) handleLogin(c *gin.Context) {
	input := jsonInput(c)
	rawToken, ok := input["token"]
	// PHP: $token = $data['token'] ?? null; if (!$token) → 「token 必填」。
	if !ok || phpFalsy(rawToken) {
		fail(c, "token 必填", 400)
		return
	}
	token, isScalar := inputString(rawToken)
	if !isScalar {
		fail(c, "token 必填", 400)
		return
	}

	ctx := c.Request.Context()
	user, err := h.store.UserByToken(ctx, token)
	if err != nil && !IsNoRows(err) {
		h.logger.Printf("login 查询 user 失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	if user == nil || !user.Enabled {
		fail(c, "token 无效", 401)
		return
	}

	accessToken, err := h.createAccessToken(ctx, user.ID)
	if err != nil {
		h.logger.Printf("login 签发 access_token 失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	success(c, jo("access_token", accessToken))
}

// handleLoginEmail 对齐 AuthController::loginEmail（含 RateGuard 的锁定语义）。
func (h *Handler) handleLoginEmail(c *gin.Context) {
	input := jsonInput(c)
	email, emailOK := inputString(input["email"])
	password, passwordOK := inputString(input["password"])

	// PHP 的判定是 !$email || !$password：null / 空串 / "0" / 0 都算未填。
	if !emailOK || !passwordOK || phpFalsy(input["email"]) || phpFalsy(input["password"]) {
		fail(c, "邮箱和密码必填", 400)
		return
	}

	ctx := c.Request.Context()
	ip := clientIP(c)
	limits := h.loadRateLimits(ctx)

	// 登录锁定：锁定期内连正确密码也拒。
	if blocked, err := h.loginBlocked(ctx, email, ip, limits); err != nil {
		h.logger.Printf("login-email 限流查询失败: %v", err)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}

	user, err := h.store.UserByEmail(ctx, email)
	if err != nil && !IsNoRows(err) {
		h.logger.Printf("login-email 查询 user 失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	if user == nil || !user.Password.Valid || user.Password.String == "" ||
		bcrypt.CompareHashAndPassword([]byte(user.Password.String), []byte(password)) != nil {
		// 邮箱不存在与密码错误共用同一条文案，计数也共用同一个键。
		if rerr := h.recordLoginFailure(ctx, email, ip, limits); rerr != nil {
			h.logger.Printf("login-email 记失败次数失败: %v", rerr)
		}
		fail(c, "邮箱或密码错误", 401)
		return
	}

	if !user.Enabled {
		fail(c, "账号已禁用", 403)
		return
	}

	if cerr := h.clearLoginFailures(ctx, email, ip); cerr != nil {
		h.logger.Printf("login-email 清失败次数失败: %v", cerr)
	}

	accessToken, err := h.createAccessToken(ctx, user.ID)
	if err != nil {
		h.logger.Printf("login-email 签发 access_token 失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	success(c, jo("access_token", accessToken))
}

// handleRegister 对齐 AuthController::register。
//
// 顺序很关键（每一步的失败文案与 code 都要对上）：
//
//	IP 限流（放行即记数）→ 必填 → 邮箱格式 → 密码至少 6 字节
//	→ 验证码（有 register_email_verify 走邮箱验证码，否则走图形验证码）
//	→ 邮箱已注册 → 建号 → 异步下发 client → 签发 access_token
func (h *Handler) handleRegister(c *gin.Context) {
	input := jsonInput(c)
	email, _ := inputString(input["email"])
	password, _ := inputString(input["password"])
	captcha, _ := inputString(input["captcha"])
	emailCode, _ := inputString(input["email_code"])

	ctx := c.Request.Context()
	ip := clientIP(c)
	limits := h.loadRateLimits(ctx)

	// 注册按 IP 限流，入口先卡一道；放行即记数（后面的校验失败分支同样占额度）。
	if blocked, err := h.registerBlocked(ctx, ip, email, limits); err != nil {
		h.logger.Printf("register 限流查询失败: %v", err)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}
	if err := h.recordRegister(ctx, ip, email, limits); err != nil {
		h.logger.Printf("register 记数失败: %v", err)
	}

	if phpFalsy(input["email"]) || phpFalsy(input["password"]) {
		fail(c, "邮箱和密码必填", 400)
		return
	}
	// PHP 用 filter_var(FILTER_VALIDATE_EMAIL)，比 Validator 的 email 规则更严。
	if !filterValidateEmail(email) {
		fail(c, "邮箱格式不正确", 400)
		return
	}
	if phpStrlen(password) < 6 {
		fail(c, "密码至少 6 位", 400)
		return
	}

	configs, cfgErr := h.store.SiteConfigMany(ctx, []string{"register_email_verify"})
	if cfgErr != nil {
		h.logger.Printf("register 读取站点配置失败: %v", cfgErr)
	}
	useEmailVerify := phpTruthy(configs["register_email_verify"])

	if useEmailVerify {
		if emailCode == "" {
			fail(c, "邮箱验证码必填", 400)
			return
		}
		cached, ok, err := h.cache.Get(ctx, emailVerifyKey(email))
		if err != nil {
			h.logger.Printf("register 读取邮箱验证码失败: %v", err)
		}
		// PHP 是严格比较（$cachedCode !== $emailCode），缓存里是字符串 ——
		// 客户端把验证码当数字提交（JSON number）时，PHP 判为不等，这里同样判不等。
		if !ok || !phpValueIsString(input["email_code"]) || cachedString(cached) != emailCode {
			fail(c, "邮箱验证码错误", 400)
			return
		}
		if err := h.cache.Forget(ctx, emailVerifyKey(email)); err != nil {
			h.logger.Printf("register 删除邮箱验证码失败: %v", err)
		}
	} else {
		// 图形验证码：PHP 从 session 读，Go 从签名 cookie 读（见 session.go 的说明）。
		stored, ok := verifyCaptcha(h.captchaSecret, captchaCookieValue(c), h.now().Unix())
		if !ok || phpStrToLower(captcha) != phpStrToLower(stored) {
			fail(c, "验证码错误", 400)
			return
		}
		clearCaptchaCookie(c)
	}

	exists, err := h.store.EmailExists(ctx, email)
	if err != nil {
		h.logger.Printf("register 查询邮箱失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	if exists {
		fail(c, "邮箱已注册", 409)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), h.bcryptCost)
	if err != nil {
		h.logger.Printf("register 生成密码哈希失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	user, err := h.store.CreateUser(ctx, CreateUserInput{
		Email:        email,
		PasswordHash: string(hash),
		Token:        "sub_" + randomString(32),
		UUID:         newUUIDv4(),
		Protocol:     "vless",
	})
	if err != nil {
		h.logger.Printf("register 建号失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	// 建号异步：派发失败只记日志，注册照样成功（与 PHP 的 try/catch 同语义）。
	if h.provisioner != nil {
		if err := h.provisioner.Dispatch(ctx, user); err != nil {
			h.logger.Printf("dispatchProvisionClient failed on register: %v", err)
		}
	}

	accessToken, err := h.createAccessToken(ctx, user.ID)
	if err != nil {
		h.logger.Printf("register 签发 access_token 失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	success(c, jo(
		"access_token", accessToken,
		"token", user.Token,
		"provisioning", true,
	), "注册成功")
}

// createAccessToken 对齐 AuthController::createAccessToken：64 位随机、30 天过期。
func (h *Handler) createAccessToken(ctx context.Context, userID uint64) (string, error) {
	token := randomString(64)
	expires := h.now().Add(h.tokenTTL)
	if err := h.store.CreateAccessToken(ctx, userID, token, expires); err != nil {
		return "", err
	}
	return token, nil
}

// ---- 随机串 / UUID ----

// randomString 对齐 Str::random($n) 的字符空间（base64 去掉 '/' '+' '='，即 [A-Za-z0-9]）。
//
// 实现方式略有不同但分布等价：直接取 62 进制字符（PHP 是先 base64 再剔除三个字符，
// 分布轻微不均但没有安全影响；令牌熵都在 32/64 字符 × log2(62) 之上）。
func randomString(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := randomBytes(n)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(out)
}

// newUUIDv4 对齐 Str::uuid()->toString()：8-4-4-4-12 小写十六进制，version=4、variant=10xx。
func newUUIDv4() string {
	b := randomBytes(16)
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}

// filterValidateEmail 对齐 filter_var($email, FILTER_VALIDATE_EMAIL)。
//
// 与 Validator 的 email 规则（RFC）不同：注册用的是 filter_var，它更严 ——
// 例如 "用户@b.co" 能过 RFC 却过不了 filter_var，a@b 也不合法（无点域名）。
// 这里实现其可观测口径（对拍自真实 PHP，见 auth_test.go 的语料）。
func filterValidateEmail(s string) bool {
	if s == "" || strings.ContainsAny(s, "\r\n ") {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	local, domain := s[:at], s[at+1:]
	// 未加引号的 local part 内不允许再出现 @（引号内的按 quoted-pair 合法，如 "a@b"@c.co）。
	if !strings.HasPrefix(local, `"`) && strings.Count(local, "@") > 0 {
		return false
	}

	if !filterLocalOK(local) {
		return false
	}
	if strings.HasPrefix(domain, "[") {
		// IP 字面量：[1.2.3.4] 通过；[IPv6:...] 通过。
		if !strings.HasSuffix(domain, "]") {
			return false
		}
		inner := domain[1 : len(domain)-1]
		if strings.HasPrefix(inner, "IPv6:") {
			return strings.Contains(inner, ":")
		}
		return ipv4Literal(inner)
	}
	return filterDomainOK(domain)
}

// filterLocalOK 对齐 FILTER_VALIDATE_EMAIL 的 local part：dot-atom（ASCII atext）
// 或引号包裹；不允许首尾点、连续点。
func filterLocalOK(local string) bool {
	if local == "" {
		return false
	}
	if strings.HasPrefix(local, `"`) {
		// filter_var 接受不加空格的引号串（"ab"@c.co），但拒绝带空格的（"a b"@c.co）——
		// 与 RFC（允许 quoted-pair）不同，这里按 PHP 的可观测口径来。
		if len(local) < 2 || !strings.HasSuffix(local, `"`) {
			return false
		}
		inner := local[1 : len(local)-1]
		for i := 0; i < len(inner); i++ {
			c := inner[i]
			if c == ' ' || c == '	' || c < 0x20 || c == 0x7F {
				return false
			}
		}
		return true
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	const atext = "!#$%&'*+-/=?^_`{|}~."
	for i := 0; i < len(local); i++ {
		c := local[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(atext, c) >= 0:
		default:
			return false // 含非 ASCII（如中文）→ filter_var 拒绝
		}
	}
	return true
}

// filterDomainOK 对齐 FILTER_VALIDATE_EMAIL 的 domain：至少一个点、标签字母数字或 '-'、
// 末段标签必须以字母开头（数字或连字符开头会被拒），标签不以 '-' 开头/结尾。
func filterDomainOK(domain string) bool {
	if domain == "" || strings.HasSuffix(domain, ".") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false // "a@b" 不合法：必须有点
	}
	last := labels[len(labels)-1]
	if last == "" {
		return false
	}
	// TLD：filter_var 要求以字母开头（"a@b.c1" 通过、"a@b.1c" 不通过）。
	if c := last[0]; c >= '0' && c <= '9' {
		return false
	}
	if last[0] == '-' || last[len(last)-1] == '-' {
		return false
	}

	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// captchaCookieValue 取验证码 cookie（不存在返回空串）。
func captchaCookieValue(c *gin.Context) string {
	v, err := c.Cookie(captchaCookieName)
	if err != nil {
		return ""
	}
	return v
}
