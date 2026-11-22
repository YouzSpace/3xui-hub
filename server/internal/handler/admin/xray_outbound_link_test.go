package admin

import (
	"encoding/base64"
	"testing"
)

// 出站分享链接「生成 / 解析」的特征测试（characterization test）。
// 锁定重构前的逐字输出，重构后必须完全一致——用于保证收敛/拆分不改行为。
func TestOutboundLinkGenerate(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		tag      string
		settings string
		stream   string
		want     string
	}{
		{"vless-reality", "vless", "node-1",
			`{"vnext":[{"address":"1.2.3.4","port":443,"users":[{"id":"11111111-1111-1111-1111-111111111111","encryption":"none","flow":"xtls-rprx-vision"}]}]}`,
			`{"network":"tcp","security":"reality","realitySettings":{"serverName":"a.example.com","publicKey":"PUBKEY","shortId":"abcd","fingerprint":"chrome"}}`,
			"vless://11111111-1111-1111-1111-111111111111@1.2.3.4:443?type=tcp&security=reality&fp=chrome&sni=a.example.com&pbk=PUBKEY&sid=abcd&flow=xtls-rprx-vision#node-1"},
		{"vmess-ws-tls", "vmess", "node-2",
			`{"vnext":[{"address":"5.6.7.8","port":8443,"users":[{"id":"22222222-2222-2222-2222-222222222222","alterId":0,"security":"auto"}]}]}`,
			`{"network":"ws","security":"tls","wsSettings":{"path":"/ws","headers":{"Host":"h.example.com"}},"tlsSettings":{"serverName":"b.example.com"}}`,
			"vmess://eyJ2IjoiMiIsInBzIjoibm9kZS0yIiwiYWRkIjoiNS42LjcuOCIsInBvcnQiOiI4NDQzIiwiaWQiOiIyMjIyMjIyMi0yMjIyLTIyMjItMjIyMi0yMjIyMjIyMjIyMjIiLCJhaWQiOiIwIiwic2N5IjoiYXV0byIsIm5ldCI6IndzIiwidHlwZSI6Im5vbmUiLCJob3N0IjoiaC5leGFtcGxlLmNvbSIsInBhdGgiOiJcL3dzIiwidGxzIjoidGxzIiwic25pIjoiYi5leGFtcGxlLmNvbSJ9"},
		{"trojan", "trojan", "node-3",
			`{"servers":[{"address":"9.9.9.9","port":443,"password":"passw0rd"}]}`,
			`{"network":"tcp","security":"tls","tlsSettings":{"serverName":"c.example.com"}}`,
			"trojan://passw0rd@9.9.9.9:443?security=tls&type=tcp&sni=c.example.com#node-3"},
		{"ss", "shadowsocks", "node-4",
			`{"servers":[{"address":"1.1.1.1","port":8388,"method":"aes-256-gcm","password":"sspass"}]}`,
			`{}`,
			"ss://YWVzLTI1Ni1nY206c3NwYXNz@1.1.1.1:8388#node-4"},
		{"socks", "socks", "node-5",
			`{"servers":[{"address":"2.2.2.2","port":1080,"users":[{"user":"u","pass":"p"}]}]}`,
			`{}`,
			"socks://u:p@2.2.2.2:1080#node-5"},
		{"http", "http", "node-6",
			`{"servers":[{"address":"3.3.3.3","port":8080,"users":[{"user":"u","pass":"p"}]}]}`,
			`{}`,
			"http://u:p@3.3.3.3:8080#node-6"},
		{"hysteria", "hysteria", "node-7",
			`{"version":2,"address":"4.4.4.4","port":7443}`,
			`{"network":"hysteria","security":"tls","hysteriaSettings":{"version":2,"auth":"authstr"},"tlsSettings":{"serverName":"d.example.com"}}`,
			"hysteria2://authstr@4.4.4.4:7443?sni=d.example.com#node-7"},
	}
	for _, c := range cases {
		ob := &XrayOutboundRecord{
			Protocol: c.protocol, Tag: c.tag,
			Settings: []byte(c.settings), StreamSettings: []byte(c.stream),
		}
		got, err := xrayGenerateOutboundLink(ob)
		if err != nil {
			t.Errorf("%s: 生成报错 %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: 生成不符\n got=%s\nwant=%s", c.name, got, c.want)
		}
	}
}

