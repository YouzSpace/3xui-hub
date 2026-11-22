package socks

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "socks", Transports: []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, Securities: []string{"none", "tls"}, Stream: true, Required: []string{"auth_user", "auth_pass"}, BuildOutbound: buildOutbound, BuildInbound: buildInbound})
}

func buildOutbound(f p.Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool) {
	return p.Encode(p.New("servers", p.Array{p.New("address", address, "port", port, "users", p.Array{p.New("user", f.Str("username"), "pass", f.Str("password"))})})), true
}
