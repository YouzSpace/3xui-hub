// Package http 是 HTTP 协议专属的「出站 settings 构造」（形状与 socks 相同）。
package http

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/fields"
)

// BuildOutboundSettings 构造 HTTP 出站 settings（servers[0].users[0]: user/pass）。
func BuildOutboundSettings(f fields.Fields, address string, port int64) *phpjson.Object {
	return phpjson.New("servers", phpjson.Array{phpjson.New(
		"address", address,
		"port",    port,
		"users",   phpjson.Array{phpjson.New("user", f.Str("username"), "pass", f.Str("password"))},
	)})
}
