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

// parseHysteriaPassword 对应 parseHysteriaPassword()：base64(密码[:/aead]) → 明文密码。
// aead 后缀（/gch、/aead）是拥塞控制/混淆方式，客户端自动处理，不进密码字段。
func parseHysteriaPassword(userPart string) (string, bool) {
	decoded, ok := decodeBase64URL(userPart)
	if !ok || decoded == "" {
		return "", false
	}
	decoded = hysteriaAeadPattern.ReplaceAllString(decoded, "")
	if decoded == "" {
		return "", false
	}
	return decoded, true
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
	proxy.set("network", mapOr(params, "type", "tcp"))
	if typ, has := params["type"]; has {
		switch typ {
		case "ws":
			wsOpts := obj("path", mapOr(params, "path", "/"))
			if host, hasHost := params["host"]; hasHost {
				wsOpts.set("headers", obj("Host", host))
			}
			proxy.set("ws-opts", wsOpts)
		case "xhttp":
			xhttpOpts := obj(
				"path", mapOr(params, "path", "/"),
				"mode", mapOr(params, "mode", "auto"),
			)
			if extra, hasExtra := params["extra"]; hasExtra {
				if decoded, isMap := decodeJSONAny(extra).(phpMap); isMap {
					if padding, found := decoded.get("xPaddingBytes"); found {
						xhttpOpts.set("x-padding-bytes", padding)
					}
				}
			}
			proxy.set("xhttp-opts", xhttpOpts)
		case "httpupgrade":
			// mihomo：network=httpupgrade + http-upgrade-opts（键名与 3x-ui clash 输出一致）
			huOpts := phpMap{}
			if p := params["path"]; p != "" {
				huOpts.set("path", p)
			}
			if h := params["host"]; h != "" {
				huOpts.set("headers", obj("Host", h))
			}
			if len(huOpts) > 0 {
				proxy.set("http-upgrade-opts", huOpts)
			}
		}
	}
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
			"password", vmessUUID,
			"client-fingerprint", mapOr(parsed.Params, "fp", "chrome"),
		)
		// mihomo：aid≠0 时透传 alter_id + 对应加密；aid=0 一律 none
		if phpInt(jsonField(auth, "alterId")) > 0 {
			proxy.set("alter_id", jsonNumber(itoa(phpInt(jsonField(auth, "alterId")))))
			proxy.set("encryption", mapOr(parsed.Params, "enc", "auto"))
		} else {
			proxy.set("encryption", "none")
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
			proxy.set("servername", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)))
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
	proxy.set("network", mapOr(parsed.Params, "type", "tcp"))
	if typ, has := parsed.Params["type"]; has {
		switch typ {
		case "ws":
			wsOpts := obj("path", mapOr(parsed.Params, "path", "/"))
			if host, hasHost := parsed.Params["host"]; hasHost {
				wsOpts.set("headers", obj("Host", host))
			}
			proxy.set("ws-opts", wsOpts)
		case "xhttp":
			// clash 的 xhttp-opts 保留（mihomo 1.19.31 实测支持）
			xhttpOpts := obj(
				"path", mapOr(parsed.Params, "path", "/"),
				"mode", mapOr(parsed.Params, "mode", "auto"),
			)
			if extra, hasExtra := parsed.Params["extra"]; hasExtra {
				if decoded, isMap := decodeJSONAny(extra).(phpMap); isMap {
					if padding, found := decoded.get("xPaddingBytes"); found {
						xhttpOpts.set("x-padding-bytes", padding)
					}
				}
			}
			proxy.set("xhttp-opts", xhttpOpts)
		case "httpupgrade":
			huOpts := phpMap{}
			if p := parsed.Params["path"]; p != "" {
				huOpts.set("path", p)
			}
			if h := parsed.Params["host"]; h != "" {
				huOpts.set("headers", obj("Host", h))
			}
			if len(huOpts) > 0 {
				proxy.set("http-upgrade-opts", huOpts)
			}
		}
	}

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
		// sing-box 只认这几种 vmess 加密
		enc := mapOr(parsed.Params, "enc", "auto")
		switch enc {
		case "none", "aes-128-gcm", "chacha20-poly1305", "xchacha20-poly1305":
			outbound.set("cipher", enc)
		default:
			outbound.set("cipher", "auto")
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
	if security, has := parsed.Params["security"]; has {
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
			if fp, hasFp := parsed.Params["fp"]; hasFp {
				tlsConfig.set("utls", obj("enabled", true, "fingerprint", fp))
			}
			outbound.set("tls", tlsConfig)
		}
	}

	// 传输协议
	network := mapOr(parsed.Params, "type", "tcp")
	if network != "" && network != "tcp" && network != "none" {
		// sing-box 1.14.1 不支持 xhttp 等传输：跳过该节点
		switch network {
		case "ws", "grpc", "http", "httpupgrade":
		default:
			return nil
		}
		transport := obj("type", network)
		if path, has := parsed.Params["path"]; has {
			transport.set("path", path)
		}
		if host, has := parsed.Params["host"]; has {
			transport.set("headers", obj("Host", host))
		}
		outbound.set("transport", transport)
	}

	return outbound
}
