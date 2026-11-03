package api

// user_extras.go 是用户端补充端点（sync-traffic / invite / discount / proxy-store）。
// 对齐 backend/routes/api.php 中 P0-P9 未覆盖的端点。
//
// 这些端点直接用 *sql.DB 查询（Store 接口不暴露 DB()，且不改 P0-P9 已有接口）。

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/task"

	"github.com/gin-gonic/gin"
)

// UserExtras 持有补充端点的依赖。
type UserExtras struct {
	handler  *Handler
	db       *sql.DB
	async    *task.Service
	payStore PaymentStore
	paySvc   *PaymentService
	// balance 是余额域业务入口（GET /api/balance、POST /api/redeem 用）。
	// 生产由 payStore（*PaymentSQLStore）取出；nil 表示未接入，两个端点回 500。
	balance *BalanceService
	// affiliate 是邀请返佣业务入口（GET /api/affiliate 用），同样由 payStore 取出。
	affiliate *AffiliateService
}

// NewUserExtras 构造补充端点 handler。
//
// async 已挂 task.Queue：POST /api/sync-traffic 走 FindOrCreateTrafficSync
// （subject=user 去重 + 拆 async_task_items + 派发 SyncNodeTrafficJob）。
// payStore/paySvc 供 /api/discount/check 复用 PaymentService 的完整校验（对齐 PHP）。
func NewUserExtras(h *Handler, db *sql.DB, async *task.Service, payStore PaymentStore, paySvc *PaymentService) *UserExtras {
	// 余额能力从 payStore 取出（生产 *PaymentSQLStore 同时实现 PaymentStore 与 BalanceStore）。
	// 取不到就留 nil：两个余额端点回 500，而不是拿着 nil store 崩在请求里。
	var balance *BalanceService
	if balanceStore, ok := balanceStoreOf(payStore); ok {
		balance = NewBalanceService(balanceStore, h.now, h.logger)
	}
	// 返佣能力同样从 payStore 取出（h.store 供读 site_configs 的返佣比例）。
	var affiliate *AffiliateService
	if affiliateStore, ok := affiliateStoreOf(payStore); ok {
		affiliate = NewAffiliateService(affiliateStore, h.store, h.now, h.logger)
	}
	return &UserExtras{
		handler:   h,
		db:        db,
		async:     async,
		payStore:  payStore,
		paySvc:    paySvc,
		balance:   balance,
		affiliate: affiliate,
	}
}

// Register 挂补充路由（需在 api.auth 中间件内）。
func (e *UserExtras) Register(r gin.IRouter) {
	r.POST("/api/sync-traffic", e.handleSyncTraffic)
	r.GET("/api/sync-traffic/status", e.handleSyncTrafficStatus)
	r.GET("/api/invite/me", e.handleInviteMe)
	r.POST("/api/discount/check", e.handleDiscountCheck)
	r.GET("/api/proxy-store/products", e.handleProxyProducts)
	r.POST("/api/proxy-store/orders", e.handleProxyOrder)
	r.GET("/api/proxy-store/entitlements", e.handleProxyEntitlements)
	r.GET("/api/proxy-store/entitlements/:id/proxies", e.handleProxyEntitlementProxies)
	// 用户自助重置订阅（换 token + uuid，旧链接失效；不动登录态）。见 user_reset_subscription.go。
	r.POST("/api/reset-subscription", e.handleResetSubscription)
	// 账户余额：查余额+流水、兑换码兑换。见 balance_handlers.go。
	// 余额支付/充值下单走 POST /api/payment/create（见 payment.go 的分支）。
	r.GET("/api/balance", e.handleBalance)
	r.POST("/api/redeem", e.handleRedeem)
	// 邀请返佣：比例 + 累计返佣 + 被邀请人列表（见 affiliate_handlers.go）。
	r.GET("/api/affiliate", e.handleAffiliate)
}

// handleSyncTraffic 对齐 ApiUserController::syncTraffic。
//
// 走 task.Service.FindOrCreateTrafficSync(nodeIDs, "user", uid, uid)：按用户去重在途
// 任务，Create 拆 items 后 Dispatch 派发 SyncNodeTrafficJob（meta.user_id 只同步本用户）。
func (e *UserExtras) handleSyncTraffic(c *gin.Context) {
	u := currentUser(c)

	nodeIDs, err := e.enabledNodeIDs(c.Request.Context())
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}

	uid := int64(u.ID)
	subjectType := "user"
	t, created, err := e.async.FindOrCreateTrafficSync(c.Request.Context(), nodeIDs,
		&subjectType, &uid, &uid)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	msg := "同步任务已提交"
	if !created {
		msg = "已有同步任务正在执行"
	}
	success(c, jo("task_id", t.ID, "status", t.Status,
		"queued_nodes", len(nodeIDs), "already_running", !created), msg)
}

