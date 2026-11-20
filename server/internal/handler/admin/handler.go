package admin

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// NoRouteHandler 是管理端前缀下的兜底 404。
//
// PHP 侧 bootstrap/app.php 会把 NotFoundHttpException 统一改写成
// HTTP 200 + {"code":404,"msg":"接口不存在"}；Gin 默认回 404 空体。
// 路由组装方（cmd/server/routes.go）应在 /admin-api 分组上挂这个兜底
// （gin 只能在 Engine 上注册 NoRoute，所以这里只提供 handler）。
func (h *Handler) NoRouteHandler() gin.HandlerFunc {
	return func(c *gin.Context) { h.notFoundRoute(c) }
}

// Register 把管理端剩余路由挂到给定分组。
//
// 代理商城（proxy-store / proxy-ops）与 Webshare 上游（upstream/webshare）产品线已下线，
// 这三段路由连同其 handler 一起移除，这里只保留教程 / 优惠码 / 站点配置。
// 调用方负责挂 AdminAuth 中间件与 /admin-api 前缀；这里只声明相对路径，
// 便于测试与 cmd/server/routes.go 各自组合。
func (h *Handler) Register(r gin.IRouter) {
	// 教程
	r.GET("/tutorials", h.listTutorials)
	r.POST("/tutorials", h.createTutorial)
	r.PUT("/tutorials/:tutorial", h.updateTutorial)
	r.DELETE("/tutorials/:tutorial", h.deleteTutorial)

	// 优惠码 / 邀请机制
	r.GET("/discount-codes", h.listDiscountCodes)
	r.POST("/discount-codes", h.createDiscountCodes)
	r.POST("/discount-codes/batch-delete", h.batchDeleteDiscountCodes)
	r.PUT("/discount-codes/:code", h.updateDiscountCode)
	r.DELETE("/discount-codes/:code", h.deleteDiscountCode)
	r.POST("/discount-code-folders", h.createFolder)
	r.PUT("/discount-code-folders/:folder", h.updateFolder)
	r.DELETE("/discount-code-folders/:folder", h.deleteFolder)
	r.GET("/invite-settings", h.inviteSettings)
	r.PUT("/invite-settings", h.updateInviteSettings)

	// 首页 / 配色 / 站点 / 订阅配置
	r.GET("/home-custom", h.showHomeCustom)
	r.PUT("/home-custom", h.saveHomeCustom)
	r.POST("/home-custom/reset", h.resetHomeCustom)
	r.GET("/panel-colors", h.showPanelColors)
	r.PUT("/panel-colors", h.savePanelColors)
	r.POST("/panel-colors/reset", h.resetPanelColors)
	r.GET("/site-settings", h.showSiteSettings)
	r.PUT("/site-settings", h.updateSiteSettings)
	r.GET("/subscription-settings", h.showSubscriptionSettings)
	r.PUT("/subscription-settings", h.updateSubscriptionSettings)
	r.GET("/app-version-settings", h.showAppVersionSettings)
	r.PUT("/app-version-settings", h.updateAppVersionSettings)
}

// Handler 持有管理端商店域所需的依赖。
//
// 代理商城下线后，Store 接口与 Webshare 上游客户端一并移除；
// 这里只保留各管理域共用的响应/参数辅助。
type Handler struct {
	store *sqlStore
	cfg   Config

	// now 供测试注入固定时钟（PHP 侧全部用 now()，时区 UTC）。
	now func() time.Time

	logger *log.Logger
}

// Config 是本域需要的站点配置项（来自 .env / 环境变量，不是 site_configs 表）。
type Config struct {
	// DiscountRateDefaults 是优惠码校验限流的 config/panel.php 默认值，
	// invite-settings 在 site_configs 为空时必须回落到这里（不是 0）。
	DiscountRateDefaults DiscountRateDefaults
}

// DiscountRateDefaults 对齐 config/panel.php 的 ratelimit 段（优惠码相关的 5 项）。
type DiscountRateDefaults struct {
	PerMinute    int64
	FailPerDay   int64
	IPFailHourly int64
	MaxAttempts  int64
	LockSeconds  int64
}

// DefaultDiscountRateDefaults 是 config/panel.php 的出厂值（.env 未覆盖时）。
func DefaultDiscountRateDefaults() DiscountRateDefaults {
	return DiscountRateDefaults{
		PerMinute:    10,
		FailPerDay:   50,
		IPFailHourly: 100,
		MaxAttempts:  10,
		LockSeconds:  900,
	}
}

