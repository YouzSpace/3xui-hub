// Package ws 是 ws 传输的 streamSettings 段构造。
package ws

import (
	p "controlhub-server/internal/protocol"
	tu "controlhub-server/internal/protocol/transport/util"
)

// Build 返回入站 wsSettings 段 JSON（path 恒在，host 空则无）。
// xray-core 入站 ws 的 Host 用平铺字段 host。
func Build(input map[string]any) []byte {
	ws := p.New("path", tu.OrSlash(tu.Str(input, "ws_path")))
	if h := tu.Str(input, "ws_host"); h != "" {
		ws.Put("host", h)
	}
	return p.Encode(p.New("wsSettings", ws))
}

// BuildOutbound 返回出站 wsSettings 段 JSON。xray-core 出站 ws 的 Host 走
// headers.Host（与入站平铺 host 不同），这里按出站口径输出。
func BuildOutbound(input map[string]any) []byte {
	ws := p.New("path", tu.OrSlash(tu.Str(input, "ws_path")))
	if h := tu.Str(input, "ws_host"); h != "" {
		ws.Put("headers", p.New("Host", h))
	}
	return p.Encode(p.New("wsSettings", ws))
}
