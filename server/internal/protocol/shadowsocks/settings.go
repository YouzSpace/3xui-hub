// Package shadowsocks 是 Shadowsocks 协议专属的「出站 settings 构造」。
//
// SS-2022 的 PSK 派生/校验仍走唯一真相源 internal/ss2022，本包不重复。
package shadowsocks

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/fields"
)

// BuildOutboundSettings 构造 Shadowsocks 出站 settings（servers[0]: method/password）。
func BuildOutboundSettings(f fields.Fields, address string, port int64, require func(string) (string, bool)) (*phpjson.Object, bool) {
	method, ok := require(f.Str("method"))
	if !ok {
		return nil, false
	}
	password, ok := require(f.Str("password"))
	if !ok {
		return nil, false
	}
	return phpjson.New("servers", phpjson.Array{phpjson.New(
		"address", address, "port", port, "method", method, "password", password,
	)}), true
}
