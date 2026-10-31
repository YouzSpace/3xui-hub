package admin

// 节点域（nodes / xray_* / warp / third-party / 出站订阅）的数据模型与接缝。
//
// 为什么不复用 P8d 的 Store / Config：任务边界规定不得修改共享文件（handler.go/types.go/
// store.go/sqlstore.go），而节点域的查询与依赖完全不同（还要行锁 merge driver_config、
// 调 3x-ui/CF/订阅拉取）。因此这里独立一套 NodeStore + NodeOps，由 NodeDomain 持有，
// 生产实现在 node_sqlstore.go / node_ops.go，测试用 node_memstore_test.go 的内存实现。
//
// 字段口径见 contracts/data-contract.md §6（driver_config）与 §2（加密）。

import (
	"context"
	"encoding/json"
	"time"
)

// ---- 节点 ----

// NodeRecord 是 nodes 一行（加密列已解密成明文，写回时由 store 加密）。
type NodeRecord struct {
	ID                uint64
	Name              string
	Host              string
	Port              int64
	Scheme            string
	WebBasePath       string
	Username          string
	Password          *string // 明文
	APIKey            *string // 明文
	Enabled           bool
	VerifySSL         bool
	Status            string
	Latency           int64
	CPU               *string // decimal(5,2) 原样字符串
	Mem               *string
	TrafficMultiplier *string // decimal(6,2)；null = 继承原始倍率
	DriverType        string
	DriverVersion     *string
	DriverConfigRaw   []byte // driver_config 原始 JSON（可能为 nil）
	NodeSecret        *string
	LastCheckAt       *time.Time
	CreatedAt         *time.Time
	UpdatedAt         *time.Time

	// PasswordEnc / APIKeyEnc 是**密文原文**（删除节点时写进清理任务的快照，
	// 与 PHP 的 getRawOriginal() 口径一致：快照里存密文，任务内按正常路径解密）。
	PasswordEnc *string
	APIKeyEnc   *string

	// 关联（列表/详情预加载）
	Inbounds     []NodeInboundRecord
	XrayInbounds []XrayInboundRecord

	// ConfigVersion 是 driver_config.config_version（0 = 未设置）。
	ConfigVersion int64
}

// IsXray 对应 Node::isXray()。
func (n *NodeRecord) IsXray() bool { return n.DriverType == "xray" }

// NodeInboundRecord 是 node_inbounds 一行（3x-ui 节点的入站挂载）。
type NodeInboundRecord struct {
	ID        uint64
	NodeID    uint64
	Protocol  string
	InboundID int64
}

// NodeCreateInput 是创建节点的落库字段（handler 已完成默认值处理）。
type NodeCreateInput struct {
	Name              string
	Host              string
	Port              int64
	Scheme            string
	WebBasePath       string
	Username          string
	Password          *string
	APIKey            *string
	VerifySSL         bool
	DriverType        string
	TrafficMultiplier *string
	// NodeSecret 仅 xray 节点：明文，由 store 加密进 driver_config.node_secret。
	NodeSecret string
	// PullInterval 仅 xray 节点：写入 driver_config.pull_interval。
	PullInterval *int64
	// HostV6 仅 xray 节点：IPv6 入口地址，写入 driver_config.host_v6
	// （nodes 表没有该列，老库也不做 ALTER；与 node_secret 同住 JSON 列）。
	HostV6 *string
	// Inbounds 仅 3x-ui 节点：新建时要落的入站。
	Inbounds map[string][]int64
}

// NodeUpdateInput 是更新节点的字段集合（指针 = 请求里出现过）。
type NodeUpdateInput struct {
	Name              *string
	Host              *string
	Port              *int64
	Scheme            *string
	WebBasePath       *string
	Username          *string
	Password          *string
	PasswordSet       bool
	APIKey            *string
	APIKeySet         bool
	Enabled           *bool
	VerifySSL         *bool
	DriverType        *string
	TrafficMultiplier *string
	MultiplierSet     bool // 传了 traffic_multiplier（null 即清空回继承）
	Inbounds          map[string][]int64
	InboundsSet       bool
	// HostV6 传了才改（null/空串 = 清空 driver_config.host_v6）。
	HostV6    *string
	HostV6Set bool
}

