package api

import (
	"context"
	"strconv"
	"time"
)

// RateGuardService 的 Go 复刻（backend/app/Services/RateGuardService.php）。
//
// 语义必须逐条对齐 —— 它同时承担「防刷」和「防邮箱枚举」两个职责：
//   - 判定（Blocked）永远排在「邮箱是否注册」之前；
//   - 只要判定放行，无论后续分支怎么早退，都必须记数（RecordXxx）；
//   - 阈值取 site_configs 的 rate_* 覆盖值，缺省/空串回退 config/panel.php 的默认值；
//   - 任何阈值 <= 0 表示该项不限；
//   - 计数键与 PHP 逐字相同（rate:xxx），并存期共享同一批 cache 行。
//
// Laravel RateLimiter 的键结构：<key> 存命中次数、<key>:timer 存窗口结束时刻（unix 秒）。
// tooManyAttempts(key, max) = attempts >= max && timer 仍在。
const rateTooOftenMsg = "操作过于频繁，请稍后再试"

// rateLimiter 复刻 Illuminate\Cache\RateLimiter 的 hit/tooManyAttempts/clear。
type rateLimiter struct {
	cache Cache
	now   func() time.Time
}

func newRateLimiter(cache Cache) *rateLimiter { return &rateLimiter{cache: cache, now: time.Now} }

// hit 计一次命中并把窗口延长到 decay（Laravel 的 hit = increment）。
// 键不存在时先 add(0)，再 increment —— 与 DatabaseStore 的分支一致。
func (r *rateLimiter) hit(ctx context.Context, key string, decay time.Duration) error {
	timerKey := key + ":timer"
	expiresAt := r.now().Add(decay)
	// add 不覆盖已有 timer（窗口从第一次命中开始算，不因后续命中顺延）。
	if _, err := r.cache.Add(ctx, timerKey, expiresAt.Unix(), decay); err != nil {
		return err
	}
	if _, err := r.cache.Add(ctx, key, int64(0), decay); err != nil {
		return err
	}
	if _, _, err := r.cache.Increment(ctx, key); err != nil {
		return err
	}
	return nil
}

// tooManyAttempts = attempts >= max 且 timer 仍在。
func (r *rateLimiter) tooManyAttempts(ctx context.Context, key string, max int) (bool, error) {
	if max <= 0 {
		return false, nil
	}
	attempts, ok, err := r.cache.Get(ctx, key)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	n, convErr := cacheInt(attempts)
	if convErr != nil || n < int64(max) {
		return false, nil
	}
	// 次数到了但窗口已过期 → Laravel 会 resetAttempts 并放行。
	_, timerOK, err := r.cache.Get(ctx, key+":timer")
	if err != nil {
		return false, err
	}
	if !timerOK {
		_ = r.cache.Forget(ctx, key)
		return false, nil
	}
	return true, nil
}

// clear 清零计数与 timer。
func (r *rateLimiter) clear(ctx context.Context, key string) error {
	if err := r.cache.Forget(ctx, key); err != nil {
		return err
	}
	return r.cache.Forget(ctx, key+":timer")
}

func cacheInt(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	case float64:
		return int64(t), nil
	}
	return 0, strconv.ErrSyntax
}

// rateLimits 是一次请求用到的全部阈值（对应 RateGuardService::limits()）。
type rateLimits struct {
	CodeInterval     int // rate_code_interval
	CodePerDay       int // rate_code_per_day
	CodeIPHourly     int // rate_code_ip_hourly
	VerifyMax        int // rate_verify_max_attempts
	VerifyLockSecond int // rate_verify_lock_seconds
	LoginMax         int // rate_login_max_attempts
	LoginLockSecond  int // rate_login_lock_seconds
	RegisterIPHourly int // rate_register_ip_hourly
	RedeemMax        int // rate_redeem_max_attempts（余额兑换码：同 IP 连续失败上限）
	RedeemLockSecond int // rate_redeem_lock_seconds（余额兑换码：锁定时长）
}

// 默认值逐字取自 config/panel.php 的 ratelimit 段（.env 未覆盖时）。
const (
	defaultRateCodeInterval     = 60
	defaultRateCodePerDay       = 10
	defaultRateCodeIPHourly     = 20
	defaultRateVerifyMax        = 10
	defaultRateVerifyLockSecond = 300
	defaultRateLoginMax         = 5
	defaultRateLoginLockSecond  = 300
	defaultRateRegisterIPHourly = 10
	defaultRateRedeemMax        = 5
	defaultRateRedeemLockSecond = 300
)

