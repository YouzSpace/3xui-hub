package admin

// login_guard.go 是管理端登录失败的限流与锁定（POST /admin-api/login）。
//
// 为什么单独一套、不复用 api 包的 RateGuardService：
// api 的 rateLimiter 绑在 api.Handler 的 cache 上，admin 包拿不到 cache；
// 管理端登录也只有「来源 IP + 用户名」两个维度，不需要 site_configs 那套可调阈值。
//
// 语义：
//   - 账号维度（user:用户名）：失败数到阈值即锁，锁定期取剩余时长 —— 与原来一致；
//   - 来源 IP 维度有两道闸门：
//       a. 每分钟尝试次数上限（默认 30 次/分钟，env ADMIN_LOGIN_IP_PER_MINUTE）：
//          挡瞬时爆破；超限只回 429、窗口到期即恢复，不会把「同一出口 IP 后面的
//          所有管理员」锁死；
//       b. 失败滑动窗口（默认 10 分钟内 10 次失败，env ADMIN_LOGIN_IP_FAIL_MAX /
//          ADMIN_LOGIN_IP_FAIL_WINDOW_SECONDS）：挡 issue #142 实测的慢速撞库 ——
//          每 15~20 秒一次、每次换个用户名，账号维度到不了阈值，每分钟 30 次也
//          到不了。窗口内失败数到上限即持续拒，直到最老一次失败滑出窗口才放行；
//   - 判定（blocked）排在查库之前 —— 限流/锁定期内连正确密码也拒，且不做任何凭据比对；
//   - 账号不存在、密码错误、2FA 输错共用同一套计数（对外文案见 handler）；
//   - 登录成功清掉账号维度的失败计数（IP 维度不因成功而重置）；
//   - 计数只在内存（进程重启即清空），惰性清理过期项 + 条目数硬上限，防止被灌大内存；
//   - 登录失败的审计由 recordFailure 返回的汇总事件驱动：同一来源 IP 在一个失败窗口
//     内只落一条 type='system' 记录（带次数），不再一次失败一行 —— 否则一次爆破
//     就能把 operation_logs 灌满（issue #142 第 3 条：18,756 行失败登录占全表 92.5%）。
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
//
// IP 失败窗口是另一种形态 —— 真正的滑动窗口：存最近 window 内的失败时刻，
// 每次判定先丢掉滑出的，再看剩下的是否到上限。不按「固定窗口翻页」是因为
// 攻击者只要把节奏卡在窗口边界就能稳定拿到满额度（实测每 15~20 秒一次）。

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

	// defaultLoginIPFailMax / defaultLoginIPFailWindowSeconds 是来源 IP 维度
	// 「失败滑动窗口」的默认值：10 分钟内累计 10 次失败即持续限流。
	//
	// 取值依据（issue #142 实测：17 天 18,756 次失败，单 IP 每 15~20 秒一次）：
	// 每分钟 3~4 次的上限永远到不了 30 次/分钟，账号维度也到不了 5 次；换成
	// 10 分钟 / 10 次后，同一 IP 最多撑 2~3 分钟就会被挡住，之后只能按窗口衰减
	// 的节奏继续（约 1 次/分钟）。对正常管理员仍是保守值：同一出口（含 CDN 共享
	// 出口）10 分钟内失败 10 次极罕见，且命中后最长等 10 分钟，不会像账号维度
	// 那样锁满 900 秒。
	defaultLoginIPFailMax           = 10
	defaultLoginIPFailWindowSeconds = 600

	// envAdminLoginMaxAttempts / envAdminLoginLockSeconds / envAdminLoginIPPerMinute /
	// envAdminLoginIPFailMax / envAdminLoginIPFailWindowSeconds 是可调项的变量名。
	envAdminLoginMaxAttempts         = "ADMIN_LOGIN_MAX_ATTEMPTS"
	envAdminLoginLockSeconds         = "ADMIN_LOGIN_LOCK_SECONDS"
	envAdminLoginIPPerMinute         = "ADMIN_LOGIN_IP_PER_MINUTE"
	envAdminLoginIPFailMax           = "ADMIN_LOGIN_IP_FAIL_MAX"
	envAdminLoginIPFailWindowSeconds = "ADMIN_LOGIN_IP_FAIL_WINDOW_SECONDS"

	// loginIPWindow 是 IP 维度「每分钟尝试次数」的计数窗口（固定 1 分钟，不可配）。
	loginIPWindow = time.Minute

	// maxLoginLockSeconds 是锁定时长上限（30 天）：再长就是配错，按非法值回退默认，
	// 同时挡住 time.Duration 溢出。
	maxLoginLockSeconds = 30 * 24 * 3600

	// maxLoginIPFailWindowSeconds 是 IP 失败窗口的上限（24 小时）：再长就是配错，
	// 按非法值回退默认，同时挡住 time.Duration 溢出。
	maxLoginIPFailWindowSeconds = 24 * 3600

	// loginGuardMaxEntries 是计数表条目上限。正常量级（几个管理员 + 少量来源 IP）
	// 远用不到，它挡的是「拿海量不同 IP / 用户名灌计数表」的撑内存手法。
	loginGuardMaxEntries = 20000

	// loginGuardSweepInterval 是惰性清理的最小间隔（不挂后台 goroutine，
	// 下一次登录请求到来时顺手扫一遍）。
	loginGuardSweepInterval = time.Minute
)

