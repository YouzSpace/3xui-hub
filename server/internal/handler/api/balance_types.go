package api

// 本文件是用户端「账户余额」域的数据模型、数据访问接缝与错误定义。
// 设计与字段口径见 docs/balance/implementation-plan.md 的 §2（表）/§3.1（接口）/§4（安全）。
//
// 三条硬规则（改之前先读）：
//  1. 金额一律**整数分**（bigint），链路里不出现 float64 —— 与支付域同一口径（payment_money.go）。
//  2. 每次余额变动**必须**同时写一条 balance_logs（含变动后余额 balance_after），可对账。
//  3. 余额只会因「扣款校验不过」而被拒：校验判据是 checkBalance（balance_credit.go），
//     SQL 侧另有 `balance >= ?` 的条件更新兜底（见 balance_sqlstore.go）。

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BalanceLog 是 balance_logs 一行（余额流水，用户端只读展示）。
type BalanceLog struct {
	ID         int64
	UserID     int64
	Amount     int64  // 变动额(分)：正=加 / 负=扣
	Type       string // recharge=充值 / consume=消费 / redeem=兑换 / admin=管理员调整 / rebate=邀请返佣
	OrderID    sql.NullInt64
	GiftcardID sql.NullInt64
	OperatorID sql.NullInt64
	// BalanceAfter 是变动后余额(分)，与 users.balance 同口径，用于对账。
	BalanceAfter int64
	Note         sql.NullString
	CreatedAt    sql.NullTime
}

// 兑换与扣款的可辨失败原因。
//
// 「码不存在 / 已使用 / 已过期 / 已作废」必须能分开（规划 §3.1.2 要求四种明确的用户文案），
// 所以走哨兵错误而不是一句话；文案映射在 redeemErrorText（balance_handlers.go）。
var (
	ErrGiftcardNotFound = errors.New("giftcards: 兑换码不存在")
	ErrGiftcardUsed     = errors.New("giftcards: 兑换码已使用")
	ErrGiftcardExpired  = errors.New("giftcards: 兑换码已过期")
	ErrGiftcardDisabled = errors.New("giftcards: 兑换码已作废")
	// ErrBalanceInsufficient 扣款时余额不足。
	ErrBalanceInsufficient = errors.New("balance: 余额不足")
)

// RedeemGiftcardInput 是「兑换码兑换」事务的入参（Code 已归一化：去空白 + 大写）。
type RedeemGiftcardInput struct {
	Code   string
	UserID uint64
	Now    time.Time
}

// RedeemGiftcardResult 是兑换结果：加了多少、加完之后是多少。
type RedeemGiftcardResult struct {
	GiftcardID int64
	Added      int64 // 面值(分)
	Balance    int64 // 兑换后余额(分)
}

// BalancePayInput 是「用余额支付一笔套餐订单」事务的入参（金额单位：分）。
//
// AmountCents 是**折后**实付金额；OriginalCents/DiscountCents 只用于订单留痕。
type BalancePayInput struct {
	OrderNo        string
	UserID         uint64
	PlanID         uint64
	AmountCents    int64
	OriginalCents  int64
	DiscountCodeID sql.NullInt64
	DiscountCents  int64
	Note           string // 流水备注（如「余额支付-月付套餐」）
	PayIP          string
	Now            time.Time
}

// CreditRechargeInput 是「充值订单到账」事务的入参（金额单位：分）。
type CreditRechargeInput struct {
	OrderID     uint64
	UserID      uint64
	AmountCents int64
	Note        string
	Now         time.Time
}

// BalanceStore 是余额域对数据库的全部依赖。
//
// 实现见 balance_sqlstore.go（*PaymentSQLStore 的方法，database/sql 手写 SQL）；
// 单测用内存实现，不连库。三个写方法各自是一个**完整事务**，调用方不需要（也无法）拼半个事务：
//   - RedeemGiftcard：锁码 → 校验 → 置 used → 加余额 → 写流水
//   - PayOrderWithBalance：锁用户 → 复核余额 → 扣款 → 写流水 → 建 paid 订单
//   - CreditRechargeOrder：锁订单 → 幂等判定 → 置 paid → 加余额 → 写流水
type BalanceStore interface {
	// UserBalance 读当前余额（分）；用户不存在返回错误。
	UserBalance(ctx context.Context, userID uint64) (int64, error)
	// BalanceLogs 按 id 倒序取该用户最近 limit 条流水（offset 用于翻页）。
	BalanceLogs(ctx context.Context, userID uint64, limit, offset int) ([]BalanceLog, error)
	// RedeemGiftcard 兑换（单次使用，并发安全）。
	RedeemGiftcard(ctx context.Context, in RedeemGiftcardInput) (*RedeemGiftcardResult, error)
	// PayOrderWithBalance 扣款并直接建一笔 status=paid 的套餐订单（不经第三方网关）。
	PayOrderWithBalance(ctx context.Context, in BalancePayInput) (*Order, error)
	// CreditRechargeOrder 把充值订单置 paid 并把钱加到余额上（幂等：已 paid 不重复加）。
	// 返回 credited=false 表示该订单此前已完成。
	CreditRechargeOrder(ctx context.Context, in CreditRechargeInput) (credited bool, balance int64, err error)
}
