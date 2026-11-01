package api

import (
	"database/sql"
	"log"
	"time"

	"controlhub-server/internal/config"

	"github.com/gin-gonic/gin"
)

// Handler 持有用户端端点所需的全部依赖。
//
// 依赖全部是接口：生产用 sqlstore/cache/mailer 的真实实现，测试用内存实现 ——
// 因此本包的单测不连库、不发信、不打面板（面板调用也只是接缝）。
type Handler struct {
	store  Store
	cache  Cache
	cfg    *config.Config
	mailer Mailer

	// bcryptCost 用配置里的 BCRYPT_ROUNDS（Laravel Hash::make 同值，默认 12）。
	bcryptCost int

	// tokenTTL 是 access_token 有效期（契约 §4：30 天）。
	tokenTTL time.Duration

	limiter        *rateLimiter
	captchaSecret  []byte
	provisioner    Provisioner
	protocolSwitch ProtocolSwitcher
	logger         *log.Logger

	// userHub 是用户端下行消息通道（WebSocket）。定位是通用通道，后期接入
	// 订单/工单/公告等通知直接复用，见 ws_message.go。
	userHub *UserHub

	// now 供测试注入固定时钟。
	now func() time.Time
}

// Deps 是 New 的依赖清单（显式传参，避免测试里构造一个「半吊子」Handler）。
type Deps struct {
	Store  Store
	Cache  Cache
	Config *config.Config
	Mailer Mailer
	// Provisioner / ProtocolSwitch 可为 nil：注册照常成功（只记日志）、协议切换返回 500 文案。
	Provisioner    Provisioner
	ProtocolSwitch ProtocolSwitcher
	// UserHub 是用户端下行消息通道；为空时自建一个（WS 端点与管理端推送需
	// 共用同一实例，生产由 cmd/server 建好传进来）。
	UserHub *UserHub
	// Now 为空时用 time.Now。
	Now func() time.Time
	// Logger 为空时用标准库默认 logger。
	Logger *log.Logger
}

// New 组装 Handler。
func New(d Deps) *Handler {
	if d.Config == nil {
		panic("api: Config 不能为空")
	}
	if d.Store == nil {
		panic("api: Store 不能为空")
	}
	if d.Cache == nil {
		panic("api: Cache 不能为空")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}

	h := &Handler{
		store:          d.Store,
		cache:          d.Cache,
		cfg:            d.Config,
		mailer:         d.Mailer,
		bcryptCost:     d.Config.BcryptRounds,
		tokenTTL:       30 * 24 * time.Hour,
		now:            now,
		provisioner:    d.Provisioner,
		protocolSwitch: d.ProtocolSwitch,
		logger:         logger,
	}
	if h.bcryptCost < 4 || h.bcryptCost > 31 {
		h.bcryptCost = 12
	}
	h.userHub = d.UserHub
	if h.userHub == nil {
		h.userHub = NewUserHub()
	}
	h.limiter = newRateLimiter(d.Cache)
	h.limiter.now = now
	h.captchaSecret = captchaSecret(d.Config.AppKey)
	return h
}

// Register 把用户端路由挂到给定分组（由 cmd/server/routes.go 传入 gin.Engine 或分组）。
//
// 路由清单对齐 backend/routes/api.php 的用户端公开 + 认证 + 鉴权段（不含支付/商城/邀请，
// 那些属于 P7/P8；也不含订阅与 node-api，那属于 P3/P4）。
func (h *Handler) Register(r gin.IRouter) {
	// 公开
	r.GET("/api/ping", h.handlePing)
	r.GET("/api/site-config", h.handleSiteConfig)
	r.GET("/api/plans", h.handlePlans)
	r.GET("/api/tutorials", h.handleTutorials)
	r.GET("/api/tutorials/:id", h.handleTutorialDetail)
	r.GET("/api/app-version", h.handleAppVersion)

	// 认证（无需 Bearer）
	r.GET("/api/captcha", h.handleCaptcha)
	r.POST("/api/login", h.handleLogin)
	r.POST("/api/login-email", h.handleLoginEmail)
	r.POST("/api/register", h.handleRegister)
	r.POST("/api/email-verify/send", h.handleEmailVerifySend)
	r.POST("/api/email-verify/check", h.handleEmailVerifyCheck)
	r.POST("/api/password-reset/send", h.handlePasswordResetSend)
	r.POST("/api/password-reset/reset", h.handlePasswordResetReset)

	// 鉴权后（api.auth）
	auth := r.Group("", h.AuthMiddleware())
	auth.GET("/api/me", h.handleMe)
	auth.GET("/api/nodes", h.handleNodes)
	auth.POST("/api/protocol", h.handleProtocol)
	auth.GET("/api/_auth_check", h.handleAuthCheck)

	// 用户端下行消息通道（WebSocket，自带 access_token 鉴权，故不套 AuthMiddleware）。
	// 浏览器 WS 无法自定义请求头，鉴权走 ?access_token= 或 Authorization 两种之一。
	r.GET("/api/ws", h.handleWS)
}

