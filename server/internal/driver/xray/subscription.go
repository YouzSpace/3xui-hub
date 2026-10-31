package xray

import (
	"encoding/base64"
	"hash/crc32"
	"net"
	"strings"

	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
)

// 逐条复刻 XrayConfigService.php 的「订阅链接」段：
//
//	subscriptionUris() / vlessUri() / vmessUri() / trojanUri() / ssUri()
//	inboundLabel() / serverNameFor() / hostOf() / sameSite()
//
// 与 3x-ui 节点的区别：xray 节点的链接由面板本地拼（零 HTTP），
// 端口/参数全来自 xray_inbounds 的结构化字段。
//
// 注意：server/internal/subscription 里已有一份等价实现（P3 交付，含 golden 对拍）。
// 本包这一份供驱动层（管理端预览链接 / 指令载荷）复用；两份的口径必须一致，
// 差异点只有「入口的数据类型」（那边是数据库读取的 Node/XrayInbound 结构体）。

// secondLevelSuffixes 是常见两级公共后缀（与 PHP 侧同一张表，不引 PSL 依赖）。
var secondLevelSuffixes = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"co.jp": true, "ne.jp": true, "or.jp": true, "ac.jp": true,
	"com.cn": true, "net.cn": true, "org.cn": true, "gov.cn": true, "edu.cn": true,
	"com.hk": true, "com.tw": true, "com.sg": true, "com.au": true,
	"co.kr": true, "co.nz": true, "com.br": true,
}

// SubscriptionURIs 复刻 subscriptionUris()：生成用户在节点上的全部订阅 URI
// （每个协议匹配的启用入站一条）。
func SubscriptionURIs(node Node, inbounds []Inbound, user User) []string {
	host := strings.TrimSpace(node.Host)
	if host == "" || user.UUID == "" {
		return []string{}
	}

	uris := []string{}
	for _, ib := range inbounds {
		// 用户全协议通用：每个启用入站都出一条链接，不按 users.protocol 过滤。
		var uri string
		switch ib.Protocol {
		case "vless":
			uri = vlessURI(node, ib, user, host)
		case "vmess":
			uri = vmessURI(node, ib, user, host)
		case "trojan":
			uri = trojanURI(node, ib, user, host)
		case "shadowsocks":
			// shadowsocks 是入站级单凭据（密码在入站而非用户），链接对同协议用户通用
			uri = ssURI(node, ib, host)
		case "hysteria":
			uri = hysteriaURI(node, ib, user, host)
		default:
			continue
		}
		uris = append(uris, uri)
	}
	return uris
}

// InboundLabel 复刻 inboundLabel()：节点名-tag 区分同节点多入站。
//
// remark 是给人看的中文备注；填了就用它替换 tag 段，没填回落 tag ——
// 老数据 remark 为 NULL，订阅输出与历史逐字节一致。tag 本身不改（引用型标识）。
func InboundLabel(node Node, ib Inbound) string {
	remark := ""
	if ib.Remark != nil {
		remark = strings.TrimSpace(*ib.Remark)
	}
	if remark != "" {
		return node.Name + "-" + remark
	}
	return node.Name + "-" + ib.Tag
}

