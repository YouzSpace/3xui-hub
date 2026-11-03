package api

import (
	"context"
	"database/sql"
	"time"

	"controlhub-server/internal/driver/payment"
	"controlhub-server/internal/tgbot"
)

// ---------------------------------------------------------------------------
// 支付域的数据模型与接缝。
//
// 与 handler/api 既有文件的分工：类型/接口放这里，SQL 实现在 payment_sqlstore.go，
// 业务流程（对齐 PHP PaymentService）在 payment_service.go，HTTP 端点与路由在 payment.go。
// 这样 P7 新增代码全部落在这四个文件里，不动 P2 的既有文件（routes_test 里的路由清单
// 断言也不会被打扰 —— 支付路由由 RegisterPayment 单独挂载）。
// ---------------------------------------------------------------------------

// PaymentConfig 是 payment_configs 一行。
//
// DriverConfig 是 driver_config JSON（新格式）；Gateway/QueryGateway/MemberID/APIKey/
// BankCode 是旧列（老数据，driver_config 为空时零影响）。取值优先级见 payment.Config.Cfg。
type PaymentConfig struct {
	ID           uint64
	Name         string
	DriverType   string
	DriverConfig map[string]string
	Gateway      string
	QueryGateway string
	MemberID     string
	APIKey       string
	BankCode     string
	NotifyURL    string
	Enabled      bool
}

// toDriverConfig 转成驱动层配置（driver_config 优先、旧列回退的规则在驱动包里）。
func (c *PaymentConfig) toDriverConfig() payment.Config {
	return payment.Config{
		ID:           c.ID,
		Name:         c.Name,
		DriverType:   c.DriverType,
		DriverConfig: c.DriverConfig,
		Gateway:      c.Gateway,
		QueryGateway: c.QueryGateway,
		MemberID:     c.MemberID,
		APIKey:       c.APIKey,
		BankCode:     c.BankCode,
		NotifyURL:    c.NotifyURL,
	}
}

// PlanRow 是 plans 表一行（支付域口径）。
//
// 与 api.Plan 的区别：金额是 decimal(10,2) 的**字符串**（Price/ResetPrice），
// 支付链路全程不出现 float64 —— 见 contracts/data-contract.md §3。
type PlanRow struct {
	ID             uint64
	Name           string
	Type           string // period | total
	IsActive       bool
	Months         sql.NullInt64
	MonthlyTraffic sql.NullInt64
	PeriodTraffic  sql.NullInt64
	TotalTraffic   sql.NullInt64
	Price          string
	ResetPrice     string
	IncludeLocal   bool
}

// IsPeriod 对齐 Plan::isPeriod()。
func (p *PlanRow) IsPeriod() bool { return p.Type == "period" }

// PriceMoney 是售价（decimal(10,2)）。
func (p *PlanRow) PriceMoney() money { return mustMoney(p.Price) }

// ResetPriceMoney 是重置流量售价（decimal(10,2)，NULL 视为 0）。
func (p *PlanRow) ResetPriceMoney() money { return mustMoney(p.ResetPrice) }

// Order 是 orders 表一行（支付域需要的列）。
//
// 金额一律是 decimal(10,2) 的文本（"98.50"），不做 float 转换 —— 见 payment_money.go。
type Order struct {
	ID              uint64
	OrderNo         string
	UserID          uint64
	OrderType       string // plan | proxy
	PlanID          sql.NullInt64
	ProxySkuID      sql.NullInt64
	Amount          string
	OriginalAmount  sql.NullString
	DiscountCodeID  sql.NullInt64
	DiscountAmount  sql.NullString
	Status          string
	PaymentConfigID sql.NullInt64
	TradeNo         sql.NullString
	PaidAt          sql.NullTime
	PayIP           sql.NullString
	CreatedAt       time.Time
}

// IsProxyOrder 对齐 Order::isProxyOrder()。
func (o *Order) IsProxyOrder() bool { return o.OrderType == "proxy" }

// IsResetOrder 对齐 `str_starts_with($order->order_no, 'RST')`。
func (o *Order) IsResetOrder() bool { return len(o.OrderNo) >= 3 && o.OrderNo[:3] == "RST" }

// IsBalanceRecharge 判定余额充值订单（order_type=balance）：支付成功后钱进余额，不开套餐。
func (o *Order) IsBalanceRecharge() bool { return o.OrderType == "balance" }

