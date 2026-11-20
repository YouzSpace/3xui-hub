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
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	// 本用例只验「每分钟尝试次数」这道闸门：把失败滑动窗口的上限抬高，
	// 免得 30 次连续失败先撞上它（那道闸门见 TestLoginIPFailureSlidingWindow）。
	f.h.auth.ipFailMax = 1000

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
	// 同上：本用例针对「每分钟尝试次数」窗口，抬高失败滑动窗口的上限。
	f.h.auth.ipFailMax = 1000

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

// ---- 用例 5：IP 失败滑动窗口（issue #142 的慢速撞库）----
//
// 实测攻击形态：同一 IP 每 15~20 秒试一次、每次换一个用户名。账号维度永远到不了
// 5 次阈值，每分钟 30 次的尝试上限也永远到不了 —— 只有「窗口内失败总数」拦得住。
//
// 变异锚点：把 blocked() 里 ipFailures 的那段判断删掉，本组用例全部变红。

func TestLoginIPFailureSlidingWindow(t *testing.T) {
	const ip = "203.0.113.80"
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))
	f.h.auth.ipFailMax = 3
	f.h.auth.ipFailWindow = 5 * time.Minute

	// 3 次失败，每次换一个用户名：账号维度一次都不会到阈值。
	for i := 1; i <= 3; i++ {
		code, body := postLogin(t, f.h, ip, "ghost"+itoa(i), "x", "")
		if code != http.StatusOK || decodeLogin(t, body).Code != 401 {
			t.Fatalf("第 %d 次失败应在 IP 窗口上限内放行（401），得到 HTTP %d: %s", i, code, body)
		}
	}

	// 第 4 次：即使账号密码正确也必须 429（判锁排在查库之前）。
	code, body := postLogin(t, f.h, ip, "admin", "correct-horse", "")
	if code != http.StatusTooManyRequests {
		t.Fatalf("同一 IP 换用户名累计 3 次失败后应 429，得到 HTTP %d: %s", code, body)
	}
	if got := decodeLogin(t, body); got.Code != 429 || !strings.Contains(got.Msg, "登录失败次数过多") {
		t.Fatalf("限流响应应为 code 429 + 现有文案，得到 %+v", got)
	}

	// 滑动窗口没走完就继续拒；且被拒的请求不再计数（不会把窗口无限顺延）。
	f.clock.advance(4*time.Minute + 59*time.Second)
	for i := 0; i < 3; i++ {
		if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
			t.Fatalf("窗口内（4m59s）应仍限流，得到 HTTP %d: %s", code, body)
		}
	}

	// 最老一次失败滑出窗口后恢复登录。
	f.clock.advance(2 * time.Second)
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusOK ||
		decodeLogin(t, body).Msg != "登录成功" {
		t.Fatalf("失败滑出窗口后应放行，得到 HTTP %d: %s", code, body)
	}

	// 窗口重置后必须重新能拦住：证明不是「一旦放行就永久失效」。
	for i := 1; i <= 3; i++ {
		if code, body := postLogin(t, f.h, ip, "ghost2-"+itoa(i), "x", ""); code != http.StatusOK ||
			decodeLogin(t, body).Code != 401 {
			t.Fatalf("第二轮第 %d 次失败应放行（401），得到 HTTP %d: %s", i, code, body)
		}
	}
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("第二轮攒满 3 次失败后应再次 429，得到 HTTP %d: %s", code, body)
	}
}

