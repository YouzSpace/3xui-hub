package admin

// 出站分享链接：生成与解析。
//
// 逐条对齐 backend/app/Drivers/Xray/OutboundLinkGenerator.php 与
// OutboundLinkParser.php —— 两者互为逆运算（生成的链接贴回解析器能还原等价结构）。
//
// 这里没有复用 internal/driver/xray 的订阅链接实现：那一份是「xray 入站 → 客户端链接」
// （字段来自入站表单），而出站链接的字段来源是 xray_outbounds.settings/stream_settings，
// 且 vless 用 vnext[0].users[0]、vmess 用 alterId/security、socks/http 用 servers[0].users[0]，
// 口径与入站侧不同。硬套会让两边都难维护，故独立实现并逐行标注对应 PHP 方法。

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// ---- 生成 ----

// xrayGenerateOutboundLink 对齐 OutboundLinkGenerator::generate()。
//
// 协议 → 生成器的分发走 outboundLinkGenerators 注册表（见 xray_outbound_link_registry.go）：
// 新增出站协议只在该注册表登记一行，不用再改这里的 switch。
func xrayGenerateOutboundLink(ob *XrayOutboundRecord) (string, error) {
	protocol := ob.Protocol
	if !phpInArray(protocol, outboundLinkSupportedProto) {
		return "", errors.New("该出站类型（" + protocol + "）没有标准分享链接")
	}

	settings, _ := parseOrderedJS(ob.Settings)
	stream, _ := parseOrderedJS(ob.StreamSettings)
	name := outboundDisplayName(ob)

	if gen, ok := outboundLinkGenerators[protocol]; ok {
		return gen(outboundJSON{settings: settings, stream: stream, name: name, protocol: protocol})
	}
	return "", errors.New("不支持的协议：" + protocol)
}

// outboundDisplayName 对齐 displayName()：备注（有则用）否则用标识。
func outboundDisplayName(ob *XrayOutboundRecord) string {
	if ob.Remark != nil {
		if remark := strings.TrimSpace(*ob.Remark); remark != "" {
			return remark
		}
	}
	return ob.Tag
}

func outboundVLESSURI(first *jsObj, stream any, name string) string {
	address := jsonGetString(first, "address")
	port := jsonIntDefault(first, "port", 443)
	user, _ := jsonGet(first, "users", "0").(*jsObj)
	uuid := phpRawURLEncode(jsonGetString(user, "id"))

	network := jsonDefaultString(stream, "tcp", "network")
	security := jsonDefaultString(stream, "none", "security")

	query := "type=" + phpRawURLEncode(network) + "&security=" + phpRawURLEncode(security)

	switch security {
	case "reality":
		reality := jsonGet(stream, "realitySettings")
		fp := jsonGetString(reality, "fingerprint")
		if fp == "" {
			fp = "chrome"
		}
		query += "&fp=" + phpRawURLEncode(fp)
		query += "&sni=" + phpRawURLEncode(jsonGetString(reality, "serverName"))
		query += "&pbk=" + phpRawURLEncode(jsonGetString(reality, "publicKey"))
		if sid := jsonGetString(reality, "shortId"); sid != "" {
			query += "&sid=" + phpRawURLEncode(sid)
		}
		if spx := jsonGetString(reality, "spiderX"); spx != "" {
			query += "&spx=" + phpRawURLEncode(spx)
		}
	case "tls":
		sni := jsonGetString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = address
		}
		query += "&fp=chrome&sni=" + phpRawURLEncode(sni)
	default:
		query += "&fp=chrome"
	}

	query += outboundTransportQuery(network, stream)

	if flow := jsonGetString(user, "flow"); flow != "" {
		query += "&flow=" + phpRawURLEncode(flow)
	}

	return "vless://" + uuid + "@" + linkHost(address) + ":" + strconv.FormatInt(port, 10) +
		"?" + query + "#" + phpRawURLEncode(name)
}

func outboundVMessURI(first *jsObj, stream any, name string) string {
	user, _ := jsonGet(first, "users", "0").(*jsObj)
	network := jsonDefaultString(stream, "tcp", "network")
	security := jsonDefaultString(stream, "none", "security")
	tls := security == "tls"

	wsHost := jsonGetString(stream, "wsSettings", "headers", "Host")
	xhttpHost := jsonGetString(stream, "xhttpSettings", "host")
	wsPath := jsonGetString(stream, "wsSettings", "path")
	xhttpPath := jsonGetString(stream, "xhttpSettings", "path")

	doc := jo(
		"v", "2",
		"ps", name,
		"add", jsonGetString(first, "address"),
		"port", strconv.FormatInt(jsonIntDefault(first, "port", 443), 10),
		"id", jsonGetString(user, "id"),
		"aid", strconv.FormatInt(jsonIntDefault(user, "alterId", 0), 10),
		"scy", stringOr(jsonGetString(user, "security"), "auto"),
		"net", network,
		"type", "none",
		"host", stringOr(wsHost, xhttpHost),
		"path", stringOr(wsPath, xhttpPath),
		"tls", tlsString(tls),
		"sni", tlsSni(stream, tls),
	)
	body := phpEncode(doc)
	return "vmess://" + base64.StdEncoding.EncodeToString(body)
}

