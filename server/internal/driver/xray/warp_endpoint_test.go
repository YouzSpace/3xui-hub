package xray

import "testing"

// CF 回执里 endpoint 同时给 v4 / v6 / host。端点写域名时 xray 会在 A/AAAA 间随机抽，
// IPv4-only 节点抽到 AAAA 就是 network is unreachable；所以必须有字面量兜底。
func TestEndpointOfPrefersLiteralAddress(t *testing.T) {
	cases := []struct {
		name     string
		endpoint map[string]any
		want     string
	}{
		{
			name: "v4 优先",
			endpoint: map[string]any{
				"v4":    "162.159.192.9:0",
				"v6":    "[2606:4700:d0::a29f:c009]:0",
				"host":  "engage.cloudflareclient.com:2408",
				"ports": []any{2408.0, 500.0},
			},
			want: "162.159.192.9:2408",
		},
		{
			name: "没有 v4 用 v6 字面量",
			endpoint: map[string]any{
				"v6":   "[2606:4700:d0::a29f:c009]:0",
				"host": "engage.cloudflareclient.com:2408",
			},
			want: "[2606:4700:d0::a29f:c009]:2408",
		},
		{
			name: "两者都没有才退回域名",
			endpoint: map[string]any{
				"host": "engage.cloudflareclient.com:2408",
			},
			want: "engage.cloudflareclient.com:2408",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointOf(tc.endpoint); got != tc.want {
				t.Fatalf("endpointOf() = %q, want %q", got, tc.want)
			}
		})
	}
}