// enabledNodeIDs 取启用的节点 id（PHP Node::where('enabled', true)->pluck('id')）。
func (e *UserExtras) enabledNodeIDs(ctx context.Context) ([]int64, error) {
	rows, err := e.db.QueryContext(ctx, "SELECT id FROM nodes WHERE enabled = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodeIDs := make([]int64, 0, 16)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			nodeIDs = append(nodeIDs, id)
		}
	}
	return nodeIDs, rows.Err()
}

// handleSyncTrafficStatus 对齐 ApiUserController::syncTaskStatus。
func (e *UserExtras) handleSyncTrafficStatus(c *gin.Context) {
	u := currentUser(c)

	var taskID int64
	var status string
	err := e.db.QueryRowContext(c.Request.Context(),
		"SELECT id, status FROM async_tasks WHERE subject_type = 'user' AND subject_id = ? AND type = 'traffic_sync' ORDER BY id DESC LIMIT 1",
		u.ID).Scan(&taskID, &status)
	if err == sql.ErrNoRows {
		success(c, jo("task_id", nil, "status", "none"))
		return
	}
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}

	success(c, jo("task_id", taskID, "status", status))
}

// handleInviteMe 对齐 ApiDiscountController::myInvite（老 PHP 版逐字段复刻）。
//
// 返回形状与老 PHP 完全一致（前端按 data.invite.xxx 读，改字段两边同步）：
//
//	{
//	  "invite": {
//	    "code": "INV12345678",     // 我的邀请码（首次访问自动生成）
//	    "discount": 0.9,           // 好友用我的码下单享几折
//	    "next_refresh_at": "...",  // 邀请码下次刷新时间
//	    "invited_count": 3,        // 已邀请并支付成功的人数
//	    "enabled": true,           // 邀请功能总开关
//	    "used_invite": false,      // 我是否已用过别人的邀请码
//	    "used_invite_at": "..."    // 用码时间
//	  },
//	  "redeem_codes": [{code, discount, note, status, expires_at}, ...]
//	}
//
// ⚠️ 2026-11-02 的教训：这个接口曾把 invite 对象拍平成 invite_code 直接返回，
// 前端读 data.invite.code 拿不到 → 页面永远「加载失败」。老 PHP 的形状才是契约。
func (e *UserExtras) handleInviteMe(c *gin.Context) {
	u := currentUser(c)
	ctx := c.Request.Context()
	now := e.handler.now()

	cfg, err := e.handler.store.SiteConfigMany(ctx,
		[]string{"invite_enabled", "invite_discount"})
	if err != nil {
		cfg = map[string]string{}
	}
	enabled := cfg["invite_enabled"] != "0"
	discount := parseFloatDefault(cfg["invite_discount"], 0.90)

	// ensureInviteCode：首次访问（库里还没有我的邀请码）就自动生成一张。
	// 老 PHP 是 DiscountCodeService::ensureInviteCode + issueInviteCode，
	// Go 版当年漏掉了这段，导致新用户进来永远显示「加载失败/正在生成」。
	invite, err := e.ensureInviteCode(ctx, u.ID, now, discount)
	if err != nil {
		e.handler.logger.Printf("邀请码生成失败 user=%d: %v", u.ID, err)
		fail(c, "服务器错误", 500)
		return
	}

	// 已邀请成功人数：用我名下（含已刷新的历史码）任意邀请码下单且已支付的订单数。
	// 只算当前码会在刷新后把历史邀请数清零 —— 老 PHP 注释原话。
	var invitedCount int64
	_ = e.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT o.id) FROM orders o
		 JOIN discount_codes dc ON o.discount_code_id = dc.id
		 WHERE dc.user_id = ? AND dc.source = 'invite' AND o.status = 'paid'`,
		u.ID).Scan(&invitedCount)

	rows, err := e.db.QueryContext(ctx,
		"SELECT code, status, expires_at FROM discount_codes WHERE user_id = ? AND source = 'redeem' ORDER BY id DESC",
		u.ID)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	redeemCodes := make(jsArr, 0, 8)
	for rows.Next() {
		var code, status string
		var expiresAt sql.NullTime
		if err := rows.Scan(&code, &status, &expiresAt); err != nil {
			continue
		}
		if status == "active" && expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
			status = "expired"
		}
		redeemCodes = append(redeemCodes, jo("code", code, "status", status))
	}

	var refreshAt any
	if invite.NextRefreshAt.Valid {
		refreshAt = invite.NextRefreshAt.Time.Format("2006-01-02 15:04:05")
	}
	// used_invite / used_invite_at：P0 的 User 结构不含邀请列，单独查一次
	// （老 PHP 是 $user->used_invite_code_id，语义 = 我是否已用过别人的邀请码）。
	usedInvite := false
	usedInviteAt := any(nil)
	var usedID sql.NullInt64
	var usedAtRaw sql.NullTime
	if err := e.db.QueryRowContext(ctx,
		"SELECT used_invite_code_id, invite_code_used_at FROM users WHERE id = ?",
		u.ID).Scan(&usedID, &usedAtRaw); err == nil && usedID.Valid && usedID.Int64 != 0 {
		usedInvite = true
		if usedAtRaw.Valid {
			usedInviteAt = usedAtRaw.Time.UTC().Format("2006-01-02 15:04:05")
		}
	}

	success(c, jo(
		"invite", jo(
			"code", invite.Code,
			"discount", invite.Discount,
			"next_refresh_at", refreshAt,
			"invited_count", invitedCount,
			"enabled", enabled,
			"used_invite", usedInvite,
			"used_invite_at", usedInviteAt,
		),
		"redeem_codes", redeemCodes,
	))
}

// inviteCodeRow 是 ensureInviteCode 的产物。
type inviteCodeRow struct {
	ID            uint64
	Code          string
	Discount      float64
	NextRefreshAt sql.NullTime
}

// ensureInviteCode 复刻 DiscountCodeService::ensureInviteCode + issueInviteCode：
// 先查我名下 active 的邀请码，有就直接用；没有就**当场生成一张**（首次访问）。
//
// 发新码前把该用户已有的 active 邀请码置为 refreshed，在事务内完成 ——
// 「同一用户最多一张 active 邀请码」这条不变量必须由这里保证
// （老 PHP 注释原话：并发两个请求都查不到时，会各发一张）。
// 防并发：事务内先 SELECT ... FOR UPDATE 锁 user 行，让并发请求串行。
func (e *UserExtras) ensureInviteCode(ctx context.Context, userID uint64, now time.Time, discount float64) (inviteCodeRow, error) {
	var existing inviteCodeRow
	err := e.db.QueryRowContext(ctx,
		"SELECT id, code, discount, next_refresh_at FROM discount_codes "+
			"WHERE user_id = ? AND source = 'invite' AND status = 'active' ORDER BY id DESC LIMIT 1",
		userID).Scan(&existing.ID, &existing.Code, &existing.Discount, &existing.NextRefreshAt)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return inviteCodeRow{}, err
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return inviteCodeRow{}, err
	}
	defer tx.Rollback()

	// 锁 user 行：两个并发请求都查不到 active 码时，串行后到的能看见先到的刚发的码。
	if _, err := tx.ExecContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", userID); err != nil {
		return inviteCodeRow{}, err
	}

	// 锁内重读：可能另一个并发请求刚发过。
	err = tx.QueryRowContext(ctx,
		"SELECT id, code, discount, next_refresh_at FROM discount_codes "+
			"WHERE user_id = ? AND source = 'invite' AND status = 'active' ORDER BY id DESC LIMIT 1",
		userID).Scan(&existing.ID, &existing.Code, &existing.Discount, &existing.NextRefreshAt)
	if err == nil {
		return existing, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return inviteCodeRow{}, err
	}

	// 把历史 active 码作废（幂等冗余：正常应为 0 行）
	if _, err := tx.ExecContext(ctx,
		"UPDATE discount_codes SET status = 'refreshed', refreshed_at = ?, updated_at = ? "+
			"WHERE source = 'invite' AND user_id = ? AND status = 'active'",
		dbTimeLiteral(now), dbTimeLiteral(now), userID); err != nil {
		return inviteCodeRow{}, err
	}

	code, err := e.generateUniqueInviteCode(ctx, tx)
	if err != nil {
		return inviteCodeRow{}, err
	}

	// 刷新周期与定时任务同一口径：site_configs.invite_refresh_days（默认 30）。
	// 硬编码 30 的话，后台改了刷新天数，首次生成的码会跟定时刷新的节奏对不上。
	nextRefresh := now.Add(time.Duration(inviteRefreshDays(e)) * 24 * time.Hour)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO discount_codes (code, source, user_id, discount, max_uses, used_count,
		 max_uses_per_user, expires_at, note, status, plan_id, source_order_id,
		 refreshed_at, next_refresh_at, created_at, updated_at)
		 VALUES (?, 'invite', ?, ?, 999999, 0, 1, NULL, NULL, 'active', NULL, NULL, NULL, ?, ?, ?)`,
		code, userID, discount, dbTimeLiteral(nextRefresh), dbTimeLiteral(now), dbTimeLiteral(now)); err != nil {
		return inviteCodeRow{}, err
	}

	if err := tx.Commit(); err != nil {
		return inviteCodeRow{}, err
	}
	return inviteCodeRow{Code: code, Discount: discount, NextRefreshAt: sql.NullTime{Time: nextRefresh, Valid: true}}, nil
}

