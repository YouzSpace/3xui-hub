package subscription

import (
	"hash/crc32"
	"net"
	"strings"

	"controlhub-server/internal/ss2022"
)

// 逐条复刻 app/Drivers/Xray/XrayConfigService.php 的「订阅链接」段：
//
//	subscriptionUris() / vlessUri() / vmessUri() / trojanUri() / ssUri()
//	inboundLabel() / serverNameFor() / hostOf() / sameSite() / flowApplies()
//
// 与 3x-ui 节点的区别：xray 节点的链接由面板本地拼（零 HTTP），端口/参数全来自
// xray_inbounds 的结构化字段。

// flow 是 VLESS+Reality 的传输流控（v26 校验要求该值）。
const xrayFlow = "xtls-rprx-vision"

// reality 默认 dest / SNI（dest 不能用 TLS 记录 >8192 字节的站）。
const (
	xrayDefaultDest = "www.cloudflare.com:443"
	xrayDefaultSNI  = "www.cloudflare.com"
)

// xrayInboundURI 生成「该用户在该入站上」的分享链接（协议不支持时返回空串）。
func xrayInboundURI(node Node, inbound XrayInbound, user *User, host string) string {
	switch inbound.Protocol {
	case "vless":
		return xrayVlessURI(node, inbound, user, host)
	case "vmess":
		return xrayVmessURI(node, inbound, user, host)
	case "trojan":
		return xrayTrojanURI(node, inbound, user, host)
	case "shadowsocks":
		// SS-2022 多用户时链接按用户生成（每用户 PSK 不同），单凭据时同协议用户共用
		return xraySSURI(node, inbound, user, host)
	case "hysteria":
		return xrayHysteriaURI(node, inbound, user, host)
	default:
		return ""
	}
}

// inboundLabel 对应 inboundLabel()：节点名-tag；填了 remark 就用 remark 替换 tag 段。
func inboundLabel(node Node, inbound XrayInbound) string {
	remark := phpTrim(inbound.Remark)
	if remark != "" {
		return node.Name + "-" + remark
	}
	return node.Name + "-" + inbound.Tag
}

// flowApplies 对应 flowApplies()：flow（xtls-rprx-vision）仅适用于 vless + tcp + reality/tls。
func flowApplies(inbound XrayInbound) bool {
	if inbound.Protocol != "vless" {
		return false
	}
	stream := decodeJSONAny(inbound.StreamSettings)
	network := phpString(jsonField(stream, "network"))
	if network == "" {
		network = "tcp"
	}
	security := phpString(jsonField(stream, "security"))
	if security == "" {
		security = "none"
	}
	return network == "tcp" && (security == "reality" || security == "tls")
}

