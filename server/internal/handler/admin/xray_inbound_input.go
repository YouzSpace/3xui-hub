package admin

// payload → XrayInboundInput 的构造（对齐 fillFromPayload）：协议组合约束、
// 传输层/安全层整形、客户端地址。从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"strings"

	"controlhub-server/internal/driver/xray"
	"controlhub-server/internal/ss2022"

	"github.com/gin-gonic/gin"
)

// buildInboundInput 对齐 fillFromPayload()（含全部业务校验）。
func (d *NodeDomain) buildInboundInput(c *gin.Context, node *NodeRecord, input map[string]any, existing *XrayInboundRecord) (XrayInboundInput, bool) {
	protocol := nodeStringField(input, "protocol")
	network := "tcp"
	if s, ok := inputString(input["network"]); ok && s != "" {
		network = s
	}
	security := "none"
	if s, ok := inputString(input["security"]); ok && s != "" {
		security = s
	}

	// 组合合法性（前后端同守）。
	if security == "reality" && protocol != "vless" {
		d.fail(c, "Reality 仅支持 VLESS 协议", 500)
		return XrayInboundInput{}, false
	}
	if security == "reality" && !phpInArray(network, []string{"tcp", "xhttp", "grpc"}) {
		d.fail(c, "Reality 仅支持 tcp / xhttp / grpc 传输", 500)
		return XrayInboundInput{}, false
	}
	if protocol == "shadowsocks" && (security != "none" || network != "tcp") {
		d.fail(c, "Shadowsocks 仅支持 tcp + 无附加安全层", 500)
		return XrayInboundInput{}, false
	}
	if protocol == "hysteria" {
		// HY2 的传输层固定：内核只认 network=hysteria + 强制 TLS（QUIC 自带加密）。
		// 前端不选传输/安全层，这里统一归一，避免历史负载带进非法组合。
		network = "hysteria"
		security = "tls"
	}
	if protocol == "wireguard" {
		// WireGuard 自带加密、固定 UDP：传输层与安全层都不存在，归一成空
		// （后面按空值跳过所有传输分支，stream_settings 落库就是 {}）。
		network = ""
		security = "none"
	}

	// Reality 后量子：seed 与 verify 成对。
	if security == "reality" {
		seed := strings.TrimSpace(nodeStringField(input, "mldsa65_seed"))
		verify := strings.TrimSpace(nodeStringField(input, "mldsa65_verify"))
		if (seed == "") != (verify == "") {
			d.fail(c, "mldsa65Seed 与 mldsa65Verify 必须成对设置（同时填写或同时留空）", 500)
			return XrayInboundInput{}, false
		}
	}

	// tag：缺省自动建议；唯一性预检（同节点内唯一）。
	var excludeID *uint64
	if existing != nil {
		excludeID = ptrUintOf(existing.ID)
	}
	tag := nodeStringField(input, "tag")
	if tag == "" {
		suggested, ok := d.suggestInboundTag(c, node, protocol, security, network, excludeID)
		if !ok {
			return XrayInboundInput{}, false
		}
		tag = suggested
	}
	dup, err := d.store.XrayInboundTagTaken(ctxOf(c), node.ID, tag, excludeID)
	if err != nil {
		d.failServer(c, "检查入站标识", err)
		return XrayInboundInput{}, false
	}
	if dup {
		d.fail(c, "标识 "+tag+" 已存在", 500)
		return XrayInboundInput{}, false
	}

	// 端口冲突：同节点启用入站不可复用端口（xray 会启动失败）。
	// 按传输族比对：tunnel 的 L4 网络在 settings.network（tcp / udp / tcp,udp），
	// 其余协议看协议本身（wireguard / hysteria 是 UDP）。
	port := inputInt(input["port"])
	// tunnel 的 L4 网络非法时这里按缺省算，真正的报错留给 settings 构造（文案更具体）。
	tunNetwork, _ := tunnelNetwork(input)
	portTaken, err := d.store.XrayInboundPortTaken(ctxOf(c), node.ID, port, inboundFamiliesOf(protocol, tunNetwork), excludeID)
	if err != nil {
		d.failServer(c, "检查入站端口", err)
		return XrayInboundInput{}, false
	}
	willEnable := true
	if existing != nil {
		willEnable = existing.Enabled
	}
	if v, present := input["enabled"]; present {
		willEnable = inputBool(v)
	}
	if willEnable && portTaken {
		d.fail(c, "端口已被该节点其他启用入站占用", 500)
		return XrayInboundInput{}, false
	}

	// settings
	var settings *jsObj
	switch protocol {
	case "vless":
		settings = jo("decryption", "none")
	case "shadowsocks":
		// 默认 2022-blake3-aes-256-gcm（多用户）：与前端 emptyForm 默认一致，
		// 新建 ss 入站开箱即按用户区分流量（对齐 3x-ui 默认）。
		method := "2022-blake3-aes-256-gcm"
		if s, ok := inputString(input["ss_method"]); ok && s != "" {
			method = s
		}
		settings = jo("method", method)
		pw := strings.TrimSpace(nodeStringField(input, "ss_password"))
		if pw == "" && existing != nil && existing.Protocol == "shadowsocks" {
			// 编辑时留空 = 沿用原值（与 socks/http 的「不改不用重填」同口径）。
			prev, _ := parseOrderedJS(existing.Settings)
			pw = strings.TrimSpace(jsonGetString(prev, "password"))
		}
		if ss2022.KeyLen(method) > 0 {
			// SS-2022：服务端 PSK 必须是 base64 且解码后正好 16/32 字节，
			// 长度不对内核会 "bad key" 拒载**整份**节点配置。留空或非法就现场生成一把
			// 合法的（前端会把落库的值回显出来，管理员能看到/复制）。
			if !ss2022.ValidKey(method, pw) {
				gen, err := ss2022.GenerateKey(method)
				if err != nil {
					d.failServer(c, "生成 Shadowsocks-2022 密钥", err)
					return XrayInboundInput{}, false
				}
				pw = gen
			}
		} else if pw == "" {
			// 传统 AEAD 的 password 是自由格式：不校验、不生成，只要求非空。
			d.fail(c, "Shadowsocks 必须设置密码", 500)
			return XrayInboundInput{}, false
		}
		settings.put("password", pw)
	case "hysteria":
		// 内核只接受 version=2；写 1 会让**整份节点配置**被拒（不是只挂这个入站）。
		settings = jo("version", 2)
	case "socks", "http":
		// 传统代理入站。落库形状（xray 26.3.27 实测可跑通真实代理流量）：
		//   socks = {auth: "password", accounts: [{user, pass}], udp: false}
		//   http  = {accounts: [{user, pass}], allowTransparent: false}
		user := strings.TrimSpace(nodeStringField(input, "auth_user"))
		pass := nodeStringField(input, "auth_pass")
		if pass == "" && existing != nil {
			// 密码不回显：编辑时留空 = 沿用原值（重填才改）。
			prev, _ := parseOrderedJS(existing.Settings)
			pass = jsonGetString(prev, "accounts", "0", "pass")
		}
		if user == "" || pass == "" {
			d.fail(c, "SOCKS / HTTP 入站必须填写用户名与密码（内核默认 noauth，公网裸代理会被扫端口滥用）", 500)
			return XrayInboundInput{}, false
		}
		account := jo("user", user, "pass", pass)
		if protocol == "socks" {
			// udp=false：只占 TCP，与端口族判定（inboundFamiliesOf）同口径。
			settings = jo("auth", "password", "accounts", jsArr{account}, "udp", false)
		} else {
			settings = jo("accounts", jsArr{account}, "allowTransparent", false)
		}
	case "tunnel":
		// dokodemo-door：把收到的流量原样转给 address:port。
		// 注意 L4 网络是 settings.network（tcp/udp/tcp,udp），写进 streamSettings.network
		// 会被内核直接拒（unknown transport protocol: udp）。
		tunAddr := strings.TrimSpace(nodeStringField(input, "tunnel_address"))
		tunPort := int64(0)
		if v, present := input["tunnel_port"]; present && v != nil {
			tunPort = inputInt(v)
		}
		if tunAddr == "" || tunPort <= 0 {
			d.fail(c, "Tunnel 入站必须填写转发目标（地址 + 端口）", 500)
			return XrayInboundInput{}, false
		}
		tunNet, netOK := tunnelNetwork(input)
		if !netOK {
			d.fail(c, "Tunnel 转发协议只支持 tcp / udp / tcp,udp", 500)
			return XrayInboundInput{}, false
		}
		settings = jo("address", tunAddr, "port", tunPort, "network", tunNet)
	case "wireguard":
		// 内核自带的 WireGuard 服务端：UDP 监听，服务端私钥 + 对端公钥清单。
		// secretKey 是服务端身份私钥（等同于节点密码），只在库里与下发给节点的配置里出现，
		// 读接口只回 wg_public_key。
		secret := ""
		if existing != nil {
			prev, _ := parseOrderedJS(existing.Settings)
			secret = jsonGetString(prev, "secretKey")
		}
		if secret == "" {
			kp, err := xray.WireGuardKeypair()
			if err != nil {
				d.failServer(c, "生成 WireGuard 密钥", err)
				return XrayInboundInput{}, false
			}
			secret = kp[0]
		}
		peers, ok := wireGuardPeers(nodeStringField(input, "wg_peers"))
		if !ok {
			d.fail(c, "WireGuard 对端格式错误：每行「公钥 [允许IP,...]」，公钥为 44 字符 base64", 500)
			return XrayInboundInput{}, false
		}
		mtu := int64(1420)
		if v, present := input["wg_mtu"]; present && inputInt(v) > 0 {
			mtu = inputInt(v)
		}
		// noKernelTun=true：内核目前只有 gVisor TUN 支持 WireGuard 入站（自带告警），
		// 显式写上，免得以后内核默认值变化把入站搞挂。
		settings = jo("secretKey", secret, "mtu", mtu, "noKernelTun", true)
		if len(peers) > 0 {
			settings.put("peers", peers)
		}
	default:
		settings = jo()
	}

	// stream_settings
	//
	// wireguard 自带加密、固定 UDP，没有可选的传输层与安全层：整段不写
	// （写入 network 虽然会被内核忽略，但列表页会把它显示成 TCP 入站，误导管理员）。
	var stream *jsObj
	if protocol == "wireguard" {
		stream = jo()
	} else {
		stream = jo("network", network, "security", security)
	}

	in := XrayInboundInput{
		NodeID:   node.ID,
		Tag:      tag,
		Protocol: protocol,
		Port:     port,
		Settings: phpEncode(settings),
		Sniffing: sniffingSettings(input),
	}

	if security == "reality" {
		privateKey := nodeStringField(input, "private_key")
		publicKey := nodeStringField(input, "public_key")
		if privateKey == "" {
			// 新建且未提供材料 → 面板自动生成完整一套。
			keypair, err := xray.GenerateKeypair()
			if err != nil {
				d.failServer(c, "生成 Reality 密钥", err)
				return XrayInboundInput{}, false
			}
			privateKey, publicKey = keypair[0], keypair[1]
		}
		if publicKey == "" && existing != nil {
			prev, _ := parseOrderedJS(existing.StreamSettings)
			publicKey = jsonGetString(prev, "realitySettings", "publicKey")
		}

		stream.put("realitySettings", d.realitySettings(input, publicKey))
		in.RealityPrivateKey = &privateKey
		in.RealityKeySet = true
	} else if existing != nil {
		// 从 reality 改成别的安全层：清掉旧私钥（对齐 setRealityPrivateKey(null)）。
		in.RealityKeySet = true
	}

	// certPickReason 是「自动挑证书为什么没挑到」的原因（"" = 挑到了，或压根没走自动挑）。
	// 只用来把下面的报错文案说准：证书库空 / 全过期，用户要做的操作不一样。
	certPickReason := ""
	if protocol == "hysteria" {
		var prevStream any
		if existing != nil {
			prevStream, _ = parseOrderedJS(existing.StreamSettings)
		}
		// 证书自动引用：请求没带 tls_cert_id、编辑时原配置也没引用时，从证书库挑一张，
		// 免得「建一个 HY2 入站先去手抄一个证书 id」。必须放在 buildHysteriaStream 之前
		// ——它读的就是 input 里的 tls_cert_id。
		certPickReason = d.autoPickHysteriaCert(c, node, input, prevStream)
		if !d.buildHysteriaStream(c, input, stream, prevStream) {
			return XrayInboundInput{}, false
		}
		// salamander 混淆密码：密码写独立加密列，stream_settings 只留 {"type":"salamander"} 占位
		// （issue #94 硬指标：库里不允许出现密码原文）。
		applyHysteriaSalamander(input, stream, prevStream, existing, &in)
	} else if existing != nil && existing.SalamanderPassword != nil {
		// 协议从 hysteria 改走别的：清掉遗留的混淆密码，别把密文留在没人读的列里。
		in.SalamanderPasswordSet = true
	}

	if network == "ws" {
		// PHP: array_filter(['path'=>..., 'host'=>...], fn($v) => $v !== '')
		// → path 恒在，host 为空时被删掉。
		ws := jo()
		path := nodeStringField(input, "ws_path")
		if path == "" {
			path = "/"
		}
		ws.put("path", path)
		if host := nodeStringField(input, "ws_host"); host != "" {
			ws.put("host", host)
		}
		stream.put("wsSettings", ws)
	} else if network == "xhttp" {
		stream.put("xhttpSettings", xhttpSettings(input))
	} else if network == "httpupgrade" {
		hu := jo()
		path := nodeStringField(input, "httpupgrade_path")
		if path == "" {
			path = "/"
		}
		hu.put("path", path)
		if host := nodeStringField(input, "httpupgrade_host"); host != "" {
			hu.put("host", host)
		}
		stream.put("httpupgradeSettings", hu)
	}

	// TCP header 伪装（type=http 时构造 request；响应段用内核默认）。
	if network == "tcp" && nodeStringField(input, "tcp_header_type") == "http" {
		reqPath := nodeStringField(input, "tcp_header_path")
		if reqPath == "" {
			reqPath = "/"
		}
		request := jo("version", "1.1", "method", "GET", "path", jsArr{reqPath})
		if host := nodeStringField(input, "tcp_header_host"); host != "" {
			request.put("headers", jo("Host", jsArr{host}))
		}
		stream.put("tcpSettings", jo("header", jo("type", "http", "request", request)))
	}

	// hysteria 的 tlsSettings 已在 buildHysteriaStream 里按证书引用写好
	// （只存 tlsCertId，私钥留在证书库），不能再走下面这段内联 PEM 分支。
	//
	// TLS 入站（vless/vmess/trojan…）一律走证书库（与 HY2 同一条引用链）：
	//   - 选了证书库条目 → 校验存在 + 域名覆盖 serverName，只存 tlsCertId；
	//   - 只贴了 PEM → 按内容匹配库里同一张（匹配不到就新建一条），同样存 tlsCertId；
	//   - 两者都没有 → 报错。
	// 存了 tlsCertId，续签任务才能通过引用找到这台节点并自动刷新；内联 PEM 可留空，
	// 写了也会在续签后自动更新（见 xray_certificates_renew.go）。
	if security == "tls" && protocol != "hysteria" {
		tls := jo()
		sni := strings.ToLower(strings.TrimSpace(nodeStringField(input, "tls_server_name")))
		if sni != "" {
			tls.put("serverName", sni)
		}
		cert := nodeStringField(input, "tls_certificate")
		key := nodeStringField(input, "tls_key")
		certID := inputInt(input["tls_cert_id"])
		if certID <= 0 && cert != "" && key != "" {
			id, ok := d.certificateIDForPEM(c, cert, key, sni)
			if !ok {
				return XrayInboundInput{}, false
			}
			certID = int64(id)
		}
		if certID <= 0 {
			d.fail(c, "TLS 入站需要引用一张证书库证书（推荐），或手填证书与私钥", 500)
			return XrayInboundInput{}, false
		}
		ref, err := d.store.XrayCertificatePEM(ctxOf(c), uint64(certID))
		if err != nil {
			d.fail(c, "证书库条目不存在（id="+itoaInt64(certID)+"），请重新选择", 500)
			return XrayInboundInput{}, false
		}
		if sni != "" && ref.Domain != nil {
			if domain := strings.ToLower(strings.TrimSpace(*ref.Domain)); domain != "" && !certDomainMatches(domain, sni) {
				d.fail(c, "选中的证书（"+domain+"）覆盖不到 serverName "+sni, 500)
				return XrayInboundInput{}, false
			}
		}
		tls.put(xray.CertRefKey, certID)
		if cert != "" && key != "" {
			tls.put("certificates", jsArr{jo("certificate", jsArr{cert}, "key", jsArr{key})})
		}
		stream.put("tlsSettings", tls)
	}

	if protocol == "hysteria" {
		// HY2 强制 TLS：没引用证书时内核起不来（与 TLS 入站同理）。
		// 自动挑证书没成功（库空 / 全过期）时，文案要直接指向用户下一步该做什么。
		if jsonGet(stream, "tlsSettings", xray.CertRefKey) == nil {
			d.fail(c, hysteriaCertMissingMessage(certPickReason), 500)
			return XrayInboundInput{}, false
		}
	}
	in.StreamSettings = phpEncode(stream)

	remark := strings.TrimSpace(nodeStringField(input, "remark"))
	if remark != "" {
		in.Remark = &remark
	}
	listen := nodeStringField(input, "listen")
	if listen == "" {
		listen = "0.0.0.0"
	}
	in.Listen = listen

	// 客户端连接地址（CDN 场景）：订阅链接里用它替换节点地址，端口仍用本入站端口。
	// 留空 = 维持原行为（节点 host / host_v6 按地址族选）。
	if ch := normalizeClientHost(nodeStringField(input, "client_host")); ch != "" {
		in.ClientHost = &ch
	}

	if v, present := input["enabled"]; present {
		b := inputBool(v)
		in.Enabled = &b
	}
	if v, present := input["sort"]; present && v != nil {
		n := inputInt(v)
		in.Sort = &n
	}
	return in, true
}