func TestLoginIPFailureWindowIsolation(t *testing.T) {
	const (
		ipA = "203.0.113.81"
		ipB = "203.0.113.82"
	)
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))
	f.h.auth.ipFailMax = 3
	f.h.auth.ipFailWindow = 5 * time.Minute

	for i := 1; i <= 3; i++ {
		postLogin(t, f.h, ipA, "ghost"+itoa(i), "x", "")
	}
	if code, _ := postLogin(t, f.h, ipA, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("A 攒满失败后应被限流，得到 HTTP %d", code)
	}

	// B 的窗口是空的：同一时刻必须能正常登录（不同 IP 不互相影响）。
	if code, body := postLogin(t, f.h, ipB, "admin", "correct-horse", ""); code != http.StatusOK ||
		decodeLogin(t, body).Msg != "登录成功" {
		t.Fatalf("B 未失败过，不应被 A 连坐，得到 HTTP %d: %s", code, body)
	}

	// B 自己攒满同样会被拦。
	for i := 1; i <= 3; i++ {
		postLogin(t, f.h, ipB, "other"+itoa(i), "x", "")
	}
	if code, _ := postLogin(t, f.h, ipB, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("B 攒满失败后应被限流，得到 HTTP %d", code)
	}
}

func TestLoginIPFailureWindowCountsEmptyUsername(t *testing.T) {
	// 空用户名也必须计入 IP 失败窗口：否则「编个空用户名」就能绕开限流，
	// 只剩每分钟 30 次的尝试上限。
	const ip = "203.0.113.83"
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "correct-horse"}))
	f.h.auth.ipFailMax = 3
	f.h.auth.ipFailWindow = 5 * time.Minute

	for i := 1; i <= 3; i++ {
		postLogin(t, f.h, ip, "", "x", "")
	}
	if code, body := postLogin(t, f.h, ip, "admin", "correct-horse", ""); code != http.StatusTooManyRequests {
		t.Fatalf("空用户名的失败也应计入 IP 窗口，第 4 次应 429，得到 HTTP %d: %s", code, body)
	}
}

func TestLoginGuardIPFailureEnv(t *testing.T) {
	t.Run("env 生效", func(t *testing.T) {
		t.Setenv(envAdminLoginIPFailMax, "7")
		t.Setenv(envAdminLoginIPFailWindowSeconds, "120")
		g := newLoginGuardFromEnv()
		if g.ipFailMax != 7 {
			t.Fatalf("ADMIN_LOGIN_IP_FAIL_MAX=7 应生效，得到 %d", g.ipFailMax)
		}
		if g.ipFailWindow != 2*time.Minute {
			t.Fatalf("ADMIN_LOGIN_IP_FAIL_WINDOW_SECONDS=120 应生效，得到 %v", g.ipFailWindow)
		}
	})

	t.Run("默认值保守", func(t *testing.T) {
		g := newLoginGuardFromEnv()
		if g.ipFailMax != defaultLoginIPFailMax || g.ipFailWindow != defaultLoginIPFailWindowSeconds*time.Second {
			t.Fatalf("默认应为 %d 次 / %d 秒，得到 %d 次 / %v",
				defaultLoginIPFailMax, defaultLoginIPFailWindowSeconds, g.ipFailMax, g.ipFailWindow)
		}
	})

	t.Run("非法值回退默认", func(t *testing.T) {
		for _, raw := range []string{"", "abc", "0", "-5"} {
			t.Setenv(envAdminLoginIPFailMax, raw)
			if got := newLoginGuardFromEnv().ipFailMax; got != defaultLoginIPFailMax {
				t.Fatalf("ADMIN_LOGIN_IP_FAIL_MAX=%q 应回退默认 %d，得到 %d", raw, defaultLoginIPFailMax, got)
			}
		}
		// 窗口超上限（> 24h）按配错处理，回退默认。
		t.Setenv(envAdminLoginIPFailWindowSeconds, "999999999")
		if got := newLoginGuardFromEnv().ipFailWindow; got != defaultLoginIPFailWindowSeconds*time.Second {
			t.Fatalf("窗口超上限应回退默认 %ds，得到 %v", defaultLoginIPFailWindowSeconds, got)
		}
	})
}

