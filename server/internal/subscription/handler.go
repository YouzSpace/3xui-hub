package subscription

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 逐条复刻 app/Http/Controllers/Api/SubscriptionController.php 的 show()。
//
// 端点契约（GET /api/sub/{token}）：
//   - 用户不存在 → 正文 "[404] 用户不存在"，HTTP 200，X-CH-Code: 404
//   - 账号禁用   → "[403] 账号已禁用"
//   - 格式未启用 → "[400] 不支持的订阅格式: clash"
//   - 业务异常   → "[{code}] {message}"
//   - 成功       → 正文 + Content-Type + Subscription-Userinfo + X-CH-Code: 0
//   - 触发限流   → "[429] 请求过于频繁"，HTTP 429（唯一一个不用 200 的分支）
//
// **注意 HTTP 状态其余情况恒为 200**：PHP 的 helper 把 code 塞进 X-CH-Code 头，前端/客户端靠它判断。
// 这一点与 api 契约（response.go：失败也 200）一致，不要"改得更 RESTful"。
// 限流是本项目唯一新增的例外：真 429 + Retry-After，客户端才会退避重试
// （订阅客户端普遍只在 429/503 上退避，继续回 200 它们会当成拉取成功）。

// Handler 是订阅端点的 HTTP 适配层。
type Handler struct {
	Service *Service
	Repo    Repository

	// limiter 是单 token 限流器（nil = 关闭，SUB_RATE_LIMIT_PER_HOUR=0）。
	limiter *tokenLimiter
	// cache 是生成结果缓存（nil = 关闭，SUB_CACHE_SECONDS=0）。
	cache *renderCache
	// now 是时钟（测试注入固定时刻；limiter/cache 通过闭包读它，替换后立即生效）。
	now func() time.Time
	// generator 是实际生成入口，默认 Service.Generate；测试用它统计真实生成次数。
	generator func(ctx context.Context, user *User, format Format) (string, error)
}

// NewHandler 构造 handler。
//
// 限流与缓存额度走环境变量（不引外部依赖，进程内 map + mutex）：
//
//	SUB_RATE_LIMIT_PER_HOUR  单 token 每小时允许的请求数，默认 60，0 = 关闭
//	SUB_CACHE_SECONDS        生成结果缓存秒数，默认 60，0 = 关闭
//
// 多实例部署时各自独立计数（没有共享存储）。
func NewHandler(repo Repository) *Handler {
	svc := NewService(repo)
	h := &Handler{Service: svc, Repo: repo, now: time.Now, generator: svc.Generate}
	h.limiter = newTokenLimiter(envInt64("SUB_RATE_LIMIT_PER_HOUR", 60), clockOf(&h.now))
	h.cache = newRenderCache(envInt64("SUB_CACHE_SECONDS", 60), clockOf(&h.now))
	return h
}

// clockOf 把「可替换的时钟字段」包成闭包：测试替换 h.now 后立即生效。
func clockOf(field *func() time.Time) func() time.Time {
	return func() time.Time {
		if *field == nil {
			return time.Now()
		}
		return (*field)()
	}
}

// Register 把 GET /api/sub/:token 挂到路由器上。
func (h *Handler) Register(r gin.IRouter) {
	r.GET("/api/sub/:token", h.Show)
}

