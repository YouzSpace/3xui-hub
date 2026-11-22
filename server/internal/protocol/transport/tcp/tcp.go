// Package tcp 是 tcp 传输的 streamSettings 段（含 HTTP header 伪装）。
package tcp

import (
	p "controlhub-server/internal/protocol"
	tu "controlhub-server/internal/protocol/transport/util"
)

// Build 返回 tcpSettings 段 JSON；非 http 伪装返回 nil（无 tcpSettings）。
func Build(input map[string]any) []byte {
	if tu.Str(input, "tcp_header_type") != "http" {
		return nil
	}
	req := p.New("version", "1.1", "method", "GET", "path", p.Array{tu.OrSlash(tu.Str(input, "tcp_header_path"))})
	if h := tu.Str(input, "tcp_header_host"); h != "" {
		req.Put("headers", p.New("Host", p.Array{h}))
	}
	return p.Encode(p.New("tcpSettings", p.New("header", p.New("type", "http", "request", req))))
}
