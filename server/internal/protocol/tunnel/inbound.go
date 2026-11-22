package tunnel

import (
	"strings"

	p "controlhub-server/internal/protocol"
)

// buildInbound tunnel（dokodemo-door）：把收到的流量原样转给 address:port。
//
// 注意 L4 网络是 settings.network（tcp/udp/tcp,udp），写进 streamSettings.network
// 会被内核直接拒（unknown transport protocol: udp）。
func buildInbound(c *p.InboundCtx) (*p.Object, bool) {
	addr := strings.TrimSpace(c.Input("tunnel_address"))
	port, _ := c.Int("tunnel_port")
	if addr == "" || port <= 0 {
		c.Fail("Tunnel 入站必须填写转发目标（地址 + 端口）")
		return nil, false
	}
	net := strings.TrimSpace(c.Input("tunnel_network"))
	if net == "" {
		net = c.DefaultStr("network") // Spec 缺省 tcp,udp
	}
	switch net {
	case "tcp", "udp", "tcp,udp":
	default:
		c.Fail("Tunnel 转发协议只支持 tcp / udp / tcp,udp")
		return nil, false
	}
	return p.New("address", addr, "port", port, "network", net), true
}
