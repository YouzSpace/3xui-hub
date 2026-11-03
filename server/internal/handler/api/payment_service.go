package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"controlhub-server/internal/driver/payment"
	"controlhub-server/internal/tgbot"
)

// PaymentService 对齐 backend/app/Services/PaymentService.php。
//
// 分工（与 PHP 一致）：
//   - 驱动只管协议（签名/下单/验签/查单），见 internal/driver/payment；
//   - 本文件管业务：建单、折扣码、复用作废、完成订单（开套餐/重置流量）、回调总额校验。
//
// 三条容易写错的规则，改这里前先读：
//  1. **网关拒绝 vs 网络异常**：前者（FailMsg 非空）说明该订单号已不可用，可作废重建；
//     后者（Err 非空）绝不能作废 —— 用户可能已经扫码在付，作废等于把钱付到废单上。
//  2. **折扣码只在支付成功时占用**（used_count+1 / 邀请码锁人），下单只登记 discount_code_id。
//     因此未支付/放弃/超时的订单天然没有需要回滚的东西。
//  3. **金额比对按分**，容差口径照抄 PHP 的 `abs((float)a-(float)b) > 0.01`（浮点比，不是「四舍五入到分」）。
type PaymentService struct {
	store PaymentStore

	now     func() time.Time
	randInt func(n int64) int64
	drivers *payment.Registry
	http    payment.HTTPDoer

	panel            PanelSyncer
	proxyOrders      ProxyOrderCompleter
	cache            PaymentCache
	siteConfigs      SiteConfigReader
	returnURL        func(ctx context.Context) string
	defaultNotifyURL func(ctx context.Context) (string, error)
	rateGuard        DiscountRateGuard
	tg               *tgbot.Sender

	// balance 是余额域入口（余额支付/充值的扣款与到账）。
	// 由 store 取出（*PaymentSQLStore 同时实现 BalanceStore）；nil 表示未接入，两条余额路径都拒。
	balance *BalanceService

	// affiliate 是邀请返佣入口（被邀请人购买套餐后给邀请人加返佣）。
	// 由 store 取出（*PaymentSQLStore 同时实现 AffiliateStore）；nil 表示未接入，返佣整体跳过。
	affiliate *AffiliateService

	logger loggerLike
}

// loggerLike 是 *log.Logger 的最小子集（避免本文件直接依赖 log 包的具体类型）。
type loggerLike interface {
	Printf(format string, v ...any)
}

// paymentErrors 是 PHP 里抛 InvalidArgumentException 的业务错误：
// handler 统一渲染成 HTTP 200 + code 400 + 原文案（见 payment.go 的 respondServiceError）。
type paymentError struct{ msg string }

func (e *paymentError) Error() string { return e.msg }

// NewPaymentError 构造一条「业务失败」错误（文案直接面向用户）。
func NewPaymentError(format string, args ...any) error {
	return &paymentError{msg: fmt.Sprintf(format, args...)}
}

// IsPaymentError 判定是否业务层错误（handler 据此选 400 还是 500）。
func IsPaymentError(err error) bool {
	var pe *paymentError
	return errors.As(err, &pe)
}

// NewPaymentService 组装支付服务。
func NewPaymentService(store PaymentStore, opts PaymentServiceOptions, logger loggerLike) *PaymentService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	randInt := opts.RandInt
	if randInt == nil {
		randInt = cryptoRandInt
	}
	registry := opts.Registry
	if registry == nil {
		registry = payment.NewRegistry()
	}
	s := &PaymentService{
		store:            store,
		now:              now,
		randInt:          randInt,
		drivers:          registry,
		http:             opts.HTTP,
		panel:            opts.Panel,
		proxyOrders:      opts.ProxyOrders,
		cache:            opts.Cache,
		siteConfigs:      opts.SiteConfigs,
		returnURL:        opts.ReturnURL,
		defaultNotifyURL: opts.DefaultNotifyURL,
		rateGuard:        opts.RateGuard,
		tg:               opts.TGSender,
		logger:           logger,
	}
	// 余额能力从 store 取出：生产实现 *PaymentSQLStore 同时实现 PaymentStore 与 BalanceStore，
	// 这样不用改 cmd/server/routes.go 的构造函数签名（单测可注入内存实现）。
	if balanceStore, ok := balanceStoreOf(store); ok {
		s.balance = NewBalanceService(balanceStore, now, logger)
	}
	// 返佣能力同样从 store 取出：生产 *PaymentSQLStore 实现了 AffiliateStore。
	if affiliateStore, ok := affiliateStoreOf(store); ok {
		s.affiliate = NewAffiliateService(affiliateStore, opts.SiteConfigs, now, logger)
	}
	return s
}

// cryptoRandInt 返回 [0,n) 的密码学随机数（对齐 PHP random_int）。
func cryptoRandInt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0
	}
	return int64(binary.BigEndian.Uint64(buf[:]) % uint64(n))
}

// driverFor 取配置对应的驱动（driver_type 为空/未注册 → payindex，与 PHP 一致）。
func (s *PaymentService) driverFor(cfg *PaymentConfig) payment.Driver {
	return s.drivers.For(cfg.toDriverConfig())
}