// siteConfigRateKeys 是 RateGuardService::CONFIG_KEYS 的用户端子集。
var siteConfigRateKeys = []string{
	"rate_code_interval", "rate_code_per_day", "rate_code_ip_hourly",
	"rate_verify_max_attempts", "rate_verify_lock_seconds",
	"rate_login_max_attempts", "rate_login_lock_seconds",
	"rate_register_ip_hourly",
	"rate_redeem_max_attempts", "rate_redeem_lock_seconds",
}

// loadRateLimits 取阈值：site_configs 有非空值就用它，否则用 config/panel.php 的默认值。
//
// 刻意不缓存（与 PHP 同）：后台刚改的规则必须对下一个请求立即生效。
func (h *Handler) loadRateLimits(ctx context.Context) rateLimits {
	values, err := h.store.SiteConfigMany(ctx, siteConfigRateKeys)
	if err != nil {
		values = map[string]string{}
	}
	pick := func(key string, def int) int {
		raw, ok := values[key]
		if !ok || raw == "" {
			return def
		}
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return def
		}
		return n
	}
	return rateLimits{
		CodeInterval:     pick("rate_code_interval", defaultRateCodeInterval),
		CodePerDay:       pick("rate_code_per_day", defaultRateCodePerDay),
		CodeIPHourly:     pick("rate_code_ip_hourly", defaultRateCodeIPHourly),
		VerifyMax:        pick("rate_verify_max_attempts", defaultRateVerifyMax),
		VerifyLockSecond: pick("rate_verify_lock_seconds", defaultRateVerifyLockSecond),
		LoginMax:         pick("rate_login_max_attempts", defaultRateLoginMax),
		LoginLockSecond:  pick("rate_login_lock_seconds", defaultRateLoginLockSecond),
		RegisterIPHourly: pick("rate_register_ip_hourly", defaultRateRegisterIPHourly),
		RedeemMax:        pick("rate_redeem_max_attempts", defaultRateRedeemMax),
		RedeemLockSecond: pick("rate_redeem_lock_seconds", defaultRateRedeemLockSecond),
	}
}

// ---- 键构造（与 PHP 逐字一致，便于并存期共用计数）----

const ratePrefix = "rate:"

// normalizeEmail 邮箱统一小写去空白，避免同一邮箱换大小写绕开计数（PHP 的 strtolower(trim())）。
func normalizeEmail(v string) string { return phpStrToLower(phpTrim(v)) }

func keyCodeInterval(email string) string {
	return ratePrefix + "code_interval:" + normalizeEmail(email)
}
func keyCodeDaily(email string) string { return ratePrefix + "code_daily:" + normalizeEmail(email) }
func keyCodeIP(ip string) string       { return ratePrefix + "code_ip:" + ip }
func keyVerify(email, ip string) string {
	return ratePrefix + "verify:" + normalizeEmail(email) + "|" + ip
}
func keyLogin(email, ip string) string {
	return ratePrefix + "login:" + normalizeEmail(email) + "|" + ip
}
func keyRegister(ip string) string { return ratePrefix + "register:" + ip }

// codeSendBlocked 对齐 RateGuardService::codeSendBlocked（间隔 / 每日 / 同 IP 每小时三条）。
func (h *Handler) codeSendBlocked(ctx context.Context, email, ip string, l rateLimits) (bool, error) {
	// 间隔：该键最多容 1 次命中，命中即「距上次发码还不到 interval 秒」。
	if l.CodeInterval > 0 {
		blocked, err := h.limiter.tooManyAttempts(ctx, keyCodeInterval(email), 1)
		if err != nil || blocked {
			return blocked, err
		}
	}
	if blocked, err := h.limiter.tooManyAttempts(ctx, keyCodeDaily(email), l.CodePerDay); err != nil || blocked {
		return blocked, err
	}
	return h.limiter.tooManyAttempts(ctx, keyCodeIP(ip), l.CodeIPHourly)
}

// recordCodeSend 记一次发码请求 —— 放行后必须无条件调用（防枚举，见函数头注释）。
func (h *Handler) recordCodeSend(ctx context.Context, email, ip string, l rateLimits) error {
	if l.CodeInterval > 0 {
		if err := h.limiter.hit(ctx, keyCodeInterval(email), time.Duration(l.CodeInterval)*time.Second); err != nil {
			return err
		}
	}
	if l.CodePerDay > 0 {
		if err := h.limiter.hit(ctx, keyCodeDaily(email), 24*time.Hour); err != nil {
			return err
		}
	}
	if l.CodeIPHourly > 0 {
		if err := h.limiter.hit(ctx, keyCodeIP(ip), time.Hour); err != nil {
			return err
		}
	}
	return nil
}

