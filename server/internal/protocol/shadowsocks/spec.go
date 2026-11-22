package shadowsocks

import p "controlhub-server/internal/protocol"

func init() {
	p.Register(p.Spec{Name: "shadowsocks", Transports: []string{"tcp"}, Securities: []string{"none"},
		RejectMessage: "Shadowsocks 仅支持 tcp + 无附加安全层", Stream: true, Required: []string{"ss_password"},
		Defaults: []p.FieldDefault{{Key: "method", Value: "2022-blake3-aes-256-gcm"}}, Client: p.ClientSS2022, RelayKey: "id",
		BuildOutbound: buildOutbound, BuildInbound: buildInbound})
}

func buildOutbound(f p.Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool) {
	method, ok := require(f.Str("method")); if !ok { return nil, false }
	pw, ok := require(f.Str("password")); if !ok { return nil, false }
	return p.Encode(p.New("servers", p.Array{p.New("address", address, "port", port, "method", method, "password", pw)})), true
}
