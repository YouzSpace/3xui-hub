package api

// 本文件是「邀请返佣」域的数据模型与数据访问接缝。
//
// 业务口径（需求见 prompts/affiliate-backend-20261103.txt）：
//   - 用户 A 邀请 B 注册（B 用过 A 的邀请码），B 之后每次「充值到账」或「买套餐消费」，
//     平台按比例给 A 的余额加一笔返佣（平台补贴，**不从 B 扣**）。
//   - 比例取 site_configs.affiliate_rebate_rate，缺省 0.10。
//   - 金额 = 订单实付（分）× 比例，四舍五入到整数分，> 0 才发。
//   - 同一笔订单只返一次（affiliate_rebates.order_id 唯一键，见 affiliate_sqlstore.go）。
//
// 金额一律整数分（bigint），链路里不出现 float64 —— 与余额/支付域同一口径。

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// errAffiliateUnavailable 表示返佣存储未接入（生产不会发生；单测/装配缺件时回 500 而不是崩在 nil 上）。
var errAffiliateUnavailable = errors.New("affiliate: 返佣存储未接入")

// AffiliateStore 是返佣域对数据库的全部依赖（实现见 affiliate_sqlstore.go）。
//
// 刻意与 PaymentStore 分开声明：支付域的单测用内存假实现（嵌入 nil 接口），
// 多一个方法就会在那份假实现上 panic —— 返佣能力由 affiliateStoreOf 用类型断言取出，
// 生产 *PaymentSQLStore 同时实现两者，单测可以只注入本接口。
type AffiliateStore interface {
	// InviterOf 返回该用户的邀请人 id：
	// users.used_invite_code_id → discount_codes.id → 该行 user_id（source='invite' 且非空）。
	// 没有邀请关系（没填过码 / 码已不存在 / 邀请人为空）时返回 0。
	InviterOf(ctx context.Context, userID uint64) (uint64, error)
	// GrantRebate 发一笔返佣，一个事务内完成：写返佣记录 → 邀请人余额加钱 → 写 balance_logs。
	// 幂等：order_id 唯一键冲突（同一笔订单已返过）时返回 granted=false，不加钱也不写流水。
	GrantRebate(ctx context.Context, in GrantRebateInput) (granted bool, err error)
	// RebateSummary 返回累计返佣（分）与「被邀请人 + 每人累计贡献」列表。
	RebateSummary(ctx context.Context, inviterID uint64) (total int64, invitees []AffiliateInvitee, err error)
}

// GrantRebateInput 是一次返佣发放的入参（AmountCents 已按比例算好，单位分）。
//
// Rate 是发放当时比例的 decimal(5,4) 文本（"0.1000"），落库留痕用 ——
// 后台改了比例之后，历史返佣仍能看出当时是多少。
type GrantRebateInput struct {
	InviterID   uint64
	InviteeID   uint64
	OrderID     uint64
	OrderNo     string
	AmountCents int64
	Rate        string
	Now         time.Time
}

// AffiliateInvitee 是被邀请人一行（用户端「我的邀请」列表）。
type AffiliateInvitee struct {
	UserID uint64
	Email  sql.NullString
	// Rebate 是这个人累计贡献的返佣（分）；没消费过就是 0。
	Rebate int64
	// InvitedAt 是被邀请人的注册时间（users.created_at）。
	InvitedAt sql.NullTime
}

// AffiliateSummary 是 GET /api/affiliate 的数据体。
type AffiliateSummary struct {
	// Rate 是当前返佣比例（0~1，如 0.1 = 10%）。
	Rate float64
	// TotalRebate 是累计返佣（分）。
	TotalRebate int64
	// Invitees 是我邀请来的人（每人贡献多少返佣）。
	Invitees []AffiliateInvitee
}
