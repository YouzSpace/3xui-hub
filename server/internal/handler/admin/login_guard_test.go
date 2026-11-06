package admin

// 管理端登录限流/锁定的单测（POST /admin-api/login）。
//
// 覆盖验收要求的四条：
//  1. 连续 N 次错密码后第 N+1 次返回 429；
//  2. 锁定期内正确密码也 429（且不做凭据比对）；
//  3. 成功登录后计数清零（变异锚点：clear 失效 → 后续失败提前触发 429）；
//  4. env 改阈值生效、非法值回退默认。
//
// 全部走 httptest + 内存版 lookupAdmin，不连库。
// 变异测试锚点：把 loginGuard.blocked 的阈值判断改成永假，本文件的锁定类用例必须变红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// ---- 测试脚手架 ----

// fakeClock 是可手动推进的时钟（handler 与 guard 共用，保证两者判定一致）。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type loginFixture struct {
	h     *AdminAuthRoutes
	clock *fakeClock
}

// newLoginFixture 造一个带内存账号表的 handler。
// attempts/lockFor 直接给阈值；传 0 表示用默认值。
func newLoginFixture(t *testing.T, attempts int, lockFor time.Duration, accounts map[string]string) *loginFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	clock := &fakeClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	guard := newLoginGuard(attempts, lockFor, clock.now)
	guard.maxEntries = loginGuardMaxEntries

	h := &AdminAuthRoutes{
		app:  httpx.NewApp(nil, nil),
		now:  clock.now,
		auth: guard,
	}
	h.lookupAdmin = func(_ context.Context, username string) (*adminCredentials, error) {
		hash, ok := accounts[username]
		if !ok {
			return nil, nil // 与生产实现同口径：查不到 → (nil, nil)
		}
		return &adminCredentials{ID: 1, Username: username, PasswordHash: hash}, nil
	}
	return &loginFixture{h: h, clock: clock}
}

// testAccounts 生成 {用户名: bcrypt(密码)}。
func testAccounts(t *testing.T, pairs map[string]string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(pairs))
	for user, pass := range pairs {
		hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		out[user] = string(hash)
	}
	return out
}

// postLogin 发一次登录请求并返回状态码与响应体。
func postLogin(t *testing.T, h *AdminAuthRoutes, ip, username, password, otp string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password, "otp": otp})
	req := httptest.NewRequest(http.MethodPost, "/admin-api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":54321" // 固定每个用例的来源 IP
	rec := httptest.NewRecorder()

	r := gin.New()
	r.POST("/admin-api/login", h.Login)
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// loginResp 是响应壳解析后的形态。
// 注意：响应走 PHP json_encode 口径，中文是 \uXXXX 转义，
// 所以断言 msg 必须先解码（直接 strings.Contains 原始中文会永远不命中）。
type loginResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func decodeLogin(t *testing.T, body string) loginResp {
	t.Helper()
	var r loginResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\nbody=%s", err, body)
	}
	return r
}

// ---- 用例 1：连续 N 次失败后第 N+1 次 429 ----

func TestLoginLocksAfterMaxAttempts(t *testing.T) {
	const maxAttempts = 5
	f := newLoginFixture(t, maxAttempts, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))

	for i := 1; i <= maxAttempts; i++ {
		code, body := postLogin(t, f.h, "203.0.113.7", "admin", "wrong", "")
		if code != http.StatusOK {
			t.Fatalf("第 %d 次失败应为 401（HTTP 200 壳），得到 HTTP %d: %s", i, code, body)
		}
		if got := decodeLogin(t, body); got.Code != 401 {
			t.Fatalf("第 %d 次失败 code 应为 401，得到 %d: %s", i, got.Code, body)
		}
	}

	// 第 N+1 次：即使密码正确也必须 429。
	code, body := postLogin(t, f.h, "203.0.113.7", "admin", "correct-horse", "")
	if code != http.StatusTooManyRequests {
		t.Fatalf("超阈值后应返回 HTTP 429，得到 HTTP %d: %s", code, body)
	}
	got := decodeLogin(t, body)
	if got.Code != 429 {
		t.Fatalf("锁定响应 code 应为 429，得到 %d: %s", got.Code, body)
	}
	if !strings.Contains(got.Msg, "登录失败次数过多") {
		t.Fatalf("锁定响应应带明确文案，得到: %s", got.Msg)
	}
}