// Show 处理一次订阅请求。
func (h *Handler) Show(c *gin.Context) {
	ctx := c.Request.Context()
	token := c.Param("token")

	// 限流放在最前面：不存在的 token 也计数，否则用随机 token 就能把库打满。
	if h.limiter != nil {
		if ok, wait := h.limiter.allow(token); !ok {
			c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			h.textStatus(c, http.StatusTooManyRequests, "[429] 请求过于频繁", 429, "请求过于频繁")
			return
		}
	}

	user, err := h.Repo.UserByToken(ctx, token)
	if err != nil {
		h.text(c, "服务器错误", 500, "服务器错误")
		return
	}
	if user == nil {
		h.text(c, "[404] 用户不存在", 404, "用户不存在")
		return
	}

	// 加载用户数据（流量由定时任务同步，不在每次请求时同步）
	// 检查封禁状态
	if !user.Enabled {
		h.text(c, "[403] 账号已禁用", 403, "账号已禁用")
		return
	}

	cfg, err := h.Service.loadSiteConfigs(ctx)
	if err != nil {
		h.text(c, "服务器错误", 500, "服务器错误")
		return
	}

	format := resolveFormat(c.Request.URL.Query())
	if !isFormatEnabled(format, cfg) {
		h.text(c, "[400] 不支持的订阅格式: "+string(format), 400, "不支持的订阅格式")
		return
	}

	body, genErr := h.renderCached(ctx, token, user, format)
	if genErr != nil {
		if bizErr, ok := genErr.(*Error); ok {
			h.text(c, "["+strconv.Itoa(bizErr.Code)+"] "+bizErr.Message, bizErr.Code, bizErr.Message)
			return
		}
		h.text(c, "服务器错误", 500, "服务器错误")
		return
	}

	// Subscription-Userinfo 头（upload 恒 0；download = traffic_used；total = traffic_limit）
	// 该头每次都用本次读到的 user 现算，不进缓存 —— 否则用量会在 TTL 内冻住。
	expire := int64(0)
	if user.ExpiredAt != nil {
		expire = user.ExpiredAt.Unix()
	}
	userinfo := "upload=0; download=" + itoa(user.TrafficUsed) +
		"; total=" + itoa(user.TrafficLimit) +
		"; expire=" + itoa(expire)

	contentType := "text/plain; charset=UTF-8"
	switch format {
	case FormatClash:
		contentType = "text/yaml; charset=UTF-8"
	case FormatSingbox:
		contentType = "application/json; charset=UTF-8"
	}

	// 订阅标题：管理员后台配置，留空保持原样（不加头、不改正文）
	title := phpTrim(cfg.get("sub_title", ""))
	if title != "" && format == FormatClash {
		// Clash 系客户端读取名称的约定：正文首行注释，值为 base64
		body = "#profile-title: " + b64encode([]byte(title)) + "\n" + body
	}

	c.Header("Content-Type", contentType)
	c.Header("Subscription-Userinfo", userinfo)
	c.Header("X-CH-Code", "0")
	c.Header("X-CH-Msg", "ok")
	if title != "" {
		c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+rawurlencode(title))
	}
	c.String(http.StatusOK, body)
}

// text 对应 PHP 的 text()：正文 + text/plain + X-CH-Code/X-CH-Msg，HTTP 200。
func (h *Handler) text(c *gin.Context, body string, code int, msg string) {
	h.textStatus(c, http.StatusOK, body, code, msg)
}

// textStatus 同 text()，但使用指定 HTTP 状态码（目前只有 429 用非 200）。
func (h *Handler) textStatus(c *gin.Context, status int, body string, code int, msg string) {
	c.Header("Content-Type", "text/plain; charset=UTF-8")
	c.Header("X-CH-Code", strconv.Itoa(code))
	c.Header("X-CH-Msg", msg)
	c.String(status, body)
}

// resolveFormat 对应 resolveFormat()：`?clash`/`?clash=任意值` 优先，其次 singbox，默认 base64。
//
// PHP 用 `$request->has('clash')`，语义是「该键存在且值不为 null」（值为空串也算存在）；
// 键名大小写敏感 —— `?Clash=1` 在 PHP 里不命中，这里同样不命中。
func resolveFormat(query url.Values) Format {
	if _, ok := query["clash"]; ok {
		return FormatClash
	}
	if _, ok := query["singbox"]; ok {
		return FormatSingbox
	}
	return FormatBase64
}

// isFormatEnabled 对应 isFormatEnabled()：base64 始终启用。
func isFormatEnabled(format Format, cfg *siteConfigReader) bool {
	switch format {
	case FormatClash:
		return cfg.get("sub_clash_enabled", "0") == "1"
	case FormatSingbox:
		return cfg.get("sub_singbox_enabled", "0") == "1"
	default:
		return true
	}
}

