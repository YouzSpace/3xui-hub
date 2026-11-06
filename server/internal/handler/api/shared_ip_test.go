package api

// 共享出口（CDN 未还原真实 IP / 大 NAT）检测的单测。
//
// 背景：nginx 把 $remote_addr 写进 X-Forwarded-For，而 set_real_ip_from 只认
// Cloudflare 网段。站点挂阿里云/腾讯云等非 CF CDN 时，全站用户共享 CDN 节点 IP；
// 此时 code_ip（20 次/小时）与 register（10 次/小时）变成全站共享额度，正常业务被打满。
//
// 覆盖验收要求的三组：
//   - 同一 IP 关联 50 个不同 email 后，code_ip / register 不再拦截；该 email 的每日上限仍生效；
//   - 未达阈值（3 个 email）时两个纯 IP 维度照旧拦截；
//   - RATE_SHARED_IP_FANOUT=0 时检测关闭、行为回到现状。
//
// 变异测试锚点：把 registerBlocked / codeSendBlocked 里的 `shared`（或 observe 返回值）
// 恒置为 false，本文件所有「共享出口不再拦截」的用例必须变红。

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- 测试脚手架 ----

// memCache 是 Cache 的最小内存实现，行为对齐 DBCache 的可观测口径：
// 读过期即视为不存在，Add 不覆盖已有键，Increment 不延长 expiration。
type memCache struct {
	vals   map[string]any
	expire map[string]time.Time
	now    func() time.Time
}

func newMemCache(now func() time.Time) *memCache {
	return &memCache{vals: map[string]any{}, expire: map[string]time.Time{}, now: now}
}

func (c *memCache) live(key string) bool {
	exp, ok := c.expire[key]
	return ok && c.now().Before(exp)
}

func (c *memCache) Get(_ context.Context, key string) (any, bool, error) {
	if !c.live(key) {
		return nil, false, nil
	}
	return c.vals[key], true, nil
}

func (c *memCache) Put(_ context.Context, key string, val any, ttl time.Duration) error {
	c.vals[key] = val
	c.expire[key] = c.now().Add(ttl)
	return nil
}

func (c *memCache) Forget(_ context.Context, key string) error {
	delete(c.vals, key)
	delete(c.expire, key)
	return nil
}

func (c *memCache) Add(_ context.Context, key string, val any, ttl time.Duration) (bool, error) {
	if c.live(key) {
		return false, nil
	}
	c.vals[key] = val
	c.expire[key] = c.now().Add(ttl)
	return true, nil
}

func (c *memCache) Increment(_ context.Context, key string) (int64, bool, error) {
	if !c.live(key) {
		return 0, false, nil
	}
	n, _ := cacheInt(c.vals[key])
	n++
	c.vals[key] = n
	return n, true, nil
}

// rateFixture 是共享出口用例的脚手架：handler + 可控时钟 + 内存 cache。
type rateFixture struct {
	h     *Handler
	clock *fakeRateClock
	cache *memCache
}

type fakeRateClock struct{ t time.Time }

func (c *fakeRateClock) now() time.Time          { return c.t }
func (c *fakeRateClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// siteConfigs 固定阈值：code_ip 每小时 20、register 每小时 10、code_daily 每日 10。
func siteConfigs() map[string]string {
	return map[string]string{
		"rate_code_interval":      "0", // 关掉发码间隔，用例只关心 IP / 每日维度
		"rate_code_per_day":       "10",
		"rate_code_ip_hourly":     "20",
		"rate_register_ip_hourly": "10",
	}
}

type rateStore struct{ Store }

func (s *rateStore) SiteConfigMany(_ context.Context, keys []string) (map[string]string, error) {
	cfg := siteConfigs()
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = cfg[k]
	}
	return out, nil
}

func newRateFixture(t *testing.T) *rateFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	clock := &fakeRateClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	cache := newMemCache(clock.now)
	h := &Handler{
		store:       &rateStore{},
		cache:       cache,
		limiter:     newRateLimiter(cache),
		sharedExits: newSharedExitDetector(log.New(io.Discard, "", 0), clock.now),
		logger:      log.New(io.Discard, "", 0),
		now:         clock.now,
	}
	h.limiter.now = clock.now
	return &rateFixture{h: h, clock: clock, cache: cache}
}

// sendCode 走一次真实的 POST /api/email-verify/send（限流判定 + 记数都在 handler 里）。
// 返回 HTTP 状态码：429 表示被限流，其余（400/500/200）表示放行到了后面的业务分支
// —— 本用例只关心「限流有没有拦」。
func sendCode(t *testing.T, h *Handler, ip, email string) int {
	t.Helper()
	engine := gin.New()
	engine.POST("/api/email-verify/send", h.handleEmailVerifySend)

	body := `{"email":"` + email + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/email-verify/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code
}

