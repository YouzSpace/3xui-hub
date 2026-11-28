package subscription

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// 逐条复刻 SubscriptionService.php 的解析段：
//
//	parseLinkToClashProxy() / parseLinkToSingboxOutbound() /
//	applyClashTlsAndNetwork() / parseSsCredentials() / parseHysteriaPassword()
//
// 两个解析器共用「协议 + userinfo + host + port / query / #name」的解析结果，
// 只在落地结构上分叉（Clash proxy 字典 vs Sing-box outbound 字典）。

// parsedLink 是链接的公共解析结果。
type parsedLink struct {
	Protocol string
	UserPart string
	Server   string
	Port     int64
	Params   map[string]string
	Name     string
}

// parseLink 对应两个解析器开头的正则 + 参数 + 名称解析。
// 解析失败（协议不支持 / 形态不匹配）返回 (nil, false)，调用方跳过该节点。
func parseLink(link string) (*parsedLink, bool) {
	// vmess 的分享格式与其它协议不同：整段 JSON 的 base64，没有 `uuid@host:port`。
	// 先单独解，否则 linkPattern 永远匹配不上 → vmess 节点在 Clash / sing-box 订阅里
	// 被静默丢弃（只剩 base64 订阅里有）。
	if strings.HasPrefix(link, "vmess://") {
		return parseVmessLink(link)
	}

	m := linkPattern.FindStringSubmatch(link)
	if m == nil {
		return nil, false
	}

	parsed := &parsedLink{
		Protocol: normalizeLinkProtocol(m[1]),
		UserPart: m[2],
		Server:   serverHost(m[3]),
		Port:     parseIntPrefix(m[4]),
		Params:   map[string]string{},
	}

	// 解析参数：'?' 之后到 '#' 之前
	if q := strings.IndexByte(link, '?'); q >= 0 {
		queryString := link[q+1:]
		if h := strings.IndexByte(queryString, '#'); h >= 0 {
			queryString = queryString[:h]
		}
		parsed.Params = parseStr(queryString)
	}

	// 解析名称：#fragment（url 解码）
	if h := strings.LastIndex(link, "#"); h >= 0 {
		parsed.Name = urldecode(link[h+1:])
	}
	return parsed, true
}

// parseVmessLink 把 vmess:// 的 base64(JSON) 分享链接折算成统一结构。
//
// 字段来源（xray/v2ray 的 vmess 分享 JSON）：
//   - add / port / id / ps：地址、端口、凭据、备注；
//   - net：传输（tcp/ws/grpc/xhttp/httpupgrade）→ Params["type"]；
//   - type：tcp 的伪装方式（none/http）→ Params["headerType"]；
//   - host / path：ws / httpupgrade / xhttp 的 Host 与 Path；**grpc 的 serviceName 也放 path**，
//     这里额外折出 Params["serviceName"]（Clash / sing-box 的 grpc 只认这个键）；
//   - tls / sni：安全层与 SNI；scy：vmess 加密方式（clash 侧叫 cipher）。
func parseVmessLink(link string) (*parsedLink, bool) {
	payload := strings.TrimPrefix(link, "vmess://")
	// 第三方订阅会给 vmess 补 `#中性名`（thirdparty 的改名校验），base64 段必须先截掉，
	// 否则整段解码失败 → 节点被丢。
	fragment := ""
	if h := strings.IndexByte(payload, '#'); h >= 0 {
		fragment = urldecode(payload[h+1:])
		payload = payload[:h]
	}
	raw, ok := decodeBase64URL(payload)
	if !ok {
		return nil, false
	}
	doc, ok := decodeJSONAny(raw).(phpMap)
	if !ok {
		return nil, false
	}

	server := phpTrim(jsonFieldString(doc, "add"))
	port := parseIntPrefix(phpTrim(jsonFieldString(doc, "port")))
	if server == "" || port <= 0 || phpTrim(jsonFieldString(doc, "id")) == "" {
		return nil, false
	}

	params := map[string]string{}
	network := phpTrim(jsonFieldString(doc, "net"))
	if network == "" {
		network = "tcp"
	}
	params["type"] = network
	if tls := phpTrim(jsonFieldString(doc, "tls")); tls != "" && tls != "none" {
		params["security"] = tls
	}
	if sni := phpTrim(jsonFieldString(doc, "sni")); sni != "" {
		params["sni"] = sni
	}
	host := phpTrim(jsonFieldString(doc, "host"))
	path := phpTrim(jsonFieldString(doc, "path"))
	if host != "" {
		params["host"] = host
	}
	if path != "" {
		params["path"] = path
	}
	if network == "grpc" && path != "" {
		params["serviceName"] = path
	}
	if headerType := phpTrim(jsonFieldString(doc, "type")); headerType != "" && headerType != "none" {
		params["headerType"] = headerType
	}
	if cipher := phpTrim(jsonFieldString(doc, "scy")); cipher != "" {
		params["enc"] = cipher
	}

	name := phpTrim(jsonFieldString(doc, "ps"))
	if phpTrim(fragment) != "" {
		name = phpTrim(fragment)
	}

	return &parsedLink{
		Protocol: "vmess",
		UserPart: payload,
		Server:   serverHost(server),
		Port:     port,
		Params:   params,
		Name:     name,
	}, true
}

