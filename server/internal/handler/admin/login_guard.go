package admin

// login_guard.go 是管理端登录失败的限流与锁定（POST /admin-api/login）。
//
// 为什么单独一套、不复用 api 包的 RateGuardService：
// api 的 rateLimiter 绑在 api.Handler 的 cache 上，admin 包拿不到 cache；
// 管理端登录也只有「来源 IP + 用户名」两个维度，不需要 site_configs 那套可调阈值。
//
// 语义：
//   - 账号维度（user:用户名）：失败数到阈值即锁，锁定期取剩余时长 —— 与原来一致；
//   - 来源 IP 维度：**不再**按失败数锁 15 分钟，改为每分钟尝试次数上限
//     （默认 30 次/分钟，env ADMIN_LOGIN_IP_PER_MINUTE）。超限只回 429、窗口
//     到期即恢复，不会把「同一出口 IP 后面的所有管理员」锁死；
//   - 判定（blocked）排在查库之前 —— 限流/锁定期内连正确密码也拒，且不做任何凭据比对；
//   - 账号不存在、密码错误、2FA 输错共用同一套计数（对外文案见 handler）；
//   - 登录成功清掉账号维度的失败计数（IP 分钟窗口不因成功而重置）；
//   - 计数只在内存（进程重启即清空），惰性清理过期项 + 条目数硬上限，防止被灌大内存。
//
// 为什么 IP 维度必须短窗口：nginx 把 $remote_addr 写进 X-Forwarded-For，而
// set_real_ip_from 只认 Cloudflare 网段。站点挂在阿里云/腾讯云等非 CF 的 CDN 后面时，
// $remote_addr 是 CDN 节点 IP，全站共享「一个 IP」。此时若 IP 维度还按失败数锁 900 秒，
// 攻击者随便编个用户名打 5 次就能把全体管理员锁 15 分钟 —— 一个真实用户名都不用知道。
//
// 计数窗口与锁定期共用一个到期时刻：每次失败把该键的到期时刻推到 now+锁定时长，
// 到期即整条删除（失败数归零）。因此「N 次失败」要求在锁定时长内连续发生，
// 第 N 次失败起到期为止就是锁定期。IP 分钟窗口同理：首次尝试开窗，窗口固定 1 分钟，
// 窗口内累计尝试次数，到期整条删除。

import (
	"strings"
	"sync"
	"time"
)

const (
	// defaultLoginMaxAttempts / defaultLoginLockSeconds 是未配置 env 时的默认阈值。
	defaultLoginMaxAttempts = 5
	defaultLoginLockSeconds = 900

	// defaultLoginIPPerMinute 是来源 IP 维度的默认上限：每分钟 30 次登录尝试。
	// 取值依据：管理员手工登录每分钟不会超过几次；同一 CDN 出口后面的正常流量
	// 也远低于 30 次/分钟，而窗口只有 60 秒，超限最多等一分钟就恢复。
	defaultLoginIPPerMinute = 30

	// envAdminLoginMaxAttempts / envAdminLoginLockSeconds / envAdminLoginIPPerMinute
	// 是三个可调项的变量名。
	envAdminLoginMaxAttempts = "ADMIN_LOGIN_MAX_ATTEMPTS"
	envAdminLoginLockSeconds = "ADMIN_LOGIN_LOCK_SECONDS"
	envAdminLoginIPPerMinute = "ADMIN_LOGIN_IP_PER_MINUTE"

	// loginIPWindow 是 IP 维度的计数窗口（固定 1 分钟，不可配）。
	loginIPWindow = time.Minute

	// maxLoginLockSeconds 是锁定时长上限（30 天）：再长就是配错，按非法值回退默认，
	// 同时挡住 time.Duration 溢出。
	maxLoginLockSeconds = 30 * 24 * 3600

	// loginGuardMaxEntries 是计数表条目上限。正常量级（几个管理员 + 少量来源 IP）
	// 远用不到，它挡的是「拿海量不同 IP / 用户名灌计数表」的撑内存手法。
	loginGuardMaxEntries = 20000

	// loginGuardSweepInterval 是惰性清理的最小间隔（不挂后台 goroutine，
	// 下一次登录请求到来时顺手扫一遍）。
	loginGuardSweepInterval = time.Minute
)

// loginAttempt 是单个维度（某用户名，或某来源 IP）的计数。
type loginAttempt struct {
	hits      int
	expiresAt time.Time // 账号维度：最后一次失败 + 锁定时长；IP 维度：窗口结束时刻
}

// loginGuard 是登录失败计数器。并发安全（一次登录可能同时被两个请求命中）。
type loginGuard struct {
	mu          sync.Mutex
	entries     map[string]*loginAttempt // "user:" 失败计数（到阈值锁 lockFor）+ "ip:" 分钟窗口计数
	now         func() time.Time
	maxAttempts int
	lockFor     time.Duration
	ipPerMinute int
	maxEntries  int
	lastSweep   time.Time
}

// newLoginGuardFromEnv 按 env 构造计数器；非法值（非整数 / <=0 / 锁定时长超上限）回退默认。
func newLoginGuardFromEnv() *loginGuard {
	lockSec := envPositiveInt(envAdminLoginLockSeconds, defaultLoginLockSeconds)
	if lockSec > maxLoginLockSeconds {
		lockSec = defaultLoginLockSeconds
	}
	g := newLoginGuard(
		envPositiveInt(envAdminLoginMaxAttempts, defaultLoginMaxAttempts),
		time.Duration(lockSec)*time.Second,
		time.Now,
	)
	g.ipPerMinute = envPositiveInt(envAdminLoginIPPerMinute, defaultLoginIPPerMinute)
	return g
}