// ---- hysteria 的 TLS 证书自动引用 ----

// normalizeClientHost 归一化「客户端连接地址」（CDN 域名 / 另一个 IP）：
// 允许直接粘 URL，会去掉协议头、路径、查询串和末尾斜杠；带端口的会去掉端口
// （端口始终用入站端口，避免拼出 host:443:443）。IPv6 字面量保持原样。
func normalizeClientHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if strings.Count(s, ":") == 1 {
		s = s[:strings.LastIndex(s, ":")]
	}
	if len(s) > 255 {
		s = s[:255]
	}
	return s
}

// sniffingSettings 对齐 sniffingSettings()：未提交时保持现状默认；默认值不落库。
func sniffingSettings(input map[string]any) []byte {
	raw, present := input["sniffing"]
	s, isObj := raw.(map[string]any)
	if !present || !isObj {
		return phpEncode(jo("enabled", true, "destOverride", jsArr{"http", "tls", "quic"}))
	}

	// `$s['enabled'] ?? true`：缺失与显式 null 都回落 true。
	enabled := true
	if v, ok := s["enabled"]; ok && v != nil {
		enabled = inputBool(v)
	}
	// `array_values(array_filter((array) ($s['destOverride'] ?? [...]), 'is_string'))`
	// —— (array) 把标量包成单元素数组，array_filter 只保留字符串（含空串）。
	destOverride := jsArr{"http", "tls", "quic"}
	if v, ok := s["destOverride"]; ok && v != nil {
		destOverride = jsArr{}
		for _, item := range phpArrayCast(v) {
			if str, isStr := item.(string); isStr {
				destOverride = append(destOverride, str)
			}
		}
	}
	config := jo("enabled", enabled, "destOverride", destOverride)
	if v, ok := s["routeOnly"]; ok && inputBool(v) {
		config.put("routeOnly", true)
	}
	return phpEncode(config)
}