// NodeTaskRecord 是 async_tasks 一行（节点域只看这些列）。
type NodeTaskRecord struct {
	ID       uint64
	Type     string
	Status   string
	Total    int64
	Failed   int64
	Meta     []byte
	MaxTries int64
}

// TaskCreateInput 是建任务（含 items）的字段。
type TaskCreateInput struct {
	Type        string
	SubjectType string
	SubjectID   uint64
	Meta        []byte
	MaxAttempts int64
	ItemKeys    []string
}

// CommandRecord 是 node_api_commands 一行。
type CommandRecord struct {
	ID     uint64
	NodeID uint64
	Action string
	Status string
	Result *string
}

// ---- xray 入站 ----

// XrayInboundRecord 是 xray_inbounds 一行；RealityPrivateKey / SalamanderPassword 为明文。
type XrayInboundRecord struct {
	ID                uint64
	NodeID            uint64
	Tag               string
	Remark            *string
	Protocol          string
	Port              int64
	Listen            string
	Settings          []byte // JSON
	StreamSettings    []byte // JSON
	Sniffing          []byte // JSON
	RealityPrivateKey *string
	// SalamanderPassword 是 HY2 的 finalmask.udp[salamander] 混淆密码（明文）。
	// 库里在独立加密列，stream_settings 里只留 {"type":"salamander"} 占位。
	SalamanderPassword *string
	Enabled            bool
	Sort               int64
	CreatedAt          *time.Time
	UpdatedAt          *time.Time
}

// XrayCertificate 是 xray_certificates 一行。
//
// KeyEnc 是库里的密文原文（与 reality_private_key 同口径），明文只在渲染时出现。
type XrayCertificate struct {
	ID       uint64
	Name     string
	Domain   *string
	CertPEM  string
	KeyEnc   string
	NotAfter *time.Time
}

// XrayCertificateInput 是证书入库字段（私钥明文，由 store 加密后落 key_enc）。
type XrayCertificateInput struct {
	Name    string
	Domain  *string
	CertPEM string
	KeyPEM  string
}

// XrayInboundInput 是入站落库字段（settings/stream/sniffing 已组装成 JSON）。
type XrayInboundInput struct {
	NodeID            uint64
	Tag               string
	Remark            *string
	Protocol          string
	Port              int64
	Listen            string
	Settings          []byte
	StreamSettings    []byte
	Sniffing          []byte
	RealityPrivateKey *string
	RealityKeySet     bool
	// SalamanderPassword 是 HY2 混淆密码明文（store 加密后落独立列）；
	// SalamanderPasswordSet=false 表示本次不动该列（编辑时没带这个字段）。
	SalamanderPassword    *string
	SalamanderPasswordSet bool
	Enabled               *bool
	Sort                  *int64
}

// ---- xray 出站 ----

// XrayOutboundRecord 是 xray_outbounds 一行；SecretKey 为明文（wireguard 私钥）。
type XrayOutboundRecord struct {
	ID                  uint64
	NodeID              uint64
	Tag                 string
	Protocol            string
	Settings            []byte
	StreamSettings      []byte
	SecretKey           *string
	Enabled             bool
	Sort                int64
	Remark              *string
	RelayTargetNodeID   *uint64
	RelayTargetInboundID *uint64
	CreatedAt           *time.Time
	UpdatedAt           *time.Time
}

// XrayOutboundInput 是出站落库字段。
type XrayOutboundInput struct {
	NodeID               uint64
	Tag                  string
	Protocol             string
	Settings             []byte
	StreamSettings       []byte
	SecretKey            *string
	SecretKeySet         bool
	Enabled              *bool
	Sort                 *int64
	Remark               *string
	RelayTargetNodeID    *uint64
	RelayTargetInboundID *uint64
}

// OutboundTraffic 是 xray_outbound_traffic 一行的累计值。
type OutboundTraffic struct {
	Upload    int64
	Download  int64
	UpdatedAt *string // 原样字符串（PHP present 直接 (string) 输出）
}

// ---- 路由 ----

// RoutingRuleRecord 是 xray_routing_rules 一行。
type RoutingRuleRecord struct {
	ID          uint64
	NodeID      uint64
	Domains     []byte
	IPs         []byte
	Port        *string
	Network     *string
	Protocol    *string
	InboundTag  *string
	OutboundTag string
	Remark      *string
	Enabled     bool
	Sort        int64
}