// DiscountCode 是 discount_codes 一行（下单校验与占用所需列）。
type DiscountCode struct {
	ID             int64
	Code           string
	Source         string // invite | redeem | admin
	UserID         sql.NullInt64
	Discount       string // decimal(3,2) 文本，如 "0.90"
	MaxUses        int64
	UsedCount      int64
	MaxUsesPerUser int64
	ExpiresAt      sql.NullTime
	Status         string
	PlanID         sql.NullInt64
	SourceOrderID  sql.NullInt64
	Note           sql.NullString // 自定义说明（展示给用户，可空）
}

// Usable 对齐 DiscountCode::isUsable()（状态 / 次数 / 有效期）。
func (d *DiscountCode) Usable(now time.Time) bool {
	if d.Status != "active" {
		return false
	}
	if d.UsedCount >= d.MaxUses {
		return false
	}
	if d.ExpiresAt.Valid && d.ExpiresAt.Time.Before(now) {
		return false
	}
	return true
}

// OrderListRow 是「我的订单」列表一行（含 plan / proxy_sku 的展示名）。
type OrderListRow struct {
	Order
	PlanName     sql.NullString
	ProxySkuName sql.NullString
}

// PaymentStore 是支付域对数据库的全部依赖。
//
// 实现见 payment_sqlstore.go（database/sql 手写 SQL）；单测用内存实现，不连库。
type PaymentStore interface {
	// PaymentConfigByID 取**启用**的支付配置；不存在或未启用返回 (nil, nil)（PHP 的 where(enabled,true)->first()）。
	PaymentConfigByID(ctx context.Context, id uint64) (*PaymentConfig, error)
	// PaymentConfigByIDAny 取配置本身，不看 enabled —— 对应 PHP 的 `$order->paymentConfig` 关联
	// （老订单在配置被停用后，回调/查单仍按原配置验签）。
	PaymentConfigByIDAny(ctx context.Context, id uint64) (*PaymentConfig, error)
	// FirstEnabledPaymentConfig 取第一个启用的支付配置（按 id 升序，与 where(...)->first() 一致）。
	FirstEnabledPaymentConfig(ctx context.Context) (*PaymentConfig, error)
	// EnabledPaymentConfigs 取全部启用的支付配置（methods 端点）。
	EnabledPaymentConfigs(ctx context.Context) ([]PaymentConfig, error)

	// PlanForPayment 取套餐（金额读成 decimal(10,2) 文本，避免任何 float 中转）。
	PlanForPayment(ctx context.Context, id uint64) (*PlanRow, error)

	OrderByNo(ctx context.Context, orderNo string) (*Order, error)
	// PendingOrderCreatedSince 取「该用户 + 该套餐」在 since 之后创建的 pending 单（最早一笔）。
	PendingOrderCreatedSince(ctx context.Context, userID, planID uint64, since time.Time) (*Order, error)
	// LatestPendingOrderCreatedBefore 取「该用户 + 该套餐」在 before 之前创建的最后一笔 pending 单。
	LatestPendingOrderCreatedBefore(ctx context.Context, userID, planID uint64, before time.Time) (*Order, error)
	// LatestPendingOrderWithCode 取「该用户 + 该套餐 + 该折扣码」最后一笔 pending 单
	// （对齐 DiscountCodeService::validateAndApply 的复用查询：不带时间条件）。
	LatestPendingOrderWithCode(ctx context.Context, userID, planID uint64, codeID int64) (*Order, error)
	// OrderByNoForUser 取属于该用户、指定状态的订单（status 为空表示不限）。
	OrderByNoForUser(ctx context.Context, userID uint64, orderNo, status string) (*Order, error)
	// UserOrders 取该用户最近 N 笔订单（按 id 降序，含展示名）。
	UserOrders(ctx context.Context, userID uint64, limit int) ([]OrderListRow, error)

	CreateOrder(ctx context.Context, in NewOrderInput) (*Order, error)
	UpdateOrderStatus(ctx context.Context, id uint64, status string) error
	// MarkOrderPaid 置 paid + trade_no + paid_at。
	MarkOrderPaid(ctx context.Context, id uint64, tradeNo sql.NullString, paidAt time.Time) error
	// AddResetTraffic 加总流量 + 清零月用量（completeResetOrder 的用户侧变更）。
	AddResetTraffic(ctx context.Context, userID uint64, addTraffic int64) error

	DiscountCodeByCode(ctx context.Context, code string) (*DiscountCode, error)
	DiscountCodeByID(ctx context.Context, id int64) (*DiscountCode, error)
	// CountPaidOrdersWithCode 数「该用户 + 该码」已支付的订单数（excludeOrderID>0 时排除它）。
	CountPaidOrdersWithCode(ctx context.Context, codeID, userID int64, excludeOrderID uint64) (int64, error)
	// ExpireDiscountCode 把过期码落库（PHP 里顺手 save 的那一步，幂等）。
	ExpireDiscountCode(ctx context.Context, id int64) error
	// ConsumeDiscountCode 原子占用一次（对齐 DiscountCodeService::consume）：
	// 加锁重读 → 状态/总次数校验 → **每用户次数复核**（排除当前订单）→ used_count+1（满额置 used_up）。
	// 任一项不过返回 ErrDiscountExhausted（调用方只记日志，不把回调打回 FAIL）。
	ConsumeDiscountCode(ctx context.Context, id int64, userID int64, excludeOrderID uint64) (*DiscountCode, error)
	// LockUserInviteCode 把邀请码锁到用户身上（used_invite_code_id + invite_code_used_at）。
	LockUserInviteCode(ctx context.Context, userID uint64, codeID int64, at time.Time) error
	// RedeemCodeExistsForOrder 判断该订单的邀请奖励是否已发（幂等）。
	RedeemCodeExistsForOrder(ctx context.Context, orderID uint64) (bool, error)
	// CreateRedeemCode 给邀请人发一张兑换码。
	CreateRedeemCode(ctx context.Context, in CreateRedeemCodeInput) error

	UserByIDPayment(ctx context.Context, id uint64) (*User, error)
	// UserInviteCodeLocked 判断用户是否已锁过邀请码（users.used_invite_code_id 非空）。
	UserInviteCodeLocked(ctx context.Context, userID uint64) (bool, error)
	// ApplyPlanFields 写 applyPlan 的字段变更（流量限额/到期时间/月限额），返回更新后的用户。
	ApplyPlanFields(ctx context.Context, userID uint64, f PlanFields) error
	// SetUserEnabled 写 users.enabled。
	SetUserEnabled(ctx context.Context, userID uint64, enabled bool) error
	// PrimaryDomain 取主域名（domains.is_primary=1 且 enabled=1）；没有时 ok=false。
	// 缺省回调地址 = https://{主域名}/api/payment/notify（对齐 AbstractPaymentDriver::defaultNotifyUrl）。
	PrimaryDomain(ctx context.Context) (string, bool, error)
}