// ---- 用例 6：登录失败审计按 IP 窗口聚合（issue #142 第 3 条）----
//
// 原来中间件对每个 POST 无论成败都落一条 operation_logs，实测 18,756 行登录失败
// 占了全表 92.5%。现在失败明细由 handler 置 OpLogSkipKey 跳过，只在「某 IP 的失败
// 窗口达到阈值」时落一条 type='system' 汇总（带次数）。

func newAuditCapture(f *loginFixture) *[]loginAuditEvent {
	events := &[]loginAuditEvent{}
	f.h.auditLogin = func(_ context.Context, ev *loginAuditEvent) error {
		*events = append(*events, *ev)
		return nil
	}
	return events
}

func TestLoginFailureAuditAggregationPerIPWindow(t *testing.T) {
	const (
		ipA = "203.0.113.90"
		ipB = "203.0.113.91"
	)
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "pw"}))
	f.h.auth.ipFailMax = 5
	f.h.auth.ipFailWindow = 10 * time.Minute
	events := newAuditCapture(f)

	// 未到阈值：一条审计都不落（正常手滑不该刷审计）。
	for i := 1; i < 5; i++ {
		postLogin(t, f.h, ipA, "ghost"+itoa(i), "x", "")
	}
	if len(*events) != 0 {
		t.Fatalf("未到阈值不应落审计，实际 %d 条", len(*events))
	}

	// 第 5 次失败：到阈值 → 恰好一条汇总，带次数与账号数。
	postLogin(t, f.h, ipA, "ghost5", "x", "")
	if len(*events) != 1 {
		t.Fatalf("到阈值应落 1 条汇总审计，实际 %d 条", len(*events))
	}
	ev := (*events)[0]
	if ev.IP != ipA || !ev.IPLimited || ev.AccountLocked {
		t.Fatalf("汇总内容不对: %+v", ev)
	}
	if ev.IPFailures != 5 || ev.Accounts != 5 || ev.Window != 10*time.Minute {
		t.Fatalf("汇总应带窗口内次数 5 / 账号 5 / 窗口 10m，得到 %+v", ev)
	}

	// 触发后的限流请求（429）不再追加审计。
	for i := 0; i < 5; i++ {
		postLogin(t, f.h, ipA, "ghostX", "x", "")
	}
	if len(*events) != 1 {
		t.Fatalf("同一 IP 同一窗口只应落 1 条审计，实际 %d 条", len(*events))
	}

	// 另一个 IP 自己一条（窗口互相独立）。
	for i := 1; i <= 5; i++ {
		postLogin(t, f.h, ipB, "ghostB"+itoa(i), "x", "")
	}
	if len(*events) != 2 || (*events)[1].IP != ipB {
		t.Fatalf("第二个 IP 应单独落一条，实际 %d 条: %+v", len(*events), *events)
	}
}

func TestLoginAccountLockEmitsSingleAudit(t *testing.T) {
	const ip = "203.0.113.92"
	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "pw"}))
	// IP 窗口上限抬高：本用例只验账号维度锁定时的汇总。
	f.h.auth.ipFailMax = 100
	events := newAuditCapture(f)

	for i := 1; i <= 5; i++ {
		postLogin(t, f.h, ip, "admin", "wrong", "")
	}
	if len(*events) != 1 {
		t.Fatalf("账号锁定应落 1 条汇总审计，实际 %d 条", len(*events))
	}
	if ev := (*events)[0]; !ev.AccountLocked || ev.Username != "admin" || ev.Accounts != 1 {
		t.Fatalf("账号锁定汇总内容不对: %+v", ev)
	}

	// 锁定期间的请求不再落审计（也不再计数）。
	for i := 0; i < 3; i++ {
		postLogin(t, f.h, ip, "admin", "pw", "")
	}
	if len(*events) != 1 {
		t.Fatalf("锁定期内不应追加审计，实际 %d 条", len(*events))
	}
}

// ---- 用例 7：汇总审计真的按 operation_logs 的列落库（假驱动，不连库）----

type loginAuditRow struct {
	Type, Actor, Action, Path string
	Status                    int64
	Detail, IP                string
}