func xrayVlessURI(node Node, inbound XrayInbound, user *User, host string) string {
	stream := decodeJSONAny(inbound.StreamSettings)
	security := phpString(jsonField(stream, "security"))
	if security == "" {
		security = "none"
	}
	network := phpString(jsonField(stream, "network"))
	if network == "" {
		network = "tcp"
	}

	query := "type=" + network + "&security=" + security

	switch security {
	case "reality":
		public := jsonFieldMap(stream, "realitySettings")
		client := jsonFieldMap(public, "settings")
		// uTLS 指纹：入站未指定则回落 chrome（历史行为）
		fp := phpTrim(jsonFieldString(client, "fingerprint"))
		if fp == "" {
			fp = "chrome"
		}
		query += "&fp=" + rawurlencode(fp)

		// dest 传进去：分派池只用同一站点的 SNI（issue #57），坏 SNI 永不分给用户
		sni := serverNameFor(
			jsonFieldSlice(public, "serverNames"),
			user.ID,
			jsonFieldString(public, "dest"),
		)
		query += "&sni=" + rawurlencode(sni) + "&pbk=" + rawurlencode(jsonFieldString(public, "publicKey"))

		if sid := firstString(jsonFieldSlice(public, "shortIds")); sid != "" {
			query += "&sid=" + rawurlencode(sid)
		}
		// spiderX：客户端段优先，兼容旧写法落在 realitySettings 顶层
		spx := jsonFieldString(client, "spiderX")
		if spx == "" {
			spx = jsonFieldString(public, "spiderX")
		}
		if spx != "" {
			query += "&spx=" + rawurlencode(spx)
		}
		// 后量子校验（pqv）
		if pqv := phpTrim(jsonFieldString(client, "mldsa65Verify")); pqv != "" {
			query += "&pqv=" + rawurlencode(pqv)
		}
		if flowApplies(inbound) {
			query += "&flow=" + xrayFlow
		}
	case "tls":
		sni := jsonFieldString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = host
		}
		query += "&fp=chrome&sni=" + rawurlencode(sni)
	default:
		query += "&fp=chrome"
	}

	switch network {
	case "ws":
		query += "&path=" + rawurlencode(coalesceString(jsonField(stream, "wsSettings", "path"), "/"))
		if h := jsonFieldString(stream, "wsSettings", "host"); h != "" {
			query += "&host=" + rawurlencode(h)
		}
	case "httpupgrade":
		query += "&path=" + rawurlencode(coalesceString(jsonField(stream, "httpupgradeSettings", "path"), "/"))
		if h := jsonFieldString(stream, "httpupgradeSettings", "host"); h != "" {
			query += "&host=" + rawurlencode(h)
		}
	case "xhttp":
		// 与出站链接生成器同源：xhttp 参数（含高级字段 extra / x_padding_bytes）
		query += xhttpQuery(jsonFieldMap(stream, "xhttpSettings"))
	}

	if network == "tcp" && jsonFieldString(stream, "tcpSettings", "header", "type") == "http" {
		// TCP header 伪装（与 3x-ui 订阅口径一致：request.path + Host，response 优先）
		// PHP: $header['request']['path'][0] ?? '/'（值为空串时用空串，不回落 '/')
		reqPath, okPath := firstStringOK(jsonFieldSlice(stream, "tcpSettings", "header", "request", "path"))
		if !okPath {
			reqPath = "/"
		}
		query += "&headerType=http&path=" + rawurlencode(reqPath)
		// PHP：$header['response']['headers']['Host'][0] ?? $header['request']['headers']['Host'][0] ?? ''
		// 注意这里是 `??`（键不存在/值为 null 才回落），空串**不会**触发回落。
		headerHost, ok := firstStringOK(jsonFieldSlice(stream, "tcpSettings", "header", "response", "headers", "Host"))
		if !ok {
			headerHost, _ = firstStringOK(jsonFieldSlice(stream, "tcpSettings", "header", "request", "headers", "Host"))
		}
		if headerHost != "" {
			query += "&host=" + rawurlencode(headerHost)
		}
	}

	return "vless://" + user.UUID + "@" + xrayURIHostPort(host, inbound.Port) +
		"?" + query + "#" + rawurlencode(inboundLabel(node, inbound))
}

func xrayVmessURI(node Node, inbound XrayInbound, user *User, host string) string {
	stream := decodeJSONAny(inbound.StreamSettings)
	network := phpString(jsonField(stream, "network"))
	if network == "" {
		network = "tcp"
	}
	security := phpString(jsonField(stream, "security"))
	if security == "" {
		security = "none"
	}
	wsHost := jsonFieldString(stream, "wsSettings", "host")
	wsPath := jsonFieldString(stream, "wsSettings", "path")

	tls, sni := "", ""
	if security == "tls" {
		tls = "tls"
		sni = jsonFieldString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = host
		}
	}

	payload := obj(
		"v", "2",
		"ps", inboundLabel(node, inbound),
		"add", host,
		"port", itoa(inbound.Port),
		"id", user.UUID,
		"aid", "0",
		"scy", "auto",
		"net", network,
		"type", "none",
		"host", wsHost,
		"path", wsPath,
		"tls", tls,
		"sni", sni,
	)

	return "vmess://" + b64encode([]byte(jsonCompactUnicode(payload)))
}

