package httpx

// 把前端处理器的 SPA fallback 变成真实 404。
//
// 背景：web.Handler 的语义是「命中文件发文件，否则回 index.html」，于是
// /robots.txt、/.env、/backup.zip、/.git/config 这类扫描器请求也拿到 200 + index.html，
// 真实 404 被吞掉（外部评估据此判「站点在掩盖路径存在性」）。
//
// 但路由层没法直接问「dist 里有没有这个文件」——产物由 go:embed 打进二进制，
// 文件系统只有 internal/web 知道。所以这里不猜，用结果反推：对「不属于前端路由」的
// 路径调用前端处理器，如果它最终回的是 index.html（text/html），说明这个文件不存在，
// 改写成真实 404；如果回的是真实文件（js/css/png/svg…），照常透传。
//
// 判定点放在 WriteHeader：web.Handler 先设好 Content-Type 再写状态码，
// 因此不需要缓冲响应体，大文件也不会被整套读进内存。

import (
	"net/http"
	"strings"
)

// indexContentTypePrefix 是前端处理器回 index.html 时设置的 Content-Type 前缀。
// 只有它会被改写成 404；静态资源不会命中。
const indexContentTypePrefix = "text/html"

// SPAFallbackNotFound 包装前端处理器：把「index.html 兜底」改写成 404，其余不动。
//
// 调用方必须已经排除了真正的前端路由（见 IsFrontendPath）——本函数不区分路径，
// 只要前端处理器回了 index.html 就认为是「文件不存在」。
func SPAFallbackNotFound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := &spaNotFoundWriter{ResponseWriter: w, method: r.Method}
		next.ServeHTTP(g, r)
	})
}

// spaNotFoundWriter 在 WriteHeader 时判定是否改写为 404。
//
// 一旦改写：立刻向底层写 404 与一段纯文本，并吞掉处理器后续写入的 index.html 正文
// （Write 返回 len(b), nil，让处理器以为写成功，不会因为 error 走别的分支）。
type spaNotFoundWriter struct {
	http.ResponseWriter
	method   string
	decided  bool // 已经处理过 WriteHeader（含隐式 200）
	notFound bool // 已改写为 404，后续 Write 一律吞掉
}

func (w *spaNotFoundWriter) WriteHeader(code int) {
	if !w.decided {
		w.decided = true
		// 只改写成功返回的 index.html；5xx / 404 / 304 等保持原样。
		if code == http.StatusOK &&
			strings.HasPrefix(w.Header().Get("Content-Type"), indexContentTypePrefix) {
			w.notFound = true
			w.writeNotFound()
			return
		}
	}
	if w.notFound {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *spaNotFoundWriter) Write(b []byte) (int, error) {
	if w.notFound {
		// 正文已替换成 404 页，这里只吞掉 index.html 的字节。
		return len(b), nil
	}
	if !w.decided {
		w.WriteHeader(http.StatusOK)
		// WriteHeader 里可能已经把它改写成了 404，此时正文要吞掉，不能继续往下写。
		if w.notFound {
			return len(b), nil
		}
	}
	return w.ResponseWriter.Write(b)
}

// writeNotFound 用真实 404 覆盖掉 index.html 的响应头与正文。
func (w *spaNotFoundWriter) writeNotFound() {
	h := w.Header()
	// index.html 分支设过 no-cache，以及 ServeContent 可能设的 Last-Modified/Content-Length，
	// 都清掉，避免出现「404 + 长假缓存」或长度对不上的怪异响应。
	h.Del("Cache-Control")
	h.Del("Last-Modified")
	h.Del("Content-Length")
	// ServeContent 会给 index.html 带上 Accept-Ranges，404 上留着是无意义的多余头。
	h.Del("Accept-Ranges")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.ResponseWriter.WriteHeader(http.StatusNotFound)
	if w.method != http.MethodHead {
		_, _ = w.ResponseWriter.Write([]byte("404 page not found\n"))
	}
}