// RoutingRuleInput 是路由规则落库字段。
type RoutingRuleInput struct {
	NodeID      uint64
	Domains     []byte
	IPs         []byte
	Port        *string
	Network     *string
	Protocol    *string
	InboundTag  *string
	OutboundTag string
	Remark      *string
	Enabled     *bool
	Sort        *int64
}

// BalancerRecord 是 xray_balancers 一行。
type BalancerRecord struct {
	ID          uint64
	NodeID      uint64
	Tag         string
	Selectors   []byte
	Strategy    string
	FallbackTag *string
	Remark      *string
	Enabled     bool
	Sort        int64
}

// BalancerInput 是负载均衡器落库字段。
type BalancerInput struct {
	NodeID      uint64
	Tag         string
	Selectors   []byte
	Strategy    string
	FallbackTag *string
	Remark      *string
	Enabled     *bool
	Sort        *int64
}

// ---- 中转凭据 ----

// RelayClientRecord 是 xray_relay_clients 一行。
type RelayClientRecord struct {
	ID          uint64
	NodeID      uint64
	InboundID   uint64
	Email       string
	ClientID    string
	SourceNodeID *uint64
}

// RelayClientInput 是插入中转凭据的字段。
type RelayClientInput struct {
	NodeID         uint64
	InboundID      uint64
	Email          string
	ClientID       string
	SourceNodeID   uint64
	SourceInboundID uint64
}

// ---- WARP ----

// WarpAccountRecord 是 node_warp_accounts 一行（密文字段已解密）。
type WarpAccountRecord struct {
	NodeID          uint64
	DeviceID        string
	AccessToken     string
	PrivateKey      string
	PublicKey       string
	PeerPublicKey   string
	PeerEndpoint    string
	Addresses       []string
	Reserved        []int64
	LicenseKey      string
	ClientID        string
	AutoRotateHours int64
	LastRotateAt    *time.Time
	Enabled         bool
	UpdatedAt       *time.Time
}

// ---- 第三方订阅 ----

// ThirdPartySubRecord 是 third_party_subs 一行。
type ThirdPartySubRecord struct {
	ID            uint64
	Name          string
	URL           string
	Enabled       bool
	CachedLinks   *string
	NodeCount     int64
	LastFetchedAt *time.Time
	LastStatus    string
	LastError     *string
}

// ThirdPartySubInput 是创建字段。
type ThirdPartySubInput struct {
	Name    string
	URL     string
	Enabled bool
}

// ThirdPartySubUpdate 是更新字段（指针 = 请求里出现过）。
type ThirdPartySubUpdate struct {
	Name    *string
	URL     *string
	Enabled *bool
}

// ---- 出站订阅 ----

// OutboundSubRecord 是 xray_outbound_subscriptions 一行。
type OutboundSubRecord struct {
	ID             uint64
	NodeID         uint64
	Remark         *string
	URL            string
	TagPrefix      *string
	UpdateInterval int64
	Prepend        bool
	Priority       int64
	Enabled        bool
	Fetched        *string
	LinkIdentities *string
	ContentHash    *string
	NodeCount      int64
	LastFetchedAt  *time.Time
	LastStatus     string
	LastError      *string
}

// OutboundSubInput 是落库字段。
type OutboundSubInput struct {
	NodeID         uint64
	Remark         *string
	URL            string
	TagPrefix      *string
	UpdateInterval *int64
	Prepend        *bool
	Priority       *int64
	Enabled        *bool
}

// OutboundSubFetchResult 是一次拉取的落库结果。
type OutboundSubFetchResult struct {
	Fetched        string
	LinkIdentities string
	ContentHash    string
	NodeCount      int64
	At             time.Time
}

// ---- 接缝：NodeStore ----

