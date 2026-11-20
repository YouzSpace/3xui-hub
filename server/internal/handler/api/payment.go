package api

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"controlhub-server/internal/driver/payment"

	"github.com/gin-gonic/gin"
)

// 本文件是用户端支付端点（backend/app/Http/Controllers/Api/PaymentController.php 的 Go 复刻）。
//
// 路由清单（契约 api-contract.md §3 用户端支付组）：
//
//	公开：    GET  /api/payment/methods
//	          POST /api/payment/notify
//	鉴权后：  POST /api/payment/create
//	          POST /api/payment/create-reset
//	          GET  /api/payment/status
//	          GET  /api/payment/orders
//	          POST /api/payment/retry
//
// 为什么走独立的 RegisterPayment 而不是塞进 Handler.Register：
//   - routes_test.go 钉住了 Handler.Register 的路由清单（P2 验收基线），支付属于 P7；
//   - 支付端点需要额外的 PaymentService 依赖，塞进 Handler 会把 P2 的组装面扩大。
//
// 响应口径与 P2 一致：HTTP 200 + {code,msg,data}，中文按 PHP 的 \uXXXX 转义。

// PaymentHandlers 持有支付端点所需的依赖。
type PaymentHandlers struct {
	svc    *PaymentService
	store  PaymentStore
	cfg    *Handler // 复用鉴权中间件、日志、时钟与 PHP 口径工具
	logger loggerLike
}

// NewPaymentHandlers 组装支付端点。h 用于取 AuthMiddleware 与 logger/now。
func NewPaymentHandlers(h *Handler, store PaymentStore, svc *PaymentService) *PaymentHandlers {
	return &PaymentHandlers{svc: svc, store: store, cfg: h, logger: h.logger}
}

// RegisterPayment 把支付路由挂到给定分组。
//
// 挂载方式与 Handler.Register 保持一致：公开端点直接挂 r（gin.Engine 或分组），
// 鉴权端点单独开一个 Group 套 AuthMiddleware —— 少套一层就是「未登录也能下单」。
func (h *PaymentHandlers) RegisterPayment(r gin.IRouter) {
	r.GET("/api/payment/methods", h.handlePaymentMethods)
	r.POST("/api/payment/notify", h.handlePaymentNotify)

	auth := r.Group("", h.cfg.AuthMiddleware())
	auth.POST("/api/payment/create", h.handlePaymentCreate)
	auth.POST("/api/payment/create-reset", h.handlePaymentCreateReset)
	auth.GET("/api/payment/status", h.handlePaymentStatus)
	auth.GET("/api/payment/orders", h.handlePaymentOrders)
	auth.POST("/api/payment/retry", h.handlePaymentRetry)
}

// ---------------------------------------------------------------------------
// POST /api/payment/create
// ---------------------------------------------------------------------------

