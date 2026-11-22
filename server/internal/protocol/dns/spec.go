package dns

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "dns", BuildOutbound: buildOutbound})
}

func buildOutbound(f p.Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool) {
	network := "udp"; if s := f.Str("dns_network"); s != "" { network = s }
	return p.Encode(p.New("network", network, "address", address, "port", port)), true
}