// serverHost 把正则的 host 捕获组归一成「能直接写进客户端 server 字段」的形态。
//
// 链接里 IPv6 必须写成 `[2001:db8::1]`（RFC 3986），但 Clash 的 `server:` 与 sing-box 的
// `server` 要的是裸地址：两边都是拿 server + port 拼 host:port 去拨号，带方括号会变成
// `[[2001:db8::1]]:443`（mihomo）或被当域名去解析（sing-box）。
// 所以 parsedLink.Server 全链路只有一种形态 —— 裸地址，下游各分支不用逐个判断。
// 反向（拼链接时补方括号）在 xrayuri 侧（xrayVlessURI 等），两端可逆。
func serverHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// bracketHost 是 serverHost 的反向：IPv6 裸地址补回方括号（IPv4/域名原样返回）。
func bracketHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// parseSsCredentials 对应 parseSsCredentials()：userinfo → [method, password]。
//
// 两种形态都要认：
//   - 传统 AEAD / 单凭据 2022：base64(method:password)（历史口径）；
//   - SS-2022 多用户：明文百分号编码 `method:服务端PSK:用户PSK`（SIP022，本站 2022
//     入站生成的就是这种）。多用户时 password 原样保留 "服务端:用户" 两段 ——
//     mihomo / sing-box 的 2022 出口都按这个格式读。
func parseSsCredentials(userInfo string) (string, string, bool) {
	// base64 字母表里没有 ':'，所以「解码后含 ':'」就可以确定是明文形态。
	// 这里只解 %XX 不动 '+'（base64 的 '+' 会被 PHP urldecode 变成空格，老链接解坏）。
	if plain := percentDecode(userInfo); strings.Contains(plain, ":") {
		colon := strings.IndexByte(plain, ':')
		method, password := plain[:colon], plain[colon+1:]
		if method == "" || password == "" {
			return "", "", false
		}
		return method, password, true
	}

	decoded, ok := decodeBase64URL(userInfo)
	if !ok {
		return "", "", false
	}
	colon := strings.IndexByte(decoded, ':')
	if colon < 0 {
		return "", "", false
	}
	method, password := decoded[:colon], decoded[colon+1:]
	if method == "" || password == "" {
		return "", "", false
	}
	return method, password, true
}

// percentDecode 只解 %XX，不把 '+' 当空格（与 PHP urldecode 的差别）。
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok := hexVal(s[i+1]); ok {
				if lo, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseHysteriaAuth 是 hy2 链接 userinfo 的读取口径：**先试 base64，回落到明文**。
//
// hysteria2 生态里两种写法并存：3x-ui 等面板发 base64（老 PHP 行为），而官方文档与
// 多数客户端发明文 auth（本站生成的链接就是明文 uuid）。只认 base64 会把明文密码
// 解成乱码或整条丢弃。
//
// 判定用「规范化往返」而不是「能不能解出来」：明文 uuid 里的 '-' 会被 decodeBase64URL
// 换成 '+'，重新编码后与原文不同 → 判定为明文。这样两条路互不误伤。
func parseHysteriaAuth(userPart string) (string, bool) {
	if decoded, ok := canonicalBase64Decode(userPart); ok {
		decoded = hysteriaAeadPattern.ReplaceAllString(decoded, "")
		if decoded != "" {
			return decoded, true
		}
	}
	plain := strings.TrimSpace(urldecode(userPart))
	plain = hysteriaAeadPattern.ReplaceAllString(plain, "")
	if plain == "" {
		return "", false
	}
	return plain, true
}

// canonicalBase64Decode 只在「解码后重新编码 == 原文（忽略 padding/URL 安全字母表）」时返回结果，
// 避免把普通明文当成 base64 解成乱码。
func canonicalBase64Decode(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	decoded, ok := decodeBase64URL(value)
	if !ok || decoded == "" || !utf8.ValidString(decoded) {
		return "", false
	}
	trimmed := strings.TrimRight(value, "=")
	std := strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(decoded)), "=")
	url := strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(decoded)), "=")
	if std == trimmed || url == trimmed {
		return decoded, true
	}
	return "", false
}