// GenerateForToken 是 handler 之外（例如测试/内部调用）复用的一条便捷路径：
// 按 token 取用户并生成指定格式。走同一条缓存路径，同一 token+format 不会重复打面板。
func (h *Handler) GenerateForToken(ctx context.Context, token string, format Format) (string, error) {
	user, err := h.Repo.UserByToken(ctx, token)
	if err != nil {
		return "", err
	}
	if user == nil {
		return "", &Error{Message: "用户不存在", Code: 404}
	}
	return h.renderCached(ctx, token, user, format)
}

// generate 调实际生成入口（generator 未注入时回落到 Service.Generate）。
func (h *Handler) generate(ctx context.Context, user *User, format Format) (string, error) {
	if h.generator != nil {
		return h.generator(ctx, user, format)
	}
	return h.Service.Generate(ctx, user, format)
}

// renderCached 走缓存生成；cache 为 nil（SUB_CACHE_SECONDS=0）时直连生成器。
//
// 只缓存**成功**的结果：业务错误（403/404）与服务器错误都不缓存，
// 否则一次瞬时故障会把用户钉在错误页面上整整一个 TTL。
func (h *Handler) renderCached(ctx context.Context, token string, user *User, format Format) (string, error) {
	if h.cache == nil {
		return h.generate(ctx, user, format)
	}
	return h.cache.load(cacheKey(token, format), func() (string, error) {
		return h.generate(ctx, user, format)
	})
}

// cacheKey 是渲染结果缓存的键：**必须**同时含 token 与 format。
//
// 三种格式的输出互不相同，少了 format 就会出现「先拉 base64、再拉 clash 却拿到 base64 正文」。
// 用 \x00 分隔：token 是 URL 路径段（不含 NUL），format 是固定枚举，两者都不会撞分隔符。
func cacheKey(token string, format Format) string {
	return token + "\x00" + string(format)
}

// —————————————————— 限流器 ——————————————————

// tokenLimiter 是单 token 的进程内限流器（滑动起点窗口，无外部依赖）。
//
// 窗口从该 token 的**首个**请求起算满一小时，而不是对齐整点：
// 对齐整点时攻击者可在 59:59 打满 60 次、下一分钟再打满 60 次，瞬间 120 次；
// 从首请求起算则任意 1 小时跨度内最多 limit 次，没有窗口边界突发。
//
// 调用点在最前面、且对不存在的 token 也计数 —— 否则拿随机 token 就能绕过限流把库打满。
// 代价是 map 会随攻击流量增长，因此带过期清扫与条目上限。
type tokenLimiter struct {
	limit  int64
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*limiterBucket
	// inserts 记录自上次清扫以来新建的桶数：按「新建次数」而不是「请求次数」触发清扫，
	// 避免热点 token 把 CPU 全花在遍历 map 上。
	inserts int
}

// limiterBucket 是单个 token 的计数窗口。
type limiterBucket struct {
	start time.Time
	count int64
}

const (
	// limiterSweepEvery 每新建多少个桶做一次过期清扫。
	limiterSweepEvery = 1024
	// limiterMaxBuckets 是桶数量上限（防御随机 token 灌内存）。
	limiterMaxBuckets = 200_000
)

// newTokenLimiter 构造限流器；limit <= 0 表示关闭（返回 nil）。
func newTokenLimiter(limit int64, now func() time.Time) *tokenLimiter {
	if limit <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &tokenLimiter{
		limit:   limit,
		window:  time.Hour,
		now:     now,
		buckets: make(map[string]*limiterBucket),
	}
}

// allow 记录一次请求。超限返回 false 与「距窗口重置还有多久」。
func (l *tokenLimiter) allow(token string) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[token]
	if !ok || now.Sub(b.start) >= l.window {
		b = &limiterBucket{start: now}
		l.buckets[token] = b
		l.inserts++
		if l.inserts >= limiterSweepEvery || len(l.buckets) > limiterMaxBuckets {
			l.sweepLocked(now)
			l.inserts = 0
		}
	}

	if b.count >= l.limit {
		return false, b.start.Add(l.window).Sub(now)
	}
	b.count++
	return true, 0
}

