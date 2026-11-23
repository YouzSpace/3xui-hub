package subscription

// 订阅链接的 flow 口径（线上故障回归）：vless + tcp + reality/tls 必须带
// flow=xtls-rprx-vision —— 节点侧渲染给这些入站的每个 client 都写了 flow，
// 链接不带 flow 客户端就不会开 vision，握手直接失败（US01 9943 tcp+TLS 连不上）。

import (
	"strings"
	"testing"
)

func TestXrayVlessURIAddsFlowOnTCPWithTLSOrReality(t *testing.T) {
	node := Node{Name: "US01", Host: "us1.example.com"}
	user := &User{ID: 25, UUID: "ddeca4bb-be29-418f-bc49-641ddd05c593"}

	cases := []struct {
		name     string
		stream   string
		wantFlow bool
	}{
		{
			// 线上那台：vless + tcp + TLS（us1.wujiecloud.top:9943）
			name:     "tcp+tls",
			stream:   `{"network":"tcp","security":"tls","tlsSettings":{"serverName":"us1.example.com"}}`,
			wantFlow: true,
		},
		{
			// 默认 network/security 缺省时按 tcp/none，不该加 flow
			name:     "tcp+none",
			stream:   `{"network":"tcp","security":"none"}`,
			wantFlow: false,
		},
		{
			name:     "tcp+reality",
			stream:   `{"network":"tcp","security":"reality","realitySettings":{"dest":"www.cloudflare.com:443","serverNames":["www.cloudflare.com"],"publicKey":"PUBKEY"}}`,
			wantFlow: true,
		},
		{
			// 非 tcp 传输层一律不吃 flow
			name:     "ws+tls",
			stream:   `{"network":"ws","security":"tls","wsSettings":{"path":"/ws"}}`,
			wantFlow: false,
		},
		{
			name:     "grpc+tls",
			stream:   `{"network":"grpc","security":"tls","grpcSettings":{"serviceName":"svc"}}`,
			wantFlow: false,
		},
		{
			name:     "xhttp+reality",
			stream:   `{"network":"xhttp","security":"reality","realitySettings":{"dest":"www.cloudflare.com:443","serverNames":["www.cloudflare.com"],"publicKey":"PUBKEY"},"xhttpSettings":{"path":"/x"}}`,
			wantFlow: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inbound := XrayInbound{
				ID: 62, NodeID: 45, Tag: "vless", Protocol: "vless",
				Port: 9943, StreamSettings: tc.stream, Enabled: true,
			}
			link := xrayVlessURI(node, inbound, user, "us1.example.com")

			if got := strings.Contains(link, "flow="+xrayFlow); got != tc.wantFlow {
				t.Fatalf("链接里 flow 是否存在 = %v，期望 %v\n链接: %s", got, tc.wantFlow, link)
			}
			if !strings.Contains(link, "type=") || !strings.Contains(link, "security=") {
				t.Fatalf("链接缺少 type/security 基本参数: %s", link)
			}
		})
	}
}