func outboundTrojanURI(first *jsObj, stream any, name string) string {
	address := jsonGetString(first, "address")
	port := jsonIntDefault(first, "port", 443)
	password := phpRawURLEncode(jsonGetString(first, "password"))

	network := jsonDefaultString(stream, "tcp", "network")
	security := jsonDefaultString(stream, "none", "security")

	sec := "none"
	if security == "tls" {
		sec = "tls"
	}
	query := "security=" + sec + "&type=" + phpRawURLEncode(network)
	if security == "tls" {
		sni := jsonGetString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = address
		}
		query += "&sni=" + phpRawURLEncode(sni)
	}
	query += outboundTransportQuery(network, stream)

	return "trojan://" + password + "@" + linkHost(address) + ":" + strconv.FormatInt(port, 10) +
		"?" + query + "#" + phpRawURLEncode(name)
}

func outboundSSURI(first *jsObj, name string) string {
	address := jsonGetString(first, "address")
	port := jsonIntDefault(first, "port", 443)
	method := jsonGetString(first, "method")
	password := jsonGetString(first, "password")

	// SIP002：ss://base64(method:password)@host:port#name
	userinfo := base64.StdEncoding.EncodeToString([]byte(method + ":" + password))
	return "ss://" + userinfo + "@" + linkHost(address) + ":" + strconv.FormatInt(port, 10) +
		"#" + phpRawURLEncode(name)
}

// outboundHysteriaURI 生成 hysteria2 出站链接：
//
//	hysteria2://<auth>@host:port?sni=..&insecure=1&obfs=salamander&obfs-password=..#name
//
// 字段落点（与面板出站侧一致）：
//   - auth 在 streamSettings.hysteriaSettings.auth（前端编辑回显也按这个读）；
//   - sni / insecure 在 streamSettings.tlsSettings（serverName / allowInsecure）；
//   - obfs 在 streamSettings.finalmask.udp[] 的 salamander 段。
//
// 参数一律「有才写」：没配就整条键不出现（客户端按各协议默认值走）。
func outboundHysteriaURI(settings, stream any, name string) (string, error) {
	address := jsonGetString(settings, "address")
	port := jsonIntDefault(settings, "port", 0)
	if address == "" {
		// 兼容把目标写成 servers[0] 的配置（导入其它面板的出站 JSON 时常见）。
		server := jsonGet(settings, "servers", "0")
		address = jsonGetString(server, "address")
		port = jsonIntDefault(server, "port", 0)
	}
	if address == "" {
		return "", errors.New("该出站没有可导出的目标地址")
	}
	if port == 0 {
		port = 443
	}

	auth := jsonGetString(stream, "hysteriaSettings", "auth")
	if auth == "" {
		auth = jsonGetString(settings, "auth")
		if auth == "" {
			auth = jsonGetString(settings, "servers", "0", "password")
		}
	}

	query := ""
	if sni := jsonGetString(stream, "tlsSettings", "serverName"); sni != "" {
		query = appendLinkParam(query, "sni", sni)
	}
	if jsonBoolAt(stream, "tlsSettings", "allowInsecure") {
		query = appendLinkParam(query, "insecure", "1")
	}
	if obfs, obfsPassword := hysteriaObfs(stream); obfs != "" {
		query = appendLinkParam(query, "obfs", obfs)
		if obfsPassword != "" {
			query = appendLinkParam(query, "obfs-password", obfsPassword)
		}
	}

	userinfo := ""
	if auth != "" {
		userinfo = phpRawURLEncode(auth) + "@"
	}
	link := "hysteria2://" + userinfo + linkHost(address) + ":" + strconv.FormatInt(port, 10)
	if query != "" {
		link += "?" + query
	}
	return link + "#" + phpRawURLEncode(name), nil
}

// appendLinkParam 往查询串追加一个已编码的键值（首个键不带 &）。
func appendLinkParam(query, key, value string) string {
	if query != "" {
		query += "&"
	}
	return query + key + "=" + phpRawURLEncode(value)
}

// hysteriaObfs 读 finalmask.udp[] 里的混淆段（面板落点，与 3x-ui / 订阅侧同一处）：
// 返回混淆类型与密码；没有配密码的段整段忽略（链接里表达不了）。
func hysteriaObfs(stream any) (string, string) {
	masks, _ := jsonGet(stream, "finalmask", "udp").(jsArr)
	for _, raw := range masks {
		kind := jsonGetString(raw, "type")
		password := jsonGetString(raw, "settings", "password")
		if kind != "" && password != "" {
			return kind, password
		}
	}
	return "", ""
}