// verifyBlocked 对齐 verifyBlocked：试错次数打满后，锁定期内连正确验证码也拒。
func (h *Handler) verifyBlocked(ctx context.Context, email, ip string, l rateLimits) (bool, error) {
	return h.limiter.tooManyAttempts(ctx, keyVerify(email, ip), l.VerifyMax)
}

func (h *Handler) recordVerifyFailure(ctx context.Context, email, ip string, l rateLimits) error {
	if l.VerifyLockSecond <= 0 {
		return nil
	}
	return h.limiter.hit(ctx, keyVerify(email, ip), time.Duration(l.VerifyLockSecond)*time.Second)
}

func (h *Handler) clearVerifyFailures(ctx context.Context, email, ip string) error {
	return h.limiter.clear(ctx, keyVerify(email, ip))
}

func (h *Handler) loginBlocked(ctx context.Context, email, ip string, l rateLimits) (bool, error) {
	return h.limiter.tooManyAttempts(ctx, keyLogin(email, ip), l.LoginMax)
}

func (h *Handler) recordLoginFailure(ctx context.Context, email, ip string, l rateLimits) error {
	if l.LoginLockSecond <= 0 {
		return nil
	}
	return h.limiter.hit(ctx, keyLogin(email, ip), time.Duration(l.LoginLockSecond)*time.Second)
}

func (h *Handler) clearLoginFailures(ctx context.Context, email, ip string) error {
	return h.limiter.clear(ctx, keyLogin(email, ip))
}

func (h *Handler) registerBlocked(ctx context.Context, ip string, l rateLimits) (bool, error) {
	return h.limiter.tooManyAttempts(ctx, keyRegister(ip), l.RegisterIPHourly)
}

func (h *Handler) recordRegister(ctx context.Context, ip string, l rateLimits) error {
	if l.RegisterIPHourly <= 0 {
		return nil
	}
	return h.limiter.hit(ctx, keyRegister(ip), time.Hour)
}

// ---- 余额兑换码限流（规划 §4「兑换码爆破」）----
//
// 与注册接口同一个思路：按 IP 记**失败次数**，打满后锁定时长内直接拒（连码表都不查）。
// 成功的请求清掉该 IP 的计数 —— 正常用户输错一两次不会被自己的正确兑换锁住。
// 阈值走 site_configs（rate_redeem_max_attempts / rate_redeem_lock_seconds），后台可调。

func keyRedeem(ip string) string { return ratePrefix + "redeem:" + ip }

func (h *Handler) redeemBlocked(ctx context.Context, ip string, l rateLimits) (bool, error) {
	return h.limiter.tooManyAttempts(ctx, keyRedeem(ip), l.RedeemMax)
}

func (h *Handler) recordRedeemFailure(ctx context.Context, ip string, l rateLimits) error {
	if l.RedeemLockSecond <= 0 {
		return nil
	}
	return h.limiter.hit(ctx, keyRedeem(ip), time.Duration(l.RedeemLockSecond)*time.Second)
}

func (h *Handler) clearRedeemFailures(ctx context.Context, ip string) error {
	return h.limiter.clear(ctx, keyRedeem(ip))
}

// ---- 优惠码限流（PaymentService 的 DiscountRateGuard 接缝，对齐 RateGuardService 折扣段）----
//
// /api/discount/check 与 /api/payment/create 共用同一套计数：两个都是能拿码试错的入口，
// 被限流/锁定时直接拦在查库之前，一个字节的库都不查，也就没有枚举反馈。

// discount 阈值默认值：逐字取 config/panel.php 的 ratelimit 段。
const (
	defaultDiscountPerMinute   = 10
	defaultDiscountFailPerDay   = 50
	defaultDiscountIPFailHourly = 100
	defaultDiscountMaxAttempts  = 10
	defaultDiscountLockSeconds  = 900
)

// siteConfigDiscountRateKeys 是 RateGuardService::CONFIG_KEYS 里优惠码的 5 项。
var siteConfigDiscountRateKeys = []string{
	"rate_discount_per_minute", "rate_discount_max_attempts", "rate_discount_lock_seconds",
	"rate_discount_fail_per_day", "rate_discount_ip_fail_hourly",
}

func keyDiscountMin(uid int64) string     { return ratePrefix + "discount_min:" + strconv.FormatInt(uid, 10) }
func keyDiscountAttempt(uid int64) string { return ratePrefix + "discount_attempt:" + strconv.FormatInt(uid, 10) }
func keyDiscountFail(uid int64) string    { return ratePrefix + "discount_fail:" + strconv.FormatInt(uid, 10) }
func keyDiscountIPFail(ip string) string  { return ratePrefix + "discount_ip_fail:" + ip }