func vlessURI(node Node, ib Inbound, user User, host string) string {
	stream := streamObject(ib)
	security := securityOf(stream)
	network := networkOf(stream)

	query := "type=" + network + "&security=" + security

	switch security {
	case "reality":
		public := phpval.ToObject(mustGet(stream, "realitySettings"))
		client := phpval.ToObject(mustGet(public, "settings"))
		// uTLS 指纹：入站未指定则回落 chrome（历史行为）
		fp := strings.TrimSpace(phpval.String(mustGet(client, "fingerprint")))
		if fp == "" {
			fp = "chrome"
		}
		query += "&fp=" + phpval.RawURLEncode(fp)

		// dest 传进去：分派池只用同一站点的 SNI（issue #57），坏 SNI 永不分给用户
		sni := ServerNameFor(
			toAnySlice(mustGet(public, "serverNames")),
			user.ID,
			phpval.String(mustGet(public, "dest")),
		)
		query += "&sni=" + phpval.RawURLEncode(sni) +
			"&pbk=" + phpval.RawURLEncode(phpval.String(mustGet(public, "publicKey")))

		if sid := firstStringValue(mustGet(public, "shortIds")); sid != "" {
			query += "&sid=" + phpval.RawURLEncode(sid)
		}
		// spiderX：客户端段优先，兼容旧写法落在 realitySettings 顶层
		spx := phpval.String(mustGet(client, "spiderX"))
		if spx == "" {
			spx = phpval.String(mustGet(public, "spiderX"))
		}
		if spx != "" {
			query += "&spx=" + phpval.RawURLEncode(spx)
		}
		// 后量子校验（pqv）
		if pqv := strings.TrimSpace(phpval.String(mustGet(client, "mldsa65Verify"))); pqv != "" {
			query += "&pqv=" + phpval.RawURLEncode(pqv)
		}
		if FlowApplies(ib) {
			query += "&flow=" + Flow
		}
	case "tls":
		sni := jsonFieldString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = host
		}
		query += "&fp=chrome&sni=" + phpval.RawURLEncode(sni)
	default:
		query += "&fp=chrome"
	}

	switch network {
	case "ws":
		query += "&path=" + phpval.RawURLEncode(coalesceString(jsonField(stream, "wsSettings", "path"), "/"))
		if h := jsonFieldString(stream, "wsSettings", "host"); h != "" {
			query += "&host=" + phpval.RawURLEncode(h)
		}
	case "httpupgrade":
		query += "&path=" + phpval.RawURLEncode(coalesceString(jsonField(stream, "httpupgradeSettings", "path"), "/"))
		if h := jsonFieldString(stream, "httpupgradeSettings", "host"); h != "" {
			query += "&host=" + phpval.RawURLEncode(h)
		}
	case "xhttp":
		query += xhttpQuery(phpval.ToObject(mustGet(stream, "xhttpSettings")))
	}

	if network == "tcp" && jsonFieldString(stream, "tcpSettings", "header", "type") == "http" {
		// PHP: $header['request']['path'][0] ?? '/'（值为空串时用空串，不回落 '/'）
		reqPath, okPath := firstStringOK(jsonFieldArray(stream, "tcpSettings", "header", "request", "path"))
		if !okPath {
			reqPath = "/"
		}
		query += "&headerType=http&path=" + phpval.RawURLEncode(reqPath)
		// PHP：$header['response']['headers']['Host'][0] ?? $header['request']['headers']['Host'][0] ?? ''
		headerHost, ok := firstStringOK(jsonFieldArray(stream, "tcpSettings", "header", "response", "headers", "Host"))
		if !ok {
			headerHost, _ = firstStringOK(jsonFieldArray(stream, "tcpSettings", "header", "request", "headers", "Host"))
		}
		if headerHost != "" {
			query += "&host=" + phpval.RawURLEncode(headerHost)
		}
	}

	return "vless://" + user.UUID + "@" + uriHostPort(host, ib.Port) +
		"?" + query + "#" + phpval.RawURLEncode(InboundLabel(node, ib))
}

func vmessURI(node Node, ib Inbound, user User, host string) string {
	stream := streamObject(ib)
	network := networkOf(stream)
	security := securityOf(stream)
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

	payload := phpjson.Object{
		{Key: "v", Value: "2"},
		{Key: "ps", Value: InboundLabel(node, ib)},
		{Key: "add", Value: host},
		{Key: "port", Value: phpval.Itoa(ib.Port)},
		{Key: "id", Value: user.UUID},
		{Key: "aid", Value: "0"},
		{Key: "scy", Value: "auto"},
		{Key: "net", Value: network},
		{Key: "type", Value: "none"},
		{Key: "host", Value: wsHost},
		{Key: "path", Value: wsPath},
		{Key: "tls", Value: tls},
		{Key: "sni", Value: sni},
	}

	body, _ := phpjson.EncodeUnescaped(payload)
	return "vmess://" + base64.StdEncoding.EncodeToString(body)
}