// handlePaymentCreate 对齐 PaymentController::create。
//
// 流程（顺序不能换）：
//  1. 校验（plan_id required|integer，payment_config_id sometimes|nullable|integer，
//     discount_code sometimes|nullable|string|max:32）
//  2. 套餐存在 / 已上架
//  3. **1 分钟内的同套餐 pending 单 → 429**（防连点重复下单）
//  4. 超过 1 分钟 → **直接新建一笔订单 + 新链接**（不再复用旧单：旧链接是一次性的，复用只会给死链）
//  5. 建单（含折扣码）→ 有 pay_url 才算成功
//
// 两个余额分支在入口处分流（字段互斥，见 docs/balance/implementation-plan.md §3.1.3/§3.1.4）：
//   - {"order_type":"balance","amount":5000}：余额充值下单（走第三方支付，回调到账）
//   - {"plan_id":1,"payment":"balance"}：用余额支付套餐（不经过网关）
func (h *PaymentHandlers) handlePaymentCreate(c *gin.Context) {
	user := currentUser(c)

	input := jsonInput(c)
	if inputStringOrEmpty(input["order_type"]) == "balance" {
		h.handleRechargeCreate(c, user, input)
		return
	}
	if inputStringOrEmpty(input["payment"]) == "balance" {
		h.handleBalancePayCreate(c, user, input)
		return
	}

	v := newPaymentValidator(input).
		field("plan_id", "required", "integer").
		field("payment_config_id", "sometimes", "nullable", "integer").
		field("discount_code", "sometimes", "nullable", "string", "max:32")
	if !v.passes() {
		respondPaymentValidation(c, v)
		return
	}

	ctx := c.Request.Context()
	planID := phpInt(input["plan_id"])
	plan, err := h.store.PlanForPayment(ctx, uint64(planID))
	if err != nil || plan == nil {
		fail(c, "套餐不存在", 400)
		return
	}
	if !plan.IsActive {
		fail(c, "该套餐已下架", 400)
		return
	}

	now := h.cfg.now()

	// 3. 1 分钟内的同套餐 pending 单 → 拒绝（防连点，避免手误生成多张单）
	recent, err := h.store.PendingOrderCreatedSince(ctx, user.ID, plan.ID, now.Add(-time.Minute))
	if err != nil && !IsNoRows(err) {
		h.logf("payment/create 查询近期订单失败: %v", err)
		fail(c, "创建订单失败", 500)
		return
	}
	if recent != nil {
		// 控制器原样：HTTP 200 + code 429（不是 HTTP 429）
		fail(c, "请勿频繁下单，请1分钟后再试", 429)
		return
	}

	// 4. 不再「复用旧单」：每次重新购买都**新开一单**。
	//
	// 原本这里会查「同套餐未过期的 pending 单」，拿它的旧单号重新出链接再返回给用户。
	// 但支付网关的 h5 链接是**一次性**的（打开过就失效，再开只剩会话 token、不出二维码），
	// 所以复用旧单必然把用户导向一个死链接——这正是「续付/重新购买拿不到二维码」的根因。
	// 现改为：直接往下走新建流程（新订单号 + 新链接），旧 pending 单交给「支付有效期 + 兜底
	// 任务」自然过期，不再拿它的单号和链接给用户。

	// 5. 建单
	result, err := h.svc.CreateOrder(ctx, PaymentCreateInput{
		User:            user,
		PlanID:          plan.ID,
		PaymentConfigID: uint64(maxInt64(phpInt(input["payment_config_id"]), 0)),
		DiscountCode:    inputStringOrEmpty(input["discount_code"]),
		PayIP:           clientIP(c),
	})
	if err != nil {
		if IsPaymentError(err) {
			fail(c, err.Error(), 400)
			return
		}
		if IsPayURLMissing(err) && result != nil {
			fail(c, "获取支付链接失败，请稍后再试", 500)
			return
		}
		h.logf("payment/create 创建订单失败: %v", err)
		fail(c, "创建订单失败", 500)
		return
	}

	success(c, jo(
		"order_no", result.OrderNo,
		"amount", result.Amount.PHPFloat(),
		"status", result.Status,
		"pay_url", nullIfEmpty(result.PayURL),
	))
}

// ---------------------------------------------------------------------------
// POST /api/payment/create —— 余额充值 / 余额支付（docs/balance/implementation-plan.md）
// ---------------------------------------------------------------------------

