package vless

import (
	p "controlhub-server/internal/protocol"
)

func init() {
	p.Register(p.Spec{
		Name: "vless", Transports: []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, Securities: []string{"none", "tls", "reality"},
		RealityTransports: []string{"tcp", "xhttp", "grpc"}, Stream: true,
		Defaults: []p.FieldDefault{{Key: "decryption", Value: "none"}},
		Client: p.ClientUUID, RelayKey: "id", Flow: true,
		BuildOutbound: buildOutbound, BuildInbound: buildInbound,
	})
}

func buildOutbound(f p.Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool) {
	id, ok := require(f.Str("user_id")); if !ok { return nil, false }
	u := p.New("id", id, "encryption", "none")
	if flow := f.Str("flow"); flow != "" { u.Put("flow", flow) }
	return p.Encode(p.New("vnext", p.Array{p.New("address", address, "port", port, "users", p.Array{u})})), true
}
