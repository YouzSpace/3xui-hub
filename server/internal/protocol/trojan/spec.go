package trojan

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "trojan", Transports: []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, Securities: []string{"none", "tls"}, Stream: true, Client: p.ClientPassword, RelayKey: "password", BuildOutbound: buildOutbound, BuildInbound: buildInbound})
}

func buildOutbound(f p.Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool) {
	pw, ok := require(f.Str("password")); if !ok { return nil, false }
	return p.Encode(p.New("servers", p.Array{p.New("address", address, "port", port, "password", pw)})), true
}
