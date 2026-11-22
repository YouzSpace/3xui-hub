package protocol_test

import (
	"strings"
	"testing"

	p "controlhub-server/internal/protocol"

	// 空导入触发各协议 init() 注册（含 BuildInbound）。
	_ "controlhub-server/internal/protocol/http"
	_ "controlhub-server/internal/protocol/hysteria"
	_ "controlhub-server/internal/protocol/shadowsocks"
	_ "controlhub-server/internal/protocol/socks"
	_ "controlhub-server/internal/protocol/trojan"
	_ "controlhub-server/internal/protocol/tunnel"
	_ "controlhub-server/internal/protocol/vless"
	_ "controlhub-server/internal/protocol/vmess"
	_ "controlhub-server/internal/protocol/wireguard"
)

// newCtx 造一个最小 InboundCtx：Input 从 map 读，缺省/整数走简单实现，Fail 记录文案。
func newCtx(in map[string]string, def map[string]string) (*p.InboundCtx, *string) {
	var failed string
	return &p.InboundCtx{
		Input:      func(k string) string { return in[k] },
		Int:        func(k string) (int64, bool) { v, ok := in[k]; if !ok { return 0, false }; return atoi(v), true },
		DefaultStr: func(k string) string { return def[k] },
		DefaultInt: func(k string) int64 { return atoi(def[k]) },
		Fail:       func(msg string) { failed = msg },
		FailServer: func(string, error) {},
	}, &failed
}

func atoi(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// TestBuildInboundDispatch 锁定分发契约：注册协议 handled=true 且自动填 Protocol；
// vless 产出带 Spec 缺省的 decryption。
func TestBuildInboundDispatch(t *testing.T) {
	ctx, _ := newCtx(nil, map[string]string{"decryption": "none"})
	out, ok, handled := p.BuildInboundSettings("vless", ctx)
	if !handled || !ok {
		t.Fatalf("vless 应 handled 且 ok，got handled=%v ok=%v", handled, ok)
	}
	if ctx.Protocol != "vless" {
		t.Fatalf("dispatcher 未填 Protocol，got %q", ctx.Protocol)
	}
	if got := string(p.Encode(out)); got != `{"decryption":"none"}` {
		t.Fatalf("vless 入站 settings 不符：%s", got)
	}
}

// TestBuildInboundUnregistered 未注册协议返回 handled=false（调用方兜底）。
func TestBuildInboundUnregistered(t *testing.T) {
	ctx, _ := newCtx(nil, nil)
	_, _, handled := p.BuildInboundSettings("nope", ctx)
	if handled {
		t.Fatal("未注册协议应 handled=false")
	}
}

// TestBuildInboundSocksRequiresAuth 缺凭据时通过 Fail 回调写文案并返回 false。
func TestBuildInboundSocksRequiresAuth(t *testing.T) {
	ctx, failed := newCtx(map[string]string{}, nil)
	_, ok, handled := p.BuildInboundSettings("socks", ctx)
	if !handled || ok {
		t.Fatalf("socks 缺凭据应 handled=true ok=false，got handled=%v ok=%v", handled, ok)
	}
	if !strings.Contains(*failed, "用户名与密码") {
		t.Fatalf("失败文案不符：%q", *failed)
	}
}