// NodeStore 是节点域对数据库的全部依赖（实现见 node_sqlstore.go）。
type NodeStore interface {
	// 站点配置
	SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error)
	// SiteConfigSet 写一个键；value == nil 时删除该行（show_node_multiplier 的关闭语义）。
	SiteConfigSet(ctx context.Context, key string, value *string) error

	// 节点
	NodesPage(ctx context.Context, all bool, page, perPage int64) ([]NodeRecord, int64, error)
	NodeByID(ctx context.Context, id uint64) (*NodeRecord, error)
	CreateNode(ctx context.Context, in NodeCreateInput) (*NodeRecord, error)
	UpdateNode(ctx context.Context, id uint64, in NodeUpdateInput) (*NodeRecord, error)
	DeleteNode(ctx context.Context, id uint64) error
	// ReplaceNodeInbounds 全量替换入站挂载，返回「确实变化的协议 → 新入站集合」。
	// 入参是「协议 → 入站 id」全集（含 hysteria / vmess / shadowsocks），不只 vless/trojan。
	ReplaceNodeInbounds(ctx context.Context, nodeID uint64, inbounds map[string][]int64) (map[string][]int64, error)
	// SetNodeSecret 加密写入 driver_config.node_secret。
	SetNodeSecret(ctx context.Context, id uint64, secret string) error
	// MergeDriverConfig 行锁内按键合并 driver_config（只 merge 自家键，不整块回写）。
	MergeDriverConfig(ctx context.Context, id uint64, patch map[string]any) error
	// BumpConfigVersion 行锁内递增 config_version 并返回新值。
	BumpConfigVersion(ctx context.Context, id uint64) (int64, error)
	// UpdateNodeStatus 写 healthCheck 结果。
	UpdateNodeStatus(ctx context.Context, id uint64, status string, latency int64, touchCheckAt bool) error

	// 用户口径
	EligibleProtocolCounts(ctx context.Context) (map[string]int64, error)
	AllUserIDs(ctx context.Context) ([]uint64, error)
	UserIDsWithPlanByProtocols(ctx context.Context, protocols []string) ([]uint64, error)

	// 异步任务
	ActiveNodeInitTask(ctx context.Context, nodeID uint64) (*NodeTaskRecord, error)
	CreateTask(ctx context.Context, in TaskCreateInput) (*NodeTaskRecord, error)
	UpdateTaskDesiredEnabled(ctx context.Context, taskID, nodeID uint64, enabled bool) error
	CompleteEmptyTask(ctx context.Context, taskID uint64) (*NodeTaskRecord, error)
	FinalizeInitTask(ctx context.Context, nodeID, taskID uint64) error

	// 节点指令
	CreateCommand(ctx context.Context, nodeID uint64, action string, payload any) (*CommandRecord, error)
	CommandByID(ctx context.Context, id uint64) (*CommandRecord, error)
	HasInFlightUpgrade(ctx context.Context, nodeID uint64) (bool, error)

	// xray 入站
	XrayInboundsByNode(ctx context.Context, nodeID uint64) ([]XrayInboundRecord, error)
	XrayInboundByID(ctx context.Context, nodeID, id uint64) (*XrayInboundRecord, error)
	XrayInboundTagTaken(ctx context.Context, nodeID uint64, tag string, excludeID *uint64) (bool, error)
	// XrayInboundPortTaken 按「传输族」判定端口占用：families 取 tcp / udp（可能同时两个，
	// 如 tunnel 的 settings.network=tcp,udp）。同一端口在 TCP 与 UDP 上可以并存
	// （vless:443/tcp 与 hysteria:443/udp 不冲突）。
	XrayInboundPortTaken(ctx context.Context, nodeID uint64, port int64, families []string, excludeID *uint64) (bool, error)
	XrayInboundTags(ctx context.Context, nodeID uint64, excludeID *uint64) ([]string, error)
	// XrayCertificates 读 TLS 证书库（私钥为密文，渲染时解密）。
	XrayCertificates(ctx context.Context) ([]XrayCertificate, error)
	CreateXrayCertificate(ctx context.Context, in XrayCertificateInput) (*XrayCertificate, error)
	// UpdateXrayCertificate 覆盖一行的 PEM / not_after（本机目录同步刷新续签后的证书用）。
	// 就地更新而不是删了重建：入站的 tlsCertId 指着这一行的 id，重建会让引用悬空。
	UpdateXrayCertificate(ctx context.Context, id uint64, in XrayCertificateInput) error
	DeleteXrayCertificate(ctx context.Context, id uint64) error
	CreateXrayInbound(ctx context.Context, in XrayInboundInput) (*XrayInboundRecord, error)
	UpdateXrayInbound(ctx context.Context, id uint64, in XrayInboundInput) (*XrayInboundRecord, error)
	DeleteXrayInbound(ctx context.Context, id uint64) error

	// xray 出站
	XrayOutboundsByNode(ctx context.Context, nodeID uint64, enabledOnly bool) ([]XrayOutboundRecord, error)
	XrayOutboundByID(ctx context.Context, nodeID, id uint64) (*XrayOutboundRecord, error)
	XrayOutboundByTag(ctx context.Context, nodeID uint64, tag string) (*XrayOutboundRecord, error)
	XrayOutboundTagTaken(ctx context.Context, nodeID uint64, tag string, excludeID *uint64) (bool, error)
	CreateXrayOutbound(ctx context.Context, in XrayOutboundInput) (*XrayOutboundRecord, error)
	UpdateXrayOutbound(ctx context.Context, id uint64, in XrayOutboundInput) (*XrayOutboundRecord, error)
	DeleteXrayOutbound(ctx context.Context, id uint64) error
	UpsertRelayOutbound(ctx context.Context, in XrayOutboundInput) (*XrayOutboundRecord, error)
	// RelayOutboundByTarget 按幂等键查中转出站（源节点 × 目标节点 × 目标入站）。
	// 没有时返回 (nil, nil)（对齐 PHP 的 first() 语义）。
	RelayOutboundByTarget(ctx context.Context, sourceNodeID, targetNodeID, targetInboundID uint64) (*XrayOutboundRecord, error)
	OutboundTraffic(ctx context.Context, nodeID uint64) (map[string]OutboundTraffic, error)
	XrayOutboundTags(ctx context.Context, nodeID uint64) ([]string, error)

	// 路由 / 负载均衡
	RoutingRulesByNode(ctx context.Context, nodeID uint64) ([]RoutingRuleRecord, error)
	RoutingRuleByID(ctx context.Context, nodeID, id uint64) (*RoutingRuleRecord, error)
	CreateRoutingRule(ctx context.Context, in RoutingRuleInput) (*RoutingRuleRecord, error)
	UpdateRoutingRule(ctx context.Context, id uint64, in RoutingRuleInput) (*RoutingRuleRecord, error)
	DeleteRoutingRule(ctx context.Context, id uint64) error
	BalancersByNode(ctx context.Context, nodeID uint64, enabledOnly bool) ([]BalancerRecord, error)
	BalancerByID(ctx context.Context, nodeID, id uint64) (*BalancerRecord, error)
	BalancerTagTaken(ctx context.Context, nodeID uint64, tag string, excludeID *uint64) (bool, error)
	CreateBalancer(ctx context.Context, in BalancerInput) (*BalancerRecord, error)
	UpdateBalancer(ctx context.Context, id uint64, in BalancerInput) (*BalancerRecord, error)
	DeleteBalancer(ctx context.Context, id uint64) error

	// 中转凭据
	RelayClientBySource(ctx context.Context, targetNodeID, inboundID, sourceNodeID uint64) (*RelayClientRecord, error)
	RelayClientsByNode(ctx context.Context, nodeID uint64) ([]RelayClientRecord, error)
	InsertRelayClient(ctx context.Context, in RelayClientInput) error
	DeleteRelayClientsBySource(ctx context.Context, targetNodeID, inboundID, sourceNodeID uint64) (int64, error)

	// 中转候选（启用节点 × 启用入站，排除自身）
	RelayTargets(ctx context.Context, selfID uint64) ([]RelayTarget, error)
	NodeNameByID(ctx context.Context, id uint64) (*string, error)

	// WARP
	WarpAccountByNode(ctx context.Context, nodeID uint64) (*WarpAccountRecord, error)
	SaveWarpAccount(ctx context.Context, in WarpAccountRecord) error
	PatchWarpAccount(ctx context.Context, nodeID uint64, license *string, autoRotateHours *int64, lastRotateAt *time.Time, enabled *bool) error
	DeleteWarpAccount(ctx context.Context, nodeID uint64) error

	// 第三方订阅
	ThirdPartySubs(ctx context.Context) ([]ThirdPartySubRecord, error)
	ThirdPartySubByID(ctx context.Context, id uint64) (*ThirdPartySubRecord, error)
	CreateThirdPartySub(ctx context.Context, in ThirdPartySubInput) (*ThirdPartySubRecord, error)
	UpdateThirdPartySub(ctx context.Context, id uint64, in ThirdPartySubUpdate) (*ThirdPartySubRecord, error)
	DeleteThirdPartySub(ctx context.Context, id uint64) error
	SaveThirdPartyFetch(ctx context.Context, id uint64, links []string, errMsg string) error
	RemoveThirdPartyFromPlans(ctx context.Context, id uint64) error

	// 出站订阅
	OutboundSubsByNode(ctx context.Context, nodeID uint64) ([]OutboundSubRecord, error)
	OutboundSubByID(ctx context.Context, nodeID, id uint64) (*OutboundSubRecord, error)
	CreateOutboundSub(ctx context.Context, in OutboundSubInput) (*OutboundSubRecord, error)
	UpdateOutboundSub(ctx context.Context, id uint64, in OutboundSubInput) (*OutboundSubRecord, error)
	DeleteOutboundSub(ctx context.Context, id uint64) error
	MaxOutboundSubPriority(ctx context.Context, nodeID uint64) (int64, error)
	SaveOutboundSubFetch(ctx context.Context, id uint64, r OutboundSubFetchResult) error
	// RecordOutboundSubError 记录一次拉取失败（只改 last_status/last_error/last_fetched_at，
	// 保留上次成功缓存 —— 与 PHP 的 recordError 同口径）。
	RecordOutboundSubError(ctx context.Context, id uint64, message string, at time.Time) error
}

