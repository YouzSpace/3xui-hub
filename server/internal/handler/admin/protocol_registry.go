package admin

// 入站协议能力表（issue #137：协议扩展点收敛）。
//
// 这张表是「协议 → 支持项 / 必填项 / 缺省值 / 渲染入口」的唯一出处。改动前的现状是：
// 协议白名单写在 validateInboundPayload 的 `in:...` 规则里、组合校验写在
// buildInboundInput 的几个 if 里、settings 形状写在一个 switch 里、回显特判写在
// present 里 —— 新增一个协议要改四五处，漏一处就是线上事故（例如 HY2 少一条
// network 白名单，整条入站根本建不出来）。
//
// 现在新增协议 = 在 inboundProtocolTable 里加一行（外加前端下拉一项）。
//
// 刻意不做的事：
//   - 不引入 interface / Adapter 层。这里的协议差异只是「一组字符串 + 一个构造函数」，
//     套一层 interface 只会多一跳，读代码还得来回找实现。
//   - 协议专有逻辑不搬家：HY2 的 QUIC/端口跳跃/混淆、wireguard 的对端解析、
//     SS-2022 的密钥生成仍留在各自文件里，表里只放「入口」指针。

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// panelInboundTransports / panelInboundSecurities 是没有协议级限制的那些行共用的取值：
// 面板校验白名单里的传输层与安全层全量（hysteria 传输层只属于 HY2，见它的行）。
//
// 校验白名单本身由整张表**并集**算出（见 inboundProtocolNetworks / inboundProtocolSecurities），
// 顺序即并集顺序，与改动前 validateInboundPayload 里写死的
// `in:tcp,ws,grpc,xhttp,httpupgrade,hysteria` / `in:none,tls,reality` 逐字一致。
var (
	panelInboundTransports = []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}
	panelInboundSecurities = []string{"none", "tls"}
)

// protocolDefault 是协议的一项缺省值（key 用面板的 payload/落库字段名）。
type protocolDefault struct {
	Key   string
	Value any
}

// inboundProtocolSpec 是能力表的一行。
type inboundProtocolSpec struct {
	// Name 是协议名，三处同一个词：payload.protocol、xray_inbounds.protocol、前端下拉。
	Name string

	// Transports / Securities 声明该协议支持的传输层与安全层：
	//   - 没有协议级组合限制的协议，列的就是白名单全量。**别为了「表达意图」把某行写窄**：
	//     现状是这些协议的传输层不做协议级限制，写窄要么是假话，要么等于新增一条校验（改行为）；
	//   - 唯一有组合限制的是 shadowsocks（tcp + 无安全层）：它列窄并带 RejectMessage。
	Transports []string
	Securities []string

	// RealityTransports 非空表示该协议支持 reality 安全层，且 reality 下只支持列出的传输层
	// （现状仅 vless：tcp / xhttp / grpc）。空 = 该协议不支持 reality。
	RealityTransports []string

	// RejectMessage 是「传输层/安全层组合不被该协议支持」时的报错文案（空 = 不校验）。
	// 文案必须与改动前逐字一致。
	RejectMessage string

	// Normalize 为真时，建入站无条件把传输层/安全层改写成 ForcedNetwork/ForcedSecurity：
	// hysteria 内核只认 hysteria + tls；wireguard 没有传输层（一律空串）。
	Normalize      bool
	ForcedNetwork  string
	ForcedSecurity string

	// Stream 为假时不写 streamSettings 的 network/security（wireguard）：写入虽然会被内核
	// 忽略，但列表页会把 wireguard 显示成 TCP 入站，误导管理员。
	Stream bool

	// PresentNetwork 是回显时列表页显示的传输层（空 = 读 streamSettings.network）。
	PresentNetwork string

	// Required 声明该协议的业务必填项（校验与文案在 Build 里，这里负责「别漏项」有据可查，
	// protocol_registry_test.go 会逐项验证「缺了就报错」）：
	//   - ss_password：传统 AEAD 必填；2022 多用户由面板现场生成一把合法密钥，所以不算缺；
	//   - tls_cert_id：HY2 必须引用一张证书；没有时面板先从证书库自动挑一张再判。
	Required []string

	// Defaults 是该协议的缺省值（有序）。Build 里按 key 取用，别在代码里再写一遍字面量。
	Defaults []protocolDefault

	// Build 是该协议 settings 段的渲染入口（buildInboundInput 的 settings 分支）。
	Build func(*inboundBuild) (*jsObj, bool)
}