// discountRateGuard 实现 DiscountRateGuard 接口（PaymentService 注入）。
type discountRateGuard struct {
	limiter *rateLimiter
	sites   SiteConfigReader
}

// NewDiscountRateGuard 构造生产版折扣码限流（routes 注入 PaymentService）。
// sites 为空时不读覆盖值、只用默认阈值。
func NewDiscountRateGuard(cache Cache, sites SiteConfigReader) *discountRateGuard {
	return &discountRateGuard{limiter: newRateLimiter(cache), sites: sites}
}

// limits 取 5 项阈值：site_configs 非空值覆盖，否则回退 config/panel.php 默认值。
// 阈值 <= 0 表示该项不限。
func (g *discountRateGuard) limits(ctx context.Context) (perMin, maxAttempts, lockSec, failPerDay, ipFailHourly int) {
	perMin, maxAttempts, lockSec = defaultDiscountPerMinute, defaultDiscountMaxAttempts, defaultDiscountLockSeconds
	failPerDay, ipFailHourly = defaultDiscountFailPerDay, defaultDiscountIPFailHourly
	if g.sites == nil {
		return
	}
	values, err := g.sites.SiteConfigMany(ctx, siteConfigDiscountRateKeys)
	if err != nil {
		return
	}
	pick := func(key string, def int) int {
		raw, ok := values[key]
		if !ok || raw == "" {
			return def
		}
		n, cerr := strconv.Atoi(raw)
		if cerr != nil {
			return def
		}
		return n
	}
	perMin = pick("rate_discount_per_minute", perMin)
	maxAttempts = pick("rate_discount_max_attempts", maxAttempts)
	lockSec = pick("rate_discount_lock_seconds", lockSec)
	failPerDay = pick("rate_discount_fail_per_day", failPerDay)
	ipFailHourly = pick("rate_discount_ip_fail_hourly", ipFailHourly)
	return
}

// CheckBlocked 对齐 discountCheckBlocked：每分钟请求 / 连续失败锁定 / 每日失败 / 同 IP 每小时失败。
func (g *discountRateGuard) CheckBlocked(ctx context.Context, userID int64, ip string) (bool, error) {
	perMin, maxAttempts, _, failPerDay, ipFailHourly := g.limits(ctx)
	if perMin > 0 {
		if b, err := g.limiter.tooManyAttempts(ctx, keyDiscountMin(userID), perMin); err != nil || b {
			return b, err
		}
	}
	if maxAttempts > 0 {
		if b, err := g.limiter.tooManyAttempts(ctx, keyDiscountAttempt(userID), maxAttempts); err != nil || b {
			return b, err
		}
	}
	if failPerDay > 0 {
		if b, err := g.limiter.tooManyAttempts(ctx, keyDiscountFail(userID), failPerDay); err != nil || b {
			return b, err
		}
	}
	if ipFailHourly > 0 {
		if b, err := g.limiter.tooManyAttempts(ctx, keyDiscountIPFail(ip), ipFailHourly); err != nil || b {
			return b, err
		}
	}
	return false, nil
}

// RecordRequest 记一次请求（所有请求都计，成功失败都算）。
func (g *discountRateGuard) RecordRequest(ctx context.Context, userID int64) error {
	perMin, _, _, _, _ := g.limits(ctx)
	if perMin <= 0 {
		return nil
	}
	return g.limiter.hit(ctx, keyDiscountMin(userID), time.Minute)
}

// RecordFailure 记一次失败：连续失败（锁定时长窗口）+ 每日 + IP 每小时。
func (g *discountRateGuard) RecordFailure(ctx context.Context, userID int64, ip string) error {
	_, maxAttempts, lockSec, failPerDay, ipFailHourly := g.limits(ctx)
	if maxAttempts > 0 && lockSec > 0 {
		if err := g.limiter.hit(ctx, keyDiscountAttempt(userID), time.Duration(lockSec)*time.Second); err != nil {
			return err
		}
	}
	if failPerDay > 0 {
		if err := g.limiter.hit(ctx, keyDiscountFail(userID), 24*time.Hour); err != nil {
			return err
		}
	}
	if ipFailHourly > 0 {
		if err := g.limiter.hit(ctx, keyDiscountIPFail(ip), time.Hour); err != nil {
			return err
		}
	}
	return nil
}

// ClearFailures 校验成功后清用户维度失败计数（刻意不清 IP 维度与每分钟请求）。
func (g *discountRateGuard) ClearFailures(ctx context.Context, userID int64) error {
	if err := g.limiter.clear(ctx, keyDiscountAttempt(userID)); err != nil {
		return err
	}
	return g.limiter.clear(ctx, keyDiscountFail(userID))
}