func xrayTrojanURI(node Node, inbound XrayInbound, user *User, host string) string {
	stream := decodeJSONAny(inbound.StreamSettings)
	network := phpString(jsonField(stream, "network"))
	if network == "" {
		network = "tcp"
	}
	security := phpString(jsonField(stream, "security"))
	if security == "" {
		security = "none"
	}

	sec := "none"
	query := ""
	if security == "tls" {
		sec = "tls"
		sni := jsonFieldString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = host
		}
		query = "&sni=" + rawurlencode(sni)
	}
	query = "security=" + sec + "&type=" + network + query
	if network == "ws" {
		query += "&path=" + rawurlencode(coalesceString(jsonField(stream, "wsSettings", "path"), "/"))
	}

	return "trojan://" + user.UUID + "@" + xrayURIHostPort(host, inbound.Port) +
		"?" + query + "#" + rawurlencode(inboundLabel(node, inbound))
}

// xrayHysteriaURI 生成 Hysteria2 分享链接：
//
//	hysteria2://<auth>@host:port/?sni=..&security=tls&mport=A-B#备注
//
// auth 是用户 UUID（= clientsFor 注入的 `auth`），明文下发（hy2 userinfo 本来就是明文，
// 3x-ui 的 base64 写法是为了兼容老客户端，解析侧两种都认）。
// 跳跃区间来自 streamSettings.finalmask.quicParams.udpHop.ports（面板 hy_hop_ports 的落点），
// 用 mport 下发；没配就整条参数不出现。
func xrayHysteriaURI(node Node, inbound XrayInbound, user *User, host string) string {
	stream := decodeJSONAny(inbound.StreamSettings)
	sni := jsonFieldString(stream, "tlsSettings", "serverName")
	if sni == "" {
		sni = host
	}

	query := "sni=" + rawurlencode(sni) + "&security=tls"
	if ports := xrayHopPorts(stream); ports != "" {
		query += "&mport=" + rawurlencode(ports)
	}
	// salamander 混淆（issue #94）：密码是入站级单凭据（同协议用户共用），库里存密文，
	// XrayInbounds 已解出明文挂到 inbound.SalamanderPassword。
	// 有密码才写 obfs；没配就整条键不出现（客户端按默认「不混淆」走）。
	// 值原样编码不 trim：写库时已 trim 过，这里再动一个字符就跟服务端密码对不上。
	if password := inbound.SalamanderPassword; strings.TrimSpace(password) != "" {
		query += "&obfs=salamander&obfs-password=" + rawurlencode(password)
	}

	return "hysteria2://" + user.UUID + "@" + xrayURIHostPort(host, inbound.Port) +
		"/?" + query + "#" + rawurlencode(inboundLabel(node, inbound))
}

// xrayHopPorts 读端口跳跃区间：finalmask.quicParams.udpHop.ports（没有则看 hysteriaSettings）。
func xrayHopPorts(stream any) string {
	if ports := phpTrim(jsonFieldString(stream, "finalmask", "quicParams", "udpHop", "ports")); ports != "" {
		return ports
	}
	return phpTrim(jsonFieldString(stream, "hysteriaSettings", "udpHop", "ports"))
}

// xraySSURI 生成 Shadowsocks 链接（SIP002）。
//
// 单凭据（传统 AEAD）：密码在入站侧，同协议用户共用同一链接，base64(method:password)。
// SS-2022 多用户：每用户一条，userinfo 是明文 method:服务端PSK:用户PSK（SIP022；
// 本机实测只给用户 PSK 客户端握不上手）。
func xraySSURI(node Node, inbound XrayInbound, user *User, host string) string {
	settings := decodeJSONAny(inbound.Settings)
	method := jsonFieldString(settings, "method")
	if method == "" {
		method = "aes-256-gcm"
	}
	password := jsonFieldString(settings, "password")

	clientPSK := ""
	if user != nil {
		clientPSK = ss2022.ClientKey(method, user.UUID)
	}
	if clientPSK == "" || password == "" {
		return "ss://" + ss2022.UserInfo(method, password, "") + "@" + xrayURIHostPort(host, inbound.Port) +
			"#" + rawurlencode(inboundLabel(node, inbound))
	}
	return "ss://" + ss2022.UserInfo(method, password, clientPSK) + "@" + xrayURIHostPort(host, inbound.Port) +
		"#" + rawurlencode(inboundLabel(node, inbound))
}