func TestOutboundLinkParse(t *testing.T) {
	vmessJSON := `{"v":"2","ps":"n","add":"5.6.7.8","port":"8443","id":"22222222-2222-2222-2222-222222222222","aid":"0","net":"ws","host":"h.example.com","path":"/ws","tls":"tls","sni":"b.example.com"}`
	vmessLink := "vmess://" + base64.StdEncoding.EncodeToString([]byte(vmessJSON))

	cases := []struct{ name, link, want string }{
		{"vmess", vmessLink,
			`{"protocol":"vmess","settings":{"vnext":[{"address":"5.6.7.8","port":8443,"users":[{"id":"22222222-2222-2222-2222-222222222222","alterId":0,"security":"auto"}]}]},"stream_settings":{"network":"ws","security":"tls","wsSettings":{"path":"\/ws","headers":{"Host":"h.example.com"}},"tlsSettings":{"serverName":"b.example.com"}},"tag":"n"}`},
		{"vless", "vless://11111111-1111-1111-1111-111111111111@1.2.3.4:443?type=tcp&security=reality&pbk=PUBKEY&sid=abcd&sni=a.example.com&fp=chrome&flow=xtls-rprx-vision#n1",
			`{"protocol":"vless","settings":{"vnext":[{"address":"1.2.3.4","port":443,"users":[{"id":"11111111-1111-1111-1111-111111111111","encryption":"none","flow":"xtls-rprx-vision"}]}]},"stream_settings":{"network":"tcp","security":"reality","realitySettings":{"serverName":"a.example.com","publicKey":"PUBKEY","shortId":"abcd","fingerprint":"chrome"}},"tag":"n1"}`},
		{"trojan", "trojan://passw0rd@9.9.9.9:443?type=tcp&security=tls&sni=c.example.com#n3",
			`{"protocol":"trojan","settings":{"servers":[{"address":"9.9.9.9","port":443,"password":""}]},"stream_settings":{"network":"tcp","security":"tls","tlsSettings":{"serverName":"c.example.com"}},"tag":"n3"}`},
		{"ss", "ss://YWVzLTI1Ni1nY206c3NwYXNz@1.1.1.1:8388#n4",
			`{"protocol":"shadowsocks","settings":{"servers":[{"address":"1.1.1.1","port":8388,"method":"aes-256-gcm","password":"sspass"}]},"stream_settings":{},"tag":"n4"}`},
		{"hysteria2", "hysteria2://authstr@4.4.4.4:7443?sni=d.example.com#n7",
			`{"protocol":"hysteria","settings":{"version":2,"address":"4.4.4.4","port":7443},"stream_settings":{"network":"hysteria","security":"tls","tlsSettings":{"serverName":"d.example.com"},"hysteriaSettings":{"version":2,"auth":"authstr"}},"tag":"n7"}`},
		{"socks", "socks://dTpw@2.2.2.2:1080#n5",
			`{"protocol":"socks","settings":{"servers":[{"address":"2.2.2.2","port":1080,"users":[{"user":"dTpw","pass":""}]}]},"stream_settings":{},"tag":"n5"}`},
		{"http", "http://dTpw@3.3.3.3:8080#n6",
			`{"protocol":"http","settings":{"servers":[{"address":"3.3.3.3","port":8080,"users":[{"user":"dTpw","pass":""}]}]},"stream_settings":{},"tag":"n6"}`},
	}
	for _, c := range cases {
		obj, err := xrayParseOutboundLink(c.link)
		if err != nil {
			t.Errorf("%s: 解析报错 %v", c.name, err)
			continue
		}
		got := string(phpEncode(obj))
		if got != c.want {
			t.Errorf("%s: 解析不符\n got=%s\nwant=%s", c.name, got, c.want)
		}
	}

	// 不支持的协议要报错。
	if _, err := xrayParseOutboundLink("wireguard://x@1.2.3.4:1"); err == nil {
		t.Errorf("不支持的链接协议应当报错")
	}
	// 不支持的出站协议不能生成链接。
	if _, err := xrayGenerateOutboundLink(&XrayOutboundRecord{Protocol: "freedom", Tag: "t"}); err == nil {
		t.Errorf("freedom 无分享链接，应当报错")
	}
}
