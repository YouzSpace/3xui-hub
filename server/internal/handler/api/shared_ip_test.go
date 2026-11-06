package api

// 共享出口（CDN 未还原真实 IP / 大 NAT）检测的单测。
//
// 背景：nginx 把 $remote_addr 写进 X-Forwarded-For，而 set_real_ip_from 只认
// Cloudflare 网段。站点挂阿里云/腾讯云等非 CF CDN 时，全站用户共享 CDN 节点 IP；
// 此时 code_ip（20 次/小时）与 register（10 次/小时）变成全站共享额度，正常业务被打满。
//
// 判定成立后的语义是**放大阈值**（RATE_SHARED_IP_MULTIPLIER，默认 10），不是跳过：
//   - 跳过等于「拿 50 个一次性邮箱换无限额度」，实测序列 0×20 → 429×30 → 0×∞；
//   - 放大后 code_ip 20→200、register 10→100，正常 CDN 够用，滥用者仍会撞上限。
//
// 覆盖验收要求：
//   - 未达阈值（3 个 email）时两个纯 IP 维度照旧拦截；
//   - 达到阈值后额度变宽，但仍有上限：累计 200 次后第 201 次拦（register 同理 100/101）；
//   - 第 51 次请求仍会被拦的等价小规模用例（放大后阈值 50）；
//   - RATE_SHARED_IP_MULTIPLIER=0 等价于不放大（第 21 次就被拦）；
//   - RATE_SHARED_IP_FANOUT=0 时检测关闭、行为回到现状。
//
// 变异测试锚点：把 sharedExitDetector.scaled 改成恒返回 0（<=0 即不限），或让
// observe 的判定恒为 true 后直接 return false —— 「仍有上限」的用例必须变红。

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

type rateStore struct {
	Store
	cfg map[string]string
}

func (s *rateStore) SiteConfigMany(_ context.Context, keys []string) (map[string]string, error) {
	cfg := s.cfg
	if cfg == nil {
		cfg = siteConfigs()
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = cfg[k]
	}
	return out, nil
}

func newRateFixture(t *testing.T) *rateFixture { return newRateFixtureWith(t, nil) }

// newRateFixtureWith 用自定义阈值构造夹具；cfg 为 nil 时用 siteConfigs() 的默认值。
func newRateFixtureWith(t *testing.T, cfg map[string]string) *rateFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	clock := &fakeRateClock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	cache := newMemCache(clock.now)
	h := &Handler{
		store:       &rateStore{cfg: cfg},
		cache:       cache,
		limiter:     newRateLimiter(cache),
		sharedExits: newSharedExitDetector(log.New(io.Discard, "", 0), clock.now),
		logger:      log.New(io.Discard, "", 0),
		now:         clock.now,
	}
	h.limiter.now = clock.now
	return &rateFixture{h: h, clock: clock, cache: cache}
}