// inboundProtocolTable 是能力表本体：一行一个协议，顺序 = 校验白名单里协议名的顺序
// （与改动前 `in:vless,vmess,trojan,shadowsocks,hysteria,socks,http,tunnel,wireguard` 一致）。
var inboundProtocolTable = []inboundProtocolSpec{
	{
		Name:       "vless",
		Transports: panelInboundTransports,
		// reality 是 vless 独有的安全层（也是校验白名单里 reality 的唯一来源）。
		Securities:        []string{"none", "tls", "reality"},
		RealityTransports: []string{"tcp", "xhttp", "grpc"},
		Stream:            true,
		Defaults:          []protocolDefault{{Key: "decryption", Value: "none"}},
		Build:             buildVLESSInboundSettings,
	},
	{
		Name:       "vmess",
		Transports: panelInboundTransports,
		Securities: panelInboundSecurities,
		Stream:     true,
		Build:      buildPlainInboundSettings,
	},
	{
		Name:       "trojan",
		Transports: panelInboundTransports,
		Securities: panelInboundSecurities,
		Stream:     true,
		Build:      buildPlainInboundSettings,
	},
	{
		Name:          "shadowsocks",
		Transports:    []string{"tcp"},
		Securities:    []string{"none"},
		RejectMessage: "Shadowsocks 仅支持 tcp + 无附加安全层",
		Stream:        true,
		Required:      []string{"ss_password"},
		// 默认 2022-blake3-aes-256-gcm（多用户）：与前端 emptyForm 默认一致，
		// 新建 ss 入站开箱即按用户区分流量（对齐 3x-ui 默认）。
		Defaults: []protocolDefault{{Key: "method", Value: "2022-blake3-aes-256-gcm"}},
		Build:    buildShadowsocksInboundSettings,
	},
	{
		Name:           "hysteria",
		Transports:     []string{"hysteria"},
		Securities:     []string{"tls"},
		Normalize:      true,
		ForcedNetwork:  "hysteria",
		ForcedSecurity: "tls",
		Stream:         true,
		Required:       []string{"tls_cert_id"},
		// 内核只接受 version=2；写 1 会让**整份节点配置**被拒（不是只挂这个入站）。
		Defaults: []protocolDefault{{Key: "version", Value: int64(2)}},
		Build:    buildHysteriaInboundSettings,
	},
	{
		Name:       "socks",
		Transports: panelInboundTransports,
		Securities: panelInboundSecurities,
		Stream:     true,
		Required:   []string{"auth_user", "auth_pass"},
		Build:      buildSocksHTTPInboundSettings,
	},
	{
		Name:       "http",
		Transports: panelInboundTransports,
		Securities: panelInboundSecurities,
		Stream:     true,
		Required:   []string{"auth_user", "auth_pass"},
		Build:      buildSocksHTTPInboundSettings,
	},
	{
		Name:       "tunnel",
		Transports: panelInboundTransports,
		Securities: panelInboundSecurities,
		Stream:     true,
		Required:   []string{"tunnel_address", "tunnel_port"},
		// L4 网络（settings.network，不是传输层）：缺省与 tunnelNetwork() 同一个常量。
		Defaults: []protocolDefault{{Key: "network", Value: defaultTunnelNetwork}},
		Build:    buildTunnelInboundSettings,
	},
	{
		Name:           "wireguard",
		Transports:     nil,
		Securities:     []string{"none"},
		Normalize:      true,
		ForcedNetwork:  "",
		ForcedSecurity: "none",
		Stream:         false,
		PresentNetwork: "udp",
		Defaults:       []protocolDefault{{Key: "mtu", Value: int64(1420)}},
		Build:          buildWireGuardInboundSettings,
	},
}

// lookupInboundProtocol 查表（协议名不存在 → false）。
func lookupInboundProtocol(name string) (*inboundProtocolSpec, bool) {
	for i := range inboundProtocolTable {
		if inboundProtocolTable[i].Name == name {
			return &inboundProtocolTable[i], true
		}
	}
	return nil, false
}

