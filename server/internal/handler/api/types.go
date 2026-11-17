// Package api 实现用户端公开端点、认证端点与 api.auth 中间件，
// 逐字段对齐 PHP 现状（backend/app/Http/Controllers/Api + Middleware/ApiAuth.php）。
//
// 契约：
//   - 响应格式、分页、Bearer 认证 → .hermes/plans/go-migration/contracts/api-contract.md
//   - 时间/布尔/金额/JSON 字段 → contracts/data-contract.md
//
// 设计约束（与 P0/P1 一致）：database/sql 手写 SQL，不用 ORM；金额与布尔按 Laravel 的输出形态
// 逐字段复刻。handler 只依赖本文件里的接口，测试用内存实现，不连库。
package api

import (
	"context"
	"database/sql"
	"time"
)

// User 是 users 表一行（只含认证/展示需要的列）。
//
// 注意：users.email 存的是用户注册邮箱（nullable）；3x-ui client 主键是 ch_user_{id}，
// 由 clientEmail() 派生，不入库。
type User struct {
	ID       uint64
	Email    sql.NullString
	Password sql.NullString // bcrypt（nullable：老用户可能没设密码）
	Token    string         // 长期订阅/登录令牌 sub_xxx
	Protocol string
	PlanID   sql.NullInt64

	TrafficLimit        int64
	TrafficUsed         int64
	MonthlyTrafficUsed  int64
	MonthlyTrafficLimit int64

	NextTrafficResetAt sql.NullTime
	ExpiredAt          sql.NullTime
	Enabled            bool

	// AppVersionCode / AppVersionName 记录客户端最近一次版本上报（app-version 接口副作用；
	// scanUser 不读取这两列，仅 memStore 测试夹具用它们断言上报是否落库）。
	AppVersionCode sql.NullInt64
	AppVersionName sql.NullString
}

// ClientEmail 与 3x-ui client 主键同源（users.id → ch_user_{id}）。
func (u *User) ClientEmail() string { return "ch_user_" + itoa(u.ID) }

// Plan 是 plans 表一行。
type Plan struct {
	ID             uint64
	Name           string
	Type           string
	Months         sql.NullInt64
	MonthlyTraffic sql.NullInt64
	PeriodTraffic  sql.NullInt64
	TotalTraffic   sql.NullInt64
	Price          float64
	ResetPrice     float64
	IncludeLocal   bool
	ThirdPartyIDs  []int64 // third_party_sub_ids（JSON 数组；空 = 不含第三方）
}

// Tutorial 是 tutorials 表一行；Category 已按 PHP 口径做过 COALESCE/NULLIF/TRIM。
type Tutorial struct {
	ID        uint64
	Title     string
	Category  string
	Content   string
	Sort      int64
	Enabled   bool
	CreatedAt sql.NullTime
}

// AccessToken 是 access_tokens 表一行。
type AccessToken struct {
	ID        uint64
	UserID    uint64
	Token     string
	ExpiresAt sql.NullTime
}

// Expired 对齐 AccessToken::isExpired()：expires_at 为 null 表示永不过期。
func (a *AccessToken) Expired(now time.Time) bool {
	return a.ExpiresAt.Valid && a.ExpiresAt.Time.Before(now)
}

// XrayInbound 是 xray_inbounds 的展示/端口所需列。
type XrayInbound struct {
	ID       uint64
	Protocol string
	Port     int
	Enabled  bool
}

// Node 是 nodes 表一行（含 xray 入站列表）。
type Node struct {
	ID           uint64
	Name         string
	Host         string
	Port         int
	Status       string
	Latency      int
	DriverType   string
	DriverConfig string // JSON 文本（"" = NULL）
	// TrafficMultiplier：NULL = 继承 site_configs.default_node_multiplier
	TrafficMultiplier *float64

	// 面板调用参数（协议切换用；password/api_key 为库内密文，由 handler 解密）
	Scheme      string
	WebBasePath string
	Username    string
	PasswordEnc sql.NullString
	APIKeyEnc   sql.NullString
	VerifySSL   bool

	XrayInbounds []XrayInbound
	Inbounds     []NodeInbound
}

// NodeInbound 是 node_inbounds 表一行（3x-ui 的 inbound_id 映射）。
type NodeInbound struct {
	Protocol  string
	InboundID int64
}

// InboundIDsFor 对齐 Node::inboundIdsFor()：按协议取全部 inbound_id。
func (n *Node) InboundIDsFor(protocol string) []int64 {
	var ids []int64
	for _, in := range n.Inbounds {
		if in.Protocol == protocol {
			ids = append(ids, in.InboundID)
		}
	}
	return ids
}

