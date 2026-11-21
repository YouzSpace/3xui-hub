// Package protocol 是协议域的聚合入口：按协议名分发到各自的文件夹（protocol/<proto>）。
//
// 「改一个协议只动一个文件夹」：每个协议的协议特有逻辑在 protocol/<proto>/ 内；
// 公共能力在 protocol/{crypto,transport,security,fields}。本文件只做「协议名 → 构造器」分发。
package protocol

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/blackhole"
	"controlhub-server/internal/protocol/dns"
	"controlhub-server/internal/protocol/fields"
	"controlhub-server/internal/protocol/freedom"
	"controlhub-server/internal/protocol/http"
	"controlhub-server/internal/protocol/hysteria"
	"controlhub-server/internal/protocol/shadowsocks"
	"controlhub-server/internal/protocol/socks"
	"controlhub-server/internal/protocol/trojan"
	"controlhub-server/internal/protocol/vless"
	"controlhub-server/internal/protocol/vmess"
)

// Require 是「必填字段」校验回调：返回去空白后的值；空则返回 ("",false)（调用方已写错误响应）。
type Require func(value string) (string, bool)

// BuildOutboundSettings 按协议名构造出站 settings。
//
// 返回 (obj, ok, handled)：
//   - handled=false 表示该协议名不在分发表内（调用方按其原有兜底处理）；
//   - ok=false 表示必填校验失败（调用方直接返回 nil）。
func BuildOutboundSettings(proto string, f fields.Fields, address string, port int64, require Require) (obj *phpjson.Object, ok bool, handled bool) {
	switch proto {
	case "freedom":
		return freedom.BuildOutboundSettings(), true, true
	case "blackhole":
		return blackhole.BuildOutboundSettings(), true, true
	case "vmess":
		o, k := vmess.BuildOutboundSettings(f, address, port, require)
		return o, k, true
	case "vless":
		o, k := vless.BuildOutboundSettings(f, address, port, require)
		return o, k, true
	case "trojan":
		o, k := trojan.BuildOutboundSettings(f, address, port, require)
		return o, k, true
	case "shadowsocks":
		o, k := shadowsocks.BuildOutboundSettings(f, address, port, require)
		return o, k, true
	case "socks":
		return socks.BuildOutboundSettings(f, address, port), true, true
	case "http":
		return http.BuildOutboundSettings(f, address, port), true, true
	case "hysteria":
		return hysteria.BuildOutboundSettings(address, port), true, true
	case "dns":
		return dns.BuildOutboundSettings(f, address, port), true, true
	}
	return nil, false, false
}