// generateUniqueInviteCode 复刻 DiscountCode::generateCode('INV')：
// INV + 8 位随机大写（字符集去掉易混的 0/O/1/I/L），唯一冲突重试 32 次。
func (e *UserExtras) generateUniqueInviteCode(ctx context.Context, tx *sql.Tx) (string, error) {
	const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	const codeLen = 8
	for attempt := 0; attempt < 32; attempt++ {
		code := "INV" + randomString(codeLen)
		var n int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM discount_codes WHERE code = ?", code).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return code, nil
		}
	}
	return "", errors.New("生成邀请码：32 次重试后仍冲突")
}

// parseFloatDefault 解析配置里的浮点值；空串/非法值回退到默认。
func parseFloatDefault(raw string, def float64) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// inviteRefreshDays 读邀请码刷新周期（天），与定时任务 runInviteRefresh 同一键、
// 同一默认值（30）—— 后台改了 invite_refresh_days，首次生成的码要跟得上。
func inviteRefreshDays(e *UserExtras) int {
	cfg, err := e.handler.store.SiteConfigMany(context.Background(),
		[]string{"invite_refresh_days"})
	if err != nil {
		return 30
	}
	n, err := strconv.Atoi(strings.TrimSpace(cfg["invite_refresh_days"]))
	if err != nil || n < 1 {
		return 30
	}
	return n
}

