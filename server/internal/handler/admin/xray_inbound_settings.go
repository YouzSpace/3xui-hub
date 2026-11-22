package admin

// 入站 settings 的「协议 → 构造器」委托层（协议扩展点收敛，issue #137）。
//
// 每协议入站 settings 的实际装配已迁入 internal/protocol/<proto>/inbound.go
// （单一真相源），本文件只把面板侧的 inboundBuild 适配成 protocol.InboundCtx：
// 读字段、取缺省、带副作用的业务动作（写失败响应 / SS-2022 生成密钥 /
// WireGuard 生成密钥对）都从这里以回调/数据注入，protocol 包不碰 gin、DB、driver。
//
// 空导入各协议子包以触发 init() 注册（构造器在 protocol.BuildInboundSettings 分发）。

import (
	p "controlhub-server/internal/protocol"
	_ "controlhub-server/internal/protocol/http"
	_ "controlhub-server/internal/protocol/hysteria"
	_ "controlhub-server/internal/protocol/shadowsocks"
	_ "controlhub-server/internal/protocol/socks"
	_ "controlhub-server/internal/protocol/trojan"
	_ "controlhub-server/internal/protocol/tunnel"
	_ "controlhub-server/internal/protocol/vless"
	_ "controlhub-server/internal/protocol/vmess"
	_ "controlhub-server/internal/protocol/wireguard"

	"controlhub-server/internal/driver/xray"
)

// buildInboundSettings 调 protocol 的入站构造器（Build 字段的实现）。
// handled=false 时回退为「空 settings」，与改动前 switch 的 default 分支等价。
func buildInboundSettings(b *inboundBuild) (*jsObj, bool) {
	out, ok, handled := p.BuildInboundSettings(b.spec.Name, b.protoCtx())
	if !handled {
		return jo(), true
	}
	if !ok {
		return nil, false
	}
	if out == nil {
		return jo(), true
	}
	parsed, err := parseOrderedJS(p.Encode(out))
	if err != nil {
		return jo(), true
	}
	if o, ok := parsed.(*jsObj); ok {
		return o, true
	}
	return jo(), true
}

// protoCtx 把 inboundBuild 适配成 protocol.InboundCtx。
func (b *inboundBuild) protoCtx() *p.InboundCtx {
	return &p.InboundCtx{
		Input: func(key string) string { return nodeStringField(b.input, key) },
		Int: func(key string) (int64, bool) {
			v, present := b.input[key]
			if !present || v == nil {
				return 0, false
			}
			return inputInt(v), true
		},
		DefaultStr:   b.spec.defaultString,
		DefaultInt:   b.spec.defaultInt,
		Prev:         jsToProtocol(b.prevSettings()),
		PrevProtocol: b.prevProtocol(),
		Fail:         b.fail,
		FailServer:   b.failServer,
		GenWireGuardKey: func() (string, error) {
			kp, err := xray.WireGuardKeypair()
			if err != nil {
				return "", err
			}
			return kp[0], nil
		},
	}
}

// prevProtocol 返回编辑前记录的协议名（新建返回空串）。
func (b *inboundBuild) prevProtocol() string {
	if b.existing == nil {
		return ""
	}
	return b.existing.Protocol
}

// jsToProtocol 把 admin 的有序对象/数组递归转成 protocol 的同构类型，
// 供 protocol.InboundCtx.Prev 使用（protocol 的 Get/PrevStr 只认 *protocol.Object）。
// 标量（string/bool/整数/json.Number/float）原样透传，两边口径一致。
func jsToProtocol(v any) any {
	switch t := v.(type) {
	case *jsObj:
		if t == nil {
			return nil
		}
		o := p.New()
		for i, k := range t.keys {
			o.Put(k, jsToProtocol(t.vals[i]))
		}
		return o
	case jsArr:
		arr := make(p.Array, len(t))
		for i, item := range t {
			arr[i] = jsToProtocol(item)
		}
		return arr
	}
	return v
}