// register 走一次真实的 POST /api/register（只到限流这一步，后面的校验失败不关心）。
func register(t *testing.T, h *Handler, ip, email string) int {
	t.Helper()
	engine := gin.New()
	engine.POST("/api/register", h.handleRegister)

	body := `{"email":"` + email + `","password":"pw123456"}`
	req := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code
}

func isBlocked(code int) bool { return code == http.StatusTooManyRequests }

// rateItoa 避免依赖本包其他测试文件里的 itoa（签名可能不同）。
func rateItoa(n int) string { return strconv.Itoa(n) }

// distinctEmails 生成 n 个互不相同的邮箱。
func distinctEmails(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = "user" + rateItoa(i) + "@example.com"
	}
	return out
}

// ---- 用例 1：未达阈值时两个纯 IP 维度照旧拦截 ----

func TestSharedIPBelowThresholdStillBlocks(t *testing.T) {
	f := newRateFixture(t)
	const ip = "198.51.100.10"

	// 3 个账号，远低于默认阈值 50。
	for _, email := range distinctEmails(3) {
		for i := 0; i < 8; i++ { // 每个账号打 8 次，合计 24 次 > code_ip 的 20
			sendCode(t, f.h, ip, email)
		}
	}
	if code := sendCode(t, f.h, ip, "fresh@example.com"); !isBlocked(code) {
		t.Fatalf("未达阈值（3 个账号）时 code_ip 应照旧拦截，得到 HTTP %d", code)
	}

	// register：同 IP 每小时 10 次，打 10 次后第 11 次拦。
	f2 := newRateFixture(t)
	for _, email := range distinctEmails(3) {
		for i := 0; i < 4; i++ { // 12 次，但只有 3 个不同邮箱
			register(t, f2.h, ip, email)
		}
	}
	if code := register(t, f2.h, ip, "fresh2@example.com"); !isBlocked(code) {
		t.Fatalf("未达阈值（3 个账号）时 register 应照旧拦截，得到 HTTP %d", code)
	}
}

// ---- 用例 2：达到阈值后两个纯 IP 维度不再拦截，账号维度仍生效 ----

func TestSharedIPAboveThresholdSkipsIPDimensions(t *testing.T) {
	f := newRateFixture(t)
	const ip = "203.0.113.77"
	emails := distinctEmails(defaultRateSharedIPFanout)

	// 让同一 IP 关联 50 个不同账号。
	// 注意：在凑满阈值之前，code_ip 仍按默认 20 次/小时拦截（这是有意的 ——
	// 检测本身就要先观察到足够多的账号）。被拦的请求同样会登记账号，
	// 所以真实站点上这些 429 会随第 50 个账号出现而消失。
	for _, email := range emails {
		sendCode(t, f.h, ip, email)
	}
	if !f.h.sharedExits.isShared(ip) {
		t.Fatalf("关联 %d 个账号后应判定为共享出口", defaultRateSharedIPFanout)
	}

	// 全新的第 51 个账号：必须放行（code_ip 被跳过，且历史窗口已清空）。
	if code := sendCode(t, f.h, ip, "brand-new@example.com"); isBlocked(code) {
		t.Fatalf("达到阈值后 code_ip 应跳过，得到 HTTP %d", code)
	}
	// 阈值判定前就被打满的那个账号：也应立刻恢复（历史 code_ip 计数已清）。
	if code := sendCode(t, f.h, ip, emails[defaultRateCodeIPHourly]); isBlocked(code) {
		t.Fatalf("判定为共享出口后，历史窗口里被打满的账号也应立刻放行，得到 HTTP %d", code)
	}

	// 但账号维度（该 email 每日上限 10 次）仍然生效。
	// 注意 code_interval 已在夹具里关掉，只剩每日 10 次这一条。
	var last int
	for i := 0; i < defaultRateCodePerDay+1; i++ {
		last = sendCode(t, f.h, ip, "daily@example.com")
	}
	if !isBlocked(last) {
		t.Fatalf("共享出口上该 email 的每日上限仍应生效，第 %d 次得到 HTTP %d",
			defaultRateCodePerDay+1, last)
	}

	// register：同一共享出口上也应跳过（register 默认每小时 10 次）。
	f2 := newRateFixture(t)
	for _, email := range emails {
		register(t, f2.h, ip, email)
	}
	if code := register(t, f2.h, ip, "register-new@example.com"); isBlocked(code) {
		t.Fatalf("达到阈值后 register 应跳过，得到 HTTP %d", code)
	}
}

// ---- 用例 3：RATE_SHARED_IP_FANOUT=0 关闭检测，行为回到现状 ----

