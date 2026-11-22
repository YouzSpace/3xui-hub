package hysteria

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "hysteria", Transports: []string{"hysteria"}, Securities: []string{"tls"},
		Normalize: true, ForcedNetwork: "hysteria", ForcedSecurity: "tls", Stream: true, Required: []string{"tls_cert_id"},
		Defaults: []p.FieldDefault{{Key: "version", Value: int64(2)}}, Client: p.ClientAuth, RelayKey: "auth",
		BuildOutbound: buildOutbound, BuildInbound: buildInbound})
}

func buildOutbound(f p.Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool) {
	return p.Encode(p.New("version", int64(2), "address", address, "port", port)), true
}
