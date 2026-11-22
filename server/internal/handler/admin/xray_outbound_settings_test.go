package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func zzCtx() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

// 特征测试：锁定 buildOutboundSettings 逐字节输出（重构前后必须一致）。
func TestOutboundSettingsChar(t *testing.T) {
	d := &NodeDomain{}
	c := zzCtx()
	cases := []struct {
		name     string
		protocol string
		input    map[string]any
		want     string
	}{
		{"freedom", "freedom", map[string]any{}, `{"domainStrategy":"AsIs"}`},
		{"blackhole", "blackhole", map[string]any{}, `{}`},
		{"unknown", "wireguard", map[string]any{}, `{}`},
		{"vmess", "vmess", map[string]any{"address": "1.2.3.4", "port": int64(443), "user_id": "UUID1", "encryption": "auto"}, `{"vnext":[{"address":"1.2.3.4","port":443,"users":[{"id":"UUID1","alterId":0,"security":"auto"}]}]}`},
		{"vless", "vless", map[string]any{"address": "1.2.3.4", "port": int64(443), "user_id": "UUID1", "flow": "xtls-rprx-vision"}, `{"vnext":[{"address":"1.2.3.4","port":443,"users":[{"id":"UUID1","encryption":"none","flow":"xtls-rprx-vision"}]}]}`},
		{"trojan", "trojan", map[string]any{"address": "9.9.9.9", "port": int64(443), "password": "pw"}, `{"servers":[{"address":"9.9.9.9","port":443,"password":"pw"}]}`},
		{"ss", "shadowsocks", map[string]any{"address": "1.1.1.1", "port": int64(8388), "method": "aes-256-gcm", "password": "ssp"}, `{"servers":[{"address":"1.1.1.1","port":8388,"method":"aes-256-gcm","password":"ssp"}]}`},
		{"socks", "socks", map[string]any{"address": "2.2.2.2", "port": int64(1080), "username": "u", "password": "p"}, `{"servers":[{"address":"2.2.2.2","port":1080,"users":[{"user":"u","pass":"p"}]}]}`},
		{"http", "http", map[string]any{"address": "3.3.3.3", "port": int64(8080), "username": "u", "password": "p"}, `{"servers":[{"address":"3.3.3.3","port":8080,"users":[{"user":"u","pass":"p"}]}]}`},
		{"hysteria", "hysteria", map[string]any{"address": "4.4.4.4", "port": int64(7443)}, `{"version":2,"address":"4.4.4.4","port":7443}`},
		{"dns-port-default", "dns", map[string]any{"address": "8.8.8.8", "port": int64(0), "dns_network": "tcp"}, `{"network":"tcp","address":"8.8.8.8","port":53}`},
	}
	for _, tc := range cases {
		got := string(d.buildOutboundSettings(c, tc.protocol, tc.input))
		if got != tc.want {
			t.Errorf("[%s] settings 不符\n got=%s\nwant=%s", tc.name, got, tc.want)
		}
	}
}

