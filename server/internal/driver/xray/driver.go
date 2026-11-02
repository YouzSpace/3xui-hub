package xray

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/driver/internal/panel"
)

// 逐条对齐 backend/app/Drivers/Xray/XrayDriver.php（xray 裸内核节点驱动，双通道）。
//
//   - 用户级操作 → 入队指令（adu/rmu），agent 经 WS 推送 / HTTP 长轮询取走本地执行
//     （秒级生效，不等回执）；
//   - 全量 config 只在结构变更时由 agent 拉取（本包 Render 负责）；
//   - 流量来自 agent 主动推送（WS report / HTTP push），本驱动不做面板侧轮询。
//
// 多入站口径（用户全协议通用）：
//   - adu/rmu 对节点上全部启用入站逐条下发，不按 users.protocol 过滤；
//   - 订阅链接对全部启用入站逐条生成；
//   - 只排除两类入站：①「单凭据协议」ss 非 2022（method+password 在入站级、所有用户
//     共用）；②「无用户概念协议」socks / http / tunnel / wireguard（内核 adu/rmu 对它们
//     一律 unsupported）。ss 2022 多用户凭据在用户维度，正常走 adu/rmu。
//
// 状态口径：
//   - GetClient 从 users 表解析（email = ch_user_{id}），代表「该用户应在此节点上」；
//     内核侧实际状态由指令队列推进（P1 不做强一致对账，P3 对账任务负责）。
//   - enable=false 的语义 = 内核移除该用户（rmu，秒级断连）；启用 = adu 重加。

// HeartbeatFreshSeconds 是心跳新鲜阈值（秒）：超过视为离线（agent 心跳周期 30s 的 4 倍）。
const HeartbeatFreshSeconds = 120

// 指令动作常量（与 nodeapi.Action* 逐字一致；此处复制一份避免驱动包反向依赖 nodeapi）。
const (
	ActionAddUser    = "adu"
	ActionRemoveUser = "rmu"
)

// Command 是一条待入队的节点指令。
type Command struct {
	NodeID  int64
	UserID  *int64
	Action  string
	Payload []byte
}

// UserRecord 是驱动需要的 users 行。
type UserRecord struct {
	ID           int64
	UUID         string
	Protocol     string
	PlanID       *int64
	Enabled      bool
	TrafficLimit int64
	ExpiredAt    *time.Time
}

// Source 是驱动的数据来源（生产实现读 MySQL；测试给内存数据）。
//
// 驱动层刻意不持有 *sql.DB：xray 的「状态」全部来自 users/xray_inbounds 两张表，
// 用接口隔离后，能力判定与指令载荷可完整单测（不需要起数据库）。
type Source interface {
	// UserByID 读 users 行；不存在返回 (nil, nil)。
	UserByID(ctx context.Context, id int64) (*UserRecord, error)
	// EnabledInbounds 读该节点启用的 xray_inbounds（按 sort, id 升序）。
	EnabledInbounds(ctx context.Context, nodeID int64) ([]Inbound, error)
	// UsersWithPlan 返回 plan_id 非空的全部用户（listClients 口径）。
	UsersWithPlan(ctx context.Context) ([]UserRecord, error)
	// TrafficSnapshot 读 traffic_snapshots 里该 user+node 的一行（up/down 累计值）。
	TrafficSnapshot(ctx context.Context, userID, nodeID int64) (up, down int64, found bool, err error)
	// RelayClients 读该节点全部入站上的中转凭据（按 id 升序），
	// 对齐 XrayRelayClient::clientsForInbound —— adu 载荷要与全量渲染口径一致。
	RelayClients(ctx context.Context, nodeID int64) ([]RelayClient, error)
}

// Enqueuer 把指令落进 node_api_commands（业务层实现，需保证 status='pending'）。
type Enqueuer interface {
	Enqueue(ctx context.Context, cmd Command) error
}

