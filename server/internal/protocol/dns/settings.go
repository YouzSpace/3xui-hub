// Package dns 是 DNS 协议专属的「出站 settings 构造」。
package dns

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/fields"
)

// BuildOutboundSettings 构造 DNS 出站 settings（{network: udp|tcp, address, port}）。
func BuildOutboundSettings(f fields.Fields, address string, port int64) *phpjson.Object {
	network := "udp"
	if s := f.Str("dns_network"); s != "" {
		network = s
	}
	return phpjson.New("network", network, "address", address, "port", port)
}