// ---- 用例 2：锁定期内正确密码也 429 ----

func TestLoginLockedRejectsCorrectPassword(t *testing.T) {
	const maxAttempts = 3
	f := newLoginFixture(t, maxAttempts, 60*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))

	// 先成功登录一次，确认账号本身可用。
	if code, body := postLogin(t, f.h, "198.51.100.1", "admin", "correct-horse", ""); code != http.StatusOK || decodeLogin(t, body).Msg != "登录成功" {
		t.Fatalf("基线登录应成功，得到 HTTP %d: %s", code, body)
	}

	for i := 1; i <= maxAttempts; i++ {
		postLogin(t, f.h, "198.51.100.1", "admin", "wrong", "")
	}

	// 锁定期内连正确密码都拒。
	code, body := postLogin(t, f.h, "198.51.100.1", "admin", "correct-horse", "")
	if code != http.StatusTooManyRequests {
		t.Fatalf("锁定期内正确密码也应 429，得到 HTTP %d: %s", code, body)
	}

	// 锁定期过后，正确密码恢复可用。
	f.clock.advance(61 * time.Second)
	code, body = postLogin(t, f.h, "198.51.100.1", "admin", "correct-horse", "")
	if code != http.StatusOK || decodeLogin(t, body).Msg != "登录成功" {
		t.Fatalf("锁定到期后应能登录，得到 HTTP %d: %s", code, body)
	}
}

// ---- 维度隔离：IP 与用户名各自独立计数 ----

func TestLoginLockDimensions(t *testing.T) {
	const maxAttempts = 3

	t.Run("IP 维度只按分钟限流、不因跨用户名失败而长锁", func(t *testing.T) {
		// 这是本次改造的核心：CDN 未还原真实 IP 时，同一 IP 换用户名失败
		// 不能再把整个出口锁 15 分钟。账号维度仍各自独立（见下一条子用例）。
		f := newLoginFixture(t, maxAttempts, 900*time.Second,
			testAccounts(t, map[string]string{"admin": "pw", "ops": "pw"}))
		for _, user := range []string{"admin", "ops", "admin"} {
			postLogin(t, f.h, "203.0.113.9", user, "wrong", "")
		}
		// ops 账号维度只失败了 1 次（阈值 3），必须放行。
		if code, body := postLogin(t, f.h, "203.0.113.9", "ops", "pw", ""); code != http.StatusOK ||
			decodeLogin(t, body).Msg != "登录成功" {
			t.Fatalf("跨用户名的失败不应锁住整个 IP，得到 HTTP %d: %s", code, body)
		}
		// admin 账号维度已失败 2 次，再错 1 次就锁（账号维度原口径不变）。
		postLogin(t, f.h, "203.0.113.9", "admin", "wrong", "")
		if code, body := postLogin(t, f.h, "203.0.113.9", "admin", "pw", ""); code != http.StatusTooManyRequests {
			t.Fatalf("admin 账号维度应在 %d 次失败后锁定，得到 HTTP %d: %s", maxAttempts, code, body)
		}
	})

	t.Run("同用户名换 IP 也会锁", func(t *testing.T) {
		f := newLoginFixture(t, maxAttempts, 900*time.Second,
			testAccounts(t, map[string]string{"admin": "pw"}))
		for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
			postLogin(t, f.h, ip, "admin", "wrong", "")
		}
		if code, body := postLogin(t, f.h, "203.0.113.4", "admin", "pw", ""); code != http.StatusTooManyRequests {
			t.Fatalf("用户名维度应在 %d 次后锁定，得到 HTTP %d: %s", maxAttempts, code, body)
		}
	})

	t.Run("用户名归一化后同桶", func(t *testing.T) {
		f := newLoginFixture(t, maxAttempts, 900*time.Second,
			testAccounts(t, map[string]string{"admin": "pw"}))

		// 每个变体用**不同 IP**，保证 IP 维度各自只记 1 次（远不到阈值）；
		// 因此最终触发的锁只可能来自归一化后的用户名桶。
		// "ADMIN" / " admin " 在内存账号表里查不到，但必须落进 "admin" 这个桶。
		for i, user := range []string{"ADMIN", " admin ", "AdMiN"} {
			postLogin(t, f.h, "203.0.113."+itoa(100+i), user, "wrong", "")
		}

		// 关键断言：用一个**全新 IP** 发正确密码。
		// 新 IP 的 IP 桶是空的，只有用户名桶能把它拦住。
		if code, body := postLogin(t, f.h, "198.51.100.99", "admin", "pw", ""); code != http.StatusTooManyRequests {
			t.Fatalf("大小写/空白变体应落进同一用户名桶，得到 HTTP %d: %s", code, body)
		}

		// 反向对照：同样次数打另一个用户名，不应锁到 admin。
		f2 := newLoginFixture(t, maxAttempts, 900*time.Second,
			testAccounts(t, map[string]string{"admin": "pw", "other": "pw"}))
		for i := 0; i < maxAttempts; i++ {
			postLogin(t, f2.h, "203.0.113."+itoa(40+i), "other", "wrong", "")
		}
		if code, body := postLogin(t, f2.h, "198.51.100.98", "admin", "pw", ""); code != http.StatusOK {
			t.Fatalf("打 other 的失败不应锁住 admin，得到 HTTP %d: %s", code, body)
		}
	})
}