// attachHTTP 给内置驱动塞上注入的 HTTP client（测试用 httptest；生产用默认）。
func (s *PaymentService) attachHTTP(d payment.Driver) payment.Driver {
	if s.http == nil {
		return d
	}
	switch t := d.(type) {
	case *payment.PayindexDriver:
		cp := *t
		cp.HTTP = s.http
		return &cp
	case *payment.EpayV1Driver:
		cp := *t
		cp.HTTP = s.http
		return &cp
	}
	return d
}

// ---------------------------------------------------------------------------
// 建单
// ---------------------------------------------------------------------------

// PaymentCreateInput 是 CreateOrder 的入参（对齐控制器校验后的字段）。
type PaymentCreateInput struct {
	User            *User
	PlanID          uint64
	PaymentConfigID uint64 // 0 = 未指定
	DiscountCode    string
	PayIP           string
}

// CreateOrderResult 是 createOrder 的返回（对齐 PHP 的返回数组）。
type CreateOrderResult struct {
	OrderNo string
	Amount  money
	Status  string
	PayURL  string
}

// CreateOrder 对齐 PaymentService::createOrder。
func (s *PaymentService) CreateOrder(ctx context.Context, in PaymentCreateInput) (*CreateOrderResult, error) {
	plan, err := s.store.PlanForPayment(ctx, in.PlanID)
	if err != nil || plan == nil {
		return nil, NewPaymentError("套餐不存在")
	}
	if !plan.IsActive {
		return nil, NewPaymentError("该套餐已下架")
	}

	payCfg, err := s.pickPaymentConfig(ctx, in.PaymentConfigID)
	if err != nil {
		return nil, err
	}
	if payCfg == nil {
		return nil, NewPaymentError("暂无可用支付方式")
	}

	original := plan.PriceMoney()
	discountCode := strings.TrimSpace(in.DiscountCode)
	// 原价为 0 的免费套餐不适用折扣码（折后价不可能 ≥ 原价），直接跳过校验。
	useDiscount := in.DiscountCode != "" && discountCode != "" && original.cents > 0
	reused := false

	var order *Order
	if useDiscount {
		order, reused, err = s.createDiscountedOrder(ctx, in, plan, payCfg, original, discountCode)
		if err != nil {
			return nil, err
		}
	} else {
		order, err = s.store.CreateOrder(ctx, NewOrderInput{
			OrderNo:         s.generateOrderNo(),
			UserID:          in.User.ID,
			PlanID:          plan.ID,
			Amount:          original.String(),
			OriginalAmount:  nullStr(original.String()),
			Status:          "pending",
			PaymentConfigID: payCfg.ID,
			PayIP:           nullStr(in.PayIP),
			Now:             s.now().UTC().Truncate(time.Second),
		})
		if err != nil {
			return nil, err
		}
	}

	// 免费套餐直接完成
	if original.cents <= 0 {
		if err := s.CompleteOrder(ctx, order, sql.NullString{}); err != nil {
			s.logf("免费订单完成失败 order=%s: %v", order.OrderNo, err)
		}
		return &CreateOrderResult{
			OrderNo: order.OrderNo,
			Amount:  money{},
			Status:  "paid",
			PayURL:  "",
		}, nil
	}

	// 复用旧单时用订单自己的支付配置（与控制器重发 pending 单的口径一致），新建单用本次选中的。
	payPayment := payCfg
	if reused {
		if oc, oerr := s.configForOrder(ctx, order); oerr == nil && oc != nil {
			payPayment = oc
		}
	}

	payURL, failMsg, _ := s.buildPayURL(ctx, payPayment, order)

	// 复用旧单出链接失败，且原因是「网关认为该订单号已存在」时，旧单号已彻底不可用：
	// 必须作废它、换新单号重下，否则用户每次购买都会撞同一笔单，永久报「获取支付链接失败」。
	if payURL == "" && reused && s.isOrderNoUnusable(failMsg) {
		s.logf("复用旧单被网关判重，作废后换新单号重下 old_order_no=%s gateway_msg=%s", order.OrderNo, failMsg)

		if err := s.store.UpdateOrderStatus(ctx, order.ID, "expired"); err != nil {
			s.logf("作废旧单失败 order=%s: %v", order.OrderNo, err)
		}

		newOrder, err := s.store.CreateOrder(ctx, NewOrderInput{
			OrderNo:         s.generateOrderNo(),
			UserID:          in.User.ID,
			PlanID:          plan.ID,
			Amount:          order.Amount,
			OriginalAmount:  order.OriginalAmount,
			DiscountCodeID:  nullInt64OrZero(order.DiscountCodeID),
			DiscountAmount:  nullStrOrZero(order.DiscountAmount),
			Status:          "pending",
			PaymentConfigID: payPayment.ID,
			PayIP:           nullStr(in.PayIP),
			Now:             s.now().UTC().Truncate(time.Second),
		})
		if err != nil {
			return nil, err
		}
		order = newOrder
		payURL, failMsg, _ = s.buildPayURL(ctx, payPayment, order)
	}

	result := &CreateOrderResult{
		OrderNo: order.OrderNo,
		Amount:  mustMoney(order.Amount),
		Status:  "pending",
		PayURL:  payURL,
	}
	if result.PayURL == "" {
		// 控制器口径：pending 但没有链接 → 「获取支付链接失败，请稍后再试」code 500。
		return result, errPayURLMissing
	}
	return result, nil
}

