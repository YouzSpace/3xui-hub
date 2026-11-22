package admin

// 出站分享链接 —— 解析（链接字符串 → {protocol,settings,stream_settings,tag}）。
// 与「生成」（xray_outbound_link.go）互为逆运算；逐条对齐 backend/app/Drivers/Xray/OutboundLinkParser.php。
// 本文件只放解析相关函数（从原 xray_outbound_link.go 拆出，行为未改）。

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ---- 解析 ----

// xrayParseOutboundLink 对齐 OutboundLinkParser::parse()。
//
// 返回 {protocol, settings, stream_settings, tag} 的保序结构。
func xrayParseOutboundLink(link string) (*jsObj, error) {
	link = strings.TrimSpace(link)
	scheme := linkScheme(link)
	// scheme → 解析器的分发走 outboundLinkParsers 注册表（见 xray_outbound_link_registry.go）：
	// 新增可导入的链接 scheme 只在该注册表登记一行，不用再改这里的 switch。
	if parse, ok := outboundLinkParsers[scheme]; ok {
		return parse(link)
	}
	if scheme == "" {
		return nil, errors.New("不支持的链接协议：（空）")
	}
	return nil, errors.New("不支持的链接协议：" + scheme)
}

// linkScheme 取 URL 的 scheme（小写）。
func linkScheme(link string) string {
	idx := strings.Index(link, "://")
	if idx <= 0 {
		return ""
	}
	return strings.ToLower(link[:idx])
}

func parseVMessLink(link string) (*jsObj, error) {
	body := link[len("vmess://"):]
	decoded, ok := base64Loose(body)
	if !ok {
		return nil, errors.New("vmess 链接内容不完整")
	}
	parsed, err := parseOrderedJS([]byte(decoded))
	if err != nil {
		return nil, errors.New("vmess 链接内容不完整")
	}
	d, isObj := parsed.(*jsObj)
	if !isObj {
		return nil, errors.New("vmess 链接内容不完整")
	}
	add := nodeScalarString(valueOrNil(d, "add"))
	portRaw := nodeScalarString(valueOrNil(d, "port"))
	id := nodeScalarString(valueOrNil(d, "id"))
	if add == "" || portRaw == "" || portRaw == "0" || id == "" {
		return nil, errors.New("vmess 链接内容不完整")
	}

	net := stringOr(nodeScalarString(valueOrNil(d, "net")), "tcp")
	tls := strings.ToLower(nodeScalarString(valueOrNil(d, "tls"))) == "tls"

	stream := jo("network", net, "security", tlsOrNone(tls))
	if net == "ws" {
		ws := jo("path", stringOr(nodeScalarString(valueOrNil(d, "path")), "/"))
		if host := nodeScalarString(valueOrNil(d, "host")); host != "" {
			ws.put("headers", jo("Host", host))
		}
		stream.put("wsSettings", ws)
	} else if net == "xhttp" {
		xhttp := jo("path", stringOr(nodeScalarString(valueOrNil(d, "path")), "/"))
		if host := nodeScalarString(valueOrNil(d, "host")); host != "" {
			xhttp.put("host", host)
		}
		stream.put("xhttpSettings", xhttp)
	}
	if tls {
		sni := stringOr(nodeScalarString(valueOrNil(d, "sni")), nodeScalarString(valueOrNil(d, "host")))
		tlsSettings := jo()
		if sni != "" {
			tlsSettings.put("serverName", sni)
		}
		stream.put("tlsSettings", tlsSettings)
	}

	settings := jo("vnext", jsArr{jo(
		"address", add,
		"port", jsonInt(valueOrNil(d, "port")),
		"users", jsArr{jo(
			"id", id,
			"alterId", jsonInt(valueOrNil(d, "aid")),
			"security", stringOr(nodeScalarString(valueOrNil(d, "scy")), "auto"),
		)},
	)})

	return jo(
		"protocol", "vmess",
		"settings", settings,
		"stream_settings", stream,
		"tag", stringOr(nodeScalarString(valueOrNil(d, "ps")), add),
	), nil
}