// xrayURIHostPort 拼链接里的「host:port」：域名/IPv4 与 "host:port" 逐字节一致，
// IPv6 字面量补方括号（否则 "@2602:fa4f::1:443" 会把末段当端口）。
func xrayURIHostPort(host string, port int64) string {
	return net.JoinHostPort(host, itoa(port))
}

// serverNameFor 对应 serverNameFor()：按用户 id 从同一站点的 serverNames 里稳定选一个。
func serverNameFor(serverNames []any, userID int64, dest string) string {
	names := make([]string, 0, len(serverNames))
	for _, raw := range serverNames {
		if s, ok := raw.(string); ok {
			if trimmed := phpTrim(s); trimmed != "" {
				names = append(names, trimmed)
			}
		}
	}
	if len(names) == 0 {
		return xrayDefaultSNI
	}

	if host := hostOf(dest); host != "" {
		usable := make([]string, 0, len(names))
		for _, name := range names {
			if sameSite(name, host) {
				usable = append(usable, name)
			}
		}
		if len(usable) > 0 {
			names = usable
		}
	}

	idx := int(crc32.ChecksumIEEE([]byte(itoa(userID)))) % len(names)
	return names[idx]
}

// hostOf 对应 XrayConfigService::hostOf()：dest 串 → 主机名。
// 支持 "host:443"、"host"、"[2001:db8::1]:443"、"[2001:db8::1]"，解析不出返回空串。
func hostOf(dest string) string {
	dest = phpTrim(dest)
	if dest == "" {
		return ""
	}
	if strings.HasPrefix(dest, "[") {
		end := strings.IndexByte(dest, ']')
		if end < 0 {
			return ""
		}
		return asciiLower(dest[1:end])
	}
	if i := strings.LastIndex(dest, ":"); i >= 0 {
		tail := dest[i+1:]
		if isAllDigitsASCII(tail) {
			dest = dest[:i]
		}
	}
	return asciiLower(phpTrimSet(dest, " \t\n\r\x00\x0B."))
}

// secondLevelSuffixes 是常见两级公共后缀（与 PHP 侧同一张表）。
var secondLevelSuffixes = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"co.jp": true, "ne.jp": true, "or.jp": true, "ac.jp": true,
	"com.cn": true, "net.cn": true, "org.cn": true, "gov.cn": true, "edu.cn": true,
	"com.hk": true, "com.tw": true, "com.sg": true, "com.au": true,
	"co.kr": true, "co.nz": true, "com.br": true,
}

// sameSite 对应 sameSite()：相等 / 互为父域 / 同属一个注册域。
func sameSite(a, b string) bool {
	a = asciiLower(phpTrimSet(a, " \t\n\r\x00\x0B."))
	b = asciiLower(phpTrimSet(b, " \t\n\r\x00\x0B."))

	if a == "" || b == "" || strings.Contains(a, "*") || strings.Contains(b, "*") {
		return false
	}
	if a == b {
		return true
	}
	if isIPLiteral(a) || isIPLiteral(b) {
		return false
	}
	if strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a) {
		return true
	}
	return registeredDomain(a) == registeredDomain(b)
}

// isIPLiteral 近似 PHP 的 filter_var($s, FILTER_VALIDATE_IP)（订阅链路只需要 v4/v6 字面量判定）。
func isIPLiteral(s string) bool {
	if !strings.Contains(s, ".") && !strings.Contains(s, ":") {
		return false
	}
	return isIPv4Literal(s) || isIPv6Literal(s)
}

func isIPv4Literal(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 3 || !isAllDigitsASCII(part) {
			return false
		}
		if parseIntPrefix(part) > 255 {
			return false
		}
	}
	return true
}

func isIPv6Literal(s string) bool {
	// 只需要「像 IPv6」：必须含 ':'、只由十六进制/冒号/点组成、冒号数量合理。
	if !strings.Contains(s, ":") {
		return false
	}
	colons := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ':':
			colons++
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F', c == '.':
		default:
			return false
		}
	}
	return colons >= 2
}