// loginAttempt 是单个维度（某用户名，或某来源 IP 的每分钟尝试）的计数。
type loginAttempt struct {
	hits      int
	expiresAt time.Time // 账号维度：最后一次失败 + 锁定时长；IP 维度：窗口结束时刻
}

// loginIPFailureWindow 是单个来源 IP 的失败滑动窗口（只记失败，不记成功）。
//
// 用时间戳切片而不是「固定窗口计数」：慢速撞库每 15~20 秒一次，固定窗口到点
// 清零等于重新给满额度；滑动窗口里只要最近 window 内还留着 max 次失败就继续拒，
// 且最老一次失败过期后才逐个放行。
type loginIPFailureWindow struct {
	failures []time.Time         // 窗口内的失败时刻，升序
	users    map[string]struct{} // 窗口内涉及的用户名（去重，审计汇总用）
	audited  bool                // 本窗口是否已落过汇总审计（同一 IP 一个窗口只落一条）
}

// loginAuditEvent 是一条「登录失败到阈值」的汇总（由 recordFailure 返回）。
// 一个来源 IP 在一个失败窗口内最多产生一条，handler 据此落 operation_logs
// （type='system'），取代原先「每次失败一行 admin 明细」的写法。
type loginAuditEvent struct {
	IP            string        // 来源 IP（空表示没取到）
	Username      string        // 触发这次汇总的用户名（归一化后的样本）
	IPFailures    int           // 该 IP 在当前失败窗口内的失败次数
	Accounts      int           // 窗口内涉及的账号数
	Window        time.Duration // 失败窗口长度
	IPLimited     bool          // 触发原因：来源 IP 失败次数达到上限
	AccountLocked bool          // 触发原因：该用户名达到账号维度阈值
}

// loginGuard 是登录失败计数器。并发安全（一次登录可能同时被两个请求命中）。
type loginGuard struct {
	mu           sync.Mutex
	entries      map[string]*loginAttempt         // "user:" 失败计数 + "ip:" 每分钟尝试计数
	ipFailures   map[string]*loginIPFailureWindow // 来源 IP 的失败滑动窗口
	now          func() time.Time
	maxAttempts  int
	lockFor      time.Duration
	ipPerMinute  int
	ipFailMax    int
	ipFailWindow time.Duration
	maxEntries   int
	lastSweep    time.Time
}