type loginAuditFakeState struct {
	mu   sync.Mutex
	rows []loginAuditRow
}

func (s *loginAuditFakeState) snapshot() []loginAuditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]loginAuditRow, len(s.rows))
	copy(out, s.rows)
	return out
}

func (s *loginAuditFakeState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = nil
}

var (
	loginAuditFake     = &loginAuditFakeState{}
	loginAuditFakeOnce sync.Once
)

const loginAuditFakeDriverName = "adminLoginAuditFake"

// openLoginAuditFakeDB 打开一个只认 operation_logs INSERT 的假库。
func openLoginAuditFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	loginAuditFakeOnce.Do(func() { sql.Register(loginAuditFakeDriverName, loginAuditFakeDriver{}) })
	db, err := sql.Open(loginAuditFakeDriverName, "fake")
	if err != nil {
		t.Fatalf("打开假库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type loginAuditFakeDriver struct{}

func (loginAuditFakeDriver) Open(string) (driver.Conn, error) { return loginAuditFakeConn{}, nil }

type loginAuditFakeConn struct{}

func (loginAuditFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("假驱动只支持 ExecContext")
}
func (loginAuditFakeConn) Close() error { return nil }
func (loginAuditFakeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("假驱动不支持事务")
}

func (loginAuditFakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if !strings.Contains(query, "INSERT INTO operation_logs") {
		return nil, fmt.Errorf("假库未识别的写入: %s", query)
	}
	// 参数顺序必须与 SQL 一致：action, error, ip, created_at（其余是字面量）。
	if len(args) != 4 {
		return nil, fmt.Errorf("operation_logs 写入参数应为 4 个，实际 %d", len(args))
	}
	row := loginAuditRow{
		Type:   "system",
		Actor:  "login-guard",
		Action: fmt.Sprint(args[0].Value),
		Path:   "admin-api/login",
		Status: 401,
		Detail: fmt.Sprint(args[1].Value),
		IP:     fmt.Sprint(args[2].Value),
	}
	if !strings.Contains(query, "'system', 'login-guard'") {
		return nil, fmt.Errorf("审计行应固定为 type=system / actor=login-guard: %s", query)
	}
	loginAuditFake.mu.Lock()
	loginAuditFake.rows = append(loginAuditFake.rows, row)
	loginAuditFake.mu.Unlock()
	return driver.RowsAffected(1), nil
}

func TestInsertLoginFailureAuditRow(t *testing.T) {
	loginAuditFake.reset()
	const ip = "203.0.113.95"

	f := newLoginFixture(t, 5, 900*time.Second,
		testAccounts(t, map[string]string{"admin": "pw"}))
	f.h.db = openLoginAuditFakeDB(t)
	f.h.auditLogin = f.h.insertLoginFailureAudit // 走真实落库实现
	f.h.auth.ipFailMax = 2

	// 两次失败（换用户名）→ 到阈值 → 一条汇总。
	for i := 1; i <= 2; i++ {
		postLogin(t, f.h, ip, "ghost"+itoa(i), "x", "")
	}

	rows := loginAuditFake.snapshot()
	if len(rows) != 1 {
		t.Fatalf("应落 1 行汇总审计，实际 %d 行", len(rows))
	}
	row := rows[0]
	if row.Type != "system" || row.Actor != "login-guard" || row.Action != loginAuditAction ||
		row.Path != "admin-api/login" || row.Status != 401 {
		t.Fatalf("审计行固定列不对: %+v", row)
	}
	if row.IP != ip {
		t.Fatalf("审计行 ip 应为 %s，实际 %s", ip, row.IP)
	}
	for _, want := range []string{"失败 2 次", "样本：ghost2", "来源 IP 失败次数达到上限"} {
		if !strings.Contains(row.Detail, want) {
			t.Fatalf("审计详情应包含 %q，实际: %s", want, row.Detail)
		}
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
