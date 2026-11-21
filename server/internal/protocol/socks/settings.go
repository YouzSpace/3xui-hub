// Package socks 是 SOCKS 协议专属的「出站 settings 构造」。
package socks

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/fields"
)

// BuildOutboundSettings 构造 SOCKS 出站 settings（servers[0].users[0]: user/pass，无需必填）。
func BuildOutboundSettings(f fields.Fields, address string, port int64) *phpjson.Object {
	return phpjson.New("servers", phpjson.Array{phpjson.New(
		"address", address,
		"port",    port,
		"users",   phpjson.Array{phpjson.New("user", f.Str("username"), "pass", f.Str("password"))},
	)})
}