// attempts 读计数键当前的命中次数（未命中为 0）。
func (f *rateFixture) attempts(key string) int {
	v, ok := f.cache.vals[key]
	if !ok || !f.cache.live(key) {
		return 0
	}
	n, err := cacheInt(v)
	if err != nil {
		return 0
	}
	return int(n)
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

// ---- 用例 2：达到阈值后额度变宽（而不是无限），账号维度仍生效 ----

func TestSharedIPAboveThresholdScalesIPDimensions(t *testing.T) {
	// 显式固定倍数：开发机上若残留 RATE_SHARED_IP_MULTIPLIER，用例会算错期望值。
	t.Setenv(envRateSharedIPMultiplier, strconv.Itoa(defaultRateSharedIPMultiplier))

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

	// 第 51 个账号（全新）：已远超原始 20 的额度，放大到 200 后必须放行。
	if code := sendCode(t, f.h, ip, "brand-new@example.com"); isBlocked(code) {
		t.Fatalf("达到阈值后 code_ip 应放大（200/小时），第 51 次不应被拦，得到 HTTP %d", code)
	}
	// 阈值判定前就被打满的那个账号：放大后额度足够，也应立刻恢复。
	if code := sendCode(t, f.h, ip, emails[defaultRateCodeIPHourly]); isBlocked(code) {
		t.Fatalf("判定为共享出口后，历史窗口里被打满的账号应落在放大后的额度内，得到 HTTP %d", code)
	}

	// 但账号维度（该 email 每日上限 10 次）仍然生效，且与共享出口无关。
	// 注意 code_interval 已在夹具里关掉，只剩每日 10 次这一条。
	var last int
	for i := 0; i < defaultRateCodePerDay+1; i++ {
		last = sendCode(t, f.h, ip, "daily@example.com")
	}
	if !isBlocked(last) {
		t.Fatalf("共享出口上该 email 的每日上限仍应生效，第 %d 次得到 HTTP %d",
			defaultRateCodePerDay+1, last)
	}

	// register：同一共享出口上同样只放大（10 → 100），第 11 次不该再被拦。
	f2 := newRateFixture(t)
	for _, email := range emails {
		register(t, f2.h, ip, email)
	}
	if code := register(t, f2.h, ip, "register-new@example.com"); isBlocked(code) {
		t.Fatalf("达到阈值后 register 应放大（100/小时），得到 HTTP %d", code)
	}
}

// ---- 用例 2b：放大不是「无限」—— 第 51 次仍会被拦 ----
//
// 验收口径原句：「关联 50 个账号后，第 51 次请求仍会被拦（因为阈值是 200 而不是无限）」。
// 默认阈值 20 下第 51 次当然放行（200 > 51），所以这里把放大后的上限对齐到 50 来
// 直接复现「第 51 次被拦」这个行为：原始阈值 5 × 倍数 10 = 50。
// 它验证的是「放大后仍是一个有限且会命中的上限」，和 200/201 是同一逻辑。
func TestSharedIPScaledLimitStillBlocksAt51st(t *testing.T) {
	t.Setenv(envRateSharedIPMultiplier, "10")

	f := newRateFixtureWith(t, map[string]string{
		"rate_code_interval":      "0",
		"rate_code_per_day":       "100000", // 关掉每日维度，只观察 code_ip
		"rate_code_ip_hourly":     "5",      // 5 × 10 = 50
		"rate_register_ip_hourly": "10",
	})
	const ip = "203.0.113.51"

	// 凑满阈值：50 个不同账号（前 5 次放行，其余被 code_ip 429，但都会登记账号）。
	for _, email := range distinctEmails(defaultRateSharedIPFanout) {
		sendCode(t, f.h, ip, email)
	}
	if !f.h.sharedExits.isShared(ip) {
		t.Fatal("应判定为共享出口")
	}

	// 把 code_ip 计数推到放大后的上限 50（未放大时 5 就已封顶，这里靠放大才到 50）。
	for f.attempts(keyCodeIP(ip)) < 50 {
		if code := sendCode(t, f.h, ip, "fill@example.com"); isBlocked(code) {
			t.Fatalf("计数 %d 低于放大后的 50 却被拦（HTTP %d）", f.attempts(keyCodeIP(ip)), code)
		}
	}
	if used := f.attempts(keyCodeIP(ip)); used != 50 {
		t.Fatalf("前置条件：code_ip 计数应为 50，实际 %d", used)
	}
	// 第 51 次必须仍被拦 —— 放大后的上限是 50，不是无限。
	if code := sendCode(t, f.h, ip, "the-51st@example.com"); !isBlocked(code) {
		t.Fatalf("放大后的上限是 50（不是无限），第 51 次应仍被拦，得到 HTTP %d", code)
	}
}

// ---- 用例 2c：放大后的硬上限（code_ip 200 / register 100） ----

func TestSharedIPScaledLimitsAreFinite(t *testing.T) {
	t.Setenv(envRateSharedIPMultiplier, strconv.Itoa(defaultRateSharedIPMultiplier))

	const ip = "203.0.113.31"
	const multiplier = defaultRateSharedIPMultiplier
	wantCodeIP := defaultRateCodeIPHourly * multiplier       // 200
	wantRegister := defaultRateRegisterIPHourly * multiplier // 100

	if wantCodeIP != 200 || wantRegister != 100 {
		t.Fatalf("默认倍数下应为 code_ip 200 / register 100，得到 %d / %d", wantCodeIP, wantRegister)
	}

	// --- code_ip：凑满阈值后继续打，第 200 次放行、第 201 次拦 ---
	// 每日维度调到很大，否则同一 email 第 11 次就被每日上限拦下，code_ip 不再计数。
	f := newRateFixtureWith(t, map[string]string{
		"rate_code_interval":      "0",
		"rate_code_per_day":       "100000",
		"rate_code_ip_hourly":     "20",
		"rate_register_ip_hourly": "10",
	})
	// 先塞满 fanout 个账号，让判定成立。
	for _, email := range distinctEmails(defaultRateSharedIPFanout) {
		sendCode(t, f.h, ip, email)
	}
	if !f.h.sharedExits.isShared(ip) {
		t.Fatal("应判定为共享出口")
	}

	// 凑账号阶段本身也已计入 code_ip；放大后 200 的额度尚未用满，下一次必须放行。
	if got := sendCode(t, f.h, ip, "scaled@example.com"); isBlocked(got) {
		t.Fatalf("已用 %d 次，仍低于放大后的 %d，不应被拦，得到 HTTP %d",
			f.attempts(keyCodeIP(ip)), wantCodeIP, got)
	}
	for f.attempts(keyCodeIP(ip)) < wantCodeIP {
		if code := sendCode(t, f.h, ip, "scaled@example.com"); isBlocked(code) {
			t.Fatalf("计数 %d 仍低于放大后的 %d 却被拦（HTTP %d）",
				f.attempts(keyCodeIP(ip)), wantCodeIP, code)
		}
	}
	if code := sendCode(t, f.h, ip, "scaled@example.com"); !isBlocked(code) {
		t.Fatalf("累计 %d 次后第 %d 次应被拦（放大后仍有上限），得到 HTTP %d",
			wantCodeIP, wantCodeIP+1, code)
	}

	// --- register：凑满阈值后第 100 次放行、第 101 次拦 ---
	f2 := newRateFixture(t)
	for _, email := range distinctEmails(defaultRateSharedIPFanout) {
		register(t, f2.h, ip, email)
	}
	if !f2.h.sharedExits.isShared(ip) {
		t.Fatal("register 场景也应判定为共享出口")
	}
	for f2.attempts(keyRegister(ip)) < wantRegister {
		register(t, f2.h, ip, "scaled-reg@example.com")
	}
	if code := register(t, f2.h, ip, "scaled-reg@example.com"); !isBlocked(code) {
		t.Fatalf("累计 %d 次后第 %d 次应被拦（放大后仍有上限），得到 HTTP %d",
			wantRegister, wantRegister+1, code)
	}
}

// ---- 用例 2d：RATE_SHARED_IP_MULTIPLIER=0 等价于不放大（第 21 次就被拦） ----

func TestSharedIPMultiplierZeroMeansNoScaling(t *testing.T) {
	t.Setenv(envRateSharedIPMultiplier, "0")

	f := newRateFixture(t)
	if f.h.sharedExits.multiplier != 1 {
		t.Fatalf("RATE_SHARED_IP_MULTIPLIER=0 应按 1 处理，得到 %d", f.h.sharedExits.multiplier)
	}
	const ip = "203.0.113.44"
	for _, email := range distinctEmails(defaultRateSharedIPFanout) {
		sendCode(t, f.h, ip, email)
	}
	if !f.h.sharedExits.isShared(ip) {
		t.Fatal("判据与倍数无关，仍应判定为共享出口")
	}
	// 已用 50 次，远超未放大时的 20 → 下一次必然还是 429。
	if code := sendCode(t, f.h, ip, "no-scale@example.com"); !isBlocked(code) {
		t.Fatalf("倍数为 0（按 1）时 code_ip 不应放大，应仍被拦，得到 HTTP %d", code)
	}
}

// ---- 用例 2e：放大倍数 env 解析 ----

func TestSharedIPMultiplierEnvParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultRateSharedIPMultiplier},
		{"10", 10},
		{"3", 3},
		{"0", 1},   // 显式 0 = 不放大，不能回落成 10
		{"-5", 1},  // 负值同样按 1
		{"abc", 1}, // 非法值按 1
		{" 7 ", 7}, // 去空白
	}
	for _, tc := range cases {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			t.Setenv(envRateSharedIPMultiplier, tc.raw)
			if got := rateSharedIPMultiplierFromEnv(); got != tc.want {
				t.Fatalf("RATE_SHARED_IP_MULTIPLIER=%q 解析为 %d，期望 %d", tc.raw, got, tc.want)
			}
		})
	}
}

