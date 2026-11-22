package subscription

// 端到端接线测试：User-Agent → resolveFormat → 渲染格式。
// 用注入的 generator 截获最终 format，不依赖数据库与节点数据。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func serveShowUA(t *testing.T, h *Handler, token, query, ua string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r)
	url := "/api/sub/" + token
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestShowFormatFromUserAgent(t *testing.T) {
	repo := &fakeRepo{
		configs: map[string]string{"sub_clash_enabled": "1", "sub_singbox_enabled": "1"},
		users:   map[string]*User{"tok": testUser(t, "tok")},
	}
	h := &Handler{Service: NewService(repo), Repo: repo, now: time.Now}

	var seen Format
	h.generator = func(_ context.Context, _ *User, f Format) (generateOutcome, error) {
		seen = f
		return generateOutcome{body: "body"}, nil
	}

	cases := []struct {
		name       string
		ua         string
		query      string
		wantFormat Format
		wantCT     string
	}{
		{"clash-verge-ua", "Clash-Verge-Rev/2.5.2", "", FormatClash, "text/yaml; charset=UTF-8"},
		{"flclash-ua", "FlClash/0.8.85", "", FormatClash, "text/yaml; charset=UTF-8"},
		{"singbox-ua", "SFA/1.10.1 (Linux; Android 14)", "", FormatSingbox, "application/json; charset=UTF-8"},
		{"browser-ua", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "", FormatBase64, "text/plain; charset=UTF-8"},
		{"explicit-clash-beats-singbox-ua", "SFA/1.10.1", "clash", FormatClash, "text/yaml; charset=UTF-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen = ""
			w := serveShowUA(t, h, "tok", c.query, c.ua)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d, body=%s", w.Code, w.Body.String())
			}
			if seen != c.wantFormat {
				t.Fatalf("format=%q，期望 %q", seen, c.wantFormat)
			}
			if got := w.Header().Get("Content-Type"); got != c.wantCT {
				t.Fatalf("Content-Type=%q，期望 %q", got, c.wantCT)
			}
		})
	}
}

// TestShowFormatUAFallbackWhenDisabled：管理员关掉 clash 格式时，
// Clash UA 必须回落 base64，而不是 400。
func TestShowFormatUAFallbackWhenDisabled(t *testing.T) {
	repo := &fakeRepo{
		configs: map[string]string{"sub_clash_enabled": "0", "sub_singbox_enabled": "0"},
		users:   map[string]*User{"tok": testUser(t, "tok")},
	}
	h := &Handler{Service: NewService(repo), Repo: repo, now: time.Now}
	var seen Format
	h.generator = func(_ context.Context, _ *User, f Format) (generateOutcome, error) {
		seen = f
		return generateOutcome{body: "body"}, nil
	}

	w := serveShowUA(t, h, "tok", "", "Clash-Verge-Rev/2.5.2")
	if w.Code != http.StatusOK {
		t.Fatalf("关掉 clash 时 Clash UA 不应报错，HTTP %d body=%s", w.Code, w.Body.String())
	}
	if seen != FormatBase64 {
		t.Fatalf("应回落 base64，got=%q", seen)
	}

	// 显式 ?clash 保持原语义：未启用 → 400。
	w = serveShowUA(t, h, "tok", "clash", "Mozilla/5.0")
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}
	if code := w.Header().Get("X-CH-Code"); code != "400" {
		t.Fatalf("显式 ?clash 未启用应返回 X-CH-Code=400，got=%q body=%s", code, w.Body.String())
	}
}