// 特征测试：锁定 buildOutboundStream 逐字节输出 + ok。
func TestOutboundStreamChar(t *testing.T) {
	d := &NodeDomain{}
	c := zzCtx()
	node := &NodeRecord{}
	cases := []struct {
		name     string
		protocol string
		input    map[string]any
		wantOK   bool
		want     string
	}{
		{"freedom", "freedom", map[string]any{}, true, `{}`},
		{"blackhole", "blackhole", map[string]any{}, true, `{}`},
		{"dns", "dns", map[string]any{}, true, `{}`},
		{"vless-tcp-tls", "vless", map[string]any{"network": "tcp", "security": "tls", "tls_server_name": "x.example.com"}, true, `{"network":"tcp","security":"tls","tlsSettings":{"serverName":"x.example.com"}}`},
		{"vless-reality", "vless", map[string]any{"network": "tcp", "security": "reality", "reality_server_name": "a.example.com", "reality_public_key": "PBK", "reality_short_id": "sid", "reality_fingerprint": "chrome", "reality_spider_x": "/"}, true, `{"network":"tcp","security":"reality","realitySettings":{"serverName":"a.example.com","publicKey":"PBK","shortId":"sid","fingerprint":"chrome","spiderX":"\/"}}`},
		{"vmess-ws-tls", "vmess", map[string]any{"network": "ws", "security": "tls", "ws_path": "/ws", "ws_host": "h.example.com", "tls_server_name": "b.example.com"}, true, `{"network":"ws","security":"tls","wsSettings":{"path":"\/ws","headers":{"Host":"h.example.com"}},"tlsSettings":{"serverName":"b.example.com"}}`},
		{"vless-xhttp", "vless", map[string]any{"network": "xhttp", "security": "tls", "tls_server_name": "x.example.com", "xhttp_path": "/xh", "xhttp_mode": "auto"}, true, `{"network":"xhttp","security":"tls","xhttpSettings":{"path":"\/xh","mode":"auto"},"tlsSettings":{"serverName":"x.example.com"}}`},
		{"vless-httpupgrade", "vless", map[string]any{"network": "httpupgrade", "security": "none", "httpupgrade_path": "/hu", "httpupgrade_host": "hu.example.com"}, true, `{"network":"httpupgrade","security":"none","httpupgradeSettings":{"path":"\/hu","host":"hu.example.com"}}`},
		{"vless-tcp-httpobfs", "vless", map[string]any{"network": "tcp", "security": "none", "tcp_header_type": "http", "tcp_header_path": "/p", "tcp_header_host": "p.example.com"}, true, `{"network":"tcp","security":"none","tcpSettings":{"header":{"type":"http","request":{"version":"1.1","method":"GET","path":["\/p"],"headers":{"Host":["p.example.com"]}}}}}`},
		// hysteria 出站缺 auth → 失败（ok=false）
		{"hysteria-noauth", "hysteria", map[string]any{}, false, ``},
		// hysteria 出站带 auth：强制 network=hysteria/security=tls，ALPN 固定 h3
		{"hysteria-auth", "hysteria", map[string]any{"password": "authstr"}, true, `{"network":"hysteria","security":"tls","hysteriaSettings":{"version":2,"auth":"authstr"},"tlsSettings":{"alpn":["h3"]}}`},
		{"hysteria-auth-sni", "hysteria", map[string]any{"password": "authstr", "tls_server_name": "d.example.com"}, true, `{"network":"hysteria","security":"tls","hysteriaSettings":{"version":2,"auth":"authstr"},"tlsSettings":{"serverName":"d.example.com","alpn":["h3"]}}`},
		// reality 但非 vless → 失败
		{"trojan-reality", "trojan", map[string]any{"network": "tcp", "security": "reality"}, false, ``},
		// reality + 非 tcp/xhttp/grpc 传输 → 失败
		{"vless-reality-ws", "vless", map[string]any{"network": "ws", "security": "reality"}, false, ``},
		// reality + grpc（vless）
		{"vless-reality-grpc", "vless", map[string]any{"network": "grpc", "security": "reality", "reality_server_name": "a.example.com", "reality_public_key": "PBK"}, true, `{"network":"grpc","security":"reality","realitySettings":{"serverName":"a.example.com","publicKey":"PBK","fingerprint":"chrome"}}`},
	}
	for _, tc := range cases {
		got, ok := d.buildOutboundStream(c, node, tc.protocol, tc.input, nil)
		if ok != tc.wantOK {
			t.Errorf("[%s] ok 不符: got=%v want=%v", tc.name, ok, tc.wantOK)
			continue
		}
		if ok && string(got) != tc.want {
			t.Errorf("[%s] stream 不符\n got=%s\nwant=%s", tc.name, got, tc.want)
		}
	}
}
