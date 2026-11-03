package admin

// 管理端余额域（P9）：兑换码发放/列表/作废 + 任意用户余额查调。
//
// 逐条对齐 docs/balance/implementation-plan.md 第 2、3.2、4 节（文档在仓库外）：
//
//	POST   /admin-api/giftcards            批量生成余额兑换码
//	GET    /admin-api/giftcards            兑换码列表（status 筛选 + 分页）
//	DELETE /admin-api/giftcards/{giftcard} 作废（只作废未使用的码）
//	GET    /admin-api/users/{user}/balance 余额 + 最近流水
//	POST   /admin-api/users/{user}/balance 人工调账（正=加 / 负=扣，必写流水）
//
// 三条口径，写在这里避免后来者改错：
//  1. 金额一律**整数分**（bigint）。不出现 float64，也不接受「元」。
//  2. 余额任何变动都必须落一条 balance_logs（含 balance_after 供对账）；
//     没有流水的余额变动视为 bug。
//  3. 改余额走事务 + `SELECT ... FOR UPDATE`（与 api/payment_sqlstore.go 的
//     ConsumeDiscountCode 同一防超卖模式），且扣减后不允许为负。
//
// 与用户端的边界：兑换（POST /api/redeem）、余额支付、充值回调都在
// internal/handler/api/，本域不碰；本域只发码、作废、看余额、人工调账。

import (
	"context"
	"errors"
	"log"
	"time"
)

// giftcards.status 的取值（与 schema 注释一致）。
const (
	GiftcardStatusActive   = "active"
	GiftcardStatusUsed     = "used"
	GiftcardStatusDisabled = "disabled"
)

// BalanceTypeAdmin 是「管理员人工调账」的流水类型（balance_logs.type）。
const BalanceTypeAdmin = "admin"

// 本域的业务错误（handler 据此映射 404/400，其余一律按服务器错误处理）。
var (
	// ErrUserNotFound 表示 users 里没有这个 id。
	ErrUserNotFound = errors.New("admin: 用户不存在")
	// ErrBalanceNotEnough 表示调账后余额会变成负数（扣减超过余额）。
	ErrBalanceNotEnough = errors.New("admin: 余额不足")
	// ErrGiftcardCodeTaken 表示生成的码与库里已有码撞车（重新生成即可）。
	ErrGiftcardCodeTaken = errors.New("admin: 兑换码重复")
)

// Giftcard 是 giftcards 一行（余额兑换码，单次使用）。
type Giftcard struct {
	ID        uint64
	Code      string
	Name      string
	Value     int64 // 面值（分）
	Status    string
	UsedBy    *uint64
	UsedAt    *time.Time
	ExpiresAt *time.Time
	CreatedBy *uint64
	Note      *string
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// BalanceLog 是 balance_logs 一行（余额流水，审计用）。
type BalanceLog struct {
	ID           uint64
	UserID       uint64
	Amount       int64 // 变动额（分），正=加 / 负=扣
	Type         string
	OrderID      *uint64
	GiftcardID   *uint64
	OperatorID   *uint64
	BalanceAfter int64
	Note         *string
	CreatedAt    *time.Time
}

// GiftcardFilter 是兑换码列表的筛选条件（空字段 = 不筛）。
type GiftcardFilter struct {
	Status *string
}

// NewGiftcard 是批量生成时单行的落库字段（status 由 SQL 层固定写 active）。
type NewGiftcard struct {
	Code      string
	Name      string
	Value     int64
	ExpiresAt *time.Time
	CreatedBy *uint64
	Note      *string
	CreatedAt time.Time
}

// BalanceAdjust 是一次人工调账请求（金额分，正=加 / 负=扣）。
type BalanceAdjust struct {
	UserID     uint64
	Amount     int64
	OperatorID *uint64
	Note       *string
	Now        time.Time
}

// BalanceAdjustment 是调账结果：新余额 + 本次落库的流水行。
type BalanceAdjustment struct {
	Balance int64
	Log     *BalanceLog
}

// BalanceStore 是余额域对数据库的全部依赖（SQL 实现见 balance_sqlstore.go）。
//
// 刻意不与 P8d 的 Store 合并：各域在自己文件里声明窄接口，互不牵连。
type BalanceStore interface {
	// GiftcardPage 返回当前页兑换码与总数（按 id 倒序）。
	GiftcardPage(ctx context.Context, f GiftcardFilter, page, perPage int64) ([]Giftcard, int64, error)
	// GiftcardByID 取单个兑换码；查无此行时返回的错误满足 NoRows。
	GiftcardByID(ctx context.Context, id uint64) (*Giftcard, error)
	// InsertGiftcards 整批落库（一个事务）；码撞唯一键时返回 ErrGiftcardCodeTaken。
	InsertGiftcards(ctx context.Context, rows []NewGiftcard) error
	// DeleteGiftcard 物理删除兑换码（任意状态）；返回是否真的删到了行。
	DeleteGiftcard(ctx context.Context, id uint64) (bool, error)
	// UserBalance 取用户当前余额；用户不存在返回 ErrUserNotFound。
	UserBalance(ctx context.Context, userID uint64) (int64, error)
	// UserBalanceLogs 返回当前页流水（按 id 倒序）与总数。
	UserBalanceLogs(ctx context.Context, userID uint64, page, perPage int64) ([]BalanceLog, int64, error)
	// AdjustUserBalance 事务内加锁改余额并写流水；扣成负数返回 ErrBalanceNotEnough。
	AdjustUserBalance(ctx context.Context, in BalanceAdjust) (*BalanceAdjustment, error)
}

// BalanceDeps 是余额域的依赖清单。
type BalanceDeps struct {
	Store  BalanceStore
	Now    func() time.Time
	Logger loggerLike
}

// BalanceDomain 是管理端余额域的 HTTP 入口。
type BalanceDomain struct {
	store  BalanceStore
	now    func() time.Time
	logger loggerLike
}

// NewBalanceDomain 组装余额域。
func NewBalanceDomain(d BalanceDeps) *BalanceDomain {
	if d.Store == nil {
		panic("admin: BalanceStore 不能为空")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &BalanceDomain{store: d.Store, now: now, logger: logger}
}
