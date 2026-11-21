package admin

// 入站 settings 段的按协议渲染 —— 协议能力表 Build 字段的实现。
//
// 这些函数是 xray_inbound_input.go 里那个 settings switch 的各分支**原样搬过来**的
// （issue #137 协议扩展点收敛）：业务校验、报错文案、写库键序都没改，只是从
// 「按 protocol 分支」变成「表里指向哪个函数」。搬家前后的逐字节一致性由
// protocol_registry_test.go 的金样本锁定。

import (
	"strings"

	"controlhub-server/internal/driver/xray"
	"controlhub-server/internal/ss2022"
)

// buildVLESSInboundSettings vless：只有 decryption 一项（缺省 none）。
func buildVLESSInboundSettings(b *inboundBuild) (*jsObj, bool) {
	return jo("decryption", b.spec.defaultString("decryption")), true
}

// buildPlainInboundSettings 没有专有 settings 的协议（vmess / trojan，以及兜底行）：{}。
func buildPlainInboundSettings(*inboundBuild) (*jsObj, bool) { return jo(), true }

// buildHysteriaInboundSettings hysteria：内核只接受 version=2；写 1 会让**整份节点配置**
// 被拒（不是只挂这个入站）。
func buildHysteriaInboundSettings(b *inboundBuild) (*jsObj, bool) {
	return jo("version", b.spec.defaultInt("version")), true
}

// buildShadowsocksInboundSettings shadowsocks：method + 服务端 PSK（2022 走密钥链）。
func buildShadowsocksInboundSettings(b *inboundBuild) (*jsObj, bool) {
	method := b.spec.defaultString("method")
	if s, ok := inputString(b.input["ss_method"]); ok && s != "" {
		method = s
	}
	settings := jo("method", method)
	pw := strings.TrimSpace(nodeStringField(b.input, "ss_password"))
	if pw == "" && b.existing != nil && b.existing.Protocol == "shadowsocks" {
		// 编辑时留空 = 沿用原值（与 socks/http 的「不改不用重填」同口径）。
		pw = strings.TrimSpace(jsonGetString(b.prevSettings(), "password"))
	}
	if ss2022.KeyLen(method) > 0 {
		// SS-2022：服务端 PSK 必须是 base64 且解码后正好 16/32 字节，
		// 长度不对内核会 "bad key" 拒载**整份**节点配置。留空或非法就现场生成一把
		// 合法的（前端会把落库的值回显出来，管理员能看到/复制）。
		if !ss2022.ValidKey(method, pw) {
			gen, err := ss2022.GenerateKey(method)
			if err != nil {
				b.failServer("生成 Shadowsocks-2022 密钥", err)
				return nil, false
			}
			pw = gen
		}
	} else if pw == "" {
		// 传统 AEAD 的 password 是自由格式：不校验、不生成，只要求非空。
		b.fail("Shadowsocks 必须设置密码")
		return nil, false
	}
	settings.Put("password", pw)
	return settings, true
}

// buildSocksHTTPInboundSettings socks / http（传统代理入站）：单组账号密码。
//
// 落库形状（xray 26.3.27 实测可跑通真实代理流量）：
//
//	socks = {auth: "password", accounts: [{user, pass}], udp: false}
//	http  = {accounts: [{user, pass}], allowTransparent: false}
func buildSocksHTTPInboundSettings(b *inboundBuild) (*jsObj, bool) {
	user := strings.TrimSpace(nodeStringField(b.input, "auth_user"))
	pass := nodeStringField(b.input, "auth_pass")
	if pass == "" && b.existing != nil {
		// 密码不回显：编辑时留空 = 沿用原值（重填才改）。
		pass = jsonGetString(b.prevSettings(), "accounts", "0", "pass")
	}
	if user == "" || pass == "" {
		b.fail("SOCKS / HTTP 入站必须填写用户名与密码（内核默认 noauth，公网裸代理会被扫端口滥用）")
		return nil, false
	}
	account := jo("user", user, "pass", pass)
	if b.spec.Name == "socks" {
		// udp=false：只占 TCP，与端口族判定（inboundFamiliesOf）同口径。
		return jo("auth", "password", "accounts", jsArr{account}, "udp", false), true
	}
	return jo("accounts", jsArr{account}, "allowTransparent", false), true
}

// buildTunnelInboundSettings tunnel（dokodemo-door）：把收到的流量原样转给 address:port。
//
// 注意 L4 网络是 settings.network（tcp/udp/tcp,udp），写进 streamSettings.network
// 会被内核直接拒（unknown transport protocol: udp）。
func buildTunnelInboundSettings(b *inboundBuild) (*jsObj, bool) {
	tunAddr := strings.TrimSpace(nodeStringField(b.input, "tunnel_address"))
	tunPort := int64(0)
	if v, present := b.input["tunnel_port"]; present && v != nil {
		tunPort = inputInt(v)
	}
	if tunAddr == "" || tunPort <= 0 {
		b.fail("Tunnel 入站必须填写转发目标（地址 + 端口）")
		return nil, false
	}
	tunNet, netOK := tunnelNetwork(b.input)
	if !netOK {
		b.fail("Tunnel 转发协议只支持 tcp / udp / tcp,udp")
		return nil, false
	}
	return jo("address", tunAddr, "port", tunPort, "network", tunNet), true
}

// buildWireGuardInboundSettings wireguard：内核自带的 WireGuard 服务端。
//
// secretKey 是服务端身份私钥（等同于节点密码），只在库里与下发给节点的配置里出现，
// 读接口只回 wg_public_key。
func buildWireGuardInboundSettings(b *inboundBuild) (*jsObj, bool) {
	secret := ""
	if b.existing != nil {
		secret = jsonGetString(b.prevSettings(), "secretKey")
	}
	if secret == "" {
		kp, err := xray.WireGuardKeypair()
		if err != nil {
			b.failServer("生成 WireGuard 密钥", err)
			return nil, false
		}
		secret = kp[0]
	}
	peers, ok := wireGuardPeers(nodeStringField(b.input, "wg_peers"))
	if !ok {
		b.fail("WireGuard 对端格式错误：每行「公钥 [允许IP,...]」，公钥为 44 字符 base64")
		return nil, false
	}
	mtu := b.spec.defaultInt("mtu")
	if v, present := b.input["wg_mtu"]; present && inputInt(v) > 0 {
		mtu = inputInt(v)
	}
	// noKernelTun=true：内核目前只有 gVisor TUN 支持 WireGuard 入站（自带告警），
	// 显式写上，免得以后内核默认值变化把入站搞挂。
	settings := jo("secretKey", secret, "mtu", mtu, "noKernelTun", true)
	if len(peers) > 0 {
		settings.Put("peers", peers)
	}
	return settings, true
}