// ---- 用例 3：成功登录后计数清零 ----

func TestLoginSuccessClearsCounters(t *testing.T) {
	const maxAttempts = 5
	f := newLoginFixture(t, maxAttempts, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))
	const ip = "203.0.113.30"

	// 先攒 4 次失败（差 1 次到阈值）。
	for i := 0; i < maxAttempts-1; i++ {
		postLogin(t, f.h, ip, "admin", "wrong", "")
	}
	// 成功登录 → 清零。
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusOK || decodeLogin(t, body).Msg != "登录成功" {
		t.Fatalf("清零前应能成功登录，得到 HTTP %d: %s", code, body)
	}

	// 清零后重新攒满一整轮：前 maxAttempts 次失败都必须是 401，不能提前 429。
	// （若 clear 失效，计数从 4 继续累加，第 2 次就会 429 → 本段变红。）
	for i := 1; i <= maxAttempts; i++ {
		code, body := postLogin(t, f.h, ip, "admin", "wrong", "")
		if code != http.StatusOK || decodeLogin(t, body).Code != 401 {
			t.Fatalf("清零后第 %d 次失败应为 401，得到 HTTP %d: %s", i, code, body)
		}
	}
	// 再下一次才是 429（证明计数确实从 0 重新开始，而不是没清）。
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("清零后重新攒满 %d 次应锁定，得到 HTTP %d: %s", maxAttempts, code, body)
	}
}

// ---- 账号不存在与密码错误同文案同计数；2FA 输错也计入 ----

func TestLoginFailuresShareCounterAndMessage(t *testing.T) {
	const maxAttempts = 2
	f := newLoginFixture(t, maxAttempts, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))
	const ip = "203.0.113.40"

	_, missing := postLogin(t, f.h, ip, "nobody", "whatever", "")
	_, wrongPw := postLogin(t, f.h, ip, "admin", "wrong", "")

	// 文案必须逐字相同（不泄露账号是否存在）：整段 body 相等才是硬证据。
	if missing != wrongPw {
		t.Fatalf("账号不存在与密码错误必须响应一致\n不存在: %s\n密码错: %s", missing, wrongPw)
	}
	if got := decodeLogin(t, missing).Msg; got != "账号或密码错误" {
		t.Fatalf("共用文案应为「账号或密码错误」，得到: %s", got)
	}
	// 账号不存在也计入**该用户名**的账号维度计数：nobody 已失败 1 次，
	// 再错 1 次（阈值 2）后即锁。
	postLogin(t, f.h, ip, "nobody", "whatever", "")
	if code, body := postLogin(t, f.h, ip, "nobody", "whatever", ""); code != http.StatusTooManyRequests {
		t.Fatalf("账号不存在的失败也应计入账号维度计数，得到 HTTP %d: %s", code, body)
	}
}