// registeredDomain 对应 registeredDomain()：取注册域；取不出时返回原串。
func registeredDomain(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	tail2 := strings.Join(parts[len(parts)-2:], ".")
	if secondLevelSuffixes[tail2] {
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return tail2
}

// xhttpQuery 对应 OutboundLinkGenerator::xhttpQuery()（与 3x-ui 的双编码对齐）。
//
// path/host/mode 之外的高级字段（如 xPaddingBytes）编码为：
//   - extra=<urlencoded-json>：xray-core 系客户端读取的完整 blob
//   - x_padding_bytes=<range>：sing-box 系客户端的 flat 参数
//
// 无高级字段时两者都不产出，老配置链接字节不变。
func xhttpQuery(xhttp phpMap) string {
	query := "&path=" + rawurlencode(coalesceString(jsonField(xhttp, "path"), "/"))
	if host := jsonFieldString(xhttp, "host"); host != "" {
		query += "&host=" + rawurlencode(host)
	}
	if mode := jsonFieldString(xhttp, "mode"); mode != "" {
		query += "&mode=" + rawurlencode(mode)
	}

	// extra 白名单（与 3x-ui sub/service.go buildXhttpExtra 对齐）：只带双向字段。
	// 服务端专属字段不进链接；等于内核默认的 sc* 值剔除（scMinPostsIntervalMs=30 是已知 DPI 指纹）。
	ext := phpMap{}
	padding := phpTrim(jsonFieldString(xhttp, "xPaddingBytes"))
	if padding != "" {
		ext.set("xPaddingBytes", padding)
	}
	if phpBool(jsonField(xhttp, "xPaddingObfsMode")) {
		ext.set("xPaddingObfsMode", true)
		for _, field := range []string{"xPaddingKey", "xPaddingHeader", "xPaddingPlacement", "xPaddingMethod"} {
			if v := phpTrim(jsonFieldString(xhttp, field)); v != "" {
				ext.set(field, v)
			}
		}
	}
	coreDefaults := map[string]string{
		"scMaxEachPostBytes":   "1000000",
		"scMinPostsIntervalMs": "30",
	}
	for _, field := range []string{
		"uplinkHTTPMethod", "sessionIDPlacement", "sessionIDKey", "sessionIDTable",
		"sessionIDLength", "seqPlacement", "seqKey", "uplinkDataPlacement", "uplinkDataKey",
		"scMaxEachPostBytes", "scMinPostsIntervalMs",
	} {
		v := phpTrim(jsonFieldString(xhttp, field))
		if v == "" || v == coreDefaults[field] {
			continue
		}
		ext.set(field, v)
	}
	if phpBool(jsonField(xhttp, "uplinkChunkSize")) {
		ext.set("uplinkChunkSize", jsonNumber(itoa(phpInt(jsonField(xhttp, "uplinkChunkSize")))))
	}
	if phpBool(jsonField(xhttp, "noGRPCHeader")) {
		ext.set("noGRPCHeader", true)
	}
	if len(ext) > 0 {
		query += "&extra=" + rawurlencode(jsonCompactUnicode(ext))
	}
	if padding != "" {
		query += "&x_padding_bytes=" + rawurlencode(padding)
	}

	return query
}

// coalesceString 对应 PHP 的 `$x ?? 'default'`：键不存在 / 值为 null 才回落，
// 空串是**有效值**（PHP 的 `??` 不是 `?:`，这个区别直接影响链接字节）。
func coalesceString(v any, def string) string {
	switch t := v.(type) {
	case nil:
		return def
	case string:
		return t
	case phpMap, []any:
		// PHP 里对数组做字符串拼接会得到 "Array"（并告警），链接里不会出现这种配置；
		// 保守回落到默认值，避免产出 "Array" 这种脏参数。
		return def
	default:
		return phpString(t)
	}
}

// firstString 取数组第一个元素的中转文本（PHP 的 (string)($arr[0] ?? ”)）。
func firstString(items []any, def ...string) string {
	s, ok := firstStringOK(items)
	if !ok || s == "" {
		if len(def) > 0 {
			return def[0]
		}
		return ""
	}
	return s
}

// firstStringOK 严格对应 PHP 的 `$arr[0] ?? null` 判定：
// 数组为空或元素不是字符串时返回 ("", false)，**空串本身是有效值**。
func firstStringOK(items []any) (string, bool) {
	if len(items) == 0 {
		return "", false
	}
	if s, ok := items[0].(string); ok {
		return s, true
	}
	return "", false
}
