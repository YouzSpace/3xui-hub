// Package hysteria 是 Hysteria2 协议专属的「出站 settings 构造」。
//
// xray-core 的 hysteria 出站 settings 只认 version/address/port；auth 在传输层
// （见 handler/admin 的 buildOutboundStream），version 必须写 2。
package hysteria

import (
	"controlhub-server/internal/phpjson"
)

// BuildOutboundSettings 构造 Hysteria2 出站 settings（version=2 + address + port）。
func BuildOutboundSettings(address string, port int64) *phpjson.Object {
	return phpjson.New("version", int64(2), "address", address, "port", port)
}
