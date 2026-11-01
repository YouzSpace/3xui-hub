package admin

// 管理端用户域（P8a）的类型定义。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/UserController.php 与
// backend/app/Services/UserAdminService.php；字段名与响应形态见
// contracts/api-contract.md §1 §3、contracts/data-contract.md §3。

import (
	"context"
	"time"
)

// UserRow 是用户域读取的一行（plan 信息来自 LEFT JOIN plans）。
//
// token/uuid 只在详情（full=true）时输出 —— 与 PHP 的 present($u, $full) 一致。
type UserRow struct {
	ID                  uint64
	Email               *string
	Token               string
	UUID                string
	Protocol            string
	PlanID              *uint64
	PlanType            *string
	PlanName            *string
	PlanMonths          *int64
	TrafficLimit        int64
	TrafficUsed         int64
	MonthlyTrafficUsed  int64
	MonthlyTrafficLimit int64
	NextTrafficResetAt  *time.Time
	ExpiredAt           *time.Time
	Enabled             bool
	CreatedAt           *time.Time

	// ProxyActiveCount 只由列表查询填充（对齐 withCount('proxyEntitlements …')）；
	// 详情/写响应在 PHP 里拿不到该属性，恒为 0。
	ProxyActiveCount int64
}

// ClientEmail 对齐 User::clientEmail()：3x-ui client 主键。
func (u *UserRow) ClientEmail() string { return "ch_user_" + itoaUint(u.ID) }

// IsPeriodPlan / IsTotalPlan 对齐 User::isPeriodPlan() / isTotalPlan()（无套餐 → false）。
func (u *UserRow) IsPeriodPlan() bool { return u.PlanType != nil && *u.PlanType == "period" }
func (u *UserRow) IsTotalPlan() bool  { return u.PlanType != nil && *u.PlanType == "total" }

// UserProxy 是代理权益明细（presentProxies 的输入）。
type UserProxy struct {
	ID        uint64
	Kind      string
	SKUName   *string
	Status    string
	ExpiresAt *time.Time
}

// IsActive 对齐 ProxyEntitlement::isActive()。
func (p *UserProxy) IsActive(now time.Time) bool {
	return p.Status == KindStatusActive && (p.ExpiresAt == nil || p.ExpiresAt.After(now))
}

// UserPlan 是套餐换算（applyPlan / create）需要的列。
type UserPlan struct {
	ID             uint64
	Name           string
	Type           string
	Months         *int64
	MonthlyTraffic *int64
	PeriodTraffic  *int64
	TotalTraffic   *int64
}

// UserCreateInput 是建号落库的完整字段（PHP UserAdminService::create 的产物）。
type UserCreateInput struct {
	Email               *string
	Token               string
	UUID                string
	Protocol            string
	PlanID              *uint64
	TrafficLimit        int64
	MonthlyTrafficLimit int64
	NextTrafficResetAt  *time.Time
	ExpiredAt           *time.Time
	Enabled             bool
	Now                 time.Time
}

// UserPatch 是 users 表的部分更新（Set 标志区分「未传」与「显式 null」）。
//
// PHP 侧 update() 用 isset / array_key_exists 区分这两种情况，Go 用显式标志复刻，
// 避免把「没传」误当成「清空」。
type UserPatch struct {
	Email               *string
	EmailSet            bool
	Protocol            *string
	ProtocolSet         bool
	PlanID              *uint64
	PlanIDSet           bool
	TrafficLimit        *int64
	TrafficLimitSet     bool
	TrafficUsed         *int64
	TrafficUsedSet      bool
	MonthlyTrafficUsed  *int64
	MonthlyUsedSet      bool
	MonthlyTrafficLimit *int64
	MonthlyLimitSet     bool
	NextTrafficResetAt  *time.Time
	NextResetSet        bool
	ExpiredAt           *time.Time
	ExpiredAtSet        bool
	Enabled             *bool
	EnabledSet          bool
	Password            *string
	PasswordSet         bool
	// Token / UUID 只服务于「重置订阅」：一次性换掉订阅链接与节点凭据。
	//
	// 为什么不并进普通编辑：普通编辑改 token/uuid 会把用户手里的订阅链接和
	// 已导入客户端的配置无端作废，必须由明确的「重置」动作触发。
	Token    *string
	TokenSet bool
	UUID     *string
	UUIDSet  bool
	Now      time.Time
}