// Config 是驱动的构造参数。
type Config struct {
	NodeID       int64
	NodeName     string
	NodeHost     string
	DriverConfig []byte // driver_config 原始 JSON（xray_version 用）
	LastCheckAt  *time.Time
	// Key 是 APP_KEY（解密订阅链接里用不到，渲染用；此驱动不放这里）。
	Source   Source
	Enqueuer Enqueuer
	// Now 可注入（测试固定时间）。
	Now func() time.Time
}

// Driver 是 xray 节点驱动。
type Driver struct {
	cfg Config
}

// capabilityList 与 PHP XrayDriver::CAPABILITIES 逐条一致、顺序也一致。
var capabilityList = []string{
	panel.CapClientCreate,
	panel.CapClientRead,
	panel.CapClientUpdate,
	panel.CapClientDelete,
	panel.CapClientList,
	panel.CapClientToggle,
	panel.CapTrafficSync,
	panel.CapTrafficReset,
	panel.CapSubscriptionLinks,
	panel.CapHealthCheck,
}

var capabilitySet = func() map[string]bool {
	m := make(map[string]bool, len(capabilityList))
	for _, c := range capabilityList {
		m[c] = true
	}
	return m
}()

// NewDriver 构造驱动。
func NewDriver(cfg Config) *Driver {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Driver{cfg: cfg}
}

// Name 对应 name()。
func (d *Driver) Name() string { return "xray" }

// Version 对应 version()：driver_config.xray_version，缺省 v26.3.27。
func (d *Driver) Version() string {
	if len(d.cfg.DriverConfig) > 0 {
		if v := jsonStringField(d.cfg.DriverConfig, "xray_version"); v != "" {
			return v
		}
	}
	return "v26.3.27"
}

// Supports 对应 supports()。
func (d *Driver) Supports(capability string) bool { return capabilitySet[capability] }

// Capabilities 对应 capabilities()。
func (d *Driver) Capabilities() []string {
	out := make([]string, len(capabilityList))
	copy(out, capabilityList)
	return out
}

// ── 客户管理（指令通道：入队即返回，agent 秒级取走执行） ──────────

// CreateClient 对应 createClient()：users 表是 uuid 的事实源。
func (d *Driver) CreateClient(ctx context.Context, data map[string]any, inboundIDs []int64) (map[string]any, error) {
	email, _ := data["email"].(string)
	if email == "" {
		return nil, nil
	}

	uuid := stringOfAny(data["uuid"])
	if uuid == "" {
		uuid = stringOfAny(data["id"])
	}

	user, err := d.userForEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if user != nil && user.UUID != "" {
		uuid = user.UUID // users 表是事实源
	}
	if uuid == "" {
		return nil, nil
	}

	if err := d.enqueueAdd(ctx, email, uuid); err != nil {
		return nil, err
	}
	return map[string]any{"uuid": uuid, "email": email}, nil
}

// UpdateClient 对应 updateClient()：enable=false 即开关指令（rmu），否则 adu 重加。
func (d *Driver) UpdateClient(ctx context.Context, identifier string, data map[string]any, inboundID *int64) error {
	if enable, ok := data["enable"]; ok && !truthyAny(enable) {
		return d.enqueueRemove(ctx, identifier)
	}

	user, err := d.userForEmail(ctx, identifier)
	if err != nil {
		return err
	}

	uuid := stringOfAny(data["uuid"])
	if uuid == "" {
		uuid = stringOfAny(data["id"])
	}
	if uuid == "" && user != nil {
		uuid = user.UUID
	}
	if uuid == "" {
		return errors.New("缺少 uuid：该用户不存在或未分配 uuid")
	}

	return d.enqueueAdd(ctx, identifier, uuid)
}

// ToggleClient 对应 toggleClient()。
func (d *Driver) ToggleClient(ctx context.Context, identifier string, enable bool) error {
	if !enable {
		return d.enqueueRemove(ctx, identifier)
	}
	user, err := d.userForEmail(ctx, identifier)
	if err != nil {
		return err
	}
	if user == nil || user.UUID == "" {
		return fmt.Errorf("用户 %s 不存在或缺少 uuid", identifier)
	}
	return d.enqueueAdd(ctx, identifier, user.UUID)
}

