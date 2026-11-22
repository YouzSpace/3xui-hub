package admin

// 出站 settings 的「协议 → 构造器」注册表（协议扩展点收敛，issue #137）。
//
// 每协议 settings 的实际构造已迁入 internal/protocol/<proto>/（单一真相源），
// 本文件只做「面板 input map → protocol.Fields 适配 → protocol.BuildOutboundSettings」。
// 空导入各协议子包以触发 init() 注册。

import (
	"strings"

	"github.com/gin-gonic/gin"

	"controlhub-server/internal/protocol"
	_ "controlhub-server/internal/protocol/blackhole"
	_ "controlhub-server/internal/protocol/dns"
	_ "controlhub-server/internal/protocol/freedom"
	_ "controlhub-server/internal/protocol/http"
	_ "controlhub-server/internal/protocol/hysteria"
	_ "controlhub-server/internal/protocol/shadowsocks"
	_ "controlhub-server/internal/protocol/socks"
	_ "controlhub-server/internal/protocol/trojan"
	_ "controlhub-server/internal/protocol/vless"
	_ "controlhub-server/internal/protocol/vmess"
)

type outboundSettingsCtx struct {
	d       *NodeDomain
	c       *gin.Context
	address string
	port    int64
	input   map[string]any
}

func (x outboundSettingsCtx) requireField(value, field string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		x.d.fail(x.c, "该字段必填", 500)
		_ = field
		return "", false
	}
	return trimmed, true
}

// adminFields 把出站表单 map 适配成 protocol.Fields。
type adminFields struct{ m map[string]any }

func (f adminFields) Str(key string) string { return nodeStringField(f.m, key) }
func (f adminFields) Int(key string) int64  { return inputInt(f.m[key]) }

func buildByProtocol(x outboundSettingsCtx, name string) ([]byte, bool) {
	b, _, handled := protocol.BuildOutboundSettings(name, adminFields{x.input}, x.address, x.port,
		func(v string) (string, bool) { return x.requireField(v, "") })
	return b, handled
}

// outboundSettingsBuilders 是 协议 → settings 构造器（委托 protocol 包）。
var outboundSettingsBuilders = map[string]func(outboundSettingsCtx) []byte{
	"vmess":       func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "vmess"); return b },
	"vless":       func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "vless"); return b },
	"trojan":      func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "trojan"); return b },
	"shadowsocks": func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "shadowsocks"); return b },
	"socks":       func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "socks"); return b },
	"http":        func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "http"); return b },
	"hysteria":    func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "hysteria"); return b },
	"dns":         func(x outboundSettingsCtx) []byte { b, _ := buildByProtocol(x, "dns"); return b },
}

// outboundStreamForced 是「传输层被协议固定」的协议。
var outboundStreamForced = map[string]struct{ network, security string }{
	"hysteria": {network: "hysteria", security: "tls"},
}

// outboundRealityAllowedProtocols 是允许 reality 的协议（仅 vless）。
var outboundRealityAllowedProtocols = map[string]bool{"vless": true}

// outboundEmptyStreamProtocols 是不写 network/security 的协议（freedom/blackhole/dns）。
var outboundEmptyStreamProtocols = map[string]bool{"freedom": true, "blackhole": true, "dns": true}