// parseDispatchLink 解析 vless / trojan（PHP 的两个方法结构几乎相同，这里合并）。
func parseDispatchLink(link, protocol string) (*jsObj, error) {
	u, err := parseURLParts(link)
	if err != nil {
		return nil, errors.New(protocol + " 链接解析失败")
	}

	var host, userPart, passPart string
	var port int64
	switch protocol {
	case "vless":
		host = u.Host
		userPart = u.User
		if host == "" || userPart == "" {
			return nil, errors.New("vless 链接解析失败")
		}
	case "trojan":
		host = u.Host
		// 标准格式把密码放 user 段；兼容 user:pass 双段写法（此时取 pass）。
		passPart = u.Pass
		if passPart == "" {
			passPart = u.User
		}
		if host == "" || passPart == "" {
			return nil, errors.New("trojan 链接解析失败")
		}
	}
	port = u.Port

	query := u.Query
	security := stringOr(query["security"], "none")
	if protocol == "trojan" {
		security = stringOr(query["security"], "tls")
	}
	network := stringOr(query["type"], "tcp")

	stream := jo("network", network, "security", security)
	if security == "reality" {
		reality := jo()
		if v := query["sni"]; v != "" {
			reality.put("serverName", v)
		}
		if v := query["pbk"]; v != "" {
			reality.put("publicKey", v)
		}
		if v := query["sid"]; v != "" {
			reality.put("shortId", v)
		}
		if v := query["fp"]; v != "" {
			reality.put("fingerprint", v)
		}
		if v := query["spx"]; v != "" {
			reality.put("spiderX", v)
		}
		stream.put("realitySettings", reality)
	} else if security == "tls" {
		sni := query["sni"]
		if sni == "" {
			sni = host
		}
		tlsSettings := jo()
		if sni != "" {
			tlsSettings.put("serverName", sni)
		}
		stream.put("tlsSettings", tlsSettings)
	}
	applyLinkTransport(stream, network, query)

	fragment := linkFragment(link)
	if fragment == "" {
		fragment = host
	}

	var settings *jsObj
	if protocol == "vless" {
		user := jo("id", u.UserDecoded, "encryption", "none")
		if flow := query["flow"]; flow != "" {
			user.put("flow", flow)
		}
		settings = jo("vnext", jsArr{jo(
			"address", host,
			"port", port,
			"users", jsArr{user},
		)})
	} else {
		settings = jo("servers", jsArr{jo(
			"address", host,
			"port", port,
			"password", u.PassDecoded,
		)})
	}

	return jo(
		"protocol", protocol,
		"settings", settings,
		"stream_settings", stream,
		"tag", fragment,
	), nil
}

func parseSSLink(link string) (*jsObj, error) {
	rest := link[len("ss://"):]
	fragment := linkFragment(link)
	if idx := strings.Index(rest, "#"); idx >= 0 {
		rest = rest[:idx]
	}

	method, password, host := "", "", ""
	var port int64

	if strings.Contains(rest, "@") {
		// SIP002: ss://base64(method:password)@host:port
		idx := strings.Index(rest, "@")
		userinfo, hostport := rest[:idx], rest[idx+1:]
		decoded, ok := base64LoosePercent(userinfo)
		if !ok || !strings.Contains(decoded, ":") {
			return nil, errors.New("ss 链接 userinfo 解析失败")
		}
		colon := strings.Index(decoded, ":")
		method, password = decoded[:colon], decoded[colon+1:]
		clean := hostport
		if q := strings.Index(clean, "?"); q >= 0 {
			clean = clean[:q]
		}
		host, port = splitHostPort(clean)
	} else {
		// 旧格式：ss://base64(method:password@host:port)
		decoded, ok := base64LoosePercent(rest)
		if !ok {
			return nil, errors.New("ss 链接解析失败")
		}
		at := strings.LastIndex(decoded, "@")
		if at <= 0 {
			return nil, errors.New("ss 链接内容不完整")
		}
		cred, hostport := decoded[:at], decoded[at+1:]
		colon := strings.Index(cred, ":")
		if colon <= 0 {
			return nil, errors.New("ss 链接内容不完整")
		}
		method, password = cred[:colon], cred[colon+1:]
		host, port = splitHostPort(hostport)
	}

	if host == "" || port <= 0 || method == "" {
		return nil, errors.New("ss 链接内容不完整")
	}
	tag := fragment
	if tag == "" {
		tag = host
	}
	return jo(
		"protocol", "shadowsocks",
		"settings", jo("servers", jsArr{jo(
			"address", host,
			"port", port,
			"method", method,
			"password", password,
		)}),
		"stream_settings", jo(),
		"tag", tag,
	), nil
}