func TestLoginWrongOTPCountsAsFailure(t *testing.T) {
	const maxAttempts = 2
	gin.SetMode(gin.TestMode)
	clock := &fakeClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	h := &AdminAuthRoutes{
		app:  httpx.NewApp(nil, nil),
		now:  clock.now,
		auth: newLoginGuard(maxAttempts, 900*time.Second, clock.now),
	}
	h.lookupAdmin = func(_ context.Context, username string) (*adminCredentials, error) {
		return &adminCredentials{
			ID: 1, Username: username, PasswordHash: string(hash),
			Google2FAEnabled: true,
			Google2FASecret:  sql.NullString{String: "JBSWY3DPEHPK3PXP", Valid: true},
		}, nil
	}

	// 密码对但 2FA 错 → 计为失败。
	for i := 1; i <= maxAttempts; i++ {
		code, body := postLogin(t, h, "203.0.113.50", "admin", "pw", "000000")
		if code != http.StatusOK || decodeLogin(t, body).Code != 401 {
			t.Fatalf("第 %d 次 2FA 错误应为 401，得到 HTTP %d: %s", i, code, body)
		}
		// 确认确实走到了 2FA 分支（密码本身是对的），而不是在更早的分支失败。
		if got := decodeLogin(t, body).Msg; got != "验证码错误" {
			t.Fatalf("第 %d 次应因 2FA 错误失败，得到: %s", i, got)
		}
	}
	if code, body := postLogin(t, h, "203.0.113.50", "admin", "pw", "000000"); code != http.StatusTooManyRequests {
		t.Fatalf("2FA 连续错误应触发锁定，得到 HTTP %d: %s", code, body)
	}
}

// ---- 用例 4：env 改阈值生效 / 非法值回退默认 ----

func TestLoginGuardEnvOverrides(t *testing.T) {
	t.Run("env 生效", func(t *testing.T) {
		t.Setenv(envAdminLoginMaxAttempts, "2")
		t.Setenv(envAdminLoginLockSeconds, "120")

		f := newLoginFixture(t, 0, 0, testAccounts(t, map[string]string{"admin": "pw"}))
		f.h.auth = newLoginGuardFromEnv() // 让 env 真正决定阈值
		f.h.auth.now = f.clock.now

		const ip = "203.0.113.60"
		for i := 1; i <= 2; i++ {
			if code, body := postLogin(t, f.h, ip, "admin", "wrong", ""); code != http.StatusOK ||
				decodeLogin(t, body).Code != 401 {
				t.Fatalf("阈值 2 时第 %d 次失败应为 401，得到 HTTP %d: %s", i, code, body)
			}
		}
		if code, body := postLogin(t, f.h, ip, "admin", "pw", ""); code != http.StatusTooManyRequests {
			t.Fatalf("ADMIN_LOGIN_MAX_ATTEMPTS=2 时第 3 次应 429，得到 HTTP %d: %s", code, body)
		}
		// ADMIN_LOGIN_LOCK_SECONDS=120：119 秒后仍锁，121 秒后放行。
		f.clock.advance(119 * time.Second)
		if code, body := postLogin(t, f.h, ip, "admin", "pw", ""); code != http.StatusTooManyRequests {
			t.Fatalf("锁定时长应取自 env（119s 仍锁），得到 HTTP %d: %s", code, body)
		}
		f.clock.advance(2 * time.Second)
		if code, body := postLogin(t, f.h, ip, "admin", "pw", ""); code != http.StatusOK ||
			decodeLogin(t, body).Msg != "登录成功" {
			t.Fatalf("121s 后应放行，得到 HTTP %d: %s", code, body)
		}
	})

	t.Run("非法值回退默认", func(t *testing.T) {
		cases := []struct {
			name       string
			attempts   string
			lockSec    string
			wantMax    int
			wantLocked time.Duration
		}{
			{"空值", "", "", defaultLoginMaxAttempts, defaultLoginLockSeconds * time.Second},
			{"非数字", "abc", "xyz", defaultLoginMaxAttempts, defaultLoginLockSeconds * time.Second},
			{"零与负数", "0", "-1", defaultLoginMaxAttempts, defaultLoginLockSeconds * time.Second},
			{"锁定时长超上限", "3", "99999999", 3, defaultLoginLockSeconds * time.Second},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv(envAdminLoginMaxAttempts, tc.attempts)
				t.Setenv(envAdminLoginLockSeconds, tc.lockSec)
				g := newLoginGuardFromEnv()
				if g.maxAttempts != tc.wantMax {
					t.Fatalf("maxAttempts 应为 %d，得到 %d", tc.wantMax, g.maxAttempts)
				}
				if g.lockFor != tc.wantLocked {
					t.Fatalf("lockFor 应为 %v，得到 %v", tc.wantLocked, g.lockFor)
				}
			})
		}
	})
}