// normalizeLinkProtocol 归一协议别名：hy2 与 hysteria2 等价（hysteria 官方文档的简写）。
// 归一后 Clash 的 type / sing-box 的 type 都写 hysteria2，避免下游出现 "hy2" 这种客户端不认的值。
func normalizeLinkProtocol(protocol string) string {
	if protocol == "hy2" {
		return "hysteria2"
	}
	return protocol
}

// clashHysteriaPorts 规整 mport：跳跃区间可能是 "A-B"、"A,B,C" 或 "A"。空白一律去掉，
// 其余原样透传（mihomo 的 ports 就是这几种写法）。
func clashHysteriaPorts(raw string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(raw, " ", "")), "")
}

// singboxServerPorts 把 mport 转成 sing-box 的 server_ports 数组。
//
// sing-box 的区间写法是 "A:B"（不是 hy2 链接里的 "A-B"），逗号分隔的多段逐个转换；
// 任何一段不是合法端口/区间就整条不带（宁可不跳跃，也不下发客户端解析不了的值）。
func singboxServerPorts(raw string) []any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make([]any, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		lo = strings.TrimSpace(lo)
		if !isPortText(lo) {
			return nil
		}
		if !isRange {
			out = append(out, lo)
			continue
		}
		hi = strings.TrimSpace(hi)
		if !isPortText(hi) {
			return nil
		}
		out = append(out, lo+":"+hi)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isPortText 判断是否全为 ASCII 数字（1..5 位，且落在 1-65535）。
func isPortText(s string) bool {
	if s == "" || len(s) > 5 || !isAllDigitsASCII(s) {
		return false
	}
	port := parseIntPrefix(s)
	return port >= 1 && port <= 65535
}

// applyClashHysteria2 追加 hysteria2 专有字段（端口跳跃 / alpn / 混淆 / 证书校验）。
//
// 这些键 PHP 老实现没有，追加在既有字段之后：不带这些参数的链接字节完全不变。
func applyClashHysteria2(proxy *phpMap, parsed *parsedLink) {
	if parsed.Protocol != "hysteria2" {
		return
	}
	if ports := clashHysteriaPorts(parsed.Params["mport"]); ports != "" {
		proxy.set("ports", ports)
	}
	// mihomo 的 hysteria2 固定走 h3（显式写出，避免客户端按别家默认值协商失败）
	proxy.set("alpn", []any{"h3"})
	if obfs := strings.TrimSpace(parsed.Params["obfs"]); obfs != "" {
		proxy.set("obfs", obfs)
		if password := strings.TrimSpace(parsed.Params["obfs-password"]); password != "" {
			proxy.set("obfs-password", password)
		}
	}
	// insecure=1 只置 tls 不够：mihomo 仍需 skip-cert-verify 才真的跳过校验（自签证书场景）
	if phpTruthy(mapOr(parsed.Params, "insecure", "0")) {
		proxy.set("skip-cert-verify", true)
	}
	// pinSHA256：证书指纹固定（mihomo 的 fingerprint 就是 hysteria2 证书 sha256）
	if pin := strings.TrimSpace(parsed.Params["pinSHA256"]); pin != "" {
		proxy.set("fingerprint", pin)
	}
}

// phpTruthy 近似 PHP 的布尔真值判定（"0" / "" / "false" 为假）。
func phpTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

// applyClashNetwork 把分享链接的传输参数映射成 mihomo 认的 proxy 字段。
//
// mihomo 的 network 只有 tcp / ws / http / h2 / grpc / xhttp，没有 httpupgrade：
//   - httpupgrade 必须写成 ws + ws-opts.v2ray-http-upgrade: true；
//   - tcp 的 HTTP 伪装写成 network: http + http-opts；
//   - grpc 的 serviceName 写 grpc-opts.grpc-service-name（链接参数名 serviceName）。
//
// 两处 parse（vless/trojan/ss 一组、vmess 一组）共用，避免只改一侧的漂移。
func applyClashNetwork(proxy *phpMap, params map[string]string) {
	network := mapOr(params, "type", "tcp")
	switch network {
	case "ws":
		proxy.set("network", "ws")
		wsOpts := obj("path", mapOr(params, "path", "/"))
		if host, hasHost := params["host"]; hasHost {
			wsOpts.set("headers", obj("Host", host))
		}
		proxy.set("ws-opts", wsOpts)
	case "httpupgrade":
		// mihomo 没有 network: httpupgrade（未知值会回落 tcp），
		// 官方写法是 ws + v2ray-http-upgrade 开关。
		proxy.set("network", "ws")
		wsOpts := obj("path", mapOr(params, "path", "/"))
		if host, hasHost := params["host"]; hasHost {
			wsOpts.set("headers", obj("Host", host))
		}
		wsOpts.set("v2ray-http-upgrade", true)
		proxy.set("ws-opts", wsOpts)
	case "grpc":
		proxy.set("network", "grpc")
		g := phpMap{}
		if sn := params["serviceName"]; sn != "" {
			g.set("grpc-service-name", sn)
		}
		if len(g) > 0 {
			proxy.set("grpc-opts", g)
		}
	case "xhttp":
		proxy.set("network", "xhttp")
		xhttpOpts := obj(
			"path", mapOr(params, "path", "/"),
			"mode", mapOr(params, "mode", "auto"),
		)
		if host := params["host"]; host != "" {
			xhttpOpts.set("host", host)
		}
		if extra, hasExtra := params["extra"]; hasExtra {
			if decoded, isMap := decodeJSONAny(extra).(phpMap); isMap {
				if padding, found := decoded.get("xPaddingBytes"); found {
					xhttpOpts.set("x-padding-bytes", padding)
				}
			}
		}
		proxy.set("xhttp-opts", xhttpOpts)
	case "tcp":
		if strings.EqualFold(params["headerType"], "http") {
			// mihomo 的 HTTP 伪装是独立 network（不是 tcp + header），
			// path / Host 都是数组（客户端每次请求随机取一个）。
			proxy.set("network", "http")
			httpOpts := obj("path", []any{mapOr(params, "path", "/")})
			if host := params["host"]; host != "" {
				httpOpts.set("headers", obj("Host", []any{host}))
			}
			proxy.set("http-opts", httpOpts)
		} else {
			proxy.set("network", "tcp")
		}
	default:
		proxy.set("network", "tcp")
	}
}

// applyClashTlsAndNetwork 对应 applyClashTlsAndNetwork()：mihomo 的 TLS / 传输段（vmess 分支复用）。
func applyClashTlsAndNetwork(proxy *phpMap, params map[string]string, server string) {
	// TLS / Reality
	if security, ok := params["security"]; ok {
		switch security {
		case "tls":
			proxy.set("tls", true)
			if sni, has := params["sni"]; has {
				proxy.set("sni", cleanSni(sni))
			}
		case "reality":
			proxy.set("tls", true)
			proxy.set("servername", cleanSni(mapOr(params, "sni", server)))
			if fp, has := params["fp"]; has {
				proxy.set("client-fingerprint", fp)
			}
			if pbk, has := params["pbk"]; has {
				reality := obj("public-key", pbk)
				if sid, hasSid := params["sid"]; hasSid {
					reality.set("short-id", sid)
				}
				proxy.set("reality-opts", reality)
			}
		}
	}

	// 传输协议
	// 传输协议：映射规则收敛在 applyClashNetwork（两处 parse 共用）。
	applyClashNetwork(proxy, params)
}

// mapOr 对应 PHP 的 `$arr[$key] ?? $default`。
func mapOr(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// parseLinkToClashProxy 对应 parseLinkToClashProxy()：解析失败/不支持返回 nil（跳过该节点）。
func (s *Service) parseLinkToClashProxy(link string, regionCounters map[string]int, cfg *siteConfigReader) phpMap {
	parsed, ok := parseLink(link)
	if !ok {
		return nil
	}

	name := parsed.Name
	if phpEmptyString(name) {
		name = fallbackName(parsed)
	}
	// 应用名称处理（用户 ID 清理 / 正则重命名 / 旗帜）
	name = s.processNodeName(name, regionCounters, cfg)
	if phpTrim(name) == "" {
		name = fallbackName(parsed)
	}

	// mihomo 的传输支持分协议：xhttp 只有 vless 支持，trojan 只有 tcp/ws/grpc（没有 HTTP 伪装）。
	// 不支持的组合写进 YAML 会让 mihomo 整份配置加载失败（比丢一个节点严重），所以跳过该节点。
	network := mapOr(parsed.Params, "type", "tcp")
	if network == "xhttp" && parsed.Protocol != "vless" {
		return nil
	}
	if parsed.Protocol == "trojan" && network == "tcp" && strings.EqualFold(parsed.Params["headerType"], "http") {
		return nil
	}

	switch parsed.Protocol {
	case "ss":
		method, password, ok := parseSsCredentials(parsed.UserPart)
		if !ok {
			return nil
		}
		return obj(
			"name", name,
			"type", "ss",
			"server", parsed.Server,
			"port", jsonNumber(itoa(parsed.Port)),
			"cipher", method,
			"password", password,
			"udp", true,
		)

	case "vmess":
		decoded, ok := decodeBase64URL(parsed.UserPart)
		if !ok {
			return nil
		}
		auth, isMap := decodeJSONAny(decoded).(phpMap)
		if !isMap {
			return nil
		}
		vmessUUID := jsonFieldString(auth, "id")
		if vmessUUID == "" {
			return nil
		}

		proxy := obj(
			"name", name,
			"type", "vmess",
			"server", parsed.Server,
			"port", jsonNumber(itoa(parsed.Port)),
			"udp", true,
			"uuid", vmessUUID,
			"client-fingerprint", mapOr(parsed.Params, "fp", "chrome"),
		)
		// mihomo 的 vmess 必填 uuid / alterId / cipher：缺任何一个，它解析整份配置时
		// 会 `has unset fields` 直接失败（不是丢一个节点，是整份 Clash 配置加载不了）。
		// aid=0（AEAD）固定 cipher=none，aid>0 才透传链接里的 scy。
		alterID := phpInt(jsonField(auth, "alterId"))
		proxy.set("alterId", jsonNumber(itoa(alterID)))
		if alterID > 0 {
			proxy.set("cipher", mapOr(parsed.Params, "enc", "auto"))
		} else {
			proxy.set("cipher", "none")
		}
		applyClashTlsAndNetwork(&proxy, parsed.Params, parsed.Server)
		return proxy

	case "hysteria2", "hysteria":
		password, ok := parseHysteriaAuth(parsed.UserPart)
		if !ok {
			return nil
		}
		proxy := obj(
			"name", name,
			"type", parsed.Protocol,
			"server", parsed.Server,
			"port", jsonNumber(itoa(parsed.Port)),
			"password", password,
			"udp", true,
		)
		if _, hasSni := parsed.Params["sni"]; hasSni || mapOr(parsed.Params, "insecure", "0") != "0" {
			proxy.set("tls", true)
			// mihomo 的 hysteria2 只认 sni（没有 servername）。
			proxy.set("sni", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)))
		}
		if fp, has := parsed.Params["fp"]; has {
			proxy.set("client-fingerprint", fp)
		}
		// mihomo 的 hysteria 依赖 sniffing 探测流量类型，显式开启
		proxy.set("sniffing", true)
		applyClashHysteria2(&proxy, parsed)
		return proxy
	}

	proxy := obj(
		"name", name,
		"type", parsed.Protocol,
		"server", parsed.Server,
		"port", jsonNumber(itoa(parsed.Port)),
		"udp", true,
		"client-fingerprint", mapOr(parsed.Params, "fp", "chrome"),
	)

	// trojan 用 password，vless 用 uuid
	if parsed.Protocol == "trojan" {
		proxy.set("password", parsed.UserPart)
	} else {
		proxy.set("uuid", parsed.UserPart)

		// flow 只透传不发明：链接没有 flow 就绝不输出
		if flow, has := parsed.Params["flow"]; has {
			proxy.set("flow", flow)
		}
		if enc, has := parsed.Params["encryption"]; has {
			proxy.set("encryption", enc)
		} else {
			// VLESS 协议默认使用 none 加密
			proxy.set("encryption", "none")
		}
	}

	// TLS / Reality（与 vmess 分支同一套字段写法，但 vless/trojan 的 sni 走 'sni' 键）
	if security, has := parsed.Params["security"]; has {
		switch security {
		case "tls":
			proxy.set("tls", true)
			if sni, hasSni := parsed.Params["sni"]; hasSni {
				proxy.set("sni", cleanSni(sni))
			}
		case "reality":
			proxy.set("tls", true)
			proxy.set("servername", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)))
			if fp, hasFp := parsed.Params["fp"]; hasFp {
				proxy.set("client-fingerprint", fp)
			}
			if pbk, hasPbk := parsed.Params["pbk"]; hasPbk {
				reality := obj("public-key", pbk)
				if sid, hasSid := parsed.Params["sid"]; hasSid {
					reality.set("short-id", sid)
				}
				proxy.set("reality-opts", reality)
			}
		}
	}

	// 传输协议
	// 传输协议：与 applyClashTlsAndNetwork 共用同一套映射。
	applyClashNetwork(&proxy, parsed.Params)

	return proxy
}

