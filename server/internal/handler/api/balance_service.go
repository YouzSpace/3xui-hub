package api

// 本文件是余额域的业务逻辑（对齐 docs/balance/implementation-plan.md §3.1）。
//
// 分工：本文件只做「判据 + 编排」，事务与 SQL 全在 BalanceStore（balance_sqlstore.go）。
// 只依赖 BalanceStore 接口，因此单测用内存实现就能覆盖余额校验（见 balance_service_test.go）。

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// BalanceService 是余额域的业务入口。
type BalanceService struct {
	store  BalanceStore
	now    func() time.Time
	logger loggerLike
}

// NewBalanceService 组装余额服务；now 为空时用 time.Now。
func NewBalanceService(store BalanceStore, now func() time.Time, logger loggerLike) *BalanceService {
	if now == nil {
		now = time.Now
	}
	return &BalanceService{store: store, now: now, logger: logger}
}

// balanceStoreOf 从 PaymentStore 取出余额能力。
//
// 生产实现 *PaymentSQLStore 同时实现 PaymentStore 与 BalanceStore（同一张 *sql.DB），
// 所以支付服务与用户端补充端点都能直接复用已有依赖，不必改构造函数签名。
func balanceStoreOf(store PaymentStore) (BalanceStore, bool) {
	if store == nil {
		return nil, false
	}
	bs, ok := store.(BalanceStore)
	return bs, ok
}

// Current 返回当前余额（分）与最近流水（分页）。
func (s *BalanceService) Current(ctx context.Context, userID uint64, limit, offset int) (int64, []BalanceLog, error) {
	balance, err := s.store.UserBalance(ctx, userID)
	if err != nil {
		return 0, nil, err
	}
	logs, err := s.store.BalanceLogs(ctx, userID, limit, offset)
	if err != nil {
		return 0, nil, err
	}
	return balance, logs, nil
}

// Redeem 兑换码兑换（规划 §3.1.2）。
//
// 码先归一化（去空白 + 转大写）再查库：用户从聊天/邮件里复制过来的码常带空格和小写。
// 限流在 handler（同一个 IP 连续失败会锁定），这里只管码本身。
func (s *BalanceService) Redeem(ctx context.Context, userID uint64, rawCode string) (*RedeemGiftcardResult, error) {
	code := normalizeGiftcardCode(rawCode)
	if code == "" {
		return nil, ErrGiftcardNotFound
	}
	return s.store.RedeemGiftcard(ctx, RedeemGiftcardInput{
		Code:   code,
		UserID: userID,
		Now:    s.now().UTC().Truncate(time.Second),
	})
}

// DeductForOrder 用余额支付一笔订单：校验余额 → 扣款 + 写流水 + 建 paid 订单（事务在 store 内）。
//
// 这里先读一次余额做**预检**（余额不够就不必开事务，也不会有「余额不足」的写放大），
// 真正的并发兜底在事务里：锁住用户行后重算一次，并用 `WHERE balance >= ?` 的条件更新
// 保证不会扣成负数（见 balance_sqlstore.go 的 PayOrderWithBalance）。
func (s *BalanceService) DeductForOrder(ctx context.Context, in BalancePayInput) (*Order, error) {
	if balance, err := s.store.UserBalance(ctx, in.UserID); err != nil {
		return nil, err
	} else if cerr := checkBalance(balance, in.AmountCents); cerr != nil {
		return nil, cerr
	}
	return s.store.PayOrderWithBalance(ctx, in)
}

// CreditRecharge 充值订单到账：把订单金额加到余额上（幂等，重复回调不会重复加钱）。
func (s *BalanceService) CreditRecharge(ctx context.Context, order *Order) (bool, int64, error) {
	cents := mustMoney(order.Amount).cents
	return s.store.CreditRechargeOrder(ctx, CreditRechargeInput{
		OrderID:     order.ID,
		UserID:      order.UserID,
		AmountCents: cents,
		Note:        "余额充值",
		Now:         s.now().UTC().Truncate(time.Second),
	})
}

// checkBalance 是「余额够不够扣」的唯一判据。
//
// 金额为 0 的订单（免费套餐走余额支付）不扣款，直接放行。
// 注意：SQL 侧还有一道 `WHERE balance >= ?` 的条件更新兜底，两处都改了才算绕过校验。
func checkBalance(balanceCents, amountCents int64) error {
	if amountCents > 0 && balanceCents < amountCents {
		return ErrBalanceInsufficient
	}
	return nil
}

// normalizeGiftcardCode 归一化兑换码：去所有空白字符 + 转大写（连字符保留）。
func normalizeGiftcardCode(raw string) string {
	trimmed := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, phpTrim(raw))
	return phpStrToUpper(trimmed)
}
