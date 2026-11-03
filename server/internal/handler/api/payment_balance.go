package api

// 本文件是余额域的「钱」相关流程（对齐 docs/balance/implementation-plan.md §3.1.3 / §3.1.4）：
//
//	POST /api/payment/create {"plan_id":1,"payment":"balance"}          余额支付套餐
//	POST /api/payment/create {"order_type":"balance","amount":5000}      充值下单（第三方支付）
//
// 余额支付 = 扣余额 + 直接置 paid + 复用第三方支付成功后的同一套副作用
// （折扣码占用 / 邀请奖励 / applyPlan / 下发节点），所以这里只负责「算钱 + 扣钱」，
// 副作用全在 payment_service.go 的 afterPaid。
//
// 充值到账（CompleteOrder 的 order_type=balance 分支）走 BalanceService.CreditRecharge，
// 幂等闸门是订单 status（见 balance_sqlstore.go 的 CreditRechargeOrder）。

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// 充值金额区间（分）：1 元 ~ 10 万元。
//
// 下限防「0.01 元充值」被网关拒单（多数网关最小 1 元），上限防订单金额超出
// orders.amount decimal(10,2) 的表示范围。改这里等于改对用户的承诺，先确认。
const (
	rechargeMinCents int64 = 100
	rechargeMaxCents int64 = 10_000_000
)

// CreateRechargeOrder 创建余额充值订单并出支付链接（规划 §3.1.4 第 1–2 步）：
// 订单 order_type=balance、amount=充值金额，支付配置与出链接复用现有第三方支付框架。
// 钱的到账在回调里（CompleteOrder → CreditRecharge）。
func (s *PaymentService) CreateRechargeOrder(ctx context.Context, user *User, amountCents int64, paymentConfigID uint64, payIP string) (*CreateOrderResult, error) {
	if amountCents < rechargeMinCents || amountCents > rechargeMaxCents {
		return nil, NewPaymentError("充值金额需在 %d 元到 %d 元之间", rechargeMinCents/100, rechargeMaxCents/100)
	}

	payCfg, err := s.pickPaymentConfig(ctx, paymentConfigID)
	if err != nil {
		return nil, err
	}
	if payCfg == nil {
		return nil, NewPaymentError("暂无可用支付方式")
	}

	amount := money{cents: amountCents}
	order, err := s.store.CreateOrder(ctx, NewOrderInput{
		OrderNo:         s.generateOrderNo(),
		UserID:          user.ID,
		OrderType:       "balance",
		Amount:          amount.String(),
		OriginalAmount:  nullStr(amount.String()),
		Status:          "pending",
		PaymentConfigID: payCfg.ID,
		PayIP:           nullStr(payIP),
		Now:             s.now().UTC().Truncate(time.Second),
	})
	if err != nil {
		return nil, err
	}

	payURL, _, _ := s.buildPayURL(ctx, payCfg, order)
	result := &CreateOrderResult{
		OrderNo: order.OrderNo,
		Amount:  amount,
		Status:  "pending",
		PayURL:  payURL,
	}
	if payURL == "" {
		// 与套餐订单同口径：pending 但没有链接 → 「获取支付链接失败，请稍后再试」。
		// 充值单号每次新生成，不做「网关判重后换单重下」那套（概率极低，逻辑越短越好）。
		return result, errPayURLMissing
	}
	return result, nil
}

// BalancePayResult 是余额支付的结果（与第三方下单的返回同形，status 直接是 paid）。
type BalancePayResult struct {
	OrderNo string
	Amount  money
	Status  string
}

// PayPlanWithBalance 用余额支付一个套餐（规划 §3.1.3）：
//  1. 校验套餐（存在 + 已上架），算折后金额（折扣码复用 validateAndApply + 同一套限流）
//  2. 扣款 + 建 paid 订单（事务、锁用户行、余额不足即回滚）—— BalanceService.DeductForOrder
//  3. 开通套餐（afterPaid，与第三方支付成功后的路径完全一致）
//
// 扣款成功后开通失败只记日志：钱已经扣了，不能把这笔支付判失败让用户重复付
// （与 CompleteOrder 对代理订单/开通失败的处理同口径）。
func (s *PaymentService) PayPlanWithBalance(ctx context.Context, user *User, planID uint64, discountCode, payIP string) (*BalancePayResult, error) {
	if s.balance == nil {
		return nil, NewPaymentError("余额支付暂未开放")
	}

	plan, err := s.store.PlanForPayment(ctx, planID)
	if err != nil || plan == nil {
		return nil, NewPaymentError("套餐不存在")
	}
	if !plan.IsActive {
		return nil, NewPaymentError("该套餐已下架")
	}

	original := plan.PriceMoney()
	final := original
	var (
		codeID        sql.NullInt64
		discountCents int64
	)
	if code := phpTrim(discountCode); code != "" && original.cents > 0 {
		// 折扣码限流与下单共用同一套计数（两个入口都能拿码试错）。
		uid := int64(user.ID)
		blocked, berr := s.discountCheckBlocked(ctx, uid, payIP)
		if berr != nil {
			return nil, berr
		}
		if blocked {
			return nil, NewPaymentError("%s", rateTooOftenMsg)
		}
		if rerr := s.recordDiscountRequest(ctx, uid); rerr != nil {
			s.logf("余额支付记录折扣码请求失败 user=%d: %v", uid, rerr)
		}
		applied, aerr := s.validateAndApply(ctx, user, code, plan, original)
		if aerr != nil {
			_ = s.recordDiscountFailure(ctx, uid, payIP)
			return nil, aerr
		}
		final = applied.Final
		codeID = nullInt64(applied.Code.ID)
		discountCents = original.cents - final.cents
		if cerr := s.clearDiscountFailures(ctx, uid); cerr != nil {
			s.logf("余额支付清理折扣码失败计数失败 user=%d: %v", uid, cerr)
		}
	}

	order, err := s.balance.DeductForOrder(ctx, BalancePayInput{
		OrderNo:        s.generateOrderNo(),
		UserID:         user.ID,
		PlanID:         plan.ID,
		AmountCents:    final.cents,
		OriginalCents:  original.cents,
		DiscountCodeID: codeID,
		DiscountCents:  discountCents,
		Note:           "余额支付-" + plan.Name,
		PayIP:          payIP,
		Now:            s.now().UTC().Truncate(time.Second),
	})
	if err != nil {
		if errors.Is(err, ErrBalanceInsufficient) {
			return nil, NewPaymentError("余额不足")
		}
		return nil, err
	}

	if aerr := s.afterPaid(ctx, order); aerr != nil {
		s.logf("余额支付开通套餐失败 order_no=%s: %v", order.OrderNo, aerr)
	}
	return &BalancePayResult{OrderNo: order.OrderNo, Amount: money{cents: final.cents}, Status: order.Status}, nil
}

// completeRechargeOrder 处理 order_type=balance 的充值订单到账（规划 §3.1.4 第 3–4 步）：
// 置 paid + 加余额 + 写流水在一个事务里完成，status==paid 时直接跳过（回调重推不会重复加钱）。
func (s *PaymentService) completeRechargeOrder(ctx context.Context, order *Order) error {
	if s.balance == nil {
		s.logf("余额服务未接入，充值订单无法到账 order_no=%s", order.OrderNo)
		return nil
	}
	credited, _, err := s.balance.CreditRecharge(ctx, order)
	if err != nil {
		return err
	}
	if !credited {
		return nil
	}
	order.Status = "paid"
	order.PaidAt = sql.NullTime{Time: s.now().UTC().Truncate(time.Second), Valid: true}
	s.notifyPaidTG(ctx, order)
	return nil
}