// handleRechargeCreate 余额充值下单：{"order_type":"balance","amount":5000}（amount 单位分）。
//
// 建的是 order_type=balance 的普通待付订单，付款仍走第三方支付；钱在回调
// （CompleteOrder → completeRechargeOrder）里进余额，重复回调不会重复加钱。
func (h *PaymentHandlers) handleRechargeCreate(c *gin.Context, user *User, input map[string]any) {
	v := newPaymentValidator(input).
		field("amount", "required", "integer").
		field("payment_config_id", "sometimes", "nullable", "integer")
	if !v.passes() {
		respondPaymentValidation(c, v)
		return
	}

	result, err := h.svc.CreateRechargeOrder(c.Request.Context(), user, phpInt(input["amount"]),
		uint64(maxInt64(phpInt(input["payment_config_id"]), 0)), clientIP(c))
	if err != nil {
		if IsPaymentError(err) {
			fail(c, err.Error(), 400)
			return
		}
		if IsPayURLMissing(err) && result != nil {
			fail(c, "获取支付链接失败，请稍后再试", 500)
			return
		}
		h.logf("payment/create 余额充值建单失败: %v", err)
		fail(c, "创建订单失败", 500)
		return
	}

	success(c, jo(
		"order_no", result.OrderNo,
		"amount", result.Amount.PHPFloat(),
		"status", result.Status,
		"pay_url", nullIfEmpty(result.PayURL),
	))
}

// handleBalancePayCreate 余额支付套餐：{"plan_id":1,"payment":"balance"}。
//
// 成功即已扣款并置 paid（status 直接是 paid，没有 pay_url）；余额不足 → code 400「余额不足」。
func (h *PaymentHandlers) handleBalancePayCreate(c *gin.Context, user *User, input map[string]any) {
	v := newPaymentValidator(input).
		field("plan_id", "required", "integer").
		field("discount_code", "sometimes", "nullable", "string", "max:32")
	if !v.passes() {
		respondPaymentValidation(c, v)
		return
	}

	result, err := h.svc.PayPlanWithBalance(c.Request.Context(), user, uint64(phpInt(input["plan_id"])),
		inputStringOrEmpty(input["discount_code"]), clientIP(c))
	if err != nil {
		if IsPaymentError(err) {
			fail(c, err.Error(), 400)
			return
		}
		h.logf("payment/create 余额支付失败 user=%d: %v", user.ID, err)
		fail(c, "支付失败", 500)
		return
	}

	success(c, jo(
		"order_no", result.OrderNo,
		"amount", result.Amount.PHPFloat(),
		"status", result.Status,
		"pay_url", nil,
	))
}

// ---------------------------------------------------------------------------
// POST /api/payment/create-reset
// ---------------------------------------------------------------------------

// handlePaymentCreateReset 对齐 PaymentController::createReset。
func (h *PaymentHandlers) handlePaymentCreateReset(c *gin.Context) {
	user := currentUser(c)
	input := jsonInput(c)

	v := newPaymentValidator(input).field("payment_config_id", "sometimes", "nullable", "integer")
	if !v.passes() {
		respondPaymentValidation(c, v)
		return
	}

	result, err := h.svc.CreateResetOrder(c.Request.Context(), user,
		uint64(maxInt64(phpInt(input["payment_config_id"]), 0)), clientIP(c))
	if err != nil {
		if IsPaymentError(err) {
			fail(c, err.Error(), 400)
			return
		}
		h.logf("payment/create-reset 失败: %v", err)
		fail(c, "创建订单失败", 500)
		return
	}

	if result.PayURL == "" && result.Status == "pending" {
		// PHP 这一条分支下仍然 success(result)（result 里 pay_url 为空）。
		// 保持原样：前端拿到空 pay_url 会提示「获取支付链接失败」。
		success(c, jo(
			"order_no", result.OrderNo,
			"pay_url", nil,
			"status", result.Status,
		))
		return
	}
	success(c, jo(
		"order_no", result.OrderNo,
		"pay_url", result.PayURL,
		"status", result.Status,
	))
}

// ---------------------------------------------------------------------------
// POST /api/payment/notify（公开）
// ---------------------------------------------------------------------------

// handlePaymentNotify 对齐 PaymentController::notify：
// 应答是**纯文本**（payindex: OK/FAIL，epay: success/fail），不是 JSON —— 网关只认这几个字母。
func (h *PaymentHandlers) handlePaymentNotify(c *gin.Context) {
	data := notifyParams(c)
	result := h.svc.HandleNotify(c.Request.Context(), data)
	c.Data(http.StatusOK, "text/html; charset=UTF-8", []byte(result.Body))
}

