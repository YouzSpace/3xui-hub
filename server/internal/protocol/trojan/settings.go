// Package trojan 是 Trojan 协议专属的「出站 settings 构造」。
package trojan

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/fields"
)

// BuildOutboundSettings 构造 Trojan 出站 settings（servers[0].password）。
func BuildOutboundSettings(f fields.Fields, address string, port int64, require func(string) (string, bool)) (*phpjson.Object, bool) {
	password, ok := require(f.Str("password"))
	if !ok {
		return nil, false
	}
	return phpjson.New("servers", phpjson.Array{phpjson.New("address", address, "port", port, "password", password)}), true
}
