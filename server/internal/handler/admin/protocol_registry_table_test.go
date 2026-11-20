package admin

// 协议能力表自身的断言（issue #137 协议扩展点收敛）。
//
// 金样本（TestProtocolRegistryGolden）锁的是**输出**；这个文件锁的是**表的内容**：
//   - 校验白名单的并集必须与改动前写死的 in:... 字面量逐字一致（顺序也算）；
//   - 九个协议一个都不能漏，每行都得有渲染入口；
//   - 表里声明的 Required 必须真的被拦住（声明与实现对得上，别写成摆设）。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestProtocolRegistryWhitelists 白名单并集 = 改动前 validateInboundPayload 里的字面量。
//
// 这三条字符串是协议的对外契约（前端下拉、历史数据、文档都按它），顺序改了虽然功能
// 一样，但会让 diff 与线上配置对不上，所以逐字钉住。
func TestProtocolRegistryWhitelists(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"协议", inRule(inboundProtocolNames()), "in:vless,vmess,trojan,shadowsocks,hysteria,socks,http,tunnel,wireguard"},
		{"传输层", inRule(inboundProtocolNetworks()), "in:tcp,ws,grpc,xhttp,httpupgrade,hysteria"},
		{"安全层", inRule(inboundProtocolSecurities()), "in:none,tls,reality"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s白名单 = %q，期望 %q（改动前的字面量）", tc.name, tc.got, tc.want)
		}
	}
}

// TestProtocolRegistryRows 表的结构完整性：一个协议一行、各有渲染入口、口头事实不矛盾。
func TestProtocolRegistryRows(t *testing.T) {
	wantProtocols := []string{"vless", "vmess", "trojan", "shadowsocks", "hysteria", "socks", "http", "tunnel", "wireguard"}
	if len(inboundProtocolTable) != len(wantProtocols) {
		t.Fatalf("表里有 %d 行，期望 %d 行（%v）", len(inboundProtocolTable), len(wantProtocols), wantProtocols)
	}
	for _, name := range wantProtocols {
		spec, ok := lookupInboundProtocol(name)
		if !ok {
			t.Fatalf("表里缺协议 %q", name)
		}
		if spec.Build == nil {
			t.Errorf("%q 没有渲染入口（Build 为空）", name)
		}
		if len(spec.Transports) == 0 && name != "wireguard" {
			t.Errorf("%q 没声明任何传输层", name)
		}
		if spec.RejectMessage != "" && (len(spec.Transports) == 0 || len(spec.Securities) == 0) {
			t.Errorf("%q 有组合限制文案却没列支持项，限制永远判不出来", name)
		}
	}

	// reality 只属于 vless（改动前是硬编码 `protocol != "vless"`）。
	for i := range inboundProtocolTable {
		spec := &inboundProtocolTable[i]
		hasReality := len(spec.RealityTransports) > 0
		if hasReality != (spec.Name == "vless") {
			t.Errorf("%q 的 RealityTransports = %v，只有 vless 该非空", spec.Name, spec.RealityTransports)
		}
	}

	// 传输层/安全层归一只有 hysteria（hysteria+tls）与 wireguard（空）。
	normalized := map[string][2]string{}
	for i := range inboundProtocolTable {
		spec := &inboundProtocolTable[i]
		if spec.Normalize {
			normalized[spec.Name] = [2]string{spec.ForcedNetwork, spec.ForcedSecurity}
		}
	}
	if len(normalized) != 2 {
		t.Fatalf("需要归一的协议 = %v，期望只有 hysteria / wireguard", normalized)
	}
	if got := normalized["hysteria"]; got != [2]string{"hysteria", "tls"} {
		t.Errorf("hysteria 归一 = %v，期望 hysteria+tls", got)
	}
	if got := normalized["wireguard"]; got != [2]string{"", "none"} {
		t.Errorf("wireguard 归一 = %v，期望空传输层 + none", got)
	}

	// 不写 streamSettings.network/security 的只有 wireguard；列表页显示 udp 的也只有它。
	for i := range inboundProtocolTable {
		spec := &inboundProtocolTable[i]
		if spec.Stream == (spec.Name == "wireguard") {
			t.Errorf("%q 的 Stream = %v（只有 wireguard 该为 false）", spec.Name, spec.Stream)
		}
		if (spec.PresentNetwork != "") != (spec.Name == "wireguard") {
			t.Errorf("%q 的 PresentNetwork = %q（只有 wireguard 该为 udp）", spec.Name, spec.PresentNetwork)
		}
	}

	// 查不到的协议走兜底行：空 settings + 常规 stream（等价改动前 switch 的 default）。
	if spec := inboundProtocolOf("amneziawg"); spec.Name != "amneziawg" || !spec.Stream || spec.Build == nil {
		t.Errorf("未知协议兜底行 = %+v，期望「常规 stream + 空 settings 渲染入口」", spec)
	}
}