func trojanURI(node Node, ib Inbound, user User, host string) string {
	stream := streamObject(ib)
	network := networkOf(stream)
	security := securityOf(stream)

	sec := "none"
	query := ""
	if security == "tls" {
		sec = "tls"
		sni := jsonFieldString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = host
		}
		query = "&sni=" + phpval.RawURLEncode(sni)
	}
	query = "security=" + sec + "&type=" + network + query
	if network == "ws" {
		query += "&path=" + phpval.RawURLEncode(coalesceString(jsonField(stream, "wsSettings", "path"), "/"))
	}

	return "trojan://" + user.UUID + "@" + uriHostPort(host, ib.Port) +
		"?" + query + "#" + phpval.RawURLEncode(InboundLabel(node, ib))
}

// hysteriaURI 生成 Hysteria2 分享链接：
//
//	hysteria2://<auth>@host:port/?sni=..&security=tls&mport=A-B#备注
//
// auth 就是用户的 UUID（与 clientsFor 注入的 `auth` 字段同源，明文——hy2 的 userinfo
// 本来就该是明文，3x-ui 的 base64 写法只是历史包袱，解析侧两种都认）。
// 端口跳跃的区间取自 streamSettings.finalmask.quicParams.udpHop.ports（面板 hy_hop_ports 的落点），
// 用 mport 参数下发；没配跳跃就整条参数不出现，客户端只连主端口。
func hysteriaURI(node Node, ib Inbound, user User, host string) string {
	stream := streamObject(ib)
	sni := jsonFieldString(stream, "tlsSettings", "serverName")
	if sni == "" {
		sni = host
	}

	query := "sni=" + phpval.RawURLEncode(sni) + "&security=tls"
	if ports := hopPortsOf(stream); ports != "" {
		query += "&mport=" + phpval.RawURLEncode(ports)
	}

	return "hysteria2://" + user.UUID + "@" + uriHostPort(host, ib.Port) +
		"/?" + query + "#" + phpval.RawURLEncode(InboundLabel(node, ib))
}

// hopPortsOf 读端口跳跃区间：finalmask.quicParams.udpHop.ports（"20000-20199" /
// "20000,20005" 这类原样值）。没配时返回空串。
//
// 兼容两种写法：quicParams 直接挂 udpHop，或 3x-ui 那种 finalmask 数组里挑 udpHop 段。
func hopPortsOf(stream phpjson.Object) string {
	quic := phpval.ToObject(jsonField(stream, "finalmask", "quicParams"))
	if ports := strings.TrimSpace(phpval.String(jsonField(quic, "udpHop", "ports"))); ports != "" {
		return ports
	}
	// hysteriaSettings 里也允许写（部分面板把跳跃参数放这里）
	hysteria := phpval.ToObject(jsonField(stream, "hysteriaSettings"))
	return strings.TrimSpace(phpval.String(jsonField(hysteria, "udpHop", "ports")))
}

// ssURI 生成 Shadowsocks 链接（SIP002）：密码在入站侧，同协议用户共用同一链接。
func ssURI(node Node, ib Inbound, host string) string {
	settingsVal, _ := phpjson.ParseOrdered(ib.Settings)
	settings := phpval.ToObject(settingsVal)
	method := jsonFieldString(settings, "method")
	if method == "" {
		method = "aes-256-gcm"
	}
	password := jsonFieldString(settings, "password")

	return "ss://" + base64.StdEncoding.EncodeToString([]byte(method+":"+password)) +
		"@" + uriHostPort(host, ib.Port) +
		"#" + phpval.RawURLEncode(InboundLabel(node, ib))
}

// uriHostPort 拼链接里的「host:port」：域名/IPv4 与 "host:port" 逐字节一致，
// IPv6 字面量补方括号（否则 "@2602:fa4f::1:443" 会把末段当端口）。
func uriHostPort(host string, port int64) string {
	return net.JoinHostPort(host, phpval.Itoa(port))
}

