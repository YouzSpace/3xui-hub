// Package transport 聚合各传输层（transport/<net>）：network → streamSettings 段 JSON。
package transport

import (
	p "controlhub-server/internal/protocol"
	"controlhub-server/internal/protocol/transport/grpc"
	"controlhub-server/internal/protocol/transport/httpupgrade"
	"controlhub-server/internal/protocol/transport/tcp"
	"controlhub-server/internal/protocol/transport/ws"
	"controlhub-server/internal/protocol/transport/xhttp"
)

// Build 按 network 返回传输段 JSON（无 → nil）。
func Build(network string, input map[string]any) []byte {
	switch network {
	case "ws":
		return ws.Build(input)
	case "xhttp":
		return xhttp.Build(input)
	case "httpupgrade":
		return httpupgrade.Build(input)
	case "tcp":
		return tcp.Build(input)
	case "grpc":
		return grpc.Build(input)
	}
	return nil
}

// BuildOutbound 按 network 返回出站传输段 JSON。与 Build（入站口径）的差异仅在
// ws：出站 Host 走 headers.Host，其余传输入/出站同形。
func BuildOutbound(network string, input map[string]any) []byte {
	switch network {
	case "ws":
		return ws.BuildOutbound(input)
	case "xhttp":
		return xhttp.Build(input)
	case "httpupgrade":
		return httpupgrade.Build(input)
	case "tcp":
		return tcp.Build(input)
	case "grpc":
		return grpc.Build(input)
	}
	return nil
}

// EncodeXHTTP 返回 xhttpSettings 内层对象的 JSON 字节（不含外层 xhttpSettings 键）。
func EncodeXHTTP(input map[string]any) []byte { return p.Encode(xhttp.Settings(input)) }