// inboundProtocolOf 取协议行；查不到时给一个「空 settings + 常规 stream」的兜底行。
//
// 校验层已经拦掉非法协议，这里兜底只是让 buildInboundInput / present 不必到处判空：
// 兜底行的行为与改动前 switch 的 default 分支等价（settings = {}、stream 照写）。
func inboundProtocolOf(name string) *inboundProtocolSpec {
	if spec, ok := lookupInboundProtocol(name); ok {
		return spec
	}
	return &inboundProtocolSpec{Name: name, Stream: true, Build: buildPlainInboundSettings}
}

// inboundProtocolNames 是校验白名单里的协议名（顺序 = 表顺序）。
func inboundProtocolNames() []string {
	out := make([]string, 0, len(inboundProtocolTable))
	for i := range inboundProtocolTable {
		out = append(out, inboundProtocolTable[i].Name)
	}
	return out
}

// inboundProtocolNetworks 是校验白名单里的传输层：整张表的并集（顺序 = 首次出现顺序）。
func inboundProtocolNetworks() []string {
	return unionProtocolValues(func(spec *inboundProtocolSpec) []string { return spec.Transports })
}

// inboundProtocolSecurities 是校验白名单里的安全层：整张表的并集（顺序 = 首次出现顺序）。
func inboundProtocolSecurities() []string {
	return unionProtocolValues(func(spec *inboundProtocolSpec) []string { return spec.Securities })
}

// unionProtocolValues 按表顺序把某一列去重拼接（稳定顺序，供 `in:...` 规则用）。
func unionProtocolValues(pick func(*inboundProtocolSpec) []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for i := range inboundProtocolTable {
		for _, v := range pick(&inboundProtocolTable[i]) {
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// inRule 生成 Laravel `in:...` 规则的取值部分。
func inRule(values []string) string { return "in:" + strings.Join(values, ",") }

// defaultOf 取一项缺省值。
func (s *inboundProtocolSpec) defaultOf(key string) (any, bool) {
	for _, d := range s.Defaults {
		if d.Key == key {
			return d.Value, true
		}
	}
	return nil, false
}

// defaultString 取字符串缺省值（无则空串）。
func (s *inboundProtocolSpec) defaultString(key string) string {
	if v, ok := s.defaultOf(key); ok {
		return nodeScalarString(v)
	}
	return ""
}

// defaultInt 取整数缺省值（无则 0）。
func (s *inboundProtocolSpec) defaultInt(key string) int64 {
	if v, ok := s.defaultOf(key); ok {
		return inputInt(v)
	}
	return 0
}

// inboundBuild 是协议 Build 入口的上下文：只给渲染入口需要的东西，
// 免得每个协议都拿一遍 gin.Context / store。
type inboundBuild struct {
	domain   *NodeDomain
	c        *gin.Context
	input    map[string]any
	existing *XrayInboundRecord
	spec     *inboundProtocolSpec
}

// text 读一个字符串字段（缺失 → 空串）。
func (b *inboundBuild) text(key string) string { return nodeStringField(b.input, key) }

// trimmed 读一个去首尾空白的字符串字段。
func (b *inboundBuild) trimmed(key string) string {
	return strings.TrimSpace(nodeStringField(b.input, key))
}

// present 读一个可选字段的原始值（缺失 → nil）。
func (b *inboundBuild) raw(key string) (any, bool) {
	v, ok := b.input[key]
	return v, ok
}

// prevSettings 取编辑前的 settings（解析失败 → nil，与 parseOrderedJS 的宽容口径一致）。
func (b *inboundBuild) prevSettings() any {
	if b.existing == nil {
		return nil
	}
	prev, _ := parseOrderedJS(b.existing.Settings)
	return prev
}

// fail 写业务失败响应（与改动前 d.fail 同一口径）。
func (b *inboundBuild) fail(msg string) {
	b.domain.fail(b.c, msg, 500)
}

// failServer 写服务器错误响应。
func (b *inboundBuild) failServer(what string, err error) {
	b.domain.failServer(b.c, what, err)
}
