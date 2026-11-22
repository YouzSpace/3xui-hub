// Package grpc 是 grpc 传输的 streamSettings 段构造。
package grpc

import (
	p "controlhub-server/internal/protocol"
	tu "controlhub-server/internal/protocol/transport/util"
)

// Build 返回 grpcSettings 段 JSON（serviceName / authority 有效才写）。
func Build(input map[string]any) []byte {
	g := p.New()
	if v := tu.Str(input, "grpc_service_name"); v != "" {
		g.Put("serviceName", v)
	}
	if v := tu.Str(input, "grpc_authority"); v != "" {
		g.Put("authority", v)
	}
	if len(g.Keys()) == 0 {
		return nil
	}
	return p.Encode(p.New("grpcSettings", g))
}
