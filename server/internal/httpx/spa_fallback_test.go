package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// newTestFrontend 用内存 fs 构造与 internal/web.Handler 同构的前端处理器：
// 命中文件发文件，否则回 index.html。判定逻辑在 web 包里，这里只复刻「回 index.html」
// 的响应特征（Content-Type: text/html），用来验证 SPAFallbackNotFound 的改写规则。
func newTestFrontend(t *testing.T) http.Handler {
	t.Helper()
	fsys := fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><div id=app></div>")},
		"assets/index-a1.js": {Data: []byte("console.log(1)")},
	}
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" || p == "/admin/users" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte("<!doctype html><div id=app></div>"))
			return
		}
		if _, err := fsys.Open(trimSlash(p)); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// 文件不存在：web.Handler 的行为是回 index.html
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte("<!doctype html><div id=app></div>"))
	})
}

func trimSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}

func TestSPAFallbackNotFound(t *testing.T) {
	h := SPAFallbackNotFound(newTestFrontend(t))

	cases := []struct {
		path     string
		status   int
		ctype    string
		bodyLeft string
	}{
		{"/robots.txt", http.StatusNotFound, "text/plain; charset=utf-8", ""},
		{"/backup.zip", http.StatusNotFound, "text/plain; charset=utf-8", ""},
		{"/.env", http.StatusNotFound, "text/plain; charset=utf-8", ""},
		// 真实存在的静态资源照常返回
		{"/assets/index-a1.js", http.StatusOK, "", "console.log(1)"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if w.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.path, w.Code, tc.status)
			continue
		}
		if tc.ctype != "" && w.Header().Get("Content-Type") != tc.ctype {
			t.Errorf("%s: Content-Type = %q, want %q", tc.path, w.Header().Get("Content-Type"), tc.ctype)
		}
		if tc.bodyLeft == "" && w.Body.Len() > 64 {
			t.Errorf("%s: 404 响应体不应再带 index.html（len=%d）", tc.path, w.Body.Len())
		}
		if tc.bodyLeft != "" && w.Body.String() != tc.bodyLeft {
			t.Errorf("%s: body = %q, want %q", tc.path, w.Body.String(), tc.bodyLeft)
		}
	}
}

// TestSPAFallbackNotFoundHEAD HEAD 请求同样要回 404，且不带正文。
func TestSPAFallbackNotFoundHEAD(t *testing.T) {
	h := SPAFallbackNotFound(newTestFrontend(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/robots.txt", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 不应带正文，实际 len=%d", w.Body.Len())
	}
}