// errPayURLMissing 对齐控制器 `empty($result['pay_url']) && status==='pending'` 的分支。
var errPayURLMissing = errors.New("获取支付链接失败")

// IsPayURLMissing 判定「下单成功但没拿到链接」。
func IsPayURLMissing(err error) bool { return errors.Is(err, errPayURLMissing) }

// createDiscountedOrder 是带折扣码的建单路径（限流 → 校验 → 复用/新建）。
func (s *PaymentService) createDiscountedOrder(
	ctx context.Context,
	in PaymentCreateInput,
	plan *PlanRow,
	payCfg *PaymentConfig,
	original money,
	code string,
) (*Order, bool, error) {
	userID := int64(in.User.ID)

	// 限流与 /api/discount/check 共用同一套计数：下单是第二个能拿码试错的入口。
	// 检查排在查库之前：被限流/锁定时直接抛，一个字节的库都不查，也就没有枚举反馈。
	blocked, err := s.discountCheckBlocked(ctx, userID, in.PayIP)
	if err != nil {
		return nil, false, err
	}
	if blocked {
		return nil, false, NewPaymentError("%s", rateTooOftenMsg)
	}
	if err := s.recordDiscountRequest(ctx, userID); err != nil {
		s.logf("记录折扣码请求失败 user=%d: %v", userID, err)
	}

	applied, err := s.validateAndApply(ctx, in.User, code, plan, original)
	if err != nil {
		// 码无效 / 已失效 / 已被占用：记一次失败（计数走 RateLimiter，与订单事务互不牵连）。
		_ = s.recordDiscountFailure(ctx, userID, in.PayIP)
		return nil, false, err
	}

	// 同一用户已有一笔用同一张码、同一套餐的 pending 单 → 复用那笔，不新建。
	if applied.ReuseOrder != nil {
		return applied.ReuseOrder, true, nil
	}

	discountAmount := money{cents: original.cents - applied.Final.cents}
	order, err := s.store.CreateOrder(ctx, NewOrderInput{
		OrderNo:         s.generateOrderNo(),
		UserID:          in.User.ID,
		PlanID:          plan.ID,
		Amount:          applied.Final.String(),
		OriginalAmount:  nullStr(original.String()),
		DiscountCodeID:  nullInt64(applied.Code.ID),
		DiscountAmount:  nullStr(discountAmount.String()),
		Status:          "pending",
		PaymentConfigID: payCfg.ID,
		PayIP:           nullStr(in.PayIP),
		Now:             s.now().UTC().Truncate(time.Second),
	})
	if err != nil {
		return nil, false, err
	}

	// 校验通过：清掉自己攒的失败计数（同 check 接口口径，不清 IP 维度）。
	if err := s.clearDiscountFailures(ctx, userID); err != nil {
		s.logf("清理折扣码失败计数失败 user=%d: %v", userID, err)
	}
	return order, false, nil
}

// pickPaymentConfig 对齐控制器 + 服务的选配置逻辑：
// 传了 id → 取该条且 enabled；没传 → 取第一条 enabled。
func (s *PaymentService) pickPaymentConfig(ctx context.Context, id uint64) (*PaymentConfig, error) {
	if id > 0 {
		return s.store.PaymentConfigByID(ctx, id)
	}
	return s.store.FirstEnabledPaymentConfig(ctx)
}

// configForOrder 取订单关联的支付配置（order.payment_config_id）。
//
// 用 PaymentConfigByIDAny（不看 enabled）：PHP 的 `$order->paymentConfig` 是普通关联，
// 老订单在配置被后台停用后仍要能回调/查单 —— 停用只影响「新下单时能不能选到它」。
func (s *PaymentService) configForOrder(ctx context.Context, order *Order) (*PaymentConfig, error) {
	if !order.PaymentConfigID.Valid {
		return nil, nil
	}
	return s.store.PaymentConfigByIDAny(ctx, uint64(order.PaymentConfigID.Int64))
}

// generateOrderNo 对齐 Order::generateOrderNo()：`YmdHis` + 6 位零填充随机数（UTC）。
func (s *PaymentService) generateOrderNo() string {
	return s.now().UTC().Format("20060102150405") + fmt.Sprintf("%06d", s.randInt(1000000))
}

// ---------------------------------------------------------------------------
// 出支付链接
// ---------------------------------------------------------------------------

// BuildPayURL 对齐 PaymentService::buildPayUrl，返回 [链接, 网关拒绝原因, 技术错误]：
//
//   - 成功：            [url, "", nil]
//   - 网关明确拒绝：    ["", msg, nil]   ← 调用方据此作废旧单重建
//   - 网络/解析异常：   ["", "", err]    ← 临时故障，调用方**不得**作废订单
//
// 区分后两类至关重要：网络抖动时用户可能已经扫码在付，作废订单会把钱付到已作废的单上。
func (s *PaymentService) BuildPayURL(ctx context.Context, cfg *PaymentConfig, order *Order) (payURL, failMsg string, err error) {
	return s.buildPayURL(ctx, cfg, order)
}

