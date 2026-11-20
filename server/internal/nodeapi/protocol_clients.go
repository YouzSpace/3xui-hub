package nodeapi

// 入站协议的「客户端凭据形态」表（渲染侧，issue #137 协议扩展点收敛）。
//
// 为什么要单独一张：nodeapi 是真正把配置下发给节点 agent 的渲染器，它 **不能** import
// internal/handler/admin（那边已经 import 了本包，会成环），所以协议能力表在 admin 侧
// （protocol_registry.go，管面板的校验/默认值/回显）与这里（管内核要的 client 形状）
// 各一张，两边用同一套协议名，新增协议要各加一行。
//
// 表里的事实（每一条都对应改动前 clientsFor 里的一个 switch 分支）：
//   - 用户凭据键：vless/vmess = id（uuid）、trojan = password、hysteria = auth、ss = password；
//   - 中转凭据键：trojan = password、hysteria = auth，其余（含 vless/vmess/ss）默认 id；
//   - 是否注入逐用户 client：只有 SS-2022 多用户形态的 shadowsocks 注入，传统 AEAD 是
//     入站级单凭据（凭据在 settings 里，没有用户维度）；
//   - flow：只有 vless 需要按 stream 追加 xtls-rprx-vision。

import "controlhub-server/internal/ss2022"

// inboundClientEnv 是一次入站渲染的共用状态：入站相关的一次性解析（目前只有
// shadowsocks 的 settings.method）在这里做一遍，所有用户复用 —— 节点可能挂着十万用户，
// 不能每个用户都重新解析一次 settings。
type inboundClientEnv struct {
	ib       RenderInbound
	ssMethod string
}

// inboundClientSpec 是能力表的一行。
type inboundClientSpec struct {
	// Protocol 是协议名（与 admin 侧协议能力表的 Name、xray_inbounds.protocol 同一个词）。
	Protocol string

	// RelayCredentialKey 是中转凭据（面板为来源节点自动生成的固定凭据）注入时用的键。
	RelayCredentialKey string

	// Flow 为真时，该协议的 client 要按 stream 追加 flow（现状只有 vless）。
	Flow bool

	// Prepare 每份入站跑一次：算好该协议渲染 client 需要的一次性状态，
	// 返回 false 表示这份入站不发逐用户凭据（nil = 恒发）。
	Prepare func(*inboundClientEnv) bool

	// Build 生成一个用户的 client 对象（键顺序即渲染顺序）。
	Build func(RenderUser, *inboundClientEnv) Object
}

// inboundClientTable 是能力表本体：顺序无关，按协议名查。
var inboundClientTable = []inboundClientSpec{
	{
		Protocol:           "vless",
		RelayCredentialKey: "id",
		Flow:               true,
		Build:              buildUUIDClient,
	},
	{
		Protocol:           "vmess",
		RelayCredentialKey: "id",
		Build:              buildUUIDClient,
	},
	{
		// trojan 以密码定位用户：统一用 uuid 作密码（面板侧保证唯一）。
		Protocol:           "trojan",
		RelayCredentialKey: "password",
		Build:              buildPasswordClient,
	},
	{
		// Hysteria2 以 auth 令牌定位用户（键名是 auth，不是 password/id）：
		// 沿用 uuid 作令牌，与订阅侧 hysteria2:// 链接的密码同源。
		Protocol:           "hysteria",
		RelayCredentialKey: "auth",
		Build:              buildAuthClient,
	},
	{
		// SS-2022 多用户：password 是该用户的 PSK（内核按它区分身份 / 统计流量），
		// 刻意不带 method —— 2022 多用户下内核拒绝 client 自带的 method。
		Protocol:           "shadowsocks",
		RelayCredentialKey: "id",
		Prepare:            prepareShadowsocksClients,
		Build:              buildShadowsocksClient,
	},
}

// lookupInboundClientSpec 查表。
func lookupInboundClientSpec(proto string) (*inboundClientSpec, bool) {
	for i := range inboundClientTable {
		if inboundClientTable[i].Protocol == proto {
			return &inboundClientTable[i], true
		}
	}
	return nil, false
}

// inboundClientSpecOf 取协议行；没有逐用户凭据形态的协议（socks / http / tunnel /
// wireguard 与未知协议）返回一行只有默认中转键的兜底行 —— 与改动前 switch 的
// default 分支一致：不注入用户 client，但中转凭据照注入（键 id）。
func inboundClientSpecOf(proto string) *inboundClientSpec {
	if spec, ok := lookupInboundClientSpec(proto); ok {
		return spec
	}
	return &inboundClientSpec{Protocol: proto, RelayCredentialKey: "id"}
}

// prepareShadowsocksClients 取代原来的 ssIsMultiUser：settings.method 只解析一次
// （渲染时要复用），顺带判定这份 ss 入站是否按用户发凭据 —— 传统 AEAD 是入站级
// 单凭据（凭据在 settings 里），2022 多用户才有逐用户 PSK。
func prepareShadowsocksClients(env *inboundClientEnv) bool {
	env.ssMethod = ssMethodOf(env.ib)
	return ss2022.IsMultiUser(env.ssMethod)
}

// buildUUIDClient 是 vless / vmess 的 client：id 即 uuid。
func buildUUIDClient(u RenderUser, _ *inboundClientEnv) Object {
	return Object{
		{Key: "id", Value: u.UUID},
		{Key: "email", Value: clientEmailOf(u)},
		{Key: "totalGB", Value: u.TrafficLimit},
		{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
	}
}

// buildPasswordClient 是 trojan 的 client：password 即 uuid。
func buildPasswordClient(u RenderUser, _ *inboundClientEnv) Object {
	return Object{
		{Key: "password", Value: u.UUID},
		{Key: "email", Value: clientEmailOf(u)},
		{Key: "totalGB", Value: u.TrafficLimit},
		{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
	}
}

// buildAuthClient 是 hysteria 的 client：auth 即 uuid。
func buildAuthClient(u RenderUser, _ *inboundClientEnv) Object {
	return Object{
		{Key: "auth", Value: u.UUID},
		{Key: "email", Value: clientEmailOf(u)},
		{Key: "totalGB", Value: u.TrafficLimit},
		{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
	}
}

// buildShadowsocksClient 是 SS-2022 的 client：password 是该用户的派生 PSK。
func buildShadowsocksClient(u RenderUser, env *inboundClientEnv) Object {
	return Object{
		{Key: "password", Value: ss2022.ClientKey(env.ssMethod, u.UUID)},
		{Key: "email", Value: clientEmailOf(u)},
		{Key: "totalGB", Value: u.TrafficLimit},
		{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
	}
}