// ---- 用例 2f：scaled 的边界（0 保持不限、nil/<=1 不放大） ----

func TestSharedIPScaledEdgeCases(t *testing.T) {
	d := &sharedExitDetector{multiplier: 10}
	if got := d.scaled(0); got != 0 {
		t.Fatalf("原始阈值 0 表示不限，放大后应仍为 0，得到 %d", got)
	}
	if got := d.scaled(-1); got != -1 {
		t.Fatalf("负阈值（不限）应原样返回，得到 %d", got)
	}
	if got := d.scaled(20); got != 200 {
		t.Fatalf("20 × 10 应为 200，得到 %d", got)
	}
	d.multiplier = 1
	if got := d.scaled(20); got != 20 {
		t.Fatalf("倍数 1 应不放大，得到 %d", got)
	}
	var nilDetector *sharedExitDetector
	if got := nilDetector.scaled(20); got != 20 {
		t.Fatalf("nil 检测器应不放大，得到 %d", got)
	}
}

// ---- 用例 2g：复现线上实测序列 0×20 → 429×30 → 阈值放大 ----
//
// 报告里的漏洞序列是：50 个不同邮箱，前 20 次 code=0，第 21–50 次 429，
// **第 51 次起全部恢复 code=0（无限额度）**。
// 拆成两个用例：本用例锁前半段（0×20 之后是 429，不会因为「账号变多」就提前放行），
// 下一个用例锁后半段（第 51 次起是「放大到 200」而不是无限，第 201 次重新拦）。
func TestSharedIPReplayReportedSequence(t *testing.T) {
	t.Setenv(envRateSharedIPMultiplier, strconv.Itoa(defaultRateSharedIPMultiplier))

	f := newRateFixtureWith(t, map[string]string{
		"rate_code_interval":      "0",      // 关掉发码间隔，只看 code_ip
		"rate_code_per_day":       "100000", // 账号维度不参与本序列（每个 email 只发 1 次）
		"rate_code_ip_hourly":     "20",
		"rate_register_ip_hourly": "10",
	})
	const ip = "129.226.209.147" // 报告里的测试机出口
	emails := distinctEmails(defaultRateSharedIPFanout)

	// 第 1–20 次：放行。
	for i := 0; i < defaultRateCodeIPHourly; i++ {
		if code := sendCode(t, f.h, ip, emails[i]); isBlocked(code) {
			t.Fatalf("第 %d 次应放行（code_ip 20/小时），得到 HTTP %d", i+1, code)
		}
	}
	// 第 21–49 次：被 code_ip 拦住（账号数 20→48，还没到 50）。
	//
	// 第 50 次本不该再拦：这一次请求本身把账号数推到 50，而登记发生在判定之前，
	// 于是它已经落在放大后的 200 里。线上实测的 429 到第 50 次为止，是因为
	// 那串请求里混了重复邮箱，第 50 个请求到来时账号集合还没真正凑满 50 个。
	for i := defaultRateCodeIPHourly; i < defaultRateSharedIPFanout-1; i++ {
		if code := sendCode(t, f.h, ip, emails[i]); !isBlocked(code) {
			t.Fatalf("第 %d 次应被 code_ip 拦截，得到 HTTP %d", i+1, code)
		}
	}
	if f.h.sharedExits.isShared(ip) {
		t.Fatalf("关联 %d 个账号前不应提前判定为共享出口", defaultRateSharedIPFanout)
	}
	// 第 50 次：判定在这一刻成立并生效。
	if code := sendCode(t, f.h, ip, emails[defaultRateSharedIPFanout-1]); isBlocked(code) {
		t.Fatalf("第 %d 次本身把账号数推到阈值，应落在放大后的额度内，得到 HTTP %d",
			defaultRateSharedIPFanout, code)
	}
	if !f.h.sharedExits.isShared(ip) {
		t.Fatalf("关联 %d 个账号后应判定为共享出口", defaultRateSharedIPFanout)
	}
}