// AdminUserStore 是用户域对数据库的全部依赖（SQL 实现见 user_sqlstore.go）。
//
// 刻意不与 P8d 的 Store 合并：各域在自己文件里声明窄接口，互不牵连，
// 测试也能只实现自己用到的部分。
type AdminUserStore interface {
	UserPage(ctx context.Context, search string, page, perPage int64, now time.Time) ([]UserRow, int64, error)
	UserByID(ctx context.Context, id uint64) (*UserRow, error)
	UserExists(ctx context.Context, id uint64) (bool, error)
	UserProxies(ctx context.Context, userID uint64) ([]UserProxy, error)
	UserPlanByID(ctx context.Context, id uint64) (*UserPlan, error)
	PlanExists(ctx context.Context, id uint64) (bool, error)
	CreateUser(ctx context.Context, in UserCreateInput) (*UserRow, error)
	UpdateUser(ctx context.Context, id uint64, patch UserPatch) error
	DeleteUser(ctx context.Context, id uint64) error
	DeleteAccessTokens(ctx context.Context, userID uint64) error
}

// UserPanelOps 是用户域对节点侧（3x-ui / xray）的动作接缝。
//
// 为什么要接缝：PHP 的 UserAdminService 会直接调 NodeDriverFactory 遍历启用节点
// （provision / resetClientTraffic / toggleClient）。节点驱动属于 P5/P8b 的地盘，
// 这里只声明语义，真实实现由 cmd/server 组装时注入；未注入时用 NoopUserPanelOps，
// 数据库侧行为与 PHP 一致（PHP 对面板失败也只是 report()，不影响接口成功）。
type UserPanelOps interface {
	ProvisionUser(ctx context.Context, user *UserRow) error
	ResetClientTraffic(ctx context.Context, user *UserRow) error
	SetClientEnabled(ctx context.Context, user *UserRow, enable bool) error
	// SwitchProtocol 负责各节点 attach/detach；protocol 列由调用方在成功后落库
	// （与 P2 PanelSwitcher 的分工一致，避免网络调用进入事务）。
	SwitchProtocol(ctx context.Context, user *UserRow, newProtocol string) error
	DeleteUserClients(ctx context.Context, user *UserRow) error
	// RebuildUserCredentials 在凭据（uuid）换新后重建该用户在各节点的 client。
	//
	// 触发场景：管理员重置订阅（订阅链接泄露处置）。用户行里的 token/uuid 换新后，
	// 各节点上的旧凭据必须一并作废，否则旧订阅导入出去的客户端照样能连。
	//
	// 实现语义（用户定稿：**先删再加**，不做原地 update）：
	//   - 3x-ui：DeleteClient(email) 后按新 uuid AddClient 重新挂到各入站；
	//   - xray（裸内核）：投 rmu 再投 adu，agent 按新配置重建 client。
	// 单节点失败只记日志、不影响接口成功（与 PHP 对面板错误的口径一致）。
	RebuildUserCredentials(ctx context.Context, user *UserRow) error
}

// NoopUserPanelOps 是未接线时的默认实现：全部成功（PHP 对面板错误同样只记日志）。
type NoopUserPanelOps struct{}

// ProvisionUser 见 UserPanelOps。
func (NoopUserPanelOps) ProvisionUser(context.Context, *UserRow) error { return nil }

// ResetClientTraffic 见 UserPanelOps。
func (NoopUserPanelOps) ResetClientTraffic(context.Context, *UserRow) error { return nil }

// SetClientEnabled 见 UserPanelOps。
func (NoopUserPanelOps) SetClientEnabled(context.Context, *UserRow, bool) error { return nil }

// SwitchProtocol 见 UserPanelOps。
func (NoopUserPanelOps) SwitchProtocol(context.Context, *UserRow, string) error { return nil }

// DeleteUserClients 见 UserPanelOps。
func (NoopUserPanelOps) DeleteUserClients(context.Context, *UserRow) error { return nil }

// RebuildUserCredentials 见 UserPanelOps。
func (NoopUserPanelOps) RebuildUserCredentials(context.Context, *UserRow) error { return nil }

// UserConfig 是用户域需要的站点配置。
type UserConfig struct {
	// BcryptRounds 对齐 BCRYPT_ROUNDS（Laravel Hash::make 默认 12）。
	BcryptRounds int

	// AppTimezone 对齐 config/app.php 的 timezone（默认 UTC）。
	AppTimezone string
	// BusinessTimezone 对齐 config/panel.php 的 business_timezone（默认 Asia/Shanghai）。
	// 空 → 回落 AppTimezone（与 PHP 的 `config('panel.business_timezone') ?: config('app.timezone')` 一致）。
	BusinessTimezone string
}

// UserNotifier 把用户侧的变更推给该用户的在线前端（可空）。
//
// 独立成小接口的原因同 UserPanelOps：admin 包不 import internal/handler/api，
// 真实实现是 api.UserHub，由 cmd/server 组装时注入。为空则跳过推送 ——
// 前端仍能靠轮询 /api/me 补到最新状态，只是慢一拍。
type UserNotifier interface {
	// NotifySubscriptionReset 通知该用户「订阅已重置」。
	NotifySubscriptionReset(ctx context.Context, userID uint64, token, subPath string) error
}

