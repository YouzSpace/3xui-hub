// Package vmess 是 VMess 协议专属的「出站 settings 构造」。
package vmess

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/fields"
)

// BuildOutboundSettings 构造 VMess 出站 settings（vnext[0].users[0]，含 alterId/security）。
func BuildOutboundSettings(f fields.Fields, address string, port int64, require func(string) (string, bool)) (*phpjson.Object, bool) {
	id, ok := require(f.Str("user_id"))
	if !ok {
		return nil, false
	}
	security := f.Str("encryption")
	if security == "" {
		security = "auto"
	}
	return phpjson.New("vnext", phpjson.Array{phpjson.New(
		"address", address,
		"port",    port,
		"users",   phpjson.Array{phpjson.New("id", id, "alterId", int64(0), "security", security)},
	)}), true
}