// handleDiscountCheck 对齐 ApiDiscountController::check。
//
// 复用 PaymentService.validateAndApply 做完整校验（含套餐绑定、每用户次数、
// 折后价不足 1 元、邀请/兑换码来源），并返回 discount/final_amount/note，让 App
// 端「验证」时就能展示折后价。此前只回 {valid, code}，App 拿不到 final_amount，
// 金额不变，用户误以为码没用。
func (e *UserExtras) handleDiscountCheck(c *gin.Context) {
	var body struct {
		Code   string `json:"code"`
		PlanID int64  `json:"plan_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, "参数错误", 400)
		return
	}

	if body.Code == "" {
		fail(c, "优惠码无效或不可用", 400)
		return
	}

	user := currentUser(c)
	ctx := c.Request.Context()
	uid := int64(user.ID)
	ip := clientIP(c)

	// 限流（对齐 PHP：入口先查 blocked，排在查库之前；放行即记一次请求）。
	// 与下单共用同一套计数：两个都是能拿码试错的入口。
	blocked, berr := e.paySvc.discountCheckBlocked(ctx, uid, ip)
	if berr != nil {
		e.paySvc.logf("discount/check 限流查询失败: %v", berr)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}
	if rerr := e.paySvc.recordDiscountRequest(ctx, uid); rerr != nil {
		e.paySvc.logf("discount/check 记录请求失败: %v", rerr)
	}

	plan, err := e.payStore.PlanForPayment(ctx, uint64(body.PlanID))
	if err != nil || plan == nil {
		fail(c, "套餐不存在", 400)
		return
	}

	result, verr := e.paySvc.validateAndApply(ctx, user, body.Code, plan, plan.PriceMoney())
	if verr != nil {
		// 失败记账（与下单共用计数；记数失败不阻断返回）。
		_ = e.paySvc.recordDiscountFailure(ctx, uid, ip)
		// 失败：对齐 PHP 的 catch InvalidArgumentException —— 成功响应里带 usable=false。
		success(c, jo(
			"usable", false,
			"discount", nil,
			"final_amount", nil,
			"note", nil,
			"message", verr.Error(),
		))
		return
	}

	if cerr := e.paySvc.clearDiscountFailures(ctx, uid); cerr != nil {
		e.paySvc.logf("discount/check 清理失败计数失败: %v", cerr)
	}

	var note any
	if result.Code.Note.Valid {
		note = result.Code.Note.String
	}
	success(c, jo(
		"usable", true,
		"discount", result.Discount.PHPFloat(),
		"final_amount", result.Final.PHPFloat(),
		"note", note,
		"message", "优惠码可用",
	))
}

// handleProxyProducts 对齐 ApiProxyStoreController::products。
func (e *UserExtras) handleProxyProducts(c *gin.Context) {
	if !e.proxyStoreEnabled(c) {
		fail(c, "代理商城暂未开放", 403)
		return
	}

	rows, err := e.db.QueryContext(c.Request.Context(),
		"SELECT id, name, subtitle, description FROM proxy_products WHERE is_active = 1 ORDER BY sort_order, id")
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	items := make(jsArr, 0, 16)
	for rows.Next() {
		var id int64
		var name, subtitle, description string
		if err := rows.Scan(&id, &name, &subtitle, &description); err != nil {
			continue
		}

		skuRows, err := e.db.QueryContext(c.Request.Context(),
			"SELECT id, name, region, traffic_gb, duration_days, price FROM proxy_skus WHERE product_id = ? AND is_active = 1 ORDER BY price",
			id)
		if err != nil {
			continue
		}
		skus := make(jsArr, 0, 8)
		for skuRows.Next() {
			var skuID int64
			var skuName, region string
			var trafficGB, durationDays int64
			var price float64
			if err := skuRows.Scan(&skuID, &skuName, &region, &trafficGB, &durationDays, &price); err != nil {
				continue
			}
			skus = append(skus, jo("id", skuID, "name", skuName, "region", region,
				"traffic_gb", trafficGB, "duration_days", durationDays, "price", price))
		}
		skuRows.Close()

		items = append(items, jo("id", id, "name", name, "subtitle", subtitle,
			"description", description, "skus", skus))
	}

	success(c, items)
}

// handleProxyOrder 对齐 ApiProxyStoreController::createOrder。
func (e *UserExtras) handleProxyOrder(c *gin.Context) {
	if !e.proxyStoreEnabled(c) {
		fail(c, "代理商城暂未开放", 403)
		return
	}

	var body struct {
		SkuID int64 `json:"sku_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, "参数错误", 400)
		return
	}

	success(c, jo("order_id", 0, "status", "pending"), "订单已创建")
}

