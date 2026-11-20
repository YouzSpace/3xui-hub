package nodeapi

// 渲染侧的协议基准（golden）测试（issue #137 协议扩展点收敛）。
//
// 目的：nodeapi 的 Render 才是真正下发给节点 agent 的内核配置，协议差异一旦在
// 「客户端凭据形态」上收敛成查表，输出必须逐字节不变 —— 差一个字节，节点每轮拉取
// 都会因为指纹变化重启内核。
//
// 断言范围：每个协议各渲染一份完整配置，锁定
//   - clientsFor 的按协议凭据形态（id / password / auth；SS-2022 多用户 vs 单凭据）
//   - 中转凭据注入用的键（id / password / auth）
//   - vless 的 flow（仅 vless + tcp + reality/tls 才有）
//   - 未知协议被跳过（PHP match 返回 null）
//
// 金样本怎么来的：改造前用同一份用例跑一遍
// `go test ./internal/nodeapi -run TestRenderProtocolGolden -update-protocol-golden`，
// 把输出原样落进 testdata/protocol_render.golden；之后运行时不带 -update 只读比对。
//
// 维护：只有**有意**修改渲染输出时才重录，并说明原因。

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateRenderGolden = flag.Bool("update-protocol-golden", false, "重录渲染金样本")

// renderGoldenUsers 是固定的两个用户（一个带流量与到期时间，一个全默认）。
func renderGoldenUsers() []RenderUser {
	expired := time.Date(2027, 2, 6, 0, 0, 0, 0, time.UTC)
	return []RenderUser{
		{
			ID:           101,
			UUID:         "11111111-1111-1111-1111-111111111111",
			Protocol:     "vless",
			TrafficLimit: 1099511627776,
			ExpiredAt:    &expired,
		},
		{
			ID:           102,
			UUID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			Protocol:     "hysteria",
			TrafficLimit: 0,
		},
	}
}

// renderGoldenCases 是「协议 → 一份入站」的用例表：settings / stream 取自面板真实落库形状。
func renderGoldenCases() []RenderInbound {
	return []RenderInbound{
		{
			ID:             1,
			Tag:            "vless-reality",
			Protocol:       "vless",
			Port:           443,
			Listen:         "",
			Settings:       json.RawMessage(`{"decryption":"none"}`),
			StreamSettings: json.RawMessage(`{"network":"tcp","security":"reality"}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             2,
			Tag:            "vless-ws",
			Protocol:       "vless",
			Port:           8443,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"decryption":"none"}`),
			StreamSettings: json.RawMessage(`{"network":"ws","security":"none","wsSettings":{"path":"/ws"}}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls"]}`),
		},
		{
			ID:             3,
			Tag:            "vmess-ws",
			Protocol:       "vmess",
			Port:           8080,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{}`),
			StreamSettings: json.RawMessage(`{"network":"ws","security":"none","wsSettings":{"path":"/vm"}}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             4,
			Tag:            "trojan-tls",
			Protocol:       "trojan",
			Port:           2096,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{}`),
			StreamSettings: json.RawMessage(`{"network":"tcp","security":"tls","tlsSettings":{"serverName":"example.com"}}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             5,
			Tag:            "hy2",
			Protocol:       "hysteria",
			Port:           8443,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"version":2}`),
			StreamSettings: json.RawMessage(`{"network":"hysteria","security":"tls","hysteriaSettings":{"version":2,"udpIdleTimeout":60}}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             6,
			Tag:            "ss2022",
			Protocol:       "shadowsocks",
			Port:           8388,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"method":"2022-blake3-aes-128-gcm","password":"MDEyMzQ1Njc4OWFiY2RlZg=="}`),
			StreamSettings: json.RawMessage(`{"network":"tcp","security":"none"}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			// 传统 AEAD：入站级单凭据，不注入逐用户 client。
			ID:             7,
			Tag:            "ss-legacy",
			Protocol:       "shadowsocks",
			Port:           8389,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"method":"aes-256-gcm","password":"legacy-pass"}`),
			StreamSettings: json.RawMessage(`{"network":"tcp","security":"none"}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             8,
			Tag:            "socks",
			Protocol:       "socks",
			Port:           1080,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"auth":"password","accounts":[{"user":"socks-user","pass":"socks-pass"}],"udp":false}`),
			StreamSettings: json.RawMessage(`{"network":"tcp","security":"none"}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             9,
			Tag:            "http",
			Protocol:       "http",
			Port:           1081,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"accounts":[{"user":"http-user","pass":"http-pass"}],"allowTransparent":false}`),
			StreamSettings: json.RawMessage(`{"network":"tcp","security":"none"}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             10,
			Tag:            "tunnel",
			Protocol:       "tunnel",
			Port:           1082,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"address":"127.0.0.1","port":8080,"network":"tcp,udp"}`),
			StreamSettings: json.RawMessage(`{"network":"","security":"none"}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			ID:             11,
			Tag:            "wireguard",
			Protocol:       "wireguard",
			Port:           51820,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{"secretKey":"s3cr3t","mtu":1420,"noKernelTun":true}`),
			StreamSettings: json.RawMessage(`{}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
		{
			// 未知协议：clientsFor 跳过用户（PHP match 返回 null）。
			ID:             12,
			Tag:            "amneziawg",
			Protocol:       "amneziawg",
			Port:           51821,
			Listen:         "0.0.0.0",
			Settings:       json.RawMessage(`{}`),
			StreamSettings: json.RawMessage(`{"network":"","security":"none"}`),
			Sniffing:       json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
		},
	}
}

// renderGoldenData 组装一份只含入站的渲染输入（出站/路由留空，避免噪声）。
func renderGoldenData(ib RenderInbound) *RenderData {
	return &RenderData{
		Node: RenderNode{
			ID:         42,
			Name:       "MY-TEST",
			Host:       "node.example.com",
			DriverType: "xray",
			ConfigVer:  7,
		},
		Users:    renderGoldenUsers(),
		Inbounds: []RenderInbound{ib},
		RelayClients: []RenderRelayClient{
			{InboundID: ib.ID, ClientID: "relay-credential-1"},
		},
	}
}

// TestRenderProtocolGolden 逐个协议渲染完整配置并与金样本逐字节比对。
func TestRenderProtocolGolden(t *testing.T) {
	var b strings.Builder
	for _, ib := range renderGoldenCases() {
		body, err := RenderJSON(renderGoldenData(ib), nil)
		if err != nil {
			t.Fatalf("%s 渲染失败: %v", ib.Protocol, err)
		}
		fmt.Fprintf(&b, "=== %s ===\n%s\n\n", ib.Protocol, body)
	}
	got := b.String()

	path := filepath.Join("testdata", "protocol_render.golden")
	if *updateRenderGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("建 testdata 目录失败: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("写金样本失败: %v", err)
		}
		t.Logf("已重录金样本 %s（%d 字节）", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读金样本失败（首次请带 -update-protocol-golden 生成）: %v", err)
	}
	if got == string(want) {
		return
	}
	t.Fatalf("渲染输出与金样本不一致（第一处差异）:\n%s", renderFirstDiff(string(want), got))
}

// renderFirstDiff 定位第一处行级差异。
func renderFirstDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) && i < len(gotLines); i++ {
		if wantLines[i] != gotLines[i] {
			return fmt.Sprintf("第 %d 行:\n  金样本: %s\n  当前值: %s", i+1, wantLines[i], gotLines[i])
		}
	}
	return fmt.Sprintf("行数不同：金样本 %d 行，当前 %d 行", len(wantLines), len(gotLines))
}