// parseHysteriaLink 解析 hysteria2:// 与 hy2://（hy2 是官方简写，归一成 hysteria 出站）。
//
// 口径对齐 sub_diag/xray-protocol-adaptation-spec.md 的出站形状：
//
//	settings        = {version: 2, address, port}
//	hysteriaSettings= {version: 2, auth}
//	tlsSettings     = {serverName?, allowInsecure?}
//	finalmask.udp[] = [{type, settings: {password}}]   ← obfs=salamander 的落点
//
// auth 两种写法都认：明文（客户端标准）与 base64（3x-ui 等老面板发的）。
func parseHysteriaLink(link string) (*jsObj, error) {
	u, err := parseURLParts(link)
	if err != nil || u.Host == "" {
		return nil, errors.New("hysteria2 链接解析失败")
	}
	port := u.Port
	if port == 0 {
		port = 443
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("hysteria2 链接解析失败")
	}

	query := u.Query
	stream := jo("network", "hysteria", "security", "tls")

	tls := jo()
	if sni := strings.TrimSpace(query["sni"]); sni != "" {
		tls.put("serverName", sni)
	}
	if nodeTruthy(query["insecure"]) {
		tls.put("allowInsecure", true)
	}
	stream.put("tlsSettings", tls)

	// 数值一律 int64：jsonInt 不认 Go 的 int，回读会变 0（DB 来回一趟才会被规范成 float64）。
	hy := jo("version", int64(2))
	if auth := hysteriaLinkAuth(u.UserDecoded); auth != "" {
		hy.put("auth", auth)
	}
	stream.put("hysteriaSettings", hy)

	if obfs := strings.TrimSpace(query["obfs"]); obfs != "" {
		mask := jo("type", obfs)
		if password := query["obfs-password"]; password != "" {
			mask.put("settings", jo("password", password))
		}
		stream.put("finalmask", jo("udp", jsArr{mask}))
	}

	tag := linkFragment(link)
	if tag == "" {
		tag = u.Host
	}
	return jo(
		"protocol", "hysteria",
		"settings", jo("version", int64(2), "address", u.Host, "port", port),
		"stream_settings", stream,
		"tag", tag,
	), nil
}

func parseSocksLikeLink(link string) (*jsObj, error) {
	u, err := parseURLParts(link)
	if err != nil || u.Host == "" {
		return nil, errors.New("socks 链接解析失败")
	}
	port := u.Port
	if port == 0 {
		port = 1080
	}
	tag := linkFragment(link)
	if tag == "" {
		tag = u.Host
	}
	return jo(
		"protocol", "socks",
		"settings", jo("servers", jsArr{jo(
			"address", u.Host,
			"port", port,
			"users", jsArr{jo("user", u.UserDecoded, "pass", u.PassDecoded)},
		)}),
		"stream_settings", jo(),
		"tag", tag,
	), nil
}

func parseHTTPLink(link string) (*jsObj, error) {
	u, err := parseURLParts(link)
	if err != nil || u.Host == "" {
		return nil, errors.New("http 链接解析失败")
	}
	port := u.Port
	if port == 0 {
		port = 8080
	}
	tag := linkFragment(link)
	if tag == "" {
		tag = u.Host
	}
	return jo(
		"protocol", "http",
		"settings", jo("servers", jsArr{jo(
			"address", u.Host,
			"port", port,
			"users", jsArr{jo("user", u.UserDecoded, "pass", u.PassDecoded)},
		)}),
		"stream_settings", jo(),
		"tag", tag,
	), nil
}