// newLoginGuardFromEnv 按 env 构造计数器；非法值（非整数 / <=0 / 时长超上限）回退默认。
func newLoginGuardFromEnv() *loginGuard {
	lockSec := envPositiveInt(envAdminLoginLockSeconds, defaultLoginLockSeconds)
	if lockSec > maxLoginLockSeconds {
		lockSec = defaultLoginLockSeconds
	}
	failWindowSec := envPositiveInt(envAdminLoginIPFailWindowSeconds, defaultLoginIPFailWindowSeconds)
	if failWindowSec > maxLoginIPFailWindowSeconds {
		failWindowSec = defaultLoginIPFailWindowSeconds
	}
	g := newLoginGuard(
		envPositiveInt(envAdminLoginMaxAttempts, defaultLoginMaxAttempts),
		time.Duration(lockSec)*time.Second,
		time.Now,
	)
	g.ipPerMinute = envPositiveInt(envAdminLoginIPPerMinute, defaultLoginIPPerMinute)
	g.ipFailMax = envPositiveInt(envAdminLoginIPFailMax, defaultLoginIPFailMax)
	g.ipFailWindow = time.Duration(failWindowSec) * time.Second
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
		entries:      make(map[string]*loginAttempt),
		ipFailures:   make(map[string]*loginIPFailureWindow),
		now:          now,
		maxAttempts:  maxAttempts,
		lockFor:      lockFor,
		ipPerMinute:  defaultLoginIPPerMinute,
		ipFailMax:    defaultLoginIPFailMax,
		ipFailWindow: defaultLoginIPFailWindowSeconds * time.Second,
		maxEntries:   loginGuardMaxEntries,
		lastSweep:    now(),
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
// 三个判定规则不同：
//   - user: 失败数到阈值 → 锁 session（默认 900 秒）；
//   - ip:   窗口内尝试次数到每分钟上限 → 只等本分钟结束（最多 60 秒），不产生长锁；
//   - ip:   失败滑动窗口内失败数到上限 → 等到最老一次失败滑出窗口（最多一个窗口）。
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
	// CDN 未还原真实 IP 时，同一出口的全体用户共用一个 IP —— 这里只做时间窗很短的
	// 限流（每分钟尝试数 + 失败滑动窗口），绝不把整个出口锁成 15 分钟以上的长锁。
	if trimmed := strings.TrimSpace(ip); trimmed != "" {
		if w := g.ipFailures[trimmed]; w != nil {
			g.trimIPFailuresLocked(w, now)
			if g.ipFailMax > 0 && len(w.failures) >= g.ipFailMax {
				// 只有「最老一次失败滑出窗口」才会剩下 max-1 次，剩余时长即这个时刻。
				if left := w.failures[0].Add(g.ipFailWindow).Sub(now); left > remaining {
					remaining = left
				}
			}
		}
		pick(g.entries[loginIPKey(trimmed)], g.ipPerMinute)
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

// recordFailure 给账号维度记一次失败，并推进来源 IP 的失败滑动窗口
// （IP 维度的「每分钟尝试次数」已由 recordAttempt 记过）。
//
// 放行前已经判过锁，所以这里不再判断是否已锁 —— 锁定期内的请求根本走不到这一步，
// 锁不会被无限顺延。
//
// 返回非 nil 表示本次失败让某个维度到达阈值、且该 IP 的当前失败窗口还没落过审计：
// handler 据此只写一条汇总审计（operation_logs.type='system'），而不是每次失败一行。
// 同一 IP 一个窗口最多返回一次 —— 触发后后续请求都会被 blocked 提前拒掉，不再计数。
func (g *loginGuard) recordFailure(ip, username string) *loginAuditEvent {
	name := normalizeLoginUsername(username)

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	g.sweepLocked(now)

	// 账号维度：失败数到阈值即锁 lockFor（口径与原来一致）。
	// 空用户名不入账号桶（否则所有「没带用户名」的请求会挤在一个桶里互相锁死），
	// 但下面的 IP 维度照样要记 —— 否则编个空用户名就能绕开 IP 失败窗口。
	accountLocked := false
	if name != "" {
		key := loginUserKey(username)
		e := g.entries[key]
		if e == nil || !now.Before(e.expiresAt) {
			e = &loginAttempt{}
			g.entries[key] = e
		}
		e.hits++
		e.expiresAt = now.Add(g.lockFor)
		accountLocked = g.maxAttempts > 0 && e.hits >= g.maxAttempts
	}

	// IP 维度：失败滑动窗口 + 一个窗口只落一条审计。
	var (
		w          *loginIPFailureWindow
		ipLimited  bool
		ipFailures int
		accounts   int
		trimmed    = strings.TrimSpace(ip)
	)
	if trimmed != "" && g.ipFailMax > 0 {
		w = g.ipFailureWindowLocked(trimmed, now)
		w.failures = append(w.failures, now)
		if len(w.failures) > g.ipFailMax {
			// 理论到不了上限（到上限就被 blocked 拦住不再计数），这里兜住并发/调参。
			w.failures = w.failures[len(w.failures)-g.ipFailMax:]
		}
		if name != "" {
			w.users[name] = struct{}{}
		}
		ipLimited = len(w.failures) >= g.ipFailMax
		ipFailures, accounts = len(w.failures), len(w.users)
	}

	if !ipLimited && !accountLocked {
		g.trimLocked(now)
		return nil
	}
	if w != nil {
		if w.audited {
			g.trimLocked(now)
			return nil
		}
		w.audited = true
	}

	g.trimLocked(now)
	return &loginAuditEvent{
		IP:            trimmed,
		Username:      name,
		IPFailures:    ipFailures,
		Accounts:      accounts,
		Window:        g.ipFailWindow,
		IPLimited:     ipLimited,
		AccountLocked: accountLocked,
	}
}

// ipFailureWindowLocked 取（必要时新建）某 IP 的失败窗口，并先把滑出窗口的旧失败清掉。
func (g *loginGuard) ipFailureWindowLocked(ip string, now time.Time) *loginIPFailureWindow {
	w := g.ipFailures[ip]
	if w == nil {
		w = &loginIPFailureWindow{users: make(map[string]struct{})}
		g.ipFailures[ip] = w
	}
	g.trimIPFailuresLocked(w, now)
	return w
}

// trimIPFailuresLocked 把滑出窗口的失败记录丢掉；整个窗口空了就一并重置审计标记
// （下一轮爆破要能再报一条）。
func (g *loginGuard) trimIPFailuresLocked(w *loginIPFailureWindow, now time.Time) {
	cut := now.Add(-g.ipFailWindow)
	keep := 0
	for keep < len(w.failures) && !w.failures[keep].After(cut) {
		keep++
	}
	if keep > 0 {
		w.failures = append(w.failures[:0], w.failures[keep:]...)
	}
	if len(w.failures) == 0 {
		w.users = make(map[string]struct{})
		w.audited = false
	}
}

// clear 清掉账号维度的失败计数（登录成功时调用）。
//
// IP 维度**刻意不清**：它是「本分钟尝试次数」与「失败滑动窗口」，不是凭据状态。
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
	for ip, w := range g.ipFailures {
		g.trimIPFailuresLocked(w, now)
		if len(w.failures) == 0 {
			delete(g.ipFailures, ip)
		}
	}
}

// trimLocked 守住条目上限：到上限先清过期项，仍满则淘汰最早到期的一条。
// 淘汰最旧的（而非拒绝新键）是为了让限流对**新出现**的 IP/用户名依旧生效。
func (g *loginGuard) trimLocked(now time.Time) {
	if g.maxEntries <= 0 {
		return
	}
	if len(g.entries) >= g.maxEntries {
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

	// 失败窗口同样要防撑大：先清过期，仍满则淘汰「最久没再失败」的 IP。
	if len(g.ipFailures) >= g.maxEntries {
		oldestIP := ""
		var oldestAt time.Time
		for ip, w := range g.ipFailures {
			g.trimIPFailuresLocked(w, now)
			if len(w.failures) == 0 {
				delete(g.ipFailures, ip)
				continue
			}
			if last := w.failures[len(w.failures)-1]; oldestIP == "" || last.Before(oldestAt) {
				oldestIP, oldestAt = ip, last
			}
		}
		if oldestIP != "" && len(g.ipFailures) >= g.maxEntries {
			delete(g.ipFailures, oldestIP)
		}
	}
}