// ThirdPartySub 是 third_party_subs 的注入所需列。
type ThirdPartySub struct {
	ID          int64
	CachedLinks string
}

// CreateUserInput 是注册建号的字段（对齐 AuthController::register 的 User::create）。
type CreateUserInput struct {
	Email        string
	PasswordHash string
	Token        string
	UUID         string
	Protocol     string
}

// Store 是本包对数据库的全部依赖。实现见 sqlstore.go；测试用内存实现。
type Store interface {
	UserByID(ctx context.Context, id uint64) (*User, error)
	UserByToken(ctx context.Context, token string) (*User, error)
	UserByEmail(ctx context.Context, email string) (*User, error)
	EmailExists(ctx context.Context, email string) (bool, error)
	CreateUser(ctx context.Context, in CreateUserInput) (*User, error)
	UpdateUserPassword(ctx context.Context, id uint64, hash string) error
	UpdateUserProtocol(ctx context.Context, id uint64, protocol string) error
	// UpdateUserAppVersion 记录客户端最近一次版本上报（app_version_code/name/reported_at）。
	UpdateUserAppVersion(ctx context.Context, id uint64, code int64, name string) error

	PlanByID(ctx context.Context, id uint64) (*Plan, error)
	ActivePlans(ctx context.Context) ([]Plan, error)

	AccessTokenByToken(ctx context.Context, token string) (*AccessToken, error)
	CreateAccessToken(ctx context.Context, userID uint64, token string, expires time.Time) error
	TouchAccessToken(ctx context.Context, id uint64, at time.Time) error
	DeleteAccessTokensByUser(ctx context.Context, userID uint64) error

	SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error)

	EnabledTutorials(ctx context.Context) ([]Tutorial, error)
	TutorialByID(ctx context.Context, id uint64) (*Tutorial, error)

	// OnlineNodesForProtocol 返回 enabled 且 status=online、且该协议有可用入站的节点
	// （3x-ui 看 node_inbounds，xray 看 xray_inbounds.enabled）。
	// userID 用于按 user_nodes 绑定过滤：该用户没有任何绑定时走全量（老行为不变）。
	OnlineNodesForProtocol(ctx context.Context, protocol string, userID uint64) ([]Node, error)
	// EnabledNodesForSwitch 返回全部 enabled 节点（含凭据密文与 inbound 映射），
	// 供协议切换使用。
	EnabledNodesForSwitch(ctx context.Context) ([]Node, error)

	ThirdPartySubsByIDs(ctx context.Context, ids []int64) ([]ThirdPartySub, error)
}

// Cache 是 Laravel Cache 门面的最小子集（database store）。
//
// 为什么复用 cache 表：并存期 PHP 与 Go 共用同一批键（email_verify_ / pwd_reset_ / rate:），
// 换进程后验证码与限流状态不丢，两边行为一致。值的序列化格式与 Laravel 的
// DatabaseStore 相同（见 cache.go）。
type Cache interface {
	Get(ctx context.Context, key string) (val any, ok bool, err error)
	Put(ctx context.Context, key string, val any, ttl time.Duration) error
	Forget(ctx context.Context, key string) error
	// Add 仅在键不存在（或已过期被回收）时写入。
	Add(ctx context.Context, key string, val any, ttl time.Duration) (bool, error)
	Increment(ctx context.Context, key string) (int64, bool, error)
}

// Mailer 发送 HTML 邮件；实现见 mailer.go（SMTP），测试注入假实现。
type Mailer interface {
	Send(ctx context.Context, to, subject, htmlBody string) error
}

// Provisioner 是「建号后异步下发 3x-ui client」的接缝。
//
// 真实实现在 P5/P6（驱动层 + 队列）。P2 只保证与 PHP 相同的失败语义：
// 派发失败只记日志，注册照样成功。
type Provisioner interface {
	Dispatch(ctx context.Context, user *User) error
}

// ProtocolSwitcher 是协议切换的接缝（真实实现走 3x-ui 面板 API，xray 节点为 no-op）。
type ProtocolSwitcher interface {
	Switch(ctx context.Context, user *User, newProtocol string) error
}

// ErrUnsupportedProtocol 对齐 ProtocolSwitchService 抛出的 DomainException('不支持的协议')。
type domainError struct{ msg string }

func (e *domainError) Error() string { return e.msg }

// NewDomainError 构造业务层 DomainException 等价错误（handler 映射为 code 400）。
func NewDomainError(msg string) error { return &domainError{msg: msg} }

// IsDomainError 判定是否业务层 DomainException。
func IsDomainError(err error) bool {
	_, ok := err.(*domainError)
	return ok
}