// ---- IP 维度：每分钟上限，超限只 429 一分钟，绝不产生 15 分钟长锁 ----
//
// 背景：nginx 把 $remote_addr 写进 X-Forwarded-For，set_real_ip_from 只认 CF 网段。
// 非 CF CDN 后面全站共享 CDN 节点 IP，若 IP 维度还按失败数锁 900 秒，
// 攻击者编个用户名打 5 次就能锁死全体管理员。IP 维度因此改成每分钟 30 次。

func TestLoginIPPerMinuteLimit(t *testing.T) {
	const ip = "203.0.113.70"
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))

	// 前 30 次放行（每次换个不存在的用户名，避免撞上账号维度）。
	for i := 1; i <= defaultLoginIPPerMinute; i++ {
		code, body := postLogin(t, f.h, ip, "ghost"+itoa(i), "x", "")
		if code != http.StatusOK || decodeLogin(t, body).Code != 401 {
			t.Fatalf("第 %d 次应在 IP 上限内放行（401），得到 HTTP %d: %s", i, code, body)
		}
	}

	// 第 31 次：即使账号密码正确也 429。
	code, body := postLogin(t, f.h, ip, "admin", "correct-horse", "")
	if code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次应返回 HTTP 429，得到 HTTP %d: %s", defaultLoginIPPerMinute+1, code, body)
	}
	if got := decodeLogin(t, body); got.Code != 429 || !strings.Contains(got.Msg, "登录失败次数过多") {
		t.Fatalf("限流响应应为 code 429 + 明确文案，得到 %+v", got)
	}

	// 关键断言：剩余等待时间必须在 1 分钟内，而不是 15 分钟。
	// 59 秒时仍限流，61 秒后恢复 —— 若退化成 lockFor（900 秒），这里会拿到 429。
	f.clock.advance(59 * time.Second)
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("窗口内应仍限流，得到 HTTP %d: %s", code, body)
	}
	f.clock.advance(2 * time.Second)
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusOK ||
		decodeLogin(t, body).Msg != "登录成功" {
		t.Fatalf("1 分钟窗口过后应恢复登录，得到 HTTP %d: %s", code, body)
	}
}