// queryInput 把 query string 读成「PHP 语义」的输入 map。
//
// 与 jsonInput 的区别不只是来源：PHP 的 parse_str 会把 `a[]=1` 解析成数组、
// 把重复键解析成数组，而 Go 的 c.Query 只取第一个值且不认 `[]` 后缀。
// 校验器据此判定 `order_no` 是「非字符串」而不是「缺失」—— 文案会差一整句。
//
// 同时套用与请求体一致的全局中间件口径（TrimStrings + ConvertEmptyStringsToNull）。
func queryInput(c *gin.Context) map[string]any {
	values := c.Request.URL.Query()
	out := make(map[string]any, len(values))

	for rawKey, vs := range values {
		if len(vs) == 0 {
			continue
		}
		key := rawKey
		isArray := false
		if strings.HasSuffix(key, "[]") {
			key = strings.TrimSuffix(key, "[]")
			isArray = true
		}
		if isArray || len(vs) > 1 {
			// PHP: `a[]=1&a[]=2` → ["1","2"]；同一键出现多次也是数组
			items := make([]any, 0, len(vs))
			for _, v := range vs {
				items = append(items, normalizeInputValue(key, v))
			}
			out[key] = items
			continue
		}
		out[key] = normalizeInputValue(key, vs[0])
	}
	return out
}