// ServerNameFor 复刻 serverNameFor()：多 SNI 入站按用户稳定抽取一个 serverName。
//
// serverNames 是数组（上限 64），历史上订阅永远只下发 [0]，等于多配的 SNI 全废。
// 取模 user_id 的好处：同一用户恒定（每次拉订阅都同一个 SNI，不会换着来），
// 用户之间分散（多 SNI 的意义就是把流量摊到多个目标域名）。
//
// dest 非空时只用「与 dest 同一站点」的 SNI 参与分派（issue #57）：
// REALITY 转发按客户端 SNI 向 dest 取证书，dest 不认的域名必然握手失败，而分派按
// crc32 固定 —— 一个坏 SNI 会永久废掉 1/n 的用户，重拉订阅也修不好
// （2026-10-22 事故：dest=aws.amazon.com 却配了 www.cloudflare.com，106 个用户连不上）。
// 全是坏值时不缩小池子（退回旧行为），避免把多 SNI 功能整条废掉。
func ServerNameFor(serverNames []any, userID int64, dest string) string {
	names := make([]string, 0, len(serverNames))
	for _, raw := range serverNames {
		if s, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				names = append(names, trimmed)
			}
		}
	}
	if len(names) == 0 {
		return DefaultSNI
	}

	if host := HostOf(dest); host != "" {
		usable := make([]string, 0, len(names))
		for _, name := range names {
			if SameSite(name, host) {
				usable = append(usable, name)
			}
		}
		if len(usable) > 0 {
			names = usable
		}
	}

	// PHP: abs(crc32((string) $userId)) % count($names)
	// crc32 返回 unsigned int，Go 的 ChecksumIEEE 同口径；% len 后天然非负。
	idx := int(crc32.ChecksumIEEE([]byte(phpval.Itoa(userID)))) % len(names)
	return names[idx]
}

// HostOf 复刻 hostOf()：dest 串 → 主机名。
// 支持 "host:443"、"host"、"[2001:db8::1]:443"、"[2001:db8::1]"；解析不出返回空串。
func HostOf(dest string) string {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return ""
	}

	// IPv6 字面量：[v6] 或 [v6]:port
	if strings.HasPrefix(dest, "[") {
		end := strings.IndexByte(dest, ']')
		if end < 0 {
			return ""
		}
		return strings.ToLower(dest[1:end])
	}

	if i := strings.LastIndex(dest, ":"); i >= 0 {
		tail := dest[i+1:]
		if isAllDigitsASCII(tail) {
			dest = dest[:i]
		}
	}
	return strings.ToLower(strings.Trim(dest, " \t\n\r\x00\x0B."))
}

// SameSite 复刻 sameSite()：相等、互为父域，或同属一个注册域。
//
// 用途：REALITY 的 serverNames 必须被 dest 证书覆盖。同一张证书通常覆盖一个注册域
// 及其子域，而跨注册域（aws.amazon.com 与 www.cloudflare.com）几乎不可能同证书。
// 通配符、IP 字面量与空串一律 false。
func SameSite(a, b string) bool {
	a = strings.ToLower(strings.Trim(a, " \t\n\r\x00\x0B."))
	b = strings.ToLower(strings.Trim(b, " \t\n\r\x00\x0B."))

	if a == "" || b == "" || strings.Contains(a, "*") || strings.Contains(b, "*") {
		return false
	}
	if a == b {
		return true
	}
	// IP 字面量之间没有「站点」概念，只认完全相等（上面已判过）
	if isIPLiteral(a) || isIPLiteral(b) {
		return false
	}
	if strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a) {
		return true
	}
	return registeredDomain(a) == registeredDomain(b)
}

// registeredDomain 取注册域（example.com / example.co.uk）；取不出时返回原串。
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