// PlanFields 是 applyPlan 对 users 行的写入（对齐 UserAdminService::applyPlan）。
type PlanFields struct {
	PlanID              sql.NullInt64
	TrafficLimit        int64
	TrafficUsed         int64
	MonthlyTrafficUsed  int64
	MonthlyTrafficLimit int64
	NextTrafficResetAt  sql.NullTime
	ExpiredAt           sql.NullTime
	// HasNextReset 决定 next_traffic_reset_at 是「写入新值」还是「置 NULL」
	// （total 套餐没有月重置，PHP 那边 applyPlan 根本不写这一列，这里显式区分）。
	HasNextReset bool
	// HasExpired 同上：total 套餐 expired_at = null。
	HasExpired bool
}

// NewOrderInput 对齐 Order::create([...])（支付路径用到的列）。
//
// OrderType 取 plan（默认，空串同义）/ proxy / balance（余额充值）；PlanID 为 0 表示无套餐
// （充值单），入库写 NULL —— orders.plan_id 有外键，写 0 会直接违反约束。
type NewOrderInput struct {
	OrderNo         string
	UserID          uint64
	OrderType       string
	PlanID          uint64
	Amount          string
	OriginalAmount  sql.NullString
	DiscountCodeID  sql.NullInt64
	DiscountAmount  sql.NullString
	Status          string
	PaymentConfigID uint64
	PayIP           sql.NullString
	// Now 是 created_at/updated_at 的字面值（UTC，与 Laravel now() 一致）。
	Now time.Time
}

// CreateRedeemCodeInput 是 rewardInviter 里 DiscountCode::create 的字段集。
type CreateRedeemCodeInput struct {
	Code           string
	UserID         uint64
	Discount       string
	MaxUses        int64
	MaxUsesPerUser int64
	SourceOrderID  uint64
}