// notifyParams 收集回调参数（POST 表单 / query 都收，后者覆盖前者）。
//
// Laravel 的 $request->all() 把 query 与 body 合并，body 优先；这里同序：先 query 再 body。
// 值一律转字符串（网关发来的都是字符串，数字按 PHP 的 (string) 口径转）。
func notifyParams(c *gin.Context) map[string]string {
	out := make(map[string]string, 16)
	for k, vs := range c.Request.URL.Query() {
		if len(vs) > 0 {
			out[k] = vs[len(vs)-1]
		}
	}
	_ = c.Request.ParseMultipartForm(1 << 20)
	form := c.Request.PostForm
	for k, vs := range form {
		if len(vs) > 0 {
			out[k] = vs[len(vs)-1]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// GET /api/payment/status
// ---------------------------------------------------------------------------

// handlePaymentStatus 对齐 PaymentController::status：
// pending 时先向网关查单（查单成功即完成订单），再回读订单状态。
func (h *PaymentHandlers) handlePaymentStatus(c *gin.Context) {
	user := currentUser(c)
	ctx := c.Request.Context()

	// GET 的入参来自 query，必须按 PHP 的 parse_str 口径读：`order_no[]=1` 是**数组**，
	// 校验文案应是「must be a string」而不是「required」（实测 PHP 如此）。
	input := queryInput(c)
	v := newPaymentValidator(input).field("order_no", "required", "string")
	if !v.passes() {
		respondPaymentValidation(c, v)
		return
	}
	orderNo, _ := input["order_no"].(string)

	order, err := h.store.OrderByNoForUser(ctx, user.ID, orderNo, "")
	if err != nil && !IsNoRows(err) {
		h.logf("payment/status 查询订单失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	if order == nil {
		fail(c, "订单不存在", 404)
		return
	}

	if order.Status == "pending" {
		_ = h.svc.QueryOrder(ctx, order)
		// 查单可能把订单置 paid，回读一次（对齐 $order->refresh()）。
		if fresh, ferr := h.store.OrderByNo(ctx, order.OrderNo); ferr == nil && fresh != nil {
			order = fresh
		}
	}

	success(c, jo(
		"order_no", order.OrderNo,
		"status", order.Status,
		"amount", mustMoney(order.Amount).PHPFloat(),
		"paid_at", nullableISOTime(order.PaidAt),
	))
}

// ---------------------------------------------------------------------------
// GET /api/payment/orders
// ---------------------------------------------------------------------------

// handlePaymentOrders 对齐 PaymentController::orders：
// 最近 20 笔，plan_name 取套餐名；代理订单没有 plan 时用「代理 · SKU 名」；都没有是 "-"。
func (h *PaymentHandlers) handlePaymentOrders(c *gin.Context) {
	user := currentUser(c)
	rows, err := h.store.UserOrders(c.Request.Context(), user.ID, 20)
	if err != nil {
		h.logf("payment/orders 查询失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	items := make(jsArr, 0, len(rows))
	now := h.cfg.now().UTC()
	// 「允许继续支付」总开关（后台可关）：关掉后前端不展示按钮，接口层也拒绝。
	continuePay := h.svc.ContinuePayEnabled(c.Request.Context())
	for i := range rows {
		row := &rows[i]

		// $o->plan?->name ?? '-'
		var planName any
		if row.PlanName.Valid {
			planName = row.PlanName.String
		} else {
			planName = "-"
		}

		var planID any
		if row.PlanID.Valid {
			planID = row.PlanID.Int64
		}

		// 支付有效期由我们自己的时钟判定（不靠定时任务）：到点即按「已过期」下发，
		// 前端据此倒计时并隐藏「继续支付」。判据与续付、兜底作废三处同源
		// （EffectivePayExpiresAt / OrderPayExpired）。
		//
		// 同时下发 remaining_seconds（**以服务端时钟为准**）：前端拿它当基线 + 本地单调时钟
		// 递减，这样设备时间被改坏也不会把倒计时算歪 —— 剩余时间只由「服务端存的绝对时刻
		// 减去真实流逝」决定，关浏览器/退出登录/换设备都不影响。
		status := row.Status
		var expires, remaining any
		if status == "pending" {
			exp := EffectivePayExpiresAt(&row.Order)
			if now.Before(exp) {
				expires = nullableISOTime(sql.NullTime{Time: exp, Valid: true})
				secs := int(exp.Sub(now) / time.Second)
				if secs < 0 {
					secs = 0
				}
				remaining = secs
			} else {
				status = "expired" // 已到点
			}
		}

		items = append(items, jo(
			"order_no", row.OrderNo,
			"plan_id", planID,
			"plan_name", planName,
			"order_type", row.OrderType,
			"amount", mustMoney(row.Amount).PHPFloat(),
			"status", status,
			"expires_at", expires,
			"remaining_seconds", remaining,
			"can_continue_pay", continuePay,
			"paid_at", nullableISOTime(row.PaidAt),
			"created_at", isoTime(row.CreatedAt),
		))
	}
	success(c, items)
}

// ---------------------------------------------------------------------------
// POST /api/payment/retry
// ---------------------------------------------------------------------------

// handlePaymentRetry 对齐 PaymentController::retry：
// 网关判重 → 作废 + code 410「支付链接已失效，请返回商城重新下单」；
// 其它失败 → code 500；成功 → {order_no, pay_url}。
func (h *PaymentHandlers) handlePaymentRetry(c *gin.Context) {
	user := currentUser(c)

	// 「允许继续支付」总开关：后台关掉后，用户端不展示入口，这里也直接拒绝
	// （双保险：防止旧页面/直接调接口绕过）。网关能力不一时由管理员自己掌阀。
	if !h.svc.ContinuePayEnabled(c.Request.Context()) {
		fail(c, "继续支付已关闭，请返回商城重新下单", 403)
		return
	}

	input := jsonInput(c)
	v := newPaymentValidator(input).field("order_no", "required", "string")
	if !v.passes() {
		respondPaymentValidation(c, v)
		return
	}
	orderNo, _ := input["order_no"].(string)

	order, err := h.store.OrderByNoForUser(c.Request.Context(), user.ID, orderNo, "pending")
	if err != nil && !IsNoRows(err) {
		h.logf("payment/retry 查询订单失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	if order == nil {
		fail(c, "订单不存在或已支付", 404)
		return
	}

	// 支付窗口由我们自己的时钟判定：到点即作废，**绝不把网关那边已失效的链接发给用户**
	// （这正是「点续付打不开/报过期」的根因）。这里顺手把状态落库，不必等定时任务。
	if OrderPayExpired(order, h.cfg.now().UTC()) {
		if uerr := h.store.UpdateOrderStatus(c.Request.Context(), order.ID, "expired"); uerr != nil {
			h.logf("payment/retry 支付窗口已过、作废订单失败 order=%s: %v", order.OrderNo, uerr)
		}
		fail(c, "支付链接已过期，请返回商城重新下单", 410)
		return
	}

	// 未过期：优先用下单时缓存的支付链接，直接返回 —— 不再拿原单号去网关重新下单。
	// payindex 等网关会把「同单号再次下单」判重，我们据此作废旧单 → 每次续付都变成 expired。
	// 缓存命中即绕开网关。
	if order.PayURL.Valid && order.PayURL.String != "" {
		success(c, jo(
			"order_no", order.OrderNo,
			"pay_url", order.PayURL.String,
		))
		return
	}

	// 没有缓存链接（老订单 / 缓存被清）→ 兜底重新出链接；成功则回填缓存，网关判重才作废。
	payCfg, cerr := h.svc.configForOrder(c.Request.Context(), order)
	if cerr != nil || payCfg == nil {
		fail(c, "支付配置异常", 500)
		return
	}

	payURL, failMsg, berr := h.svc.BuildPayURL(c.Request.Context(), payCfg, order)
	if berr != nil {
		h.logf("payment/retry 出链接异常 order=%s: %v", order.OrderNo, berr)
	}
	if payURL == "" {
		// 网关判重：这笔单号已不可用，作废并提示用户重新下单（不能提示「稍后再试」——
		// 那会让用户反复点，每次都是同一个结果）。
		if h.svc.IsOrderNoUnusable(failMsg) {
			if uerr := h.store.UpdateOrderStatus(c.Request.Context(), order.ID, "expired"); uerr != nil {
				h.logf("payment/retry 作废订单失败 order=%s: %v", order.OrderNo, uerr)
			}
			fail(c, "支付链接已失效，请返回商城重新下单", 410)
			return
		}
		fail(c, "获取支付链接失败，请稍后再试", 500)
		return
	}

	h.svc.CachePayURL(c.Request.Context(), order.ID, payURL)
	success(c, jo(
		"order_no", order.OrderNo,
		"pay_url", payURL,
	))
}

// ---------------------------------------------------------------------------
// GET /api/payment/methods（公开）
// ---------------------------------------------------------------------------

// handlePaymentMethods 对齐 PaymentController::methods：
// PaymentConfig::where('enabled',true)->select('id','name','bank_code')->get()
// —— 只有三个字段（api_key 等密不落响应）。
func (h *PaymentHandlers) handlePaymentMethods(c *gin.Context) {
	configs, err := h.store.EnabledPaymentConfigs(c.Request.Context())
	if err != nil {
		h.logf("payment/methods 查询失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	items := make(jsArr, 0, len(configs))
	for i := range configs {
		cfg := &configs[i]
		items = append(items, jo(
			"id", cfg.ID,
			"name", cfg.Name,
			"bank_code", cfg.BankCode,
		))
	}
	success(c, items)
}

// ---------------------------------------------------------------------------
// 供其他 handler 复用的接缝（P8/代理商城会用到）
// ---------------------------------------------------------------------------

// PaymentServiceFrom 返回当前 handler 绑定的支付服务（nil 表示未接支付）。
func (h *PaymentHandlers) PaymentServiceFrom() *PaymentService { return h.svc }

// Drivers 返回驱动注册中心的协议清单（后台 payments 页面用；P8c 会消费）。
func Drivers() []payment.Definition { return payment.NewRegistry().Definitions() }

// ---- 小工具 ----

func (h *PaymentHandlers) logf(format string, args ...any) {
	if h.logger != nil {
		h.logger.Printf(format, args...)
	}
}

// inputStringOrEmpty 取字符串入参（已做 trim/空串转 null 的输入里，null 视为空串）。
func inputStringOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