// Deps 是 New 的依赖清单（显式传参，避免测试构造半吊子 Handler）。
type Deps struct {
	Store    *sqlStore
	Config   Config
	Now      func() time.Time
	Logger   *log.Logger
}

// New 组装 Handler。
func New(d Deps) *Handler {
	if d.Store == nil {
		panic("admin: Store 不能为空")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	cfg := d.Config
	if cfg.DiscountRateDefaults == (DiscountRateDefaults{}) {
		cfg.DiscountRateDefaults = DefaultDiscountRateDefaults()
	}
	return &Handler{
		store:    d.Store,
		cfg:      cfg,
		now:      now,
		logger:   logger,
	}
}


func (h *Handler) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

// successNoMsg 对齐控制器里手写的 response()->json(['code'=>0,'data'=>...])（tutorials 就是这样）。
func (h *Handler) successNoMsg(c *gin.Context, data any) {
	writeJSON(c, phpEncode(jo("code", 0, "data", data)))
}

// fail 对齐 ApiResponse::error($msg, $code)（HTTP 200，前端只看 code）。
func (h *Handler) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// failValidation 把校验失败渲染成 PHP 形态：HTTP 200 + code 500 + 合并文案。
func (h *Handler) failValidation(c *gin.Context, v *validator) {
	if err := v.storeFailure(); err != nil {
		h.logger.Printf("admin 校验时查库失败: %v", err)
		h.fail(c, "服务器错误", 500)
		return
	}
	h.fail(c, v.summary(), 500)
}

// failServer 记录真实错误并回通用 500 文案（不把 SQL 细节透给前端）。
func (h *Handler) failServer(c *gin.Context, what string, err error) {
	h.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	h.fail(c, "服务器错误", 500)
}

// notFoundRoute 对齐 bootstrap/app.php 对 404 的统一改写：
// 路由模型绑定失败（id 非数字 / 记录不存在）→ {"code":404,"msg":"接口不存在"}。
func (h *Handler) notFoundRoute(c *gin.Context) {
	h.fail(c, "接口不存在", 404)
}

// successPage 对齐 ApiResponse::successPage：data 为当前页数组，pagination 在顶层。
func (h *Handler) successPage(c *gin.Context, items []any, page, perPage, total int64) {
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page < 1 {
		page = 1
	}
	if page > lastPage {
		// 越界夹回最后一页（与 PHP 的 paginate 二次查询同口径）。
		page = lastPage
	}
	writeJSON(c, phpEncode(jo(
		"code", 0,
		"msg", "ok",
		"data", jsArr(items),
		"pagination", jo(
			"current_page", page,
			"last_page", lastPage,
			"total", total,
			"per_page", perPage,
		),
	)))
}

// writeJSON 输出 HTTP 200 + 给定 JSON 字节。
func writeJSON(c *gin.Context, body []byte) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// ---- 路径参数 ----

// paramID 解析路由里的数字 id（Laravel 的隐式模型绑定在非数字时同样 404）。
func (h *Handler) paramID(c *gin.Context, name string) (uint64, bool) {
	return parseUint(c.Param(name))
}

// ctxOf 便于 handler 里少写一行。
func ctxOf(c *gin.Context) context.Context { return c.Request.Context() }

// existsChecker 把 Store 的 exists 查询绑定到当前请求上下文，供校验器使用。
func (h *Handler) existsChecker() func(table, column string, value any) (bool, error) {
	return func(table, column string, value any) (bool, error) {
		// 校验器的 exists 是同步的，没有 context 参数；这里用一个短超时上下文，
		// 避免查库卡住整个请求（PHP 侧同样受 PDO 超时约束）。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return h.store.ExistsValue(ctx, table, column, value)
	}
}

// parseUint 解析路由里的无符号十进制 id（非数字 → false，对应 Laravel 隐式绑定的 404）。
func parseUint(raw string) (uint64, bool) {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}

// noRowsToNotFound 把 sql.ErrNoRows 映射成 404 路由文案，其余交给调用方。
func (h *Handler) noRowsToNotFound(c *gin.Context, err error) bool {
	if err == sql.ErrNoRows {
		h.notFoundRoute(c)
		return true
	}
	return false
}
