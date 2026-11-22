package wireguard

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "wireguard", Transports: nil, Securities: []string{"none"},
		Normalize: true, ForcedNetwork: "", ForcedSecurity: "none", Stream: false, PresentNetwork: "udp",
		Defaults: []p.FieldDefault{{Key: "mtu", Value: int64(1420)}}, BuildInbound: buildInbound})
}