func outboundSocksLikeURI(first *jsObj, name string, isHTTP bool) string {
	address := jsonGetString(first, "address")
	port := jsonIntDefault(first, "port", 443)
	if isHTTP {
		port = jsonIntDefault(first, "port", 8080)
	} else {
		port = jsonIntDefault(first, "port", 1080)
	}
	user, _ := jsonGet(first, "users", "0").(*jsObj)
	username := jsonGetString(user, "user")
	password := jsonGetString(user, "pass")

	cred := ""
	if username != "" || password != "" {
		cred = phpRawURLEncode(username) + ":" + phpRawURLEncode(password) + "@"
	}
	scheme := "socks"
	if isHTTP {
		scheme = "http"
	}
	return scheme + "://" + cred + linkHost(address) + ":" + strconv.FormatInt(port, 10) +
		"#" + phpRawURLEncode(name)
}

// outboundTransportQuery 对齐 transportQuery()（ws / xhttp / grpc / httpupgrade / tcp header）。
func outboundTransportQuery(network string, stream any) string {
	switch network {
	case "tcp":
		if jsonGetString(stream, "tcpSettings", "header", "type") != "http" {
			return ""
		}
		reqPath := jsonGetString(stream, "tcpSettings", "header", "request", "path", "0")
		if reqPath == "" {
			// PHP：`$request['path'][0] ?? '/'` —— 缺失才兜底；存在但为空串则原样输出。
			reqPath = "/"
		}
		query := "&headerType=http&path=" + phpRawURLEncode(reqPath)
		host := jsonGetString(stream, "tcpSettings", "header", "response", "headers", "Host", "0")
		if host == "" {
			host = jsonGetString(stream, "tcpSettings", "header", "request", "headers", "Host", "0")
		}
		if host != "" {
			query += "&host=" + phpRawURLEncode(host)
		}
		return query
	case "ws":
		ws := jsonGet(stream, "wsSettings")
		query := "&path=" + phpRawURLEncode(stringOr(jsonGetString(ws, "path"), "/"))
		if host := jsonGetString(ws, "headers", "Host"); host != "" {
			query += "&host=" + phpRawURLEncode(host)
		}
		return query
	case "xhttp":
		return outboundXHTTPQuery(jsonGet(stream, "xhttpSettings"))
	case "httpupgrade":
		hu := jsonGet(stream, "httpupgradeSettings")
		query := "&path=" + phpRawURLEncode(stringOr(jsonGetString(hu, "path"), "/"))
		if host := jsonGetString(hu, "host"); host != "" {
			query += "&host=" + phpRawURLEncode(host)
		}
		return query
	case "grpc":
		grpc := jsonGet(stream, "grpcSettings")
		query := ""
		if v := jsonGetString(grpc, "serviceName"); v != "" {
			query += "&serviceName=" + phpRawURLEncode(v)
		}
		if v := jsonGetString(grpc, "authority"); v != "" {
			query += "&authority=" + phpRawURLEncode(v)
		}
		return query
	}
	return ""
}

// outboundXHTTPQuery 对齐 xhttpQuery()：extra（xray-core blob）+ x_padding_bytes（sing-box flat）。
func outboundXHTTPQuery(xhttp any) string {
	query := "&path=" + phpRawURLEncode(stringOr(jsonGetString(xhttp, "path"), "/"))
	if host := jsonGetString(xhttp, "host"); host != "" {
		query += "&host=" + phpRawURLEncode(host)
	}
	if mode := jsonGetString(xhttp, "mode"); mode != "" {
		query += "&mode=" + phpRawURLEncode(mode)
	}

	ext := jo()
	padding := strings.TrimSpace(jsonGetString(xhttp, "xPaddingBytes"))
	if padding != "" {
		ext.put("xPaddingBytes", padding)
	}
	if jsonBoolAt(xhttp, "xPaddingObfsMode") {
		ext.put("xPaddingObfsMode", true)
		for _, field := range []string{"xPaddingKey", "xPaddingHeader", "xPaddingPlacement", "xPaddingMethod"} {
			if v := strings.TrimSpace(jsonGetString(xhttp, field)); v != "" {
				ext.put(field, v)
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
		v := strings.TrimSpace(jsonGetString(xhttp, field))
		if v == "" || v == coreDefaults[field] {
			continue
		}
		ext.put(field, v)
	}
	if v := jsonIntAt(xhttp, "uplinkChunkSize"); v != 0 {
		ext.put("uplinkChunkSize", v)
	}
	if jsonBoolAt(xhttp, "noGRPCHeader") {
		ext.put("noGRPCHeader", true)
	}
	if len(ext.keys) > 0 {
		query += "&extra=" + phpRawURLEncode(string(phpEncode(ext)))
	}
	if padding != "" {
		query += "&x_padding_bytes=" + phpRawURLEncode(padding)
	}
	return query
}