func TestSharedIPDetectionDisabledByZero(t *testing.T) {
	t.Setenv(envRateSharedIPFanout, "0")

	f := newRateFixture(t)
	if f.h.sharedExits.fanout != 0 {
		t.Fatalf("RATE_SHARED_IP_FANOUT=0 应关闭检测，fanout 得到 %d", f.h.sharedExits.fanout)
	}
	const ip = "203.0.113.90"

	// 关联 50+ 个账号，code_ip 仍按 20 次/小时拦截。
	var blockedAt int
	for i, email := range distinctEmails(defaultRateSharedIPFanout + 5) {
		if isBlocked(sendCode(t, f.h, ip, email)) {
			blockedAt = i + 1
			break
		}
	}
	if blockedAt != defaultRateCodeIPHourly+1 {
		t.Fatalf("关闭检测后 code_ip 应在第 %d 次拦截，实际第 %d 次",
			defaultRateCodeIPHourly+1, blockedAt)
	}

	// register 同理：第 11 次拦截。
	f2 := newRateFixture(t)
	blockedAt = 0
	for i, email := range distinctEmails(defaultRateSharedIPFanout + 5) {
		if isBlocked(register(t, f2.h, ip, email)) {
			blockedAt = i + 1
			break
		}
	}
	if blockedAt != defaultRateRegisterIPHourly+1 {
		t.Fatalf("关闭检测后 register 应在第 %d 次拦截，实际第 %d 次",
			defaultRateRegisterIPHourly+1, blockedAt)
	}
}

// ---- env 解析：默认 50、非法值回退、0 是合法关闭信号 ----

func TestSharedIPFanoutEnvParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultRateSharedIPFanout},
		{"0", 0},
		{"50", 50},
		{"120", 120},
		{"abc", defaultRateSharedIPFanout},
		{"-1", defaultRateSharedIPFanout},
	}
	for _, tc := range cases {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			t.Setenv(envRateSharedIPFanout, tc.raw)
			if got := rateSharedIPFanoutFromEnv(); got != tc.want {
				t.Fatalf("RATE_SHARED_IP_FANOUT=%q 解析为 %d，期望 %d", tc.raw, got, tc.want)
			}
		})
	}
}

// ---- 内存上限与惰性清理 ----

func TestSharedExitDetectorBoundsMemory(t *testing.T) {
	clock := &fakeRateClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	d := newSharedExitDetector(log.New(io.Discard, "", 0), clock.now)
	d.fanout = defaultRateSharedIPFanout
	// 换一个更小的上限便于验证淘汰：直接改包级常量做不到，这里用等价断言
	// —— 灌满 rateSharedIPMaxEntries 之外的 IP 后，表规模不能超过上限。
	for i := 0; i < rateSharedIPMaxEntries+50; i++ {
		d.observe("10.0."+rateItoa(i/250)+"."+rateItoa(i%250), "u"+rateItoa(i)+"@example.com")
	}
	if len(d.entries) > rateSharedIPMaxEntries {
		t.Fatalf("IP 表条目数应被上限约束在 %d 内，实际 %d", rateSharedIPMaxEntries, len(d.entries))
	}

	// 单 IP 账号集合封顶。
	ip := "10.9.9.9"
	for i := 0; i < rateSharedIPMaxAccounts+100; i++ {
		d.observe(ip, "a"+rateItoa(i)+"@example.com")
	}
	if got := len(d.entries[ip].accounts); got > rateSharedIPMaxAccounts {
		t.Fatalf("单 IP 账号集合应封顶 %d，实际 %d", rateSharedIPMaxAccounts, got)
	}

	// 窗口过期后惰性清理应把表清空。
	clock.advance(rateSharedIPWindow + rateSharedIPSweepInterval + time.Second)
	d.observe("10.1.1.1", "sweep@example.com")
	if len(d.entries) != 1 {
		t.Fatalf("过期项应被惰性清理，实际残留 %d 条", len(d.entries))
	}
}

// ---- 判定必须发生在拦截之前：低阈值环境下也能长到 fanout ----
//
// 这条用例锁住实现顺序：若把 observe 挪到 code_ip 判定之后，被 429 的请求
// 就不会再登记账号，账号集合永远停在 20（code_ip 默认上限），检测永不触发。
func TestSharedIPDetectionGrowsPastIPLimit(t *testing.T) {
	t.Setenv(envRateSharedIPFanout, "25")

	f := newRateFixture(t)
	if f.h.sharedExits.fanout != 25 {
		t.Fatalf("阈值应为 25，得到 %d", f.h.sharedExits.fanout)
	}
	const ip = "203.0.113.200"

	// 30 个账号依次尝试：前 20 个放行，第 21–25 个被 code_ip 429，
	// 但每个请求都必须被登记，第 25 个之后判定为共享出口。
	for _, email := range distinctEmails(25) {
		sendCode(t, f.h, ip, email)
	}
	if !f.h.sharedExits.isShared(ip) {
		t.Fatalf("被 429 的请求也应登记账号：25 个账号后仍未判定为共享出口")
	}
	if code := sendCode(t, f.h, ip, "after-threshold@example.com"); isBlocked(code) {
		t.Fatalf("达到阈值后 code_ip 应跳过，得到 HTTP %d", code)
	}
}