// RelayTarget 是中转候选：一个启用节点及其（启用的）入站。
type RelayTarget struct {
	Node         NodeRecord
	XrayInbounds []XrayInboundRecord
	Inbounds     []NodeInboundRecord
}

// ---- 接缝：NodeOps（外部副作用） ----

// HealthResult 是健康检查的归一化结果（对齐 panel.Health 的可空语义）。
type HealthResult struct {
	OK        bool
	LatencyMs int64
	CPU       any
	Mem       *float64
	XrayState *string
	Error     string
}

// ProbeConn 是 probe-inbounds 的表单连接参数。
type ProbeConn struct {
	Scheme      string
	Host        string
	Port        int64
	WebBasePath string
	APIKey      *string
	Username    string
	Password    string
	VerifySSL   bool
}

// ManifestAgent 是面板 manifest 的 agent 段。
type ManifestAgent struct {
	Version string
	Assets  []ManifestAsset
}

// ManifestAsset 是 manifest.agent.assets[] 的一条。
type ManifestAsset struct {
	Arch   string
	File   string
	SHA256 string
}

// NodeOps 是节点域的全部外部副作用（HTTP / 面板 / CF / 订阅拉取 / 文件读取）。
//
// 全部走接口：handler 单测注入假实现，不连库、不发外网请求。
type NodeOps interface {
	// HealthCheck 对节点做一次健康检查。
	HealthCheck(ctx context.Context, n *NodeRecord) (HealthResult, error)
	// ProbeInbounds 用表单参数临时连 3x-ui 拉入站清单。
	ProbeInbounds(ctx context.Context, conn ProbeConn) ([]map[string]any, error)
	// ThreeXInbounds 读某 3x-ui 节点的入站清单（relay 候选用）。
	ThreeXInbounds(ctx context.Context, n *NodeRecord) ([]map[string]any, error)
	// ThreeXEnsureRelayClient 在 3x-ui 目标节点上确保 relay client 并返回其 uuid/密码。
	ThreeXEnsureRelayClient(ctx context.Context, target *NodeRecord, inboundID int64, protocol, email string) (string, error)
	// ThreeXDeleteClient 删除 3x-ui 上的 relay client（失败不阻断本地删除）。
	ThreeXDeleteClient(ctx context.Context, target *NodeRecord, email string) error
	// ThreeXClientLinks 读 3x-ui 面板为某 email 生成的分享链接。
	ThreeXClientLinks(ctx context.Context, target *NodeRecord, email string) ([]string, error)
	// RelayTestTarget 下发 test_outbound 指令并同步等待回执（≤8s）。
	RelayTestTarget(ctx context.Context, nodeID uint64, host string, port int64) (status string, output string, pending bool)

	// WarpRegister 注册 WARP 设备（保留已有 license）。nodeID 由调用方给：首次注册时
	// existing 为 nil，返回的账户没有 node_id 会撞外键约束。
	WarpRegister(ctx context.Context, nodeID uint64, nodeName string, existing *WarpAccountRecord) (*WarpAccountRecord, error)
	// WarpSetLicense 设置 WARP+ license。
	WarpSetLicense(ctx context.Context, account *WarpAccountRecord, license string) error

	// OutboundSubFetch 拉取并解析一条出站订阅；返回解析出的出站与 identity。
	OutboundSubFetch(ctx context.Context, url string) (outbounds []any, identities []string, err error)
	// SanitizeSubURL 校验订阅 URL（仅 http/https + 非内网）。
	SanitizeSubURL(ctx context.Context, url string) (string, error)
	// ThirdPartyFetch 拉取第三方机场订阅，返回节点链接。
	ThirdPartyFetch(ctx context.Context, url string) ([]string, error)

	// Manifest 读面板上的 agent manifest；不存在返回 (nil, nil)。
	Manifest(ctx context.Context) (*ManifestAgent, error)
}