// PaymentCache 是恢复邮件通知标记所需的缓存写口（Cache.Forget）。
// Laravel 的 MailNotifyService::restoreReminders 走 database store，与 Cache 接口同源。
type PaymentCache interface {
	Forget(ctx context.Context, key string) error
}

// PanelSyncer 是「把用户最新配置推给所有节点」的接缝。
//
// 真实实现依赖驱动层（3x-ui / xray），且**允许异步**：生产实现把下发丢给
// `user_delivery` 后台任务，支付回调不必等节点（一轮下发 2–3 秒，回调有自己的超时）。
// 失败只记日志（与 PHP 里 report() 后继续的语义一致）。
type PanelSyncer interface {
	// DeliverUser 把该用户的最新状态下发到各节点：清流量计数 → 建/更新 client → 开 client
	// （PHP 的 resetClientOnNodes + provisionClient + BanService::toggleClient）。
	// 调用方不等待节点，只需要「派发成功 / 派发失败」这个结果。
	DeliverUser(ctx context.Context, user *User) error
}

// ProxyOrderCompleter 是代理订单支付成功后的开通接缝（PHP ProxyStoreService::completeOrder）。
//
// 代理商城属于 P8d；本阶段为 nil 时只记日志并把订单置 paid —— 与 PHP 里「服务缺失只
// 影响开通、不影响收款」的容错口径一致（见交付汇报的风险项）。
type ProxyOrderCompleter interface {
	CompleteOrder(ctx context.Context, order *Order) error
}

// PaymentServiceOptions 是 PaymentService 的可调项（测试注入时钟、随机源、副作用接缝）。
type PaymentServiceOptions struct {
	// Now 为空时用 time.Now（**必须**是 UTC：Laravel app.timezone=UTC）。
	Now func() time.Time
	// RandInt 生成订单号后 6 位（0..n-1）；为空时用 crypto/rand。
	RandInt func(n int64) int64
	// Registry 为空时用内置协议（payindex + epay_v1）。
	Registry *payment.Registry
	// HTTP 是驱动发请求用的 client（测试注入 httptest 的 client）。
	HTTP payment.HTTPDoer
	// Panel 为空时常驻副作用全部跳过（只记日志）。真实实现依赖 P5。
	Panel PanelSyncer
	// ProxyOrders 为空时代理订单只置 paid 不开通。
	ProxyOrders ProxyOrderCompleter
	// Cache 用于恢复邮件通知标记（restoreReminders）；为空时跳过。
	Cache PaymentCache
	// SiteConfigs 读 site_configs（invite_discount / redeem_discount / invite_enabled 等）。
	// 为空时不能使用折扣码（下单带码直接报「优惠码无效或不可用」，与查不到码同口径）。
	SiteConfigs SiteConfigReader
	// ReturnURL 返回 pay_callbackurl（PHP: request()->getSchemeAndHttpHost() . '/'）。
	ReturnURL func(ctx context.Context) string
	// DefaultNotifyURL 返回缺省异步通知地址（PHP: domains 主域名 + /api/payment/notify）。
	DefaultNotifyURL func(ctx context.Context) (string, error)
	// RateGuard 是折扣码限流（与 /api/discount/check 共用计数）；为空时不限流。
	RateGuard DiscountRateGuard
	// TGSender Telegram 通知（支付到账通知；nil 则跳过）。
	TGSender *tgbot.Sender
}

// DiscountRateGuard 是 RateGuardService 里折扣码那一段的接缝（限流键与 PHP 逐字一致）。
type DiscountRateGuard interface {
	// CheckBlocked 判定是否应拒绝（返回 nil 表示放行）。
	CheckBlocked(ctx context.Context, userID int64, ip string) (blocked bool, err error)
	// RecordRequest 记一次请求（所有请求都计）。
	RecordRequest(ctx context.Context, userID int64) error
	// RecordFailure 记一次失败（用户维度连续/每日 + IP 维度每小时）。
	RecordFailure(ctx context.Context, userID int64, ip string) error
	// ClearFailures 清用户维度的失败计数（不清 IP 维度）。
	ClearFailures(ctx context.Context, userID int64) error
}

// SiteConfigReader 读 site_configs（折扣率 / 邀请开关等）。
// 与既有 Store 的 SiteConfigMany 同签名，便于直接把 Handler 的 store 传进来。
type SiteConfigReader interface {
	SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error)
}