// newLoginGuard 直接给阈值构造（单测用）。now 为 nil 时用 time.Now。
func newLoginGuard(maxAttempts int, lockFor time.Duration, now func() time.Time) *loginGuard {
	if maxAttempts <= 0 {
		maxAttempts = defaultLoginMaxAttempts
	}
	if lockFor <= 0 {
		lockFor = time.Duration(defaultLoginLockSeconds) * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &loginGuard{
		entries:     make(map[string]*loginAttempt),
		now:         now,
		maxAttempts: maxAttempts,
		lockFor:     lockFor,
		ipPerMinute: defaultLoginIPPerMinute,
		maxEntries:  loginGuardMaxEntries,
		lastSweep:   now(),
	}
}

// normalizeLoginUsername 是计数键的归一化口径：to lower + trim。
// 只是为了「Admin」「 admin 」落进同一个桶，不改变实际查库用的原始用户名。
func normalizeLoginUsername(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func loginIPKey(ip string) string     { return "ip:" + strings.TrimSpace(ip) }
func loginUserKey(name string) string { return "user:" + normalizeLoginUsername(name) }

// 空 IP / 空用户名一律不入键 —— 否则所有「没带用户名」的请求会挤进同一个桶，
// 互相锁死。两个维度的入键判断分别写在 blocked / recordFailure 里。

// blocked 判断是否应拒绝本次登录。返回 true 时同时给出还需等待的时长。
//
// 两个维度的判定规则不同：
//   - user: 失败数到阈值 → 锁 session（默认 900 秒）；
//   - ip:   窗口内失败次数到每分钟上限 → 只等窗口结束（最多 60 秒），不产生长锁。
func (g *loginGuard) blocked(ip, username string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	g.sweepLocked(now)

	var remaining time.Duration
	pick := func(e *loginAttempt, threshold int) {
		if e == nil || threshold <= 0 || e.hits < threshold {
			return
		}
		if left := e.expiresAt.Sub(now); left > remaining {
			remaining = left
		}
	}

	if normalizeLoginUsername(username) != "" {
		pick(g.entries[loginUserKey(username)], g.maxAttempts)
	}
	// CDN 未还原真实 IP 时，同一出口的全体用户共用一个 IP —— 这里只做短窗口限流，
	// 超限最多等到本分钟结束，绝不把整个出口锁死。
	if strings.TrimSpace(ip) != "" {
		pick(g.entries[loginIPKey(ip)], g.ipPerMinute)
	}

	if remaining <= 0 {
		return false, 0
	}
	return true, remaining
}

// recordAttempt 给 IP 维度记一次登录尝试（成功失败都算）。
//
// 语义是「每分钟尝试次数」，不是失败次数：共享出口上攻击者可以用大量不存在的
// 用户名制造失败，也可以混入成功请求；按尝试计数才能给出稳定的上限。
// 窗口固定 1 分钟，到期整条删除 —— 计数本身不进 cache，进程重启即清空。
func (g *loginGuard) recordAttempt(ip string) {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" || g.ipPerMinute <= 0 {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	g.sweepLocked(now)

	key := loginIPKey(trimmed)
	e := g.entries[key]
	if e == nil || !now.Before(e.expiresAt) {
		e = &loginAttempt{expiresAt: now.Add(loginIPWindow)}
		g.entries[key] = e
	}
	e.hits++
	g.trimLocked(now)
}

// recordFailure 给账号维度记一次失败（IP 维度已由 recordAttempt 记过）。
// 放行前已经判过锁，所以这里不再判断是否已锁 —— 锁定期内的请求根本走不到这一步，
// 锁不会被无限顺延。
func (g *loginGuard) recordFailure(ip, username string) {
	key := loginUserKey(username)
	if normalizeLoginUsername(username) == "" {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	g.sweepLocked(now)

	e := g.entries[key]
	if e == nil || !now.Before(e.expiresAt) {
		e = &loginAttempt{}
		g.entries[key] = e
	}
	e.hits++
	e.expiresAt = now.Add(g.lockFor)
	g.trimLocked(now)
}

// clear 清掉账号维度的失败计数（登录成功时调用）。
//
// IP 维度**刻意不清**：它是「本分钟失败次数」的滑动窗口，不是凭据状态。
// 成功登录若把它清零，攻击者只要握有一个合法账号（或管理员自己顺手登录一次），
// 就能反复把共享出口 IP 的计数刷回 0，限流形同虚设。
func (g *loginGuard) clear(ip, username string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if normalizeLoginUsername(username) != "" {
		delete(g.entries, loginUserKey(username))
	}
}

// sweepLocked 惰性清理过期项：最多每 sweepInterval 扫一遍全表。
func (g *loginGuard) sweepLocked(now time.Time) {
	if now.Sub(g.lastSweep) < loginGuardSweepInterval {
		return
	}
	g.lastSweep = now
	for key, e := range g.entries {
		if !now.Before(e.expiresAt) {
			delete(g.entries, key)
		}
	}
}

// trimLocked 守住条目上限：到上限先清过期项，仍满则淘汰最早到期的一条。
// 淘汰最旧的（而非拒绝新键）是为了让限流对**新出现**的 IP/用户名依旧生效。
func (g *loginGuard) trimLocked(now time.Time) {
	if g.maxEntries <= 0 || len(g.entries) < g.maxEntries {
		return
	}

	oldestKey := ""
	var oldestAt time.Time
	for key, e := range g.entries {
		if !now.Before(e.expiresAt) {
			delete(g.entries, key)
			continue
		}
		if oldestKey == "" || e.expiresAt.Before(oldestAt) {
			oldestKey, oldestAt = key, e.expiresAt
		}
	}
	if oldestKey != "" && len(g.entries) >= g.maxEntries {
		delete(g.entries, oldestKey)
	}
}
