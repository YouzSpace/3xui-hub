package subscription

// mihomo 传输层映射的回归测试（照 Xboard ClashMeta::buildVless 的口径）：
//   - httpupgrade 不是独立 network，必须 ws + v2ray-http-upgrade；
//   - grpc 的 serviceName 走 grpc-opts；
//   - xhttp 必须带 host（服务端配了 Host 匹配时否则握手失败）；
//   - tcp 的 HTTP 伪装是独立 network=http + http-opts（path/Host 为数组）。

import (
	"strings"
	"testing"
)

func TestClashTransportMapping(t *testing.T) {
	s := &Service{}
	user := &User{ID: 1, UUID: "11111111-2222-3333-4444-555555555555", Protocol: "vless"}
	cfg := &siteConfigReader{values: map[string]string{}}

	cases := []struct {
		name string
		link string
		want []string
	}{
		{
			"vless-httpupgrade",
			"vless://11111111-2222-3333-4444-555555555555@1.2.3.4:443?type=httpupgrade&path=%2Fup&host=h.example.com&security=tls#n1",
			[]string{"network: ws", "v2ray-http-upgrade: true", `path: "/up"`, `Host: "h.example.com"`},
		},
		{
			"vless-grpc",
			"vless://11111111-2222-3333-4444-555555555555@1.2.3.4:443?type=grpc&serviceName=svc&security=tls#n2",
			[]string{"network: grpc", "grpc-opts:", `grpc-service-name: "svc"`},
		},
		{
			"vless-xhttp-host",
			"vless://11111111-2222-3333-4444-555555555555@1.2.3.4:443?type=xhttp&path=%2Fx&host=xh.example.com&mode=auto&security=tls#n3",
			[]string{"network: xhttp", `host: "xh.example.com"`},
		},
		{
			"vless-tcp-http",
			"vless://11111111-2222-3333-4444-555555555555@1.2.3.4:80?type=tcp&headerType=http&path=%2Fp&host=p.example.com#n4",
			[]string{"network: http", "http-opts:", `- "/p"`, "Host:"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := s.generateClash(user, nil, []string{c.link}, cfg)
			if out == "" {
				t.Fatalf("生成结果为空（链接未被解析？）link=%s", c.link)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("输出缺少 %q\n--- 输出 ---\n%s", w, out)
				}
			}
		})
	}
}

// TestXrayURIGrpcServiceName 生产端：我们自己的 grpc 入站生成链接时必须带 serviceName。
func TestXrayURIGrpcServiceName(t *testing.T) {
	node := Node{Name: "hk-01"}
	inbound := XrayInbound{
		Tag:            "vl-grpc",
		Protocol:       "vless",
		Port:           443,
		StreamSettings: `{"network":"grpc","security":"tls","grpcSettings":{"serviceName":"my-svc"},"tlsSettings":{"serverName":"sni.example.com"}}`,
	}
	user := &User{ID: 7, UUID: "11111111-2222-3333-4444-555555555555"}
	uri := XrayInboundURI(node, inbound, user, "1.2.3.4")
	if !strings.Contains(uri, "serviceName=my-svc") {
		t.Fatalf("grpc 链接缺 serviceName: %s", uri)
	}
	if !strings.Contains(uri, "type=grpc") {
		t.Fatalf("grpc 链接 type 不对: %s", uri)
	}
}
