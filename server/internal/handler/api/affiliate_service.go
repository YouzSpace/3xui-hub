package api

// 本文件是邀请返佣的业务逻辑（判据 + 编排；SQL 事务在 affiliate_sqlstore.go）。
//
// 触发点只有两处，都由 PaymentService 调用（返佣是「钱已到账」之后的副作用）：
//   - 套餐消费：payment_service.go 的 afterPaid（第三方回调 / 余额支付共用）
//   - 余额充值：payment_balance.go 的 completeRechargeOrder（CreditRecharge 置 paid 之后）
//
// 任何失败都只记日志、不向上抛：钱已经收了，不能因为返佣把回调打回 FAIL 让网关无限重推。

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// 返佣比例配置（site_configs key）与缺省值。
const (
	affiliateRebateRateKey = "affiliate_rebate_rate"
	// defaultRebateRate 是「读不到 / 值非法」时的兜底（需求 1：默认 0.10 = 10%）。
	defaultRebateRate = 0.10
)

// AffiliateService 是返佣域的业务入口。
type AffiliateService struct {
	store       AffiliateStore
	siteConfigs SiteConfigReader
	now         func() time.Time
	logger      loggerLike
}

// NewAffiliateService 组装返佣服务；now 为空用 time.Now，logger 为空则不打日志。
func NewAffiliateService(store AffiliateStore, cfg SiteConfigReader, now func() time.Time, logger loggerLike) *AffiliateService {
	if now == nil {
		now = time.Now
	}
	return &AffiliateService{store: store, siteConfigs: cfg, now: now, logger: logger}
}

// affiliateStoreOf 从 PaymentStore 取出返佣能力。
//
// 生产实现 *PaymentSQLStore 同时实现 PaymentStore 与 AffiliateStore（同一张 *sql.DB），
// 因此不必改 NewPaymentService 的签名；支付域的内存假实现取不到就留 nil（返佣整体跳过）。
func affiliateStoreOf(store PaymentStore) (AffiliateStore, bool) {
	if store == nil {
		return nil, false
	}
	as, ok := store.(AffiliateStore)
	return as, ok
}

// Rate 读返佣比例（0~1）。
//
// 取值规则（需求 1）：配置缺失/空串 → 默认 0.10；非数字、负数、> 1 视为配错 → 记日志回默认 0.10；
// 显式 0 合法，表示「暂停返佣」（此时返佣金额算出来是 0，直接不发）。
func (s *AffiliateService) Rate(ctx context.Context) float64 {
	rate := defaultRebateRate
	if s == nil || s.siteConfigs == nil {
		return rate
	}
	values, err := s.siteConfigs.SiteConfigMany(ctx, []string{affiliateRebateRateKey})
	if err != nil {
		s.logf("读取返佣比例失败，按默认 %.2f 处理: %v", defaultRebateRate, err)
		return rate
	}
	raw := strings.TrimSpace(values[affiliateRebateRateKey])
	if raw == "" {
		return rate
	}
	v, perr := strconv.ParseFloat(raw, 64)
	if perr != nil || v != v || v < 0 || v > 1 {
		s.logf("返佣比例配置非法（%q），按默认 %.2f 处理", raw, defaultRebateRate)
		return rate
	}
	return v
}

// Grant 给该订单的邀请人发返佣（无邀请关系 / 金额为 0 / 已返过都直接跳过，不报错）。
//
// 主键幂等在 store（affiliate_rebates.order_id 唯一键），这里只负责「算该不该发、发多少」。
func (s *AffiliateService) Grant(ctx context.Context, order *Order) error {
	if s == nil || s.store == nil || order == nil {
		return nil
	}
	cents := mustMoney(order.Amount).cents
	if cents <= 0 {
		return nil
	}
	inviter, err := s.store.InviterOf(ctx, order.UserID)
	if err != nil {
		return err
	}
	// 没有邀请关系，或自己邀自己（正常下单路径已拦，这里再兜一层）。
	if inviter == 0 || inviter == order.UserID {
		return nil
	}
	rate := s.Rate(ctx)
	amount := rebateCents(cents, rate)
	if amount <= 0 {
		return nil
	}

	granted, err := s.store.GrantRebate(ctx, GrantRebateInput{
		InviterID:   inviter,
		InviteeID:   order.UserID,
		OrderID:     order.ID,
		OrderNo:     order.OrderNo,
		AmountCents: amount,
		Rate:        formatRebateRate(rate),
		Now:         s.now().UTC().Truncate(time.Second),
	})
	if err != nil {
		return err
	}
	if granted {
		s.logf("邀请返佣已发放 order_no=%s inviter=%d invitee=%d amount=%d分 rate=%s",
			order.OrderNo, inviter, order.UserID, amount, formatRebateRate(rate))
	}
	return nil
}

// Summary 返回「我的邀请」数据（比例 + 累计返佣 + 被邀请人列表）。
func (s *AffiliateService) Summary(ctx context.Context, userID uint64) (*AffiliateSummary, error) {
	if s == nil || s.store == nil {
		return nil, errAffiliateUnavailable
	}
	total, invitees, err := s.store.RebateSummary(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &AffiliateSummary{
		Rate:        s.Rate(ctx),
		TotalRebate: total,
		Invitees:    invitees,
	}, nil
}

func (s *AffiliateService) logf(format string, args ...any) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Printf(format, args...)
}

// rebateCents = round(订单实付分 × 比例)，四舍五入到整数分（需求 2：> 0 才发）。
//
// 走 roundPHP（与 PHP round() 逐条一致的十进制进位），不用 math.Round：
// 9850 × 0.1 在二进制里是 985.0000000000001，直接截断会少一分。
func rebateCents(orderCents int64, rate float64) int64 {
	if orderCents <= 0 || rate <= 0 {
		return 0
	}
	return int64(roundPHP(float64(orderCents)*rate, 0))
}

// formatRebateRate 把比例写成 decimal(5,4) 文本（"0.1000"），落库留痕。
func formatRebateRate(rate float64) string {
	return strconv.FormatFloat(roundPHP(rate, 4), 'f', 4, 64)
}