func (s *PaymentService) buildPayURL(ctx context.Context, cfg *PaymentConfig, order *Order) (string, string, error) {
	driver := s.attachHTTP(s.driverFor(cfg))

	notifyURL, rerr := s.notifyURLFor(ctx, cfg)
	if rerr != nil {
		// 回调地址解析不到属于**临时故障**（而不是网关拒绝）：FailMsg 留空，
		// 调用方据此不会作废订单 —— 否则一次域名表抖动就会把所有用户的待付单作废。
		return "", "", rerr
	}
	returnURL := "/"
	if s.returnURL != nil {
		returnURL = s.returnURL(ctx)
	}

	productName := "套餐购买-" // 无套餐时 PHP 也是这个前缀（$order->plan->name ?? ''）
	if order.IsBalanceRecharge() {
		// 充值单没有套餐：网关账单上显示「余额充值-50.00元」
		productName = "余额充值-" + mustMoney(order.Amount).String() + "元"
	} else if order.PlanID.Valid {
		if plan, perr := s.store.PlanForPayment(ctx, uint64(order.PlanID.Int64)); perr == nil && plan != nil {
			productName = "套餐购买-" + plan.Name
		}
	}

	payIP := ""
	if order.PayIP.Valid {
		payIP = order.PayIP.String
	}

	res := driver.Pay(ctx, payment.PayRequest{
		Order: payment.Order{
			OrderNo:     order.OrderNo,
			Amount:      order.Amount,
			ProductName: productName,
			PayIP:       payIP,
		},
		NotifyURL: notifyURL,
		ReturnURL: returnURL,
	}, cfg.toDriverConfig())

	if res.Success {
		return res.PayURL, "", nil
	}
	if res.Err != nil {
		return "", "", res.Err
	}
	return "", res.FailMsg, nil
}