// DeleteClient 对应 deleteClient()。
func (d *Driver) DeleteClient(ctx context.Context, identifier string, keepTraffic bool) error {
	return d.enqueueRemove(ctx, identifier)
}

// GetClient 对应 getClient()：从 users 表解析（代表「该用户应在此节点上」）。
func (d *Driver) GetClient(ctx context.Context, identifier string) (map[string]any, error) {
	user, err := d.userForEmail(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, nil
	}

	client := map[string]any{
		"id":    user.UUID,
		"uuid":  user.UUID,
		"email": identifier,
		// enable 的语义 = 「有套餐 且 账号启用」（不含 traffic_disabled ——
		// 那是「关流量」的标记，用户在节点上仍应能登录续费，见 behavior-contract §4）。
		"enable":     user.PlanID != nil && user.Enabled,
		"totalGB":    user.TrafficLimit,
		"expiryTime": 0,
	}
	if user.ExpiredAt != nil {
		client["expiryTime"] = user.ExpiredAt.Unix() * 1000
	}
	return client, nil
}

// ListClients 对应 listClients()：有套餐的用户（plan_id 非空）。
func (d *Driver) ListClients(ctx context.Context) ([]map[string]any, error) {
	users, err := d.cfg.Source.UsersWithPlan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"email":      ClientEmail(u.ID),
			"uuid":       u.UUID,
			"inboundIds": []int64{},
		})
	}
	return out, nil
}

// ── 流量 ────────────────────────────────────────────────────

