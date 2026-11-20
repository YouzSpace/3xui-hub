package nodeapi

// 渲染侧「客户端凭据形态」表的断言（issue #137 协议扩展点收敛）。
//
// 金样本（TestRenderProtocolGolden）锁的是**渲染输出**；这里锁**表的形状**：
// 哪些协议有逐用户凭据、凭据键是什么、flow / SS-2022 特判只属于谁 —— 免得以后
// 谁往 clientsFor 里加回一个 switch 分支，或者把某个协议的凭据键改错（改错的表现是
// 节点上用户连不上，而且没有任何报错）。

import "testing"

// TestInboundClientTable 表里每行的键与特判。
func TestInboundClientTable(t *testing.T) {
	// 凭据键 → 该协议的用户 client 第一个键（顺序即渲染顺序）。
	want := map[string]string{
		"vless":       "id",
		"vmess":       "id",
		"trojan":      "password",
		"hysteria":    "auth",
		"shadowsocks": "password",
	}
	if len(inboundClientTable) != len(want) {
		t.Fatalf("表里有 %d 行，期望 %d 行（%v）", len(inboundClientTable), len(want), want)
	}
	for _, proto := range []string{"vless", "vmess", "trojan", "hysteria", "shadowsocks"} {
		spec, ok := lookupInboundClientSpec(proto)
		if !ok {
			t.Fatalf("表里缺协议 %q", proto)
		}
		if spec.Build == nil {
			t.Fatalf("%q 没有 client 构造函数", proto)
		}
		if spec.RelayCredentialKey == "" {
			t.Errorf("%q 没声明中转凭据键（会导致中转凭据注入成空键）", proto)
		}
		env := &inboundClientEnv{ib: RenderInbound{Protocol: proto}}
		client := spec.Build(RenderUser{ID: 7, UUID: "user-uuid"}, env)
		if len(client) == 0 || client[0].Key != want[proto] {
			t.Errorf("%q 的用户凭据键 = %+v，期望首键 %q", proto, client, want[proto])
		}
		// 只有 vless 要 flow；只有 shadowsocks 有「按 method 决定是否注入」的预判。
		if (spec.Flow) != (proto == "vless") {
			t.Errorf("%q 的 Flow = %v（只有 vless 该为 true）", proto, spec.Flow)
		}
		if (spec.Prepare != nil) != (proto == "shadowsocks") {
			t.Errorf("%q 的 Prepare 是否设置 = %v（只有 shadowsocks 该设置）", proto, spec.Prepare != nil)
		}
	}

	// 中转凭据键：trojan = password、hysteria = auth，其余默认 id。
	for proto, key := range map[string]string{
		"vless": "id", "vmess": "id", "trojan": "password", "hysteria": "auth", "shadowsocks": "id",
	} {
		if got := inboundClientSpecOf(proto).RelayCredentialKey; got != key {
			t.Errorf("%q 的中转凭据键 = %q，期望 %q", proto, got, key)
		}
	}
}

// TestInboundClientTableFallback 没有逐用户凭据的协议：不注入用户 client，
// 但中转凭据仍按默认键 id 注入（与改动前 switch 的 default 分支一致）。
func TestInboundClientTableFallback(t *testing.T) {
	for _, proto := range []string{"socks", "http", "tunnel", "wireguard", "amneziawg"} {
		if _, ok := lookupInboundClientSpec(proto); ok {
			t.Errorf("%q 不该有逐用户凭据行（它没有用户维度凭据）", proto)
		}
		spec := inboundClientSpecOf(proto)
		if spec.Build != nil {
			t.Errorf("%q 的兜底行不该有 client 构造函数", proto)
		}
		if spec.RelayCredentialKey != "id" {
			t.Errorf("%q 的兜底中转凭据键 = %q，期望 id", proto, spec.RelayCredentialKey)
		}
	}
}

// TestInboundClientTableCoversGoldenCases 金样本里的每个协议都得有明确归属：
// 要么在表里，要么是「 knowingly 没有用户凭据」的那几个 —— 漏一个就是悄悄少注入用户。
func TestInboundClientTableCoversGoldenCases(t *testing.T) {
	noUserClients := map[string]bool{"socks": true, "http": true, "tunnel": true, "wireguard": true, "amneziawg": true}
	for _, ib := range renderGoldenCases() {
		_, inTable := lookupInboundClientSpec(ib.Protocol)
		if !inTable && !noUserClients[ib.Protocol] {
			t.Errorf("协议 %q 既不在客户端凭据表里，也不在「无用户凭据」名单里", ib.Protocol)
		}
	}
}
