package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"controlhub-server/internal/config"
	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// probeClientIP 用 main.applyProxyTrust 真实配置一次引擎，再发一条
// RemoteAddr=remote、带 X-Forwarded-For=xff 的请求，返回 ClientIP()。
func probeClientIP(t *testing.T, trustedEnv, remote, xff string) string {
	t.Helper()
	t.Setenv("TRUSTED_PROXIES", trustedEnv)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })
	applyProxyTrust(r, func(string, ...any) {})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remote
	req.Header.Set("X-Forwarded-For", xff)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Body.String()
}

// 默认名单（本机回环）下，本机反代递上来的 XFF 被采信 —— nginx 就是这么告诉我们真实 IP 的。
func TestProxyTrust_DefaultTrustsLoopbackXFF(t *testing.T) {
	if got := probeClientIP(t, "", "127.0.0.1:5000", "8.8.8.8"); got != "8.8.8.8" {
		t.Fatalf("ClientIP = %q, want 8.8.8.8（本机反代应当被信任）", got)
	}
}

// 非法 TRUSTED_PROXIES 必须退回「不信任任何代理头」，而不是退回全信任。
func TestProxyTrust_InvalidEnvFallsBackToNoTrust(t *testing.T) {
	if got := probeClientIP(t, "not-an-ip", "127.0.0.1:5000", "8.8.8.8"); got != "127.0.0.1" {
		t.Fatalf("ClientIP = %q, want 127.0.0.1（配错时必须忽略转发头）", got)
	}
}

// 0.0.0.0/0 等价于 gin 出厂默认的全信任，必须当配错处理。
func TestProxyTrust_OpenCIDRTreatedAsMisconfig(t *testing.T) {
	if got := probeClientIP(t, "127.0.0.1,0.0.0.0/0", "127.0.0.1:5000", "8.8.8.8"); got != "127.0.0.1" {
		t.Fatalf("ClientIP = %q, want 127.0.0.1（0.0.0.0/0 不应被接受）", got)
	}
}

// 非回环来源即使带 XFF 也不采信。
func TestProxyTrust_PublicPeerIgnoresXFF(t *testing.T) {
	if got := probeClientIP(t, "", "203.0.113.9:5000", "8.8.8.8"); got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want 203.0.113.9", got)
	}
}

// 走真实的 newRouter（applyProxyTrust 在最前），验证两条路径：
//   - 直连来源不在可信名单（非回环）→ 伪造的 X-Forwarded-For 一律不采信；
//   - 直连来源是本机反代（回环）→ 采信 nginx 写入的 XFF，这正是限流拿到真实客户端 IP 的前提。
func TestClientIP_RealRouter(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")

	gin.SetMode(gin.TestMode)
	r, _, _ := newRouter(httpx.NewApp(nil, newAcceptanceConfig()), nil, nil)

	var got string
	r.GET("/__clientip", func(c *gin.Context) { got = c.ClientIP() })

	probe := func(remote string) string {
		got = ""
		req := httptest.NewRequest(http.MethodGet, "/__clientip", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "8.8.8.8")
		r.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	// 非回环直连（公网直打 8080 的那类流量）：伪造头必须被丢弃。
	if v := probe("203.0.113.9:54321"); v != "203.0.113.9" {
		t.Fatalf("非可信来源 ClientIP = %q, want 203.0.113.9（伪造的 XFF 不得替换真实来源）", v)
	} else {
		t.Logf("RemoteAddr=203.0.113.9  X-Forwarded-For=8.8.8.8  =>  ClientIP = %q", v)
	}

	// 本机反代：nginx 写入的 XFF 必须被采信，否则全站限流会塌缩成一个 IP。
	if v := probe("127.0.0.1:54321"); v != "8.8.8.8" {
		t.Fatalf("本机反代 ClientIP = %q, want 8.8.8.8（nginx 传来的真实 IP 必须采信）", v)
	} else {
		t.Logf("RemoteAddr=127.0.0.1  X-Forwarded-For=8.8.8.8  =>  ClientIP = %q（nginx 路径）", v)
	}
}

// newAcceptanceConfig 给 newRouter 一份最小可用配置（不走数据库）。
func newAcceptanceConfig() *config.Config {
	return &config.Config{AppEnv: "test", BcryptRounds: 4}
}

// TRUSTED_PROXIES 覆盖默认名单后，应按新名单判定可信来源。
func TestProxyTrust_EnvOverride(t *testing.T) {
	if got := probeClientIP(t, "203.0.113.0/24", "203.0.113.9:5000", "8.8.8.8"); got != "8.8.8.8" {
		t.Fatalf("ClientIP = %q, want 8.8.8.8（名单内来源应采信 XFF）", got)
	}
	if got := probeClientIP(t, "203.0.113.0/24", "127.0.0.1:5000", "8.8.8.8"); got != "127.0.0.1" {
		t.Fatalf("ClientIP = %q, want 127.0.0.1（覆盖后回环不再默认可信）", got)
	}
}