// UserDeps 是 NewUserDomain 的依赖清单。
type UserDeps struct {
	// Store / Users 是同一个 AdminUserStore（Store 为兼容旧命名）。
	Store AdminUserStore
	Users AdminUserStore
	Ops   UserPanelOps
	// Notifier 是用户端下行通知（可空）。
	Notifier UserNotifier
	// Alerts 解锁邮件提醒防重标记（可空：为空则续费/重置后不再提醒）。
	Alerts  AlertRestorer
	Plans   PlanStore
	Orders  OrderStore
	Stats   StatsStore
	Traffic TrafficStore
	Config  UserConfig
	Now     func() time.Time
	Logger  loggerLike
}

// userDomainError 对齐 ProtocolSwitchService 抛出的 DomainException
// （handler 映射成 code=400 而不是 500）。
type userDomainError struct{ msg string }

func (e *userDomainError) Error() string { return e.msg }

// NewUserDomainError 构造 DomainException 等价错误（供 ops 实现使用）。
func NewUserDomainError(msg string) error { return &userDomainError{msg: msg} }

// loggerLike 是 *log.Logger 的最小面（与 UserDeps.Logger 同型，测试可塞假实现）。
type loggerLike interface {
	Printf(format string, v ...any)
}

// UserTrafficRow 是流量排行的一行（user-ranking / node-ranking / 单用户明细共用）。
//
// 缺数据 ≠ 0：库里查不到的用户/节点直接不出现；并集补出来的行把三列留 null。
type UserTrafficRow struct {
	NodeID     uint64
	UserID     uint64
	Name       string  // 节点名（node-ranking / 明细）
	EmailPtr   *string // user-ranking 的 users.email（nullable）
	PlanType   *string
	PlanName   *string
	PlanMonths *int64
	Weighted   int64
	Up         int64
	Down       int64

	// 仅 userNodeBreakdown：倍率与占比。
	Multiplier *float64
	Share      *float64

	// nodeRanking：用户腿缺数据时为 nil（不是 0），网卡口径同理。
	WeightedPtr *int64
	UpPtr       *int64
	DownPtr     *int64
	UserRaw     *int64
	NicUp       *int64
	NicDown     *int64
	NicTotal    *int64
}

// TrafficWindow 是 resolve() 的产物：表、桶列、起止与窗口键。
type TrafficWindow struct {
	Table  string
	Column string
	Start  string
	End    string
	Key    string
}

// TrafficStore 是流量排行对数据库的依赖（SQL 实现见 user_traffic_sqlstore.go）。
type TrafficStore interface {
	// ResolveWindow 把 window 参数解析成表/桶列/起止（非法值回退 24h）。
	ResolveWindow(window string, now time.Time) TrafficWindow
	// DataStart 取表里最早那个桶的业务日期（没数据 → nil）。
	DataStart(ctx context.Context, w TrafficWindow, now time.Time) (*string, error)
	UserRanking(ctx context.Context, w TrafficWindow, limit int64) ([]UserTrafficRow, error)
	NodeRanking(ctx context.Context, w TrafficWindow) ([]UserTrafficRow, error)
	UserNodeBreakdown(ctx context.Context, w TrafficWindow, userID uint64) ([]UserTrafficRow, error)
	// NodesByID 取节点名与 traffic_multiplier（含停用节点，倍率是配置属性）。
	NodesByID(ctx context.Context, ids []uint64) (map[uint64]NodeTrafficMeta, error)
	// DefaultNodeMultiplier 取 site_configs.default_node_multiplier（缺省 1.0）。
	DefaultNodeMultiplier(ctx context.Context) (float64, error)
	// NicSummary 取网卡口径（monitor_samples 差分合计）；无样本的节点不出现。
	NicSummary(ctx context.Context, w TrafficWindow) (map[uint64]NicTraffic, error)
}

// NodeTrafficMeta 是倍率换算需要的节点列。
type NodeTrafficMeta struct {
	Name              string
	TrafficMultiplier *float64
}

// NicTraffic 是网卡口径的 up/down/total（单位字节，窗口合计）。
type NicTraffic struct {
	Up    int64
	Down  int64
	Total int64
}

// MultiplierFor 对齐 TrafficSyncService::multiplierFor：节点值非 null 用节点值，
// 否则回落 site_configs.default_node_multiplier（空 → 1.0）。
//
// 不做进程内缓存（behavior-contract §1：queue worker 长驻，静态缓存会让改倍率后按旧值计费）。
func MultiplierFor(meta NodeTrafficMeta, defaultMultiplier float64) float64 {
	if meta.TrafficMultiplier != nil {
		return *meta.TrafficMultiplier
	}
	return defaultMultiplier
}
