package nodeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

// ───────────────────── 静态 IP 专属出站（#静态IP-32） ─────────────────────
//
// 本文件两块内容：
//  1. TestRenderBytesUnchangedWithoutStaticChain —— 纯内存 fixture 的渲染指纹。
//     指纹是**改动前**跑出来的：普通用户（没有静态 IP 用户）的渲染结果必须逐字节不变。
//  2. TestStaticChain* —— 连 MySQL（独立库 ch_region32_*，自建自删）跑
//     LoadRenderData → RenderJSON 全链路，验证动态出站与路由规则。

// renderFixtureKey 是 fixture 用的 APP_KEY（32 字节）。fixture 里的密文都不是
// 用这把钥匙加密的，所以解密必然失败 —— 正好覆盖「解密失败退回原始值」的兼容分支。
var renderFixtureKey = []byte("0123456789abcdef0123456789abcdef")

// staticChainRegressionFixture 是一份「有普通用户、没有任何静态 IP 用户」的节点数据。
//
// 刻意把渲染路径上会产出内容的部分都铺上（入站 + 中转凭据 + 自定义出站 +
// wireguard secretKey + 订阅出站 + balancer + 观测器 + 两条路由规则），
// 这样「新增动态出站/路由时不小心动到老分支」才会被指纹抓住。
func staticChainRegressionFixture() *RenderData {
	expired := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	secret := "not-a-real-ciphertext"
	return &RenderData{
		Node: RenderNode{
			ID:          1,
			Name:        "n1",
			Host:        "node1.example.com",
			DriverType:  "xray",
			ConfigVer:   7,
			RoutingOpts: RoutingOptions{DomainStrategy: "AsIs"},
		},
		Users: []RenderUser{
			{ID: 1, UUID: "11111111-1111-1111-1111-111111111111", Protocol: "vless", TrafficLimit: 107374182400, ExpiredAt: &expired},
			{ID: 2, UUID: "22222222-2222-2222-2222-222222222222", Protocol: "trojan", TrafficLimit: 0},
		},
		Inbounds: []RenderInbound{{
			ID:         11,
			Tag:        "in-443",
			Protocol:   "vless",
			Port:       443,
			Listen:     "0.0.0.0",
			Settings:   json.RawMessage(`{"decryption":"none","clients":[]}`),
			Sniffing:   json.RawMessage(`{"enabled":true,"destOverride":["http","tls","quic"]}`),
			StreamSettings: json.RawMessage(`{"network":"tcp","security":"none"}`),
		}},
		Outbounds: []RenderOutbound{
			{Tag: "proxy-a", Protocol: "socks", Settings: json.RawMessage(`{"servers":[{"address":"10.0.0.2","port":1080,"users":[{"user":"ua","pass":"pa"}]}]}`)},
			{Tag: "warp", Protocol: "wireguard", Settings: json.RawMessage(`{"address":["172.16.0.2/32"],"peers":[]}`), SecretKey: &secret},
		},
		Balancers: []RenderBalancer{{Tag: "auto", Selectors: json.RawMessage(`["proxy-"]`), Strategy: "leastPing"}},
		RoutingRules: []RenderRoutingRule{
			{Domains: json.RawMessage(`["example.com","*.example.org"]`), OutboundTag: "proxy-a"},
			{IPs: json.RawMessage(`["1.1.1.1/32"]`), Network: strPtr("tcp"), OutboundTag: "warp"},
		},
		Subscriptions: []RenderSubscription{{
			Prepend: true,
			Fetched: json.RawMessage(`[{"tag":"sub-1","protocol":"freedom","settings":{}}]`),
		}},
		RelayClients: []RenderRelayClient{{InboundID: 11, ClientID: "relay-uuid-1"}},
	}
}

func strPtr(s string) *string { return &s }

// renderDigest 渲染 fixture 并取 sha256（节点下发的字节内容就是这个）。
func renderDigest(t *testing.T, d *RenderData) (string, []byte) {
	t.Helper()
	body, err := RenderJSON(d, renderFixtureKey)
	if err != nil {
		t.Fatalf("RenderJSON 出错: %v", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), body
}

// TestRenderBytesUnchangedWithoutStaticChain 是「没有静态 IP 用户时逐字节不变」的回归锁。
//
// baseline 是本次改动**之前**用同一份 fixture 跑出来的 sha256；
// 只要渲染结果有一个字节不同（顺序、空格、字段），这条用例就会变红。
func TestRenderBytesUnchangedWithoutStaticChain(t *testing.T) {
	const baseline = "bb6986396a6ac9e01bf4a3fcfc9135c5004226231e95c13014234e61202af3ca"

	got, body := renderDigest(t, staticChainRegressionFixture())
	if got != baseline {
		t.Fatalf("普通用户的渲染结果变了（逐字节回归失败）\nbaseline=%s\n实际    =%s\n渲染输出:\n%s", baseline, got, body)
	}
	t.Logf("渲染指纹 %s（%d 字节），与改动前一致", got, len(body))
}
