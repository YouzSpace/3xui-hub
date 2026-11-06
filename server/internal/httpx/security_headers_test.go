package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSecurityHeaders 逐条断言四个强制头 + Report-Only CSP 的值。
func TestSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	// NoRoute 也要带上头（真正的前端路由 / 404 都走这里）
	r.NoRoute(func(c *gin.Context) {
		if IsFrontendPath(c.Request.URL.Path) {
			c.String(http.StatusOK, "<html></html>")
			return
		}
		c.String(http.StatusNotFound, "404")
	})

	cases := []struct {
		path   string
		status int
	}{
		{"/", http.StatusOK},
		{"/admin/users", http.StatusOK},
		{"/robots.txt", http.StatusNotFound},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.ServeHTTP(w, req)

		if w.Code != tc.status {
			t.Fatalf("%s: status = %d, want %d", tc.path, w.Code, tc.status)
		}
		for _, kv := range FixedHeaders {
			if got := w.Header().Get(kv[0]); got != kv[1] {
				t.Errorf("%s: %s = %q, want %q", tc.path, kv[0], got, kv[1])
			}
		}
		if got := w.Header().Get(headerCSPReportOnly); got != CSPReportOnly {
			t.Errorf("%s: %s = %q, want %q", tc.path, headerCSPReportOnly, got, CSPReportOnly)
		}
		// 强制 CSP 不能出现（拍板：只上 Report-Only）
		if got := w.Header().Get("Content-Security-Policy"); got != "" {
			t.Errorf("%s: 不应下发强制 CSP，实际 %q", tc.path, got)
		}
	}
}