// NodeConfig 是节点域额外需要的配置（P8d 的 Config 结构在本任务边界内不可改）。
type NodeConfig struct {
	// AppKey 是 APP_KEY 解出的 32 字节（加密 node_secret / reality 私钥 / warp 私钥）。
	AppKey []byte
	// PanelURL 对齐 config('app.url')（install_command 用）；空则回落 http://localhost。
	PanelURL string
	// PanelURLV6 是面板的 IPv6 地址（APP_URL_V6，可选，issue #88）。
	// 填了才会在节点安装命令里带 --hub-v6，agent 才有 v6 兜底通道；留空 = 行为与改动前一致。
	PanelURLV6 string
	// RealityScanWait 对齐 config('panel.reality_scan_wait', 18) 秒。
	RealityScanWait int
	// OutboundTestWait 是出站连通测试的等待上限（PHP 写死 8 秒）。
	OutboundTestWait int
	// HopWait 是端口跳跃指令的等待上限（秒）。
	//
	// 比 reality_scan 长：firewall_hop_apply 首次可能先 apt-get 装 iptables，
	// 再写链 + 读回自验 + netfilter-persistent save，正常 2-5 秒，缺依赖时十几秒。
	HopWait int
	// SysctlWait 是 sysctl_apply（系统调优 / BBR）的等待上限（秒）。
	//
	// 比端口跳跃短：写一个 conf + modprobe + sysctl -p + 读回 /proc/sys，正常 1-2 秒；
	// 极端情况（内核模块要现 disk 上找）也只要几秒。
	SysctlWait int
	// OutboundSubFetchTimeout 是订阅拉取超时（PHP 30s）。
	OutboundSubFetchTimeout int
	// ThirdPartyFetchTimeout 是第三方拉取超时（PHP 15s）。
	ThirdPartyFetchTimeout int
}

// 默认等待时长（与 PHP 常量一致）。
const (
	defaultRealityScanWait  = 18
	defaultOutboundTestWait = 8
	// defaultHopWait 覆盖「缺 iptables 时 apt-get 安装」这条最慢路径。
	defaultHopWait = 40
	// defaultSysctlWait 覆盖「模块现加载 + sysctl -p + 读回」的正常路径。
	defaultSysctlWait = 15
)

// nodeConfigDefaults 补齐零值配置。
func nodeConfigDefaults(c NodeConfig) NodeConfig {
	if c.PanelURL == "" {
		c.PanelURL = "http://localhost"
	}
	if c.RealityScanWait <= 0 {
		c.RealityScanWait = defaultRealityScanWait
	}
	if c.OutboundTestWait <= 0 {
		c.OutboundTestWait = defaultOutboundTestWait
	}
	if c.HopWait <= 0 {
		c.HopWait = defaultHopWait
	}
	if c.SysctlWait <= 0 {
		c.SysctlWait = defaultSysctlWait
	}
	return c
}

// jsonRaw 把 JSON 文本保留为 json.RawMessage（nil 保持 nil，避免输出 "null"）。
func jsonRaw(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}
