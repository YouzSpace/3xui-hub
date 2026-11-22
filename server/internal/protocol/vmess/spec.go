package vmess

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "vmess", Transports: []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, Securities: []string{"none", "tls"}, Stream: true, Client: p.ClientUUID, RelayKey: "id", BuildOutbound: buildOutbound, BuildInbound: buildInbound})
}

func buildOutbound(f p.Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool) {
	id, ok := require(f.Str("user_id")); if !ok { return nil, false }
	sec := f.Str("encryption"); if sec == "" { sec = "auto" }
	return p.Encode(p.New("vnext", p.Array{p.New("address", address, "port", port, "users", p.Array{p.New("id", id, "alterId", int64(0), "security", sec)})})), true
}