// notifyURLFor 对齐 AbstractPaymentDriver::notifyUrl：
// 配置里带 http 开头的完整 URL 用它，否则用主域名默认值（网关固定打主域）。
func (s *PaymentService) notifyURLFor(ctx context.Context, cfg *PaymentConfig) (string, error) {
	if strings.HasPrefix(cfg.NotifyURL, "http") {
		return cfg.NotifyURL, nil
	}
	if s.defaultNotifyURL != nil {
		return s.defaultNotifyURL(ctx)
	}
	// 兜底与 PHP 的 url('/api/payment/notify') 同源：主域名（domains 表）+ 固定路径。
	if s.store != nil {
		if domain, ok, err := s.store.PrimaryDomain(ctx); err == nil && ok && domain != "" {
			return "https://" + domain + "/api/payment/notify", nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("支付回调地址未配置")
}

// isOrderNoUnusable 对齐 PaymentService::isOrderNoUnusable：
// 各网关文案不统一（实测有「重复订单！」），做关键词匹配。命中即作废旧单重建；
// 不命中（含网络异常 msg=""）一律保留原单，避免误杀用户正在支付的订单。
func (s *PaymentService) isOrderNoUnusable(msg string) bool {
	if msg == "" {
		return false
	}
	for _, needle := range []string{"重复订单", "订单已存在", "订单号已存在", "重复提交", "已下单"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// IsOrderNoUnusable 供 handler 复用（控制器里「重发旧单失败」的场景要同一个判断）。
func (s *PaymentService) IsOrderNoUnusable(msg string) bool { return s.isOrderNoUnusable(msg) }

// ---------------------------------------------------------------------------
// 回调
// ---------------------------------------------------------------------------

// NotifyResult 是 handleNotify 的返回（对齐 PHP 的 [ok, body]）。
type NotifyResult struct {
	OK   bool
	Body string
}

// HandleNotify 对齐 PaymentService::handleNotify：
// 提取订单号 → 按订单 payment_config 的 driver_type 分发验签/判状态 → 金额匹配 → completeOrder。
//
// 订单号兼容 payindex 的 orderid 与 epay 系/支付宝的 out_trade_no。
func (s *PaymentService) HandleNotify(ctx context.Context, data map[string]string) NotifyResult {
	orderNo := data["orderid"]
	if orderNo == "" {
		orderNo = data["out_trade_no"]
	}

	order, err := s.store.OrderByNo(ctx, orderNo)
	if err != nil || order == nil {
		s.logf("支付回调: 订单不存在 order_no=%s err=%v", orderNo, err)
		return NotifyResult{OK: false, Body: "FAIL"}
	}

	payCfg, err := s.configForOrder(ctx, order)
	if err != nil || payCfg == nil {
		s.logf("支付回调: 支付配置缺失 order_no=%s err=%v", orderNo, err)
		return NotifyResult{OK: false, Body: "FAIL"}
	}

	driver := s.attachHTTP(s.driverFor(payCfg))
	res := driver.HandleCallback(data, payCfg.toDriverConfig())

	if !res.Success {
		s.logf("支付回调: 驱动校验未通过 order_no=%s error=%s", orderNo, res.Err)
		return NotifyResult{OK: false, Body: driver.NotifyResponse(false)}
	}

	// 金额误差 > 0.01 拒绝。口径照抄 PHP 的 `abs((float)a-(float)b) > 0.01`：
	// 这是**浮点比较**，不是「四舍五入到分」—— 1 分钱的差额在多数十进制组合下确实 > 0.01
	// （如 100.00 vs 99.99 → 0.010000000000005），所以这里必须按浮点算，不能改成整数分比较。
	if amountMismatch(res.Amount, order.Amount) {
		s.logf("支付回调: 金额不匹配 order_no=%s expected=%s actual=%s", orderNo, order.Amount, res.Amount)
		return NotifyResult{OK: false, Body: driver.NotifyResponse(false)}
	}

	if cerr := s.CompleteOrder(ctx, order, nullStrMaybe(res.TradeNo)); cerr != nil {
		s.logf("支付回调: 完成订单失败 order_no=%s err=%v", orderNo, cerr)
		return NotifyResult{OK: false, Body: driver.NotifyResponse(false)}
	}

	return NotifyResult{OK: true, Body: driver.NotifyResponse(true)}
}

// amountMismatch 复刻 PHP 的 `abs((float)$a - (float)$b) > 0.01`。
func amountMismatch(gatewayAmount, orderAmount string) bool {
	a := parseFloatOr(gatewayAmount, 0)
	b := parseFloatOr(orderAmount, 0)
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff > 0.01
}

// ---------------------------------------------------------------------------
// 完成订单
// ---------------------------------------------------------------------------

// CompleteOrder 对齐 PaymentService::completeOrder（按订单号前缀分流普通/重置订单）。
func (s *PaymentService) CompleteOrder(ctx context.Context, order *Order, tradeNo sql.NullString) error {
	if order.IsResetOrder() {
		return s.completeResetOrder(ctx, order, tradeNo)
	}

	if order.Status == "paid" {
		return nil
	}

	// 余额充值订单（order_type=balance）：置 paid + 加余额 + 写流水在一个事务里，不开套餐。
	if order.IsBalanceRecharge() {
		return s.completeRechargeOrder(ctx, order)
	}

	now := s.now().UTC().Truncate(time.Second)
	if err := s.store.MarkOrderPaid(ctx, order.ID, tradeNo, now); err != nil {
		return err
	}
	order.Status = "paid"
	order.TradeNo = tradeNo
	order.PaidAt = sql.NullTime{Time: now, Valid: true}

	s.notifyPaidTG(ctx, order)

	// 代理订单：走独立开通流程，不碰节点套餐、不发邀请奖励。
	if order.IsProxyOrder() {
		if s.proxyOrders != nil {
			if err := s.proxyOrders.CompleteOrder(ctx, order); err != nil {
				// 钱已经收了：开通失败只记日志，不能把回调打回 FAIL（网关会无限重推）。
				s.logf("代理订单开通失败 order_no=%s: %v", order.OrderNo, err)
			}
		} else {
			s.logf("ProxyOrders 未配置，order_no=%s 仅置 paid", order.OrderNo)
		}
		return nil
	}

	return s.afterPaid(ctx, order)
}

// notifyPaidTG 发「支付到账」TG 通知（开关 tg_notify_payment 开启才发）。
func (s *PaymentService) notifyPaidTG(ctx context.Context, order *Order) {
	if s.tg == nil {
		return
	}
	text := fmt.Sprintf("💰 新订单支付成功\n订单号：%s\n金额：%s", order.OrderNo, order.Amount)
	if err := s.tg.Notify(ctx, tgbot.KeyNotifyPayment, text); err != nil {
		s.logger.Printf("TG 支付到账通知失败: %v", err)
	}
}

// afterPaid 是「钱已到账」之后的业务副作用：折扣码占用 → 邀请奖励 → 开通套餐 → 下发/解封。
//
// 独立成函数是为了让余额支付复用同一条路径：余额支付的订单在扣款事务里就已经置成 paid
// （不会走上面那次 MarkOrderPaid），但它同样要占折扣码、发邀请奖励、开通套餐。
func (s *PaymentService) afterPaid(ctx context.Context, order *Order) error {
	now := s.now().UTC().Truncate(time.Second)

	user, err := s.store.UserByIDPayment(ctx, order.UserID)
	if err != nil || user == nil {
		s.logf("完成订单: 用户不存在 order_no=%s user_id=%d err=%v", order.OrderNo, order.UserID, err)
		return err
	}

	// 支付成功才真正占用折扣码：used_count+1、满额置 used_up、邀请码锁死用户。
	// 必须排在 rewardInviter 之前（发奖建立在「本次邀请已生效」之上）。
	if order.DiscountCodeID.Valid {
		codeID := order.DiscountCodeID.Int64
		// 传入当前订单 id：调用前本单已被置为 paid，consume 内的
		// 「每用户次数」复核必须排除它，否则第一笔就自计数被误拦。
		code, cerr := s.store.ConsumeDiscountCode(ctx, codeID, int64(user.ID), order.ID)
		if cerr != nil {
			// 并发兜底：码在支付前已被别处用满。用户钱已经付了，订单必须照常完成，
			// 这里只记日志，绝不向上抛 —— 抛出去回调会返回 FAIL，网关会反复重推。
			s.logf("支付成功但折扣码占用失败（码可能已用满）order_no=%s code_id=%d: %v",
				order.OrderNo, codeID, cerr)
		} else if code != nil && code.Source == "invite" {
			if lerr := s.store.LockUserInviteCode(ctx, user.ID, code.ID, now); lerr != nil {
				s.logf("邀请码锁人失败 user=%d code=%d: %v", user.ID, code.ID, lerr)
			}
		}
	}

	if err := s.rewardInviter(ctx, order); err != nil {
		s.logf("邀请奖励发放失败 order_no=%s: %v", order.OrderNo, err)
	}

	// 邀请返佣（平台补贴给邀请人，不从下单人扣）：同一订单只发一次（store 内 order_id 唯一键），
	// 失败只记日志 —— 钱已经收了，不能因为这个副作用把回调打回 FAIL。
	if aerr := s.affiliate.Grant(ctx, order); aerr != nil {
		s.logf("返佣发放失败 order_no=%s: %v", order.OrderNo, aerr)
	}

	if !order.PlanID.Valid {
		return nil
	}
	plan, perr := s.store.PlanForPayment(ctx, uint64(order.PlanID.Int64))
	if perr != nil || plan == nil {
		s.logf("完成订单: 套餐不存在 order_no=%s plan_id=%d", order.OrderNo, order.PlanID.Int64)
		return nil
	}

	// 用户换套餐 → applyPlan（重算限额/到期）→ provisionClient（下发节点）。
	fields := planFieldsForUser(user.ID, plan, now)
	if err := s.store.ApplyPlanFields(ctx, user.ID, fields); err != nil {
		return err
	}
	user.PlanID = fields.PlanID
	user.TrafficLimit = fields.TrafficLimit
	user.TrafficUsed = fields.TrafficUsed
	user.MonthlyTrafficUsed = fields.MonthlyTrafficUsed
	user.MonthlyTrafficLimit = fields.MonthlyTrafficLimit
	user.NextTrafficResetAt = fields.NextTrafficResetAt
	user.ExpiredAt = fields.ExpiredAt

	if s.panel != nil {
		if err := s.panel.ProvisionClient(ctx, user); err != nil {
			s.logf("provisionClient 失败 user=%d: %v", user.ID, err)
		}
	}

	// 续费后总是启用 3x-ui client（PHP: if (!$user->enabled) enabled=true）
	if !user.Enabled {
		if err := s.store.SetUserEnabled(ctx, user.ID, true); err != nil {
			return err
		}
		user.Enabled = true
	}
	// BanService::unban = enabled=true + toggleClient(true)
	if s.panel != nil {
		if err := s.panel.ToggleClient(ctx, user, true); err != nil {
			s.logf("unban toggleClient 失败 user=%d: %v", user.ID, err)
		}
	}
	return nil
}

// planFieldsForUser 复刻 UserAdminService::applyPlan 的字段计算。
//
// period：expired_at = now + months 个月（Carbon addMonths，**月溢出**：1-31 + 1 月 = 3-03），
// next_traffic_reset_at = now + 30 天；total：expired_at = null。
func planFieldsForUser(_ uint64, plan *PlanRow, now time.Time) PlanFields {
	fields := PlanFields{
		PlanID:             sql.NullInt64{Int64: int64(plan.ID), Valid: true},
		TrafficUsed:        0,
		MonthlyTrafficUsed: 0,
	}
	if plan.IsPeriod() {
		fields.TrafficLimit = plan.PeriodTraffic.Int64
		fields.MonthlyTrafficLimit = plan.MonthlyTraffic.Int64
		months := plan.Months.Int64
		// Carbon::addMonths 走 PHP 的日期溢出语义（1-31 + 1 月 = 3-03），见 addMonthsNoClamp。
		expired := addMonthsNoClamp(now, int(months))
		fields.ExpiredAt = sql.NullTime{Time: dateOnly(expired), Valid: true}
		fields.NextTrafficResetAt = sql.NullTime{Time: now.Add(30 * 24 * time.Hour), Valid: true}
		fields.HasExpired = true
		fields.HasNextReset = true
	} else {
		// total 套餐：expired_at 为 null，monthly_traffic_limit = 0。
		fields.TrafficLimit = plan.TotalTraffic.Int64
		fields.MonthlyTrafficLimit = 0
		fields.HasExpired = false
		fields.HasNextReset = false
	}
	return fields
}

// addMonthsNoClamp 复刻 Carbon::addMonths（PHP 的 DateTime::modify('+N month')）：
// 超出的日期会**溢出到下个月**，不做月末夹取 —— 1 月 31 日 + 1 个月 = 3 月 3 日。
//
// 实测 PHP 8.5 + Carbon（2026-10-24）：
//
//	2026-01-31 +1 → 2026-03-03；2026-08-31 +1 → 2026-10-01；2026-12-31 +2 → 2027-03-03
func addMonthsNoClamp(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	total := (int(m) - 1) + months
	ny := y + total/12
	nm := total % 12
	if nm < 0 {
		nm += 12
		ny--
	}
	// 目标月的天数（用于溢出计算）
	first := time.Date(ny, time.Month(nm+1), 1, 0, 0, 0, 0, t.Location())
	daysInMonth := first.AddDate(0, 1, -1).Day()
	if d <= daysInMonth {
		return time.Date(ny, time.Month(nm+1), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	}
	// 溢出：多出来的天数落到下个月
	overflow := d - daysInMonth
	return time.Date(ny, time.Month(nm+2), overflow, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// dateOnly 取当天 00:00:00（PHP `->toDateString()` 写进 timestamp 列的结果）。
func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// completeResetOrder 对齐 PaymentService::completeResetOrder：
// 置 paid → 恢复通知 → 加总流量 + 清零月用量 → 下发节点 + 重置流量计数 + 打开连接。
func (s *PaymentService) completeResetOrder(ctx context.Context, order *Order, tradeNo sql.NullString) error {
	if order.Status == "paid" {
		return nil
	}

	now := s.now().UTC().Truncate(time.Second)
	if err := s.store.MarkOrderPaid(ctx, order.ID, tradeNo, now); err != nil {
		return err
	}
	order.Status = "paid"

	user, err := s.store.UserByIDPayment(ctx, order.UserID)
	if err != nil || user == nil {
		s.logf("重置订单完成: 用户不存在 order_no=%s err=%v", order.OrderNo, err)
		return err
	}
	if !order.PlanID.Valid {
		return nil
	}
	plan, perr := s.store.PlanForPayment(ctx, uint64(order.PlanID.Int64))
	if perr != nil || plan == nil {
		return nil
	}

	// 付费重置流量 → 恢复该用户所有自动通知（旧防重标记全部作废）
	s.restoreReminders(ctx, user)

	addTraffic := plan.MonthlyTraffic.Int64
	if err := s.store.AddResetTraffic(ctx, user.ID, addTraffic); err != nil {
		return err
	}
	user.TrafficLimit += addTraffic
	user.MonthlyTrafficUsed = 0

	if s.panel != nil {
		if err := s.panel.ProvisionClient(ctx, user); err != nil {
			s.logf("重置订单 provisionClient 失败 user=%d: %v", user.ID, err)
		}
		if err := s.panel.ResetClientTraffic(ctx, user); err != nil {
			s.logf("重置订单 resetClientTraffic 失败 user=%d: %v", user.ID, err)
		}
		if err := s.panel.ToggleClient(ctx, user, true); err != nil {
			s.logf("重置订单 toggleClient 失败 user=%d: %v", user.ID, err)
		}
	}
	return nil
}

// restoreReminders 对齐 MailNotifyService::restoreReminders：
// 清掉该用户所有场景的「已发过」标记（邮件通知去重键）。
//
// 键格式见 MailNotifyService：`mail_notify:{scene}:{who}`，who = email:xxx / admin:{id}。
func (s *PaymentService) restoreReminders(ctx context.Context, user *User) {
	if s.cache == nil {
		return
	}
	email := ""
	if user.Email.Valid {
		email = strings.ToLower(phpTrim(user.Email.String))
	}
	for _, scene := range mailNotifyScenes {
		if err := s.cache.Forget(ctx, "mail_notify:"+scene+":email:"+email); err != nil {
			s.logf("恢复通知标记失败 key=%s: %v", scene, err)
		}
		if err := s.cache.Forget(ctx, "mail_notify:"+scene+":admin:"+itoa(user.ID)); err != nil {
			s.logf("恢复通知标记失败 key=%s/admin: %v", scene, err)
		}
	}
}

// mailNotifyScenes 对齐 MailNotifyService::SCENES 的键（顺序无关）。
var mailNotifyScenes = []string{"traffic_exhausted", "traffic_almost", "expiring", "expired"}

// rewardInviter 对齐 DiscountCodeService::rewardInviter：
// 订单用了 invite 码才发奖；同一订单只发一次（source_order_id 幂等）。
func (s *PaymentService) rewardInviter(ctx context.Context, order *Order) error {
	if !order.DiscountCodeID.Valid {
		return nil
	}
	code, err := s.store.DiscountCodeByID(ctx, order.DiscountCodeID.Int64)
	if err != nil || code == nil || code.Source != "invite" {
		return nil
	}
	if !code.UserID.Valid {
		return nil
	}

	exists, err := s.store.RedeemCodeExistsForOrder(ctx, order.ID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	discount, err := s.redeemDiscount(ctx)
	if err != nil {
		return err
	}
	newCode, err := s.generateDiscountCode(ctx, "GIF")
	if err != nil {
		return err
	}
	return s.store.CreateRedeemCode(ctx, CreateRedeemCodeInput{
		Code:           newCode,
		UserID:         uint64(code.UserID.Int64),
		Discount:       discount,
		MaxUses:        1,
		MaxUsesPerUser: 1,
		SourceOrderID:  order.ID,
	})
}

// ---------------------------------------------------------------------------
// 查单
// ---------------------------------------------------------------------------

// QueryOrderResult 是 queryOrder 的返回（对齐 PHP 的 ['status','trade_state']）。
type QueryOrderResult struct {
	Status     string
	TradeState string
}

// QueryOrder 对齐 PaymentService::queryOrder：查单成功且已支付 → 立即完成订单。
func (s *PaymentService) QueryOrder(ctx context.Context, order *Order) *QueryOrderResult {
	payCfg, err := s.configForOrder(ctx, order)
	if err != nil || payCfg == nil {
		return nil
	}

	driver := s.attachHTTP(s.driverFor(payCfg))
	if !driver.SupportsQuery() {
		return nil
	}
	res := driver.Query(ctx, payment.Order{OrderNo: order.OrderNo, Amount: order.Amount}, payCfg.toDriverConfig())
	if res == nil {
		return nil
	}

	if res.Status == "paid" && order.Status != "paid" {
		if cerr := s.CompleteOrder(ctx, order, nullStrMaybe(res.TradeNo)); cerr != nil {
			s.logf("查单后完成订单失败 order_no=%s: %v", order.OrderNo, cerr)
		}
	}
	return &QueryOrderResult{Status: res.Status, TradeState: res.TradeState}
}

// ---------------------------------------------------------------------------
// 重置流量订单
// ---------------------------------------------------------------------------

// CreateResetOrderResult 是 createResetOrder 的返回。
type CreateResetOrderResult struct {
	OrderNo string
	PayURL  string
	Status  string
}

// CreateResetOrder 对齐 PaymentService::createResetOrder：价格 = 套餐重置价，重置量 = 套餐月流量。
func (s *PaymentService) CreateResetOrder(ctx context.Context, user *User, paymentConfigID uint64, payIP string) (*CreateResetOrderResult, error) {
	if !user.PlanID.Valid {
		return nil, NewPaymentError("仅周期套餐可购买重置流量")
	}
	plan, err := s.store.PlanForPayment(ctx, uint64(user.PlanID.Int64))
	if err != nil || plan == nil || !plan.IsPeriod() {
		return nil, NewPaymentError("仅周期套餐可购买重置流量")
	}

	payCfg, err := s.pickPaymentConfig(ctx, paymentConfigID)
	if err != nil {
		return nil, err
	}
	if payCfg == nil {
		return nil, NewPaymentError("暂无可用支付方式")
	}

	resetPrice := plan.ResetPriceMoney()
	order, err := s.store.CreateOrder(ctx, NewOrderInput{
		OrderNo:         "RST" + s.generateOrderNo(),
		UserID:          user.ID,
		PlanID:          plan.ID,
		Amount:          resetPrice.String(),
		Status:          "pending",
		PaymentConfigID: payCfg.ID,
		PayIP:           nullStr(payIP),
		Now:             s.now().UTC().Truncate(time.Second),
	})
	if err != nil {
		return nil, err
	}

	// 免费直接完成
	if resetPrice.cents <= 0 {
		if err := s.completeResetOrder(ctx, order, sql.NullString{}); err != nil {
			s.logf("免费重置订单完成失败 order_no=%s: %v", order.OrderNo, err)
		}
		return &CreateResetOrderResult{OrderNo: order.OrderNo, Status: "paid"}, nil
	}

	payURL, failMsg, _ := s.buildPayURL(ctx, payCfg, order)

	// 重置订单号带 RST 前缀且每次新生成，正常不会判重；万一网关仍拒绝，
	// 作废后换新单号重下一次，避免把一笔永远拿不到链接的单丢给用户。
	if payURL == "" && s.isOrderNoUnusable(failMsg) {
		s.logf("重置订单被网关判重，作废后换新单号重下 old_order_no=%s gateway_msg=%s", order.OrderNo, failMsg)
		if err := s.store.UpdateOrderStatus(ctx, order.ID, "expired"); err != nil {
			s.logf("作废重置旧单失败 order_no=%s: %v", order.OrderNo, err)
		}
		newOrder, cerr := s.store.CreateOrder(ctx, NewOrderInput{
			OrderNo:         "RST" + s.generateOrderNo(),
			UserID:          user.ID,
			PlanID:          plan.ID,
			Amount:          resetPrice.String(),
			Status:          "pending",
			PaymentConfigID: payCfg.ID,
			PayIP:           nullStr(payIP),
			Now:             s.now().UTC().Truncate(time.Second),
		})
		if cerr != nil {
			return nil, cerr
		}
		order = newOrder
		payURL, _, _ = s.buildPayURL(ctx, payCfg, order)
	}

	return &CreateResetOrderResult{OrderNo: order.OrderNo, PayURL: payURL, Status: "pending"}, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func (s *PaymentService) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

func nullStr(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

// nullStrMaybe 把「可能为空」的字符串转成 null（PHP 的 null vs "" 在库里都是 NULL）。
func nullStrMaybe(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func nullStrOrZero(v sql.NullString) sql.NullString { return v }

func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func nullInt64OrZero(v sql.NullInt64) sql.NullInt64 { return v }