// applyLinkTransport 对齐 applyTransport()：ws / xhttp / grpc / httpupgrade / tcp header。
func applyLinkTransport(stream *jsObj, network string, query map[string]string) {
	switch network {
	case "tcp":
		if query["headerType"] != "http" {
			return
		}
		path := query["path"]
		if path == "" {
			path = "/"
		}
		request := jo("version", "1.1", "method", "GET", "path", jsArr{path})
		if host := strings.TrimSpace(query["host"]); host != "" {
			request.put("headers", jo("Host", jsArr{host}))
		}
		stream.put("tcpSettings", jo("header", jo("type", "http", "request", request)))
	case "ws":
		ws := jo("path", stringOr(query["path"], "/"))
		if query["host"] != "" {
			ws.put("headers", jo("Host", query["host"]))
		}
		stream.put("wsSettings", ws)
	case "xhttp":
		xhttp := jo("path", stringOr(query["path"], "/"))
		for _, key := range []string{"host", "mode"} {
			if query[key] != "" {
				xhttp.put(key, query[key])
			}
		}
		if extra := query["extra"]; extra != "" {
			if parsed, err := parseOrderedJS([]byte(extra)); err == nil {
				if obj, isObj := parsed.(*jsObj); isObj {
					for i, k := range obj.keys {
						xhttp.put(k, obj.vals[i])
					}
				}
			}
		}
		if v := query["x_padding_bytes"]; v != "" {
			if _, exists := xhttp.get("xPaddingBytes"); !exists {
				xhttp.put("xPaddingBytes", v)
			}
		}
		stream.put("xhttpSettings", xhttp)
	case "httpupgrade":
		hu := jo("path", stringOr(query["path"], "/"))
		if query["host"] != "" {
			hu.put("host", query["host"])
		}
		stream.put("httpupgradeSettings", hu)
	case "grpc":
		grpc := jo()
		if v := query["serviceName"]; v != "" {
			grpc.put("serviceName", v)
		}
		if v := query["authority"]; v != "" {
			grpc.put("authority", v)
		}
		stream.put("grpcSettings", grpc)
	}
}

// ---- URL 解析（复刻 PHP parse_url + parse_str + rawurldecode）----

// urlParts 是 parse_url 在本域用到的字段。
type urlParts struct {
	Host        string
	Port        int64
	User        string // rawurldecode 后的 user
	Pass        string // rawurldecode 后的 pass
	UserDecoded string
	PassDecoded string
	Query       map[string]string
}

// parseURLParts 解析 scheme://[user[:pass]@]host[:port][/path][?query]。
func parseURLParts(raw string) (*urlParts, error) {
	idx := strings.Index(raw, "://")
	if idx <= 0 {
		return nil, errors.New("缺少 scheme")
	}
	rest := raw[idx+3:]
	if h := strings.Index(rest, "#"); h >= 0 {
		rest = rest[:h]
	}
	query := map[string]string{}
	if q := strings.Index(rest, "?"); q >= 0 {
		query = parseQueryString(rest[q+1:])
		rest = rest[:q]
	}
	// 去路径
	if slash := strings.Index(rest, "/"); slash >= 0 {
		rest = rest[:slash]
	}

	userinfo := ""
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo = rest[:at]
		rest = rest[at+1:]
	}

	host, port := splitHostPort(rest)

	out := &urlParts{Host: host, Port: port, Query: query}
	if userinfo != "" {
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			out.User = urlDecode(userinfo[:colon])
			out.Pass = urlDecode(userinfo[colon+1:])
		} else {
			out.User = urlDecode(userinfo)
		}
	}
	out.UserDecoded = out.User
	out.PassDecoded = out.Pass
	return out, nil
}

