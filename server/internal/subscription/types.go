package subscription

import (
	"context"
	"encoding/json"
	"time"
)

// 订阅链路用到的领域对象。全部是「数据库读出 + Laravel casts 之后」的形态，
// 字段命名与 backend/app/Models 对齐（便于与 PHP 对照阅读）。

// Format 是订阅输出格式。
type Format string

const (
	FormatBase64  Format = "base64"
	FormatClash   Format = "clash"
	FormatSingbox Format = "singbox"
)

// Error 是订阅生成业务异常（对应 PHP 的 SubscriptionException）：
// 用户禁用 / 无套餐 / 过期 / 超量 / 节点没填连接地址。Code 为业务 code（403/404）。
type Error struct {
	Message string
	Code    int
}

func (e *Error) Error() string { return e.Message }

// User 是 users 表的一行（只取订阅需要的列）。
type User struct {
	ID                 int64
	Token              string
	UUID               string
	Protocol           string
	PlanID             *int64
	TrafficLimit       int64
	TrafficUsed        int64
	MonthlyTrafficUsed int64
	MonthlyTrafficLim  int64
	NextTrafficResetAt *time.Time // datetime，按 DSN 的 loc=UTC 打 UTC 标签（与 PHP 读取一致）
	ExpiredAt          *time.Time
	Enabled            bool
}

// Plan 是 plans 表的一行（只取订阅需要的列）。
type Plan struct {
	ID               int64
	Type             string // period / total
	Months           int64
	IncludeLocal     bool
	ThirdPartySubIDs []int64
}

// IsPeriod 对应 Plan::isPeriod()。
func (p *Plan) IsPeriod() bool { return p != nil && p.Type == "period" }

// IncludesLocal 对应 Plan::includesLocal()：老套餐默认 true。
func (p *Plan) IncludesLocal() bool { return p == nil || p.IncludeLocal }

// Node 是 nodes 表的一行。Password/APIKey 是**已解密**的明文
// （PHP 侧由 Eloquent accessor 解密；Go 侧由 repository 用 crypto 解密）。
type Node struct {
	ID                int64
	Name              string
	Host              string
	Port              int64
	Scheme            string
	WebBasePath       string
	Username          string
	Password          string
	APIKey            string
	Enabled           bool
	VerifySSL         bool
	Status            string
	DriverType        string
	DriverConfig      string // 原始 JSON 文本
	TrafficMultiplier *float64
}

// IsXray 对应 Node::isXray()。
func (n Node) IsXray() bool {
	if n.DriverType == "" {
		return false
	}
	return n.DriverType == "xray"
}

// XrayInbound 是 xray_inbounds 表的一行。
type XrayInbound struct {
	ID             int64
	NodeID         int64
	Tag            string
	Remark         string
	Protocol       string
	Port           int64
	Listen         string // 绑定地址（DB 默认 0.0.0.0）；订阅按地址族选 host / host_v6
	// ClientHost 是「客户端连接地址」覆盖（CDN 域名 / 另一个地址）；
	// 非空时订阅链接的地址用它，端口仍用本入站端口。
	ClientHost     string
	Settings       string // JSON 文本
	StreamSettings string // JSON 文本
	// SalamanderPassword 是 hysteria 入站的 salamander 混淆密码明文
	// （库里 salamander_password 是密文，由 XrayInbounds 解密后挂上来）。
	// 空 = 未启用混淆；解密失败也按空处理（不能因为密码坏掉丢掉整条链接）。
	SalamanderPassword string
	Enabled            bool
	Sort               int64
}

// ThirdPartySub 是 third_party_subs 表的一行。
type ThirdPartySub struct {
	ID          int64
	Enabled     bool
	CachedLinks string
}

// Repository 是订阅链路的数据来源。生产实现读 MySQL + 拉 3x-ui 面板；
// 测试实现直接给内存数据（golden 对拍用）。
type Repository interface {
	// SiteConfigs 返回全部 site_configs（key → value）。缺失的键由调用方按默认值处理。
	SiteConfigs(ctx context.Context) (map[string]string, error)

	// UserByToken 按 users.token 精确查询；不存在返回 (nil, nil)。
	UserByToken(ctx context.Context, token string) (*User, error)

	// PlanByID 按 plans.id 查询；不存在返回 (nil, nil)。
	PlanByID(ctx context.Context, id int64) (*Plan, error)

	// SubscribableNodes 返回「已启用 + 在线 + 有该协议入口」的节点（按 id 升序）。
	//
	// 对齐 SubscriptionService::getLinks() 的 where 子句：
	//   - 3x-ui 节点：node_inbounds 里存在 protocol 匹配的记录；
	//   - xray 节点：xray_inbounds 里存在 protocol 匹配且 enabled 的记录。
	SubscribableNodes(ctx context.Context, protocol string) ([]Node, error)

	// NodeInboundIDs 返回该节点上协议匹配的 3x-ui 入站 id（按 id 升序）。
	NodeInboundIDs(ctx context.Context, nodeID int64, protocol string) ([]int64, error)

	// XrayInbounds 返回该节点启用的入站（按 sort、id 升序），对齐 XrayInbound::enabledFor()。
	XrayInbounds(ctx context.Context, nodeID int64) ([]XrayInbound, error)

	// ThirdPartySubs 按 id 升序返回给定 id 中「已启用」的第三方订阅。
	ThirdPartySubs(ctx context.Context, ids []int64) ([]ThirdPartySub, error)

	// HasEnabledOnlineXrayNodeWithoutHost 对应 generate() 里的诊断查询：
	// 是否存在「已启用 + 在线 + driver_type=xray + host 为空」的节点。
	HasEnabledOnlineXrayNodeWithoutHost(ctx context.Context) (bool, error)

	// PanelLinks 取 3x-ui 节点上该 email 的全部分享链接（GET /panel/api/clients/links/{email}）。
	// 返回原始链接（未过滤、未去重），由服务层按 filterNodeLinks() 的口径处理。
	PanelLinks(ctx context.Context, node Node, email string) ([]string, error)

	// PanelInbounds 取 3x-ui 节点的一次性全量入站列表（listInbounds），
	// 服务层用它做端口过滤（对齐 PHP 的「一次 listInbounds 拿端口」）。
	PanelInbounds(ctx context.Context, node Node) ([]PanelInbound, error)
}

// 兼容性：ensure we don't accidentally depend on encoding/json elsewhere.
var _ = json.Marshal