// sweepLocked 清掉已过期窗口；仍超上限时淘汰最老的一批。
func (l *tokenLimiter) sweepLocked(now time.Time) {
	for token, b := range l.buckets {
		if now.Sub(b.start) >= l.window {
			delete(l.buckets, token)
		}
	}
	for len(l.buckets) > limiterMaxBuckets {
		var (
			oldestToken string
			oldestStart time.Time
			first       = true
		)
		for token, b := range l.buckets {
			if first || b.start.Before(oldestStart) {
				oldestToken, oldestStart, first = token, b.start, false
			}
		}
		delete(l.buckets, oldestToken)
	}
}

// retryAfterSeconds 把等待时长换算成 Retry-After 的秒数（向上取整，至少 1）。
func retryAfterSeconds(wait time.Duration) int {
	if wait <= 0 {
		return 1
	}
	secs := int((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// —————————————————— 渲染缓存 ——————————————————

// renderCache 是订阅正文的进程内缓存（无外部依赖）。
//
// 只缓存正文：Subscription-Userinfo 头依赖 users.traffic_used 的实时值，
// 缓存整条响应会把用量钉死 TTL 之久；标题也来自 site_configs，每次现算。
//
// 并发未命中同一个键时只放一个请求去生成（防击穿），其余等它的结果 ——
// 400 并发拉同一个 token 只会产生一次面板生成。
type renderCache struct {
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	items  map[string]cacheItem
	flight map[string]*cacheFlight
}

// cacheItem 是一条缓存记录。
type cacheItem struct {
	body      string
	expiresAt time.Time
}

// cacheFlight 是一次在途生成：等待方在 done 关闭后读 body/err。
type cacheFlight struct {
	done chan struct{}
	body string
	err  error
}

// cacheMaxEntries 是缓存条目上限（token × format 的组合远小于它）。
const cacheMaxEntries = 20_000

// newRenderCache 构造缓存；ttlSeconds <= 0 表示关闭（返回 nil）。
func newRenderCache(ttlSeconds int64, now func() time.Time) *renderCache {
	if ttlSeconds <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &renderCache{
		ttl:    time.Duration(ttlSeconds) * time.Second,
		now:    now,
		items:  make(map[string]cacheItem),
		flight: make(map[string]*cacheFlight),
	}
}

// load 返回键对应的正文：命中直接返回，未命中调用 gen 并缓存成功结果。
func (c *renderCache) load(key string, gen func() (string, error)) (string, error) {
	if c == nil || c.ttl <= 0 {
		return gen()
	}

	now := c.now()
	c.mu.Lock()
	if item, ok := c.items[key]; ok {
		if now.Before(item.expiresAt) {
			c.mu.Unlock()
			return item.body, nil
		}
		delete(c.items, key)
	}
	if f, ok := c.flight[key]; ok {
		c.mu.Unlock()
		<-f.done
		return f.body, f.err
	}
	f := &cacheFlight{done: make(chan struct{})}
	c.flight[key] = f
	c.mu.Unlock()

	body, err := gen()

	c.mu.Lock()
	f.body, f.err = body, err
	delete(c.flight, key)
	if err == nil {
		c.storeLocked(key, body, c.now())
	}
	c.mu.Unlock()
	close(f.done)

	return body, err
}

// storeLocked 写入一条记录，条目过多时先清过期、再按最早到期淘汰。
func (c *renderCache) storeLocked(key, body string, now time.Time) {
	if len(c.items) >= cacheMaxEntries {
		for k, item := range c.items {
			if !now.Before(item.expiresAt) {
				delete(c.items, k)
			}
		}
		for len(c.items) >= cacheMaxEntries {
			var (
				oldestKey string
				oldestExp time.Time
				first     = true
			)
			for k, item := range c.items {
				if first || item.expiresAt.Before(oldestExp) {
					oldestKey, oldestExp, first = k, item.expiresAt, false
				}
			}
			delete(c.items, oldestKey)
		}
	}
	c.items[key] = cacheItem{body: body, expiresAt: now.Add(c.ttl)}
}

// envInt64 读非负整数环境变量；缺省/非法/负数一律回落默认值（0 是合法值，表示关闭）。
func envInt64(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}