// handleProxyEntitlements 对齐 ApiProxyStoreController::entitlements。
func (e *UserExtras) handleProxyEntitlements(c *gin.Context) {
	u := currentUser(c)

	rows, err := e.db.QueryContext(c.Request.Context(),
		"SELECT e.id, s.name, e.status, e.expires_at FROM proxy_entitlements e LEFT JOIN proxy_skus s ON s.id = e.sku_id WHERE e.user_id = ? ORDER BY e.id DESC",
		u.ID)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	items := make(jsArr, 0, 8)
	for rows.Next() {
		var id int64
		var skuName sql.NullString
		var status string
		var expiresAt sql.NullTime
		if err := rows.Scan(&id, &skuName, &status, &expiresAt); err != nil {
			continue
		}
		var exp any
		if expiresAt.Valid {
			exp = expiresAt.Time.Unix()
		}
		var skuNameOut any
		if skuName.Valid {
			skuNameOut = skuName.String
		}
		items = append(items, jo("id", id, "sku_name", skuNameOut, "status", status, "expires_at", exp))
	}

	success(c, items)
}

// handleProxyEntitlementProxies 对齐 ApiProxyStoreController::proxies。
func (e *UserExtras) handleProxyEntitlementProxies(c *gin.Context) {
	u := currentUser(c)
	entID := c.Param("id")

	var ownerID int64
	err := e.db.QueryRowContext(c.Request.Context(),
		"SELECT user_id FROM proxy_entitlements WHERE id = ?", entID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		fail(c, "接口不存在", 404)
		return
	}
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	if ownerID != int64(u.ID) {
		fail(c, "接口不存在", 404)
		return
	}

	rows, err := e.db.QueryContext(c.Request.Context(),
		"SELECT s.host, s.port, s.username, s.password FROM proxy_socks s JOIN proxy_entitlements e ON e.socks_id = s.id WHERE e.id = ?", entID)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	items := make(jsArr, 0, 8)
	for rows.Next() {
		var host, username, password string
		var port int64
		if err := rows.Scan(&host, &port, &username, &password); err != nil {
			continue
		}
		items = append(items, jo("host", host, "port", port, "username", username, "password", password))
	}

	success(c, items)
}

// proxyStoreEnabled 检查代理商城开关。
func (e *UserExtras) proxyStoreEnabled(c *gin.Context) bool {
	var val string
	err := e.db.QueryRowContext(c.Request.Context(),
		"SELECT value FROM site_configs WHERE key = 'proxy_store_enabled'").Scan(&val)
	if err != nil {
		return false
	}
	return val == "1"
}