// fallbackName 对应 PHP 的 `"{$protocol}-{$server}:{$port}"`。
// server 是裸地址（见 serverHost），IPv6 补回方括号，否则 `vless-2001:db8::1:443`
// 分不清哪段是端口。
func fallbackName(parsed *parsedLink) string {
	return parsed.Protocol + "-" + bracketHost(parsed.Server) + ":" + itoa(parsed.Port)
}

// parseLinkToSingboxOutbound 对应 parseLinkToSingboxOutbound()。
func (s *Service) parseLinkToSingboxOutbound(link string, regionCounters map[string]int, cfg *siteConfigReader) phpMap {
	parsed, ok := parseLink(link)
	if !ok {
		return nil
	}

	name := parsed.Name
	if phpEmptyString(name) {
		name = fallbackName(parsed)
	}
	// 应用名称处理（用户 ID 清理 / 正则重命名 / 旗帜）
	name = s.processNodeName(name, regionCounters, cfg)
	if phpTrim(name) == "" {
		name = fallbackName(parsed)
	}

	outboundType := parsed.Protocol
	if parsed.Protocol == "ss" {
		outboundType = "shadowsocks"
	}
	outbound := obj(
		"type", outboundType,
		"tag", name,
		"server", parsed.Server,
		"server_port", jsonNumber(itoa(parsed.Port)),
	)

	switch parsed.Protocol {
	case "vless":
		outbound.set("uuid", parsed.UserPart)
		// flow 只透传不发明：链接没有 flow 就绝不输出
		if flow, has := parsed.Params["flow"]; has {
			outbound.set("flow", flow)
		}
		// 注意：sing-box 的 vless 不输出 encryption 字段

	case "trojan":
		outbound.set("password", parsed.UserPart)

	case "vmess":
		decoded, ok := decodeBase64URL(parsed.UserPart)
		if !ok {
			return nil
		}
		auth, isMap := decodeJSONAny(decoded).(phpMap)
		if !isMap {
			return nil
		}
		vmessUUID := jsonFieldString(auth, "id")
		if vmessUUID == "" {
			return nil
		}
		outbound.set("uuid", vmessUUID)
		// sing-box 的 vmess 字段名是 security（不是 cipher），alter_id 缺省 0 可省。
		enc := mapOr(parsed.Params, "enc", "auto")
		switch enc {
		case "none", "aes-128-gcm", "chacha20-poly1305", "xchacha20-poly1305":
			outbound.set("security", enc)
		default:
			outbound.set("security", "auto")
		}
		if alterID := phpInt(jsonField(auth, "alterId")); alterID > 0 {
			outbound.set("alter_id", jsonNumber(itoa(alterID)))
		}

	case "ss":
		method, password, ok := parseSsCredentials(parsed.UserPart)
		if !ok {
			return nil
		}
		outbound.set("method", method)
		outbound.set("password", password)

	case "hysteria2", "hysteria":
		password, ok := parseHysteriaAuth(parsed.UserPart)
		if !ok {
			return nil
		}
		outbound.set("type", parsed.Protocol)
		outbound.set("password", password)
		// 端口跳跃：sing-box 1.11+ 的 server_ports（区间写法是 "A:B"，不是链接里的 "A-B"）。
		// 不带 hop_interval：链接里没有这个信息，sing-box 自己按默认间隔重拨。
		if ports := singboxServerPorts(parsed.Params["mport"]); ports != nil {
			outbound.set("server_ports", ports)
		}
		// sing-box hysteria2 出站要求 TLS 段：有 sni 用 sni，否则默认带 server_name
		tls := obj("enabled", true)
		if sni, has := parsed.Params["sni"]; has {
			tls.set("server_name", cleanSni(sni))
		}
		if mapOr(parsed.Params, "insecure", "0") == "1" {
			tls.set("insecure", true)
		}
		if fp, has := parsed.Params["fp"]; has {
			tls.set("utls", obj("enabled", true, "fingerprint", fp))
		}
		outbound.set("tls", tls)
		// obfs（salamander）：sing-box 是对象形态 {type, password}
		if obfs := strings.TrimSpace(parsed.Params["obfs"]); obfs != "" && parsed.Protocol == "hysteria2" {
			obfsObj := obj("type", obfs)
			if pwd := strings.TrimSpace(parsed.Params["obfs-password"]); pwd != "" {
				obfsObj.set("password", pwd)
			}
			outbound.set("obfs", obfsObj)
		}
	}

	// TLS / Reality
	// hysteria2 的 TLS 段在上面按协议分支单独建好了（含 insecure / utls），这里不能再覆盖：
	// 通用块只写 {enabled, server_name}，覆盖后自签证书的 insecure 与指纹全丢。
	if security, has := parsed.Params["security"]; has && parsed.Protocol != "hysteria2" && parsed.Protocol != "hysteria" {
		switch security {
		case "tls":
			outbound.set("tls", obj(
				"enabled", true,
				"server_name", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)),
			))
		case "reality":
			tlsConfig := obj(
				"enabled", true,
				"server_name", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)),
				"reality", obj(
					"enabled", true,
					"public_key", mapOr(parsed.Params, "pbk", ""),
					"short_id", mapOr(parsed.Params, "sid", ""),
				),
			)
			// sing-box 的 reality client 强制 uTLS：缺 fp 会直接报
			// "uTLS is required by reality client"，链接没带就回落 chrome。
			fp := mapOr(parsed.Params, "fp", "chrome")
			tlsConfig.set("utls", obj("enabled", true, "fingerprint", fp))
			outbound.set("tls", tlsConfig)
		}
	}

	// 传输协议
	// Xray 的「TCP + HTTP 伪装」在 sing-box 里没有忠实对应（它的 http transport 在 TLS 下是
	// HTTP/2、且请求方法/头语义不同），映射过去只会得到一条连不上的节点 —— 直接跳过。
	if mapOr(parsed.Params, "type", "tcp") == "tcp" && strings.EqualFold(parsed.Params["headerType"], "http") {
		return nil
	}
	network := mapOr(parsed.Params, "type", "tcp")
	if network != "" && network != "tcp" && network != "none" {
		// sing-box 1.14.1 不支持 xhttp 等传输：跳过该节点
		switch network {
		case "ws", "grpc", "httpupgrade":
		default:
			return nil
		}
		transport := obj("type", network)
		if network == "grpc" {
			// sing-box 的 grpc transport 字段是 service_name（缺省空串会连到错误的 service）。
			if sn := parsed.Params["serviceName"]; sn != "" {
				transport.set("service_name", sn)
			}
		}
		// sing-box 的 grpc transport 没有 path 字段：vmess 的 serviceName 恰好放在链接的
		// path 里，若不排除会把 path 一起写进 grpc 段 —— sing-box 对未知字段是**致命错误**，
		// 整份配置都加载不了。
		if path, has := parsed.Params["path"]; has && network != "grpc" {
			transport.set("path", path)
		}
		if host, has := parsed.Params["host"]; has {
			if network == "httpupgrade" {
				// sing-box 的 httpupgrade Host 是独立字段，不从 headers 取。
				transport.set("host", host)
			} else {
				transport.set("headers", obj("Host", host))
			}
		}
		outbound.set("transport", transport)
	}

	return outbound
}