// GetClientTraffic 对应 getClientTraffic()：读 traffic_snapshots 的累计值。
func (d *Driver) GetClientTraffic(ctx context.Context, identifier string) (map[string]any, error) {
	userID, ok := UserIDFromEmail(identifier)
	if !ok {
		return nil, nil
	}
	up, down, found, err := d.cfg.Source.TrafficSnapshot(ctx, userID, d.cfg.NodeID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return map[string]any{"up": up, "down": down}, nil
}

// ResetClientTraffic 对应 resetClientTraffic()：**no-op**。
//
// 「绝对值快照 + 增量落账」口径下，清零 users.traffic_used 后下一轮推送的
// delta = now - last 会继续正常累计（快照自带面板侧重置分支），无需动内核计数；
// 内核侧计数清理由采集层的强制断连链路统一处理。
func (d *Driver) ResetClientTraffic(ctx context.Context, identifier string) error { return nil }

// ── 入站（入站列表在 xray_inbounds 自建；驱动不暴露面板式入站挂载） ──

// ListInbounds 对应 listInbounds()：返回空（xray 节点无面板式入站）。
func (d *Driver) ListInbounds(ctx context.Context) ([]map[string]any, error) {
	return []map[string]any{}, nil
}

// GetInbound 对应 getInbound()：返回 (nil, nil)。
func (d *Driver) GetInbound(ctx context.Context, id int64) (map[string]any, error) {
	return nil, nil
}

// GetClientStatsGroupedByInbound 对应 getClientStatsGroupedByInbound()：返回空。
func (d *Driver) GetClientStatsGroupedByInbound(ctx context.Context) (map[int64]map[string]panel.ClientTraffic, error) {
	return map[int64]map[string]panel.ClientTraffic{}, nil
}

// GetClientStatsByEmail 对应 getClientStatsByEmail()：
// 面板侧轮询不提供流量（agent push 通道负责）—— 返回空，轮询路径自然跳过。
func (d *Driver) GetClientStatsByEmail(ctx context.Context) (map[string]panel.ClientTraffic, error) {
	return map[string]panel.ClientTraffic{}, nil
}

// GetClientLinks 对应 getClientLinks()：按「用户协议 == 入站协议」的启用入站逐条生成。
func (d *Driver) GetClientLinks(ctx context.Context, identifier string) ([]string, error) {
	user, err := d.userForEmail(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if user == nil || user.UUID == "" {
		return []string{}, nil
	}

	inbounds, err := d.cfg.Source.EnabledInbounds(ctx, d.cfg.NodeID)
	if err != nil {
		return nil, err
	}

	node := Node{ID: d.cfg.NodeID, Name: d.cfg.NodeName, Host: d.cfg.NodeHost, DriverType: "xray"}
	u := User{
		ID:           user.ID,
		UUID:         user.UUID,
		Protocol:     user.Protocol,
		TrafficLimit: user.TrafficLimit,
		ExpiredAt:    user.ExpiredAt,
	}
	return SubscriptionURIs(node, inbounds, u), nil
}

// AttachClient 对应 attachClient()：xray 多入站按协议自动匹配用户，无面板式挂载概念
// —— 协议切换链路只对有 3x-ui 入站记录的节点生效，xray 节点不会被触达。保持 no-op。
func (d *Driver) AttachClient(ctx context.Context, identifier string, inboundIDs []int64) error {
	return nil
}

// DetachClient 对应 detachClient()：同 AttachClient，no-op。
func (d *Driver) DetachClient(ctx context.Context, identifier string, inboundIDs []int64) error {
	return nil
}

// ── 健康检查（心跳新鲜度，不做面板请求） ─────────────────────

// HealthCheck 对应 healthCheck()：心跳超时即离线；在线时补一次入站端口 TCP 握手延迟。
func (d *Driver) HealthCheck(ctx context.Context) (panel.Health, error) {
	last := d.cfg.LastCheckAt
	if last == nil {
		return panel.Health{OK: false, LatencyMs: 0, Error: "节点尚未注册（无心跳）"}, nil
	}

	age := int64(d.cfg.Now().Sub(*last).Seconds())
	if age > HeartbeatFreshSeconds {
		return panel.Health{OK: false, LatencyMs: 0, Error: fmt.Sprintf("心跳超时（%ds 未上报）", age)}, nil
	}

	return panel.Health{OK: true, LatencyMs: d.tcpLatencyMs(ctx)}, nil
}

// tcpLatencyMs 复刻 tcpLatencyMs()：面板 → 节点第一个启用入站端口的 TCP 握手延迟。
//
// xray 节点没有可请求的面板 API，健康检查原本只判心跳新鲜度、latency 恒 0，
// 前端只能显示「—」。这里取「连入站端口」的握手耗时 —— 与 3x-ui 节点的
// 「面板 API 耗时」同义（都是面板到这个节点的网络往返）。无 host / 无启用
// 入站 / 连不上时返回 0（显示「—」），不影响 online 判定（心跳才是主判据）。
func (d *Driver) tcpLatencyMs(ctx context.Context) int64 {
	host := strings.TrimSpace(d.cfg.NodeHost)
	port := d.firstEnabledPort(ctx)
	if host == "" || port <= 0 {
		return 0
	}
	return probeTCPLatency(ctx, host, port)
}

// TCPInbound 是量延迟所需的最小入站信息（协议 + 端口）。
//
// 单独抽出来是为了让 collector（走 xray 驱动）和后台「测试」按钮（走 RealNodeOps）
// 用同一套口径 —— 两条路径各写一份迟早会分叉，第一版就分叉了。
type TCPInbound struct {
	Protocol string
	Port     int64
}

// TCPLatencyMs 量一次「面板 → 节点第一个 TCP 入站端口」的握手延迟（毫秒）。
//
// 返回 0 表示「测不到」（无 host / 没有 TCP 入站 / 连不上）—— 前端据此显示「—」。
// 只挑 TCP 协议的入站：hysteria 走 QUIC/UDP，对它 dial TCP 永远失败。
func TCPLatencyMs(ctx context.Context, host string, inbounds []TCPInbound) int64 {
	host = strings.TrimSpace(host)
	if host == "" {
		return 0
	}
	port := firstTCPPort(inbounds)
	if port <= 0 {
		return 0
	}
	return probeTCPLatency(ctx, host, port)
}

// firstTCPPort 取入站清单里最小端口的 TCP 协议入站；没有则返回 0。
func firstTCPPort(inbounds []TCPInbound) int64 {
	best := int64(0)
	for _, ib := range inbounds {
		if ib.Port <= 0 || !isTCPProtocol(ib.Protocol) {
			continue
		}
		if best == 0 || ib.Port < best {
			best = ib.Port
		}
	}
	return best
}

// firstEnabledPort 取该节点最小端口的**TCP** 启用入站。
//
// ⚠️ 必须排除 UDP 协议（hysteria）：延迟是用 TCP 握手量的，连 UDP 端口必然失败 →
// 恒返回 0 → 前端只能显示「—」。只按最小端口挑会踩这个坑：一个节点同时有
// hy2(2083/udp) 和 vless(6443/tcp) 时，2083 更小，于是一直测不到延迟。
// 实测四个裸内核节点的最小端口全是 HY2，面板侧延迟全部为 0。
func (d *Driver) firstEnabledPort(ctx context.Context) int64 {
	if d.cfg.Source == nil {
		return 0
	}
	inbounds, err := d.cfg.Source.EnabledInbounds(ctx, d.cfg.NodeID)
	if err != nil {
		return 0
	}
	ports := make([]TCPInbound, 0, len(inbounds))
	for _, ib := range inbounds {
		ports = append(ports, TCPInbound{Protocol: ib.Protocol, Port: ib.Port})
	}
	return firstTCPPort(ports)
}

// isTCPProtocol 判断该入站协议是否走 TCP（可用来量 TCP 握手延迟）。
//
// hysteria 是 QUIC/UDP，对它做 TCP dial 永远失败；其余（vless / vmess / trojan /
// shadowsocks）都是 TCP。未知协议按 TCP 处理：宁可多试一次，也不要让延迟恒为 0。
func isTCPProtocol(protocol string) bool {
	return !strings.EqualFold(strings.TrimSpace(protocol), "hysteria")
}

// probeTCPLatency 测 host:port 的 TCP 连接耗时（毫秒），失败返回 0。
//
// PHP 用 2 秒超时；这里用 ctx（业务层给 2s）并在成功时 max(ms, 1)
// （0ms 与「测不到」语义冲突，PHP 侧同样 max($ms, 1)）。
func probeTCPLatency(ctx context.Context, host string, port int64) int64 {
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(host, strconv.FormatInt(port, 10)))
	if err != nil {
		return 0
	}
	ms := time.Since(start).Milliseconds()
	_ = conn.Close()
	if ms < 1 {
		ms = 1
	}
	return ms
}

// ── 指令入队（多入站：每协议匹配的启用入站一条指令） ──────────

// enqueueAdd 对应 enqueueAdd()：为每个协议匹配的启用入站投一条 adu。
func (d *Driver) enqueueAdd(ctx context.Context, email, uuid string) error {
	user, err := d.userForEmail(ctx, email)
	if err != nil {
		return err
	}
	if user == nil {
		return nil // 面板用户必须已入库（调用方先建 users 行）
	}
	if uuid != "" && user.UUID == "" {
		user.UUID = uuid // 极端情况下用户表缺 uuid，用传入值渲染
	}

	inbounds, err := d.inboundsFor(ctx)
	if err != nil {
		return err
	}
	relayClients, err := d.relayClientsFor(ctx)
	if err != nil {
		return err
	}

	for _, ib := range inbounds {
		fragment, err := ADUInboundFragment(ib, toRenderUser(user), relayClients, nil)
		if err != nil {
			return err
		}
		payload, err := marshalPayload(map[string]any{
			"inbounds": []any{fragment},
		})
		if err != nil {
			return err
		}
		userID := user.ID
		if err := d.enqueue(ctx, Command{
			NodeID:  d.cfg.NodeID,
			UserID:  &userID,
			Action:  ActionAddUser,
			Payload: payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

// enqueueRemove 对应 enqueueRemove()：为每个协议匹配的启用入站投一条 rmu。
func (d *Driver) enqueueRemove(ctx context.Context, email string) error {
	user, err := d.userForEmail(ctx, email)
	if err != nil {
		return err
	}

	var inboundTags []string
	if user != nil {
		inbounds, err := d.inboundsFor(ctx)
		if err != nil {
			return err
		}
		for _, ib := range inbounds {
			inboundTags = append(inboundTags, ib.Tag)
		}
	}

	for _, tag := range inboundTags {
		payload, err := marshalPayload(map[string]any{
			"tag":    tag,
			"emails": []any{email},
		})
		if err != nil {
			return err
		}
		var userID *int64
		if user != nil {
			id := user.ID
			userID = &id
		}
		if err := d.enqueue(ctx, Command{
			NodeID:  d.cfg.NodeID,
			UserID:  userID,
			Action:  ActionRemoveUser,
			Payload: payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

// inboundsFor 对应 inboundsFor()：该节点上**全部启用入站**，不按 users.protocol 过滤
// （用户全协议通用：任何协议的用户都拿到每个入站的链接）。
//
// 只排除两类「不按用户走指令通道」的入站：
//   - 单凭据协议：ss 非 2022 时 method+password 在入站级，所有用户共用一套；
//   - 无用户概念协议：socks / http / tunnel / wireguard（见 userlessProtocols）。
//
// 这两类发 adu/rmu 都是空转：agent 把指令转给内核，内核回 unsupported，指令却仍记
// success —— 看着「下发成功」，实际什么也没发生，白占指令队列与回执轮询。
func (d *Driver) inboundsFor(ctx context.Context) ([]Inbound, error) {
	all, err := d.cfg.Source.EnabledInbounds(ctx, d.cfg.NodeID)
	if err != nil {
		return nil, err
	}
	out := make([]Inbound, 0, len(all))
	for _, ib := range all {
		if userlessProtocol(ib.Protocol) {
			continue
		}
		if ib.Protocol == "shadowsocks" && !SSIsMultiUser(ib) {
			continue
		}
		out = append(out, ib)
	}
	return out, nil
}

// userlessProtocols 是「没有用户概念」的入站协议：凭据在入站级（socks/http 一组账号
// 密码、wireguard 一份对端公钥清单），或者根本没有凭据（tunnel 是 dokodemo-door 的
// 固定目标转发）。内核的 AddUser/RemoveUser 对它们一律 unsupported。
var userlessProtocols = map[string]bool{
	"socks":     true,
	"http":      true,
	"tunnel":    true,
	"wireguard": true,
}

// userlessProtocol 判断该协议是否「无用户概念」（不走 adu/rmu 指令通道）。
func userlessProtocol(protocol string) bool { return userlessProtocols[protocol] }

// relayClientsFor 读该节点入站上的中转凭据（adu 载荷要和渲染口径一致地带上）。
func (d *Driver) relayClientsFor(ctx context.Context) ([]RelayClient, error) {
	if d.cfg.Source == nil {
		return nil, nil
	}
	return d.cfg.Source.RelayClients(ctx, d.cfg.NodeID)
}

func (d *Driver) enqueue(ctx context.Context, cmd Command) error {
	if d.cfg.Enqueuer == nil {
		return errors.New("驱动未配置 Enqueuer，无法投递节点指令")
	}
	return d.cfg.Enqueuer.Enqueue(ctx, cmd)
}

func (d *Driver) userForEmail(ctx context.Context, email string) (*UserRecord, error) {
	userID, ok := UserIDFromEmail(email)
	if !ok {
		return nil, nil
	}
	return d.cfg.Source.UserByID(ctx, userID)
}

// ── 口径工具 ────────────────────────────────────────────────

var emailPattern = regexp.MustCompile(`^ch_user_(\d+)$`)

// UserIDFromEmail 对应 userIdFromEmail()：解析 ch_user_{id}。
func UserIDFromEmail(email string) (int64, bool) {
	m := emailPattern.FindStringSubmatch(email)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// 编译期断言。
var _ panel.Driver = (*Driver)(nil)
