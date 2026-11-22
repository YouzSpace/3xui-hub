package tunnel

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "tunnel", Transports: []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, Securities: []string{"none", "tls"}, Stream: true, Required: []string{"tunnel_address", "tunnel_port"}, Defaults: []p.FieldDefault{{Key: "network", Value: "tcp,udp"}}, BuildInbound: buildInbound})
}