// ---- 用例 2h：第 51 次起是「放大到 200」，不是无限 ----

func TestSharedIPReplayAfterThreshold(t *testing.T) {
	t.Setenv(envRateSharedIPMultiplier, strconv.Itoa(defaultRateSharedIPMultiplier))

	f := newRateFixtureWith(t, map[string]string{
		"rate_code_interval":      "0",
		"rate_code_per_day":       "100000",
		"rate_code_ip_hourly":     "20",
		"rate_register_ip_hourly": "10",
	})
	const ip = "129.226.209.148"
	for _, email := range distinctEmails(defaultRateSharedIPFanout) {
		sendCode(t, f.h, ip, email)
	}
	if !f.h.sharedExits.isShared(ip) {
		t.Fatalf("关联 %d 个账号后应判定为共享出口", defaultRateSharedIPFanout)
	}

	// 第 51 次：原实现从这里起无限放行；新实现只是放大到 200，正常放行。
	if code := sendCode(t, f.h, ip, "fifty-first@example.com"); isBlocked(code) {
		t.Fatalf("第 51 次落在放大后的 200 额度内，应放行，得到 HTTP %d", code)
	}

	// 推到 200 次（放大后的上限），继续放行；第 201 次必须重新拦。
	for f.attempts(keyCodeIP(ip)) < defaultRateCodeIPHourly*defaultRateSharedIPMultiplier {
		if code := sendCode(t, f.h, ip, "fill@example.com"); isBlocked(code) {
			t.Fatalf("计数 %d 仍低于放大后的 200 却被拦（HTTP %d）",
				f.attempts(keyCodeIP(ip)), code)
		}
	}
	if code := sendCode(t, f.h, ip, "two-hundred-first@example.com"); !isBlocked(code) {
		t.Fatalf("共享出口应是「放大到 200」而不是无限：第 201 次必须被拦，得到 HTTP %d", code)
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
	t.Setenv(envRateSharedIPMultiplier, strconv.Itoa(defaultRateSharedIPMultiplier))

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
		t.Fatalf("达到阈值后 code_ip 应放大到 %d，得到 HTTP %d",
			defaultRateCodeIPHourly*defaultRateSharedIPMultiplier, code)
	}
}