// xhttpQuery 复刻 OutboundLinkGenerator::xhttpQuery()（与 3x-ui 的双编码对齐）。
//
// path/host/mode 之外的高级字段编码为：
//   - extra=<urlencoded-json>：xray-core 系客户端读取的完整 blob
//   - x_padding_bytes=<range>：sing-box 系客户端的 flat 参数
//
// 无高级字段时两者都不产出，老配置链接字节不变。
func xhttpQuery(xhttp phpjson.Object) string {
	query := "&path=" + phpval.RawURLEncode(coalesceString(mustGet(xhttp, "path"), "/"))
	if host := jsonFieldString(xhttp, "host"); host != "" {
		query += "&host=" + phpval.RawURLEncode(host)
	}
	if mode := jsonFieldString(xhttp, "mode"); mode != "" {
		query += "&mode=" + phpval.RawURLEncode(mode)
	}

	// extra 白名单（与 3x-ui sub/service.go buildXhttpExtra 对齐）：只带双向字段。
	// 服务端专属字段不进链接；等于内核默认的 sc* 值剔除（scMinPostsIntervalMs=30 是已知 DPI 指纹）。
	ext := phpjson.Object{}
	padding := strings.TrimSpace(jsonFieldString(xhttp, "xPaddingBytes"))
	if padding != "" {
		ext.Set("xPaddingBytes", padding)
	}
	if phpval.Bool(mustGet(xhttp, "xPaddingObfsMode")) {
		ext.Set("xPaddingObfsMode", true)
		for _, field := range []string{"xPaddingKey", "xPaddingHeader", "xPaddingPlacement", "xPaddingMethod"} {
			if v := strings.TrimSpace(jsonFieldString(xhttp, field)); v != "" {
				ext.Set(field, v)
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
		v := strings.TrimSpace(jsonFieldString(xhttp, field))
		if v == "" || v == coreDefaults[field] {
			continue
		}
		ext.Set(field, v)
	}
	if phpval.Bool(mustGet(xhttp, "uplinkChunkSize")) {
		ext.Set("uplinkChunkSize", phpval.Int(mustGet(xhttp, "uplinkChunkSize")))
	}
	if phpval.Bool(mustGet(xhttp, "noGRPCHeader")) {
		ext.Set("noGRPCHeader", true)
	}
	if len(ext) > 0 {
		body, _ := phpjson.EncodeUnescaped(ext)
		query += "&extra=" + phpval.RawURLEncode(string(body))
	}
	if padding != "" {
		query += "&x_padding_bytes=" + phpval.RawURLEncode(padding)
	}

	return query
}

// ── 内部小工具 ───────────────────────────────────────────────

func streamObject(ib Inbound) phpjson.Object {
	val, _ := phpjson.ParseOrdered(ib.StreamSettings)
	return phpval.ToObject(val)
}

func securityOf(stream phpjson.Object) string {
	security := phpval.String(mustGet(stream, "security"))
	if security == "" {
		return "none"
	}
	return security
}

func networkOf(stream phpjson.Object) string {
	network := phpval.String(mustGet(stream, "network"))
	if network == "" {
		return "tcp"
	}
	return network
}

// jsonField 从保序对象里取嵌套字段（PHP 的 $arr['a']['b'] 读取，缺任何一层都是 null）。
func jsonField(m any, path ...string) any {
	cur := m
	for _, key := range path {
		obj, ok := cur.(phpjson.Object)
		if !ok {
			return nil
		}
		next, ok := obj.Get(key)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// jsonFieldString 等价于 PHP 的 `(string)($arr['a']['b'] ?? ”)`。
func jsonFieldString(m any, path ...string) string { return phpval.String(jsonField(m, path...)) }

// jsonFieldArray 取嵌套数组；不是数组时返回 nil。
func jsonFieldArray(m any, path ...string) []any {
	v := jsonField(m, path...)
	if arr, ok := v.(phpjson.Array); ok {
		return arr
	}
	return nil
}

// toAnySlice 把 JSON 数组转成 []any（非数组 → nil）。
func toAnySlice(v any) []any {
	if arr, ok := v.(phpjson.Array); ok {
		return arr
	}
	return nil
}

// coalesceString 对应 PHP 的 `$x ?? 'default'`：键不存在 / 值为 null 才回落，
// 空串是**有效值**（PHP 的 `??` 不是 `?:`，这个区别直接影响链接字节）。
func coalesceString(v any, def string) string {
	switch t := v.(type) {
	case nil:
		return def
	case string:
		return t
	case phpjson.Object, phpjson.Array:
		// PHP 里对数组做字符串拼接会得到 "Array"（并告警），链接里不会出现这种配置；
		// 保守回落到默认值，避免产出 "Array" 这种脏参数。
		return def
	default:
		return phpval.String(t)
	}
}

// firstStringValue 取数组第一个元素的字符串（PHP 的 (string)($arr[0] ?? ”)）。
func firstStringValue(v any) string {
	s, _ := firstStringOK(toAnySlice(v))
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

// isAllDigitsASCII 判断字符串是否全为 ASCII 数字（空串返回 false）。
func isAllDigitsASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
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
		if phpval.IntFromString(part) > 255 {
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