func TestLoginIPRateDoesNotLockAccountDimension(t *testing.T) {
	// 同一 IP 同一用户名连续失败 5 次 → 账号维度仍按原口径锁 900 秒。
	const ip = "203.0.113.71"
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))

	for i := 1; i <= 5; i++ {
		postLogin(t, f.h, ip, "admin", "wrong", "")
	}
	// 第 6 次：账号维度锁定（即使 IP 窗口远未到 30 次）。
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("同 IP 同用户名 %d 次失败后应锁定，得到 HTTP %d: %s", 5, code, body)
	}

	// 900 秒内不恢复；901 秒后恢复 —— 证明账号维度的锁定期没被缩短成 1 分钟。
	f.clock.advance(899 * time.Second)
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("账号维度应锁 900 秒（899s 仍锁），得到 HTTP %d: %s", code, body)
	}
	f.clock.advance(2 * time.Second)
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusOK ||
		decodeLogin(t, body).Msg != "登录成功" {
		t.Fatalf("901s 后账号维度应解锁，得到 HTTP %d: %s", code, body)
	}
}

func TestLoginIPPerMinuteEnv(t *testing.T) {
	t.Run("env 生效", func(t *testing.T) {
		t.Setenv(envAdminLoginIPPerMinute, "3")
		g := newLoginGuardFromEnv()
		if g.ipPerMinute != 3 {
			t.Fatalf("ADMIN_LOGIN_IP_PER_MINUTE=3 应生效，得到 %d", g.ipPerMinute)
		}
	})

	t.Run("非法值回退默认", func(t *testing.T) {
		for _, raw := range []string{"", "abc", "0", "-5"} {
			t.Setenv(envAdminLoginIPPerMinute, raw)
			if got := newLoginGuardFromEnv().ipPerMinute; got != defaultLoginIPPerMinute {
				t.Fatalf("ADMIN_LOGIN_IP_PER_MINUTE=%q 应回退默认 %d，得到 %d",
					raw, defaultLoginIPPerMinute, got)
			}
		}
	})
}

// 成功登录不清 IP 窗口：否则握有任一合法账号即可把共享出口的计数刷回 0。
func TestLoginSuccessDoesNotResetIPWindow(t *testing.T) {
	const ip = "203.0.113.72"
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "pw"}))

	for i := 1; i < defaultLoginIPPerMinute; i++ {
		postLogin(t, f.h, ip, "ghost"+itoa(i), "x", "")
	}
	// 成功登录一次（同时清掉 admin 的账号维度计数）。
	if code, body := postLogin(t, f.h, ip, "admin", "pw", ""); code != http.StatusOK {
		t.Fatalf("成功登录应放行，得到 HTTP %d: %s", code, body)
	}
	// IP 窗口已记满 30 次（29 次失败 + 1 次成功），下一次必须仍被限流。
	if code, body := postLogin(t, f.h, ip, "admin", "pw", ""); code != http.StatusTooManyRequests {
		t.Fatalf("成功登录不应重置 IP 分钟窗口，得到 HTTP %d: %s", code, body)
	}
}

// ---- 内存防撑大：条目上限与过期清理 ----

func TestLoginGuardBoundsMemory(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	g := newLoginGuard(5, time.Minute, clock.now)
	g.maxEntries = 3

	// 灌 100 个不同 IP（IP 维度走 recordAttempt：每分钟尝试计数）。
	for i := 0; i < 100; i++ {
		g.recordAttempt("192.0.2." + itoa(i))
	}
	if len(g.entries) > g.maxEntries {
		t.Fatalf("条目数应被上限约束在 %d 内，实际 %d", g.maxEntries, len(g.entries))
	}

	// 推进到全部过期后，惰性清理应把表清空。
	clock.advance(2 * time.Minute)
	g.blocked("192.0.2.1", "")
	if len(g.entries) != 0 {
		t.Fatalf("过期项应被惰性清理，实际残留 %d 条", len(g.entries))
	}

	// 账号维度同样受上限约束：灌海量不同用户名也撑不大表。
	for i := 0; i < 100; i++ {
		g.recordFailure("198.51.100.1", "u"+itoa(i))
	}
	if len(g.entries) > g.maxEntries {
		t.Fatalf("账号维度条目数应被上限约束在 %d 内，实际 %d", g.maxEntries, len(g.entries))
	}
}

// itoa 避免为一句格式化引入 strconv 依赖歧义。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
