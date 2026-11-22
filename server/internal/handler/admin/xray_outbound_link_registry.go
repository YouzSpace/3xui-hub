package admin

import "errors"

// 出站分享链接的「协议 → 生成器 / 解析器」注册表（协议扩展点收敛）。
//
// 背景（issue #137 本意「协议扩展点收敛」）：改动前 xrayGenerateOutboundLink /
// xrayParseOutboundLink 各有一个 `switch protocol/scheme`，新增一个出站协议要在
// 生成、解析两处各加一个 case —— 漏一处就是「能存不能导出 / 能导入不能存」的错位。
//
// 收敛后：新增出站协议只需要
//  1. 在 outboundLinkSupportedProto（本包 xray_outbound.go，生成白名单唯一出处）里加协议名；
//  2. 在 outboundLinkGenerators 登记生成器；
//  3. 在 outboundLinkParsers 登记解析器（scheme 可多个别名）。
// 生成 / 解析两个入口不再有 switch。
//
// 刻意不做：不引入 interface / Adapter 层（这些链接函数的差异只是「一组字段拼装」，
// 套接口只会多一跳）。也不抽到独立 package —— 这里用的有序 JSON（jsObj/jo/phpEncode）
// 是 handler/admin 包内实现，抽包会牵动跨层 JSON 表示统一，超出「协议收敛」范围。

// errNoOutboundTarget 是「出站没有可导出的目标地址」的错误（与改动前文案一致）。
var errNoOutboundTarget = errors.New("该出站没有可导出的目标地址")

// outboundJSON 是生成器的上下文：出站 settings / streamSettings 解析后的有序结构 + 展示名 / 协议。
type outboundJSON struct {
	settings any
	stream   any
	name     string
	protocol string
}

// outboundFirstTarget 取「第一跳目标」对象：优先 settings.vnext[0]，回退 settings.servers[0]。
// 找不到时返回 (nil,false)。hysteria 的目标在 settings 顶层，不走这里。
func outboundFirstTarget(settings any) (*jsObj, bool) {
	first := jsonGet(settings, "vnext", "0")
	if _, isObj := first.(*jsObj); !isObj {
		first = jsonGet(settings, "servers", "0")
	}
	obj, isObj := first.(*jsObj)
	return obj, isObj
}

// outboundLinkGenerators 是 协议 → 生成器。
var outboundLinkGenerators = map[string]func(outboundJSON) (string, error){
	"vless": func(c outboundJSON) (string, error) {
		first, ok := outboundFirstTarget(c.settings)
		if !ok || jsonGetString(first, "address") == "" {
			return "", errNoOutboundTarget
		}
		return outboundVLESSURI(first, c.stream, c.name), nil
	},
	"vmess": func(c outboundJSON) (string, error) {
		first, ok := outboundFirstTarget(c.settings)
		if !ok || jsonGetString(first, "address") == "" {
			return "", errNoOutboundTarget
		}
		return outboundVMessURI(first, c.stream, c.name), nil
	},
	"trojan": func(c outboundJSON) (string, error) {
		first, ok := outboundFirstTarget(c.settings)
		if !ok || jsonGetString(first, "address") == "" {
			return "", errNoOutboundTarget
		}
		return outboundTrojanURI(first, c.stream, c.name), nil
	},
	"shadowsocks": func(c outboundJSON) (string, error) {
		first, ok := outboundFirstTarget(c.settings)
		if !ok || jsonGetString(first, "address") == "" {
			return "", errNoOutboundTarget
		}
		return outboundSSURI(first, c.name), nil
	},
	"socks": func(c outboundJSON) (string, error) {
		first, ok := outboundFirstTarget(c.settings)
		if !ok || jsonGetString(first, "address") == "" {
			return "", errNoOutboundTarget
		}
		return outboundSocksLikeURI(first, c.name, false), nil
	},
	"http": func(c outboundJSON) (string, error) {
		first, ok := outboundFirstTarget(c.settings)
		if !ok || jsonGetString(first, "address") == "" {
			return "", errNoOutboundTarget
		}
		return outboundSocksLikeURI(first, c.name, true), nil
	},
	// hysteria 出站的目标在 settings 顶层（{version,address,port}），不走 firstTarget 校验。
	"hysteria": func(c outboundJSON) (string, error) {
		return outboundHysteriaURI(c.settings, c.stream, c.name)
	},
}

// outboundLinkParsers 是 URL scheme → 解析器（含别名）。
var outboundLinkParsers = map[string]func(string) (*jsObj, error){
	"vmess":     parseVMessLink,
	"vless":     func(link string) (*jsObj, error) { return parseDispatchLink(link, "vless") },
	"trojan":    func(link string) (*jsObj, error) { return parseDispatchLink(link, "trojan") },
	"ss":        parseSSLink,
	"hysteria2": parseHysteriaLink,
	"hy2":       parseHysteriaLink,
	"socks":     parseSocksLikeLink,
	"socks5":    parseSocksLikeLink,
	"http":      parseHTTPLink,
	"https":     parseHTTPLink,
}
