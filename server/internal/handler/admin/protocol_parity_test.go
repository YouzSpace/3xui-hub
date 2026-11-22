package admin

// 防漂移测试：PR #149 把协议实现搬进了 internal/protocol，但能力表（Transports/Securities/
// Required/Defaults 等）在 admin 的 inboundProtocolTable 与 protocol.Spec 各存了一份。
// 目前只有 admin 那份被消费；两份一旦不一致，维护者会以为改 spec.go 生效了（实际没有），
// 或者改 admin 表后 spec.go 变成假文档。这个测试把两份钉在一起。
//
// 另外锁住「有序 JSON 两份实现字节一致」：admin.phpEncode 与 protocol.Encode 是独立实现
// （protocol 包不依赖 admin，只能复制），落库字节必须完全相同。

import (
	"encoding/json"
	"reflect"
	"testing"

	p "controlhub-server/internal/protocol"
)

func TestProtocolSpecMatchesAdminTable(t *testing.T) {
	for i := range inboundProtocolTable {
		spec := &inboundProtocolTable[i]
		reg, ok := p.Lookup(spec.Name)
		if !ok {
			t.Errorf("协议 %q 在 inboundProtocolTable 里但 protocol 注册表没有（空导入漏了？）", spec.Name)
			continue
		}
		if got, want := reg.Transports, spec.Transports; !reflect.DeepEqual(got, want) {
			t.Errorf("%s.Transports 不一致: protocol=%v admin=%v", spec.Name, got, want)
		}
		if got, want := reg.Securities, spec.Securities; !reflect.DeepEqual(got, want) {
			t.Errorf("%s.Securities 不一致: protocol=%v admin=%v", spec.Name, got, want)
		}
		if got, want := reg.RealityTransports, spec.RealityTransports; !reflect.DeepEqual(got, want) {
			t.Errorf("%s.RealityTransports 不一致: protocol=%v admin=%v", spec.Name, got, want)
		}
		if reg.RejectMessage != spec.RejectMessage {
			t.Errorf("%s.RejectMessage 不一致: protocol=%q admin=%q", spec.Name, reg.RejectMessage, spec.RejectMessage)
		}
		if reg.Normalize != spec.Normalize {
			t.Errorf("%s.Normalize 不一致: protocol=%v admin=%v", spec.Name, reg.Normalize, spec.Normalize)
		}
		if reg.ForcedNetwork != spec.ForcedNetwork || reg.ForcedSecurity != spec.ForcedSecurity {
			t.Errorf("%s.Forced 不一致: protocol=%q/%q admin=%q/%q",
				spec.Name, reg.ForcedNetwork, reg.ForcedSecurity, spec.ForcedNetwork, spec.ForcedSecurity)
		}
		if reg.Stream != spec.Stream {
			t.Errorf("%s.Stream 不一致: protocol=%v admin=%v", spec.Name, reg.Stream, spec.Stream)
		}
		if reg.PresentNetwork != spec.PresentNetwork {
			t.Errorf("%s.PresentNetwork 不一致: protocol=%q admin=%q", spec.Name, reg.PresentNetwork, spec.PresentNetwork)
		}
		if got, want := reg.Required, spec.Required; !reflect.DeepEqual(got, want) {
			t.Errorf("%s.Required 不一致: protocol=%v admin=%v", spec.Name, got, want)
		}
		if len(reg.Defaults) != len(spec.Defaults) {
			t.Errorf("%s.Defaults 数量不一致: protocol=%d admin=%d", spec.Name, len(reg.Defaults), len(spec.Defaults))
		} else {
			for j := range reg.Defaults {
				if reg.Defaults[j].Key != spec.Defaults[j].Key ||
					!reflect.DeepEqual(reg.Defaults[j].Value, spec.Defaults[j].Value) {
					t.Errorf("%s.Defaults[%d] 不一致: protocol=%s=%v admin=%s=%v",
						spec.Name, j, reg.Defaults[j].Key, reg.Defaults[j].Value,
						spec.Defaults[j].Key, spec.Defaults[j].Value)
				}
			}
		}
	}
}

// TestEncodeParity 锁定 admin.phpEncode 与 protocol.Encode 的字节一致。
// 出站 settings 由 protocol.Encode 直接产出并落库，两边一旦不同就是落库字节变化。
func TestEncodeParity(t *testing.T) {
	cases := []any{
		jo("ascii", "plain", "中文", "香港节点-01", "quote", `a"b\c/d`, "ctrl", "a\tb\nc\rd\x01e", "emoji", "🚀 rocket"),
		jo("num_int", int64(1420), "num_float", 4.5, "num_float2", 4.0, "bool_t", true, "bool_f", false, "nilv", nil),
		jo("arr", jsArr{"x", int64(1), true, nil, jo("k", "v")}, "nested", jo("a", jo("b", jsArr{jo("c", "中")}))),
		jo("pct", "100%", "pqv", "pqv+value/="),
		jo("mtu", json.Number("1420"), "port", json.Number("443")),
		jo(),
		jsArr{},
	}
	for i, v := range cases {
		got := string(phpEncode(v))
		want := string(p.Encode(jsToProtocol(v)))
		if got != want {
			t.Errorf("case %d 字节不一致\n admin=%s\n proto=%s", i, got, want)
		}
	}
}