// TestProtocolRegistryRequiredFields 表里声明的必填项必须真的被拦住。
//
// 每条必填项在对应协议的正常用例上删掉，期望拿到一条业务报错（而不是建成功）。
// 这是「声明与实现一致」的机器检查：以后谁把校验删了，这里会红。
func TestProtocolRegistryRequiredFields(t *testing.T) {
	// 探针用例：取金样本里的正常用例（ss 用传统 AEAD 那条 —— 2022 多用户会自动生成密钥）。
	probe := map[string]string{
		"vless":       "vless-plain",
		"vmess":       "vmess-ws",
		"trojan":      "trojan-tls",
		"shadowsocks": "shadowsocks-legacy",
		"hysteria":    "hysteria-basic",
		"socks":       "socks",
		"http":        "http",
		"tunnel":      "tunnel-udp",
		"wireguard":   "wireguard-edit",
	}
	// casesByName 每次返回一份全新的用例（payload 里的字段会被删，不能复用同一份）。
	casesByName := func() map[string]protocolCase {
		out := map[string]protocolCase{}
		for _, tc := range protocolCases() {
			out[tc.name] = tc
		}
		return out
	}

	d := NewNodeDomain(NodeDeps{Store: newProtocolStore()})
	gin.SetMode(gin.TestMode)

	for i := range inboundProtocolTable {
		spec := &inboundProtocolTable[i]
		if len(spec.Required) == 0 {
			continue
		}
		// 先确认探针本身能建成功，否则「删字段失败」说明不了任何事。
		base, ok := casesByName()[probe[spec.Name]]
		if !ok {
			t.Fatalf("%q 的探针用例 %q 不存在", spec.Name, probe[spec.Name])
		}
		if !buildOK(t, d, base) {
			t.Fatalf("%q 的探针用例 %q 本身建不成功，探针无效", spec.Name, base.name)
		}
		for _, field := range spec.Required {
			tc := casesByName()[probe[spec.Name]]
			delete(tc.input, field)
			if buildOK(t, d, tc) {
				t.Errorf("%q 的必填项 %q 删掉后仍然建成功（表里声明了 Required，实现没拦）", spec.Name, field)
			}
		}
	}
}

// buildOK 跑一次 validate + build，返回是否都通过。
func buildOK(t *testing.T, d *NodeDomain, tc protocolCase) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/nodes/42/xray-inbounds", nil)
	if !d.validateInboundPayload(c, tc.input) {
		return false
	}
	_, ok := d.buildInboundInput(c, protocolGoldenNode(), tc.input, tc.existing)
	return ok
}

// TestProtocolRegistryGoldenCoversAllProtocols 金样本必须覆盖表里的每个协议：
// 少一个协议的正常用例，那个协议的输出就没被锁住。
func TestProtocolRegistryGoldenCoversAllProtocols(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range protocolCases() {
		covered[tc.input["protocol"].(string)] = true
	}
	for i := range inboundProtocolTable {
		name := inboundProtocolTable[i].Name
		if !covered[name] {
			t.Errorf("金样本没有 %q 的用例（协议输出没被锁住）", name)
		}
	}
	for name := range covered {
		if _, ok := lookupInboundProtocol(name); !ok && name != "amneziawg" {
			t.Errorf("金样本里的 %q 不在能力表里，也不是故意的未知协议用例", name)
		}
	}
}