// splitHostPort 拆 host:port，支持 [v6]:port 与 [v6]。
func splitHostPort(s string) (string, int64) {
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return s, 0
		}
		host := s[1:end]
		rest := s[end+1:]
		if strings.HasPrefix(rest, ":") {
			port, _ := strconv.ParseInt(rest[1:], 10, 64)
			return host, port
		}
		return host, 0
	}
	colon := strings.LastIndex(s, ":")
	if colon < 0 {
		return s, 0
	}
	port, err := strconv.ParseInt(s[colon+1:], 10, 64)
	if err != nil {
		return s, 0
	}
	return s[:colon], port
}

// parseQueryString 复刻 PHP 的 parse_str（只处理扁平键值 + 百分号解码 + '+' → 空格）。
func parseQueryString(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		key := urlDecode(kv[0])
		if key == "" {
			continue
		}
		value := ""
		if len(kv) == 2 {
			value = urlDecode(kv[1])
		}
		// parse_str 对重复键取最后一个值（非 [] 形态）。
		out[key] = value
	}
	return out
}

// urlDecode 复刻 PHP 的 urldecode（'+' → 空格 + 百分号解码）。
func urlDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	return percentDecode(s)
}

// percentDecode 做百分号解码；非法序列原样保留（与 PHP rawurldecode 对非法输入的最接近行为）。
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// linkFragment 取 #fragment（百分号解码）；PHP 的 parse_url(PHP_URL_FRAGMENT) + rawurldecode。
func linkFragment(link string) string {
	idx := strings.LastIndex(link, "#")
	if idx < 0 {
		return ""
	}
	return percentDecode(link[idx+1:])
}

// base64Loose 复刻 b64decodeLoose：标准/URL-safe、有无填充都收。
func base64Loose(input string) (string, bool) {
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimSpace(input))
	if pad := len(normalized) % 4; pad > 0 {
		normalized += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// base64LoosePercent 同 base64Loose，但先对 userinfo 做百分号解码（SIP002 的 userinfo 可能被编码）。
func base64LoosePercent(input string) (string, bool) {
	return base64Loose(percentDecode(input))
}

// hysteriaLinkAuth 读 hy2 链接 userinfo 里的 auth：能当 base64 就按 base64，否则明文。
//
// 判据与订阅侧 parseHysteriaAuth 同源：「解码 + 重新编码必须等于原文」才算 base64。
// 明文 uuid 里的 '-' 会被当成 URL-safe 字母表解成乱码，重编码后与原文不同 → 判为明文，
// 两条路互不误伤。
func hysteriaLinkAuth(userinfo string) string {
	if userinfo == "" {
		return ""
	}
	if decoded, ok := canonicalBase64Text(userinfo); ok {
		return decoded
	}
	return userinfo
}

// canonicalBase64Text 只在「解码后重新编码 == 原文（忽略 padding 与 URL-safe 字母表差异）」时返回结果。
func canonicalBase64Text(value string) (string, bool) {
	decoded, ok := base64Loose(value)
	if !ok || decoded == "" || !utf8.ValidString(decoded) {
		return "", false
	}
	normalized := strings.TrimRight(strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimSpace(value)), "=")
	if normalized != strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(decoded)), "=") {
		return "", false
	}
	return decoded, true
}

// ---- 小工具 ----

func jsonIntDefault(obj any, path string, def int64) int64 {
	v := jsonGet(obj, path)
	if v == nil {
		return def
	}
	n := jsonInt(v)
	if n == 0 {
		return def
	}
	return n
}

func stringOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func tlsString(tls bool) string {
	if tls {
		return "tls"
	}
	return ""
}

func tlsOrNone(tls bool) string {
	if tls {
		return "tls"
	}
	return "none"
}

func tlsSni(stream any, tls bool) string {
	if !tls {
		return ""
	}
	return jsonGetString(stream, "tlsSettings", "serverName")
}

// linkHost 对齐 host()：目标主机（IPv6 加方括号）。
func linkHost(address string) string {
	address = strings.TrimSpace(address)
	if strings.Contains(address, ":") && !strings.HasPrefix(address, "[") {
		return "[" + address + "]"
	}
	return address
}