// handlePing 对齐 routes/api.php 里的闭包：{"code":0,"msg":"ok","data":{"pong":true}}。
func (h *Handler) handlePing(c *gin.Context) {
	success(c, jo("pong", true))
}

// handleAuthCheck 对齐 /_auth_check：{"user_id": <id>}。
func (h *Handler) handleAuthCheck(c *gin.Context) {
	u := currentUser(c)
	success(c, jo("user_id", u.ID))
}

// currentUser 取中间件注入的用户（鉴权组内必然非 nil）。
func currentUser(c *gin.Context) *User {
	if v, ok := c.Get(ctxUserKey); ok {
		if u, ok := v.(*User); ok {
			return u
		}
	}
	return nil
}

// siteConfigKeys 是 GET /api/site-config 暴露的键（逐字对齐 routes/api.php）。
// 顺序即输出顺序；不在这里的键（如 smtp_password）永不外泄。
var siteConfigKeys = []string{
	"site_title", "site_subtitle", "site_description", "site_keywords",
	"site_logo", "site_favicon", "register_email_verify", "announcement",
	"feedback_link", "sub_clash_enabled", "sub_singbox_enabled",
	"sub_show_flag", "sub_rename_enabled", "sub_rename_regex", "sub_rename_replacement",
	// 自定义公开首页：前端读到空值就走默认首页，老面板行为不变
	"home_custom_html", "home_custom_css",
	// 代理商城总开关（默认关闭；用户端据此隐藏「代理」入口）
	"proxy_store_enabled",
}

// handleSiteConfig 对齐 SiteConfig::getMany：缺失键补空串，且只输出白名单内的键。
func (h *Handler) handleSiteConfig(c *gin.Context) {
	values, err := h.store.SiteConfigMany(c.Request.Context(), siteConfigKeys)
	if err != nil {
		h.logger.Printf("site-config 查询失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	obj := &jsObj{keys: make([]string, 0, len(siteConfigKeys)), vals: make([]any, 0, len(siteConfigKeys))}
	for _, k := range siteConfigKeys {
		obj.put(k, values[k])
	}
	success(c, obj)
}

// handlePlans 对齐 PlanController::index。
//
// 字段口径（逐字对齐，含类型）：
//   - price：(float) $p->price → 数字（0.00 输出 0，4.90 输出 4.9）
//   - include_local：(bool) ($p->include_local ?? true)
//   - third_party_sub_ids：array_values(array_map('intval', (array) ($p->third_party_sub_ids ?? [])))
func (h *Handler) handlePlans(c *gin.Context) {
	plans, err := h.store.ActivePlans(c.Request.Context())
	if err != nil {
		h.logger.Printf("plans 查询失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	items := make(jsArr, 0, len(plans))
	for i := range plans {
		p := &plans[i]
		ids := make(jsArr, 0, len(p.ThirdPartyIDs))
		for _, id := range p.ThirdPartyIDs {
			ids = append(ids, id)
		}
		items = append(items, jo(
			"id", p.ID,
			"name", p.Name,
			"price", phpFloatOf(p.Price),
			"type", p.Type,
			"months", intOrNull(p.Months),
			"monthly_traffic", intOrNull(p.MonthlyTraffic),
			"period_traffic", intOrNull(p.PeriodTraffic),
			"total_traffic", intOrNull(p.TotalTraffic),
			"include_local", p.IncludeLocal,
			"third_party_sub_ids", ids,
		))
	}
	success(c, items)
}

// intOrNull 把 sql.NullInt64 映射成 PHP 的 int|null。
func intOrNull(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}
