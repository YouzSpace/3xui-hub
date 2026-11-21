package admin

// 出站 settings 的「协议 → 构造器」注册表（协议扩展点收敛，issue #137）。
//
// 改动前 buildOutboundSettings 里有两个 `switch protocol`：一个分流 freedom/blackhole
// 与「是否需要地址端口」，另一个逐协议拼 settings。新增出站协议要在 switch 里加分支，
// 容易漏（漏了就是 settings={} 的空壳，能存但连不上）。现收敛为：
//   - freedom / blackhole：无地址端口的固定 settings（在 buildOutboundSettings 内直接返回）；
//   - 其余协议：outboundSettingsBuilders 里登记一个构造器。
//
// 行为逐字不变：由 xray_outbound_settings_test.go 的特征测试锁定（重构前后必须一致）。

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// outboundSettingsCtx 是构造器上下文：地址/端口已校验通过，构造器只负责拼 settings。
type outboundSettingsCtx struct {
	d       *NodeDomain
	c       *gin.Context
	address string
	port    int64
	input   map[string]any
}

// requireField 取必填字段（去空白）；空则写失败响应并返回 false。
func (x outboundSettingsCtx) requireField(value, field string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		x.d.fail(x.c, "该字段必填", 500)
		_ = field
		return "", false
	}
	return trimmed, true
}

// outboundSettingsBuilders 是 协议 → settings 构造器。返回 nil 表示校验失败（已写响应）。
var outboundSettingsBuilders = map[string]func(outboundSettingsCtx) []byte{
	"vmess": func(x outboundSettingsCtx) []byte {
		id, ok := x.requireField(nodeStringField(x.input, "user_id"), "user_id")
		if !ok {
			return nil
		}
		security := nodeStringField(x.input, "encryption")
		if security == "" {
			security = "auto"
		}
		return phpEncode(jo("vnext", jsArr{jo(
			"address", x.address,
			"port", x.port,
			"users", jsArr{jo("id", id, "alterId", int64(0), "security", security)},
		)}))
	},
	"vless": func(x outboundSettingsCtx) []byte {
		id, ok := x.requireField(nodeStringField(x.input, "user_id"), "user_id")
		if !ok {
			return nil
		}
		user := jo("id", id, "encryption", "none")
		if flow := nodeStringField(x.input, "flow"); flow != "" {
			user.put("flow", flow)
		}
		return phpEncode(jo("vnext", jsArr{jo(
			"address", x.address,
			"port", x.port,
			"users", jsArr{user},
		)}))
	},
	"trojan": func(x outboundSettingsCtx) []byte {
		password, ok := x.requireField(nodeStringField(x.input, "password"), "password")
		if !ok {
			return nil
		}
		return phpEncode(jo("servers", jsArr{jo("address", x.address, "port", x.port, "password", password)}))
	},
	"shadowsocks": func(x outboundSettingsCtx) []byte {
		method, ok := x.requireField(nodeStringField(x.input, "method"), "method")
		if !ok {
			return nil
		}
		password, ok := x.requireField(nodeStringField(x.input, "password"), "password")
		if !ok {
			return nil
		}
		return phpEncode(jo("servers", jsArr{jo(
			"address", x.address, "port", x.port, "method", method, "password", password,
		)}))
	},
	// socks 与 http 的 settings 形状一致（servers[0].users[0]）。
	"socks": outboundSocksHTTPSettings,
	"http":  outboundSocksHTTPSettings,
	"hysteria": func(x outboundSettingsCtx) []byte {
		// xray-core 的 hysteria 出站 settings 只认 version/address/port（auth 在传输层，见 buildOutboundStream）。
		return phpEncode(jo(
			"version", int64(2),
			"address", x.address,
			"port", x.port,
		))
	},
	"dns": func(x outboundSettingsCtx) []byte {
		// dns 出站：{network: udp|tcp, address, port}；端口缺省在 buildOutboundSettings 里已补 53。
		network := "udp"
		if s := nodeStringField(x.input, "dns_network"); s != "" {
			network = s
		}
		return phpEncode(jo("network", network, "address", x.address, "port", x.port))
	},
}

// outboundStreamForced 是「传输层被协议固定」的协议：hysteria 内核只认 network=hysteria + tls。
// 用表替掉原 buildOutboundStream 里的 `if protocol == "hysteria"` 赋值。
var outboundStreamForced = map[string]struct{ network, security string }{
	"hysteria": {network: "hysteria", security: "tls"},
}

// outboundRealityAllowedProtocols 是允许 reality 安全层的协议（仅 vless）。
var outboundRealityAllowedProtocols = map[string]bool{
	"vless": true,
}

// outboundEmptyStreamProtocols 是不写 network/security 的协议：
//   - freedom / blackhole：无传输层，stream={}；
//   - dns：udp/tcp 写在 settings.network，stream 只保留「借道出口」。
//
// 用集合替掉原 buildOutboundStream 里的 `protocol == "freedom" || "blackhole"` / `== "dns"` 判断。
var outboundEmptyStreamProtocols = map[string]bool{
	"freedom":   true,
	"blackhole": true,
	"dns":       true,
}

// outboundSocksHTTPSettings 是 socks / http 共用的 settings 形状（servers[0].users[0]）。
func outboundSocksHTTPSettings(x outboundSettingsCtx) []byte {
	return phpEncode(jo("servers", jsArr{jo(
		"address", x.address,
		"port", x.port,
		"users", jsArr{jo(
			"user", nodeStringField(x.input, "username"),
			"pass", nodeStringField(x.input, "password"),
		)},
	)}))
}
