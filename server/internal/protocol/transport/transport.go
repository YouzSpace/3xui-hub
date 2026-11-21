// Package transport 是协议流传输层「network → streamSettings 传输段」的公共实现。
//
// 当前从 handler/admin 的出站 stream 组装中抽出（ws / xhttp / httpupgrade / tcp-header）。
// 参数用 map[string]any 直读，刻意不依赖任何包私有类型，供各协议包与后续调用方复用。
// 行为与抽取前逐字节一致（由 handler/admin 的出站 stream 特征测试守护）。
package transport

import (
	"strings"

	"controlhub-server/internal/phpjson"
)

// Apply 按 network 组装传输段并写入 stream（ws/xhttp/httpupgrade/tcp-header）。
// 无匹配 network 时不写任何键。input 的键名与面板表单一致（ws_path/ws_host/…）。
func Apply(stream *phpjson.Object, network string, input map[string]any) {
	switch network {
	case "ws":
		ws := phpjson.New()
		path := str(input, "ws_path")
		if path == "" {
			path = "/"
		}
		ws.Put("path", path)
		if host := str(input, "ws_host"); host != "" {
			ws.Put("headers", phpjson.New("Host", host))
		}
		stream.Put("wsSettings", ws)
	case "xhttp":
		stream.Put("xhttpSettings", XHTTPSettings(input))
	case "httpupgrade":
		hu := phpjson.New()
		path := str(input, "httpupgrade_path")
		if path == "" {
			path = "/"
		}
		hu.Put("path", path)
		if host := str(input, "httpupgrade_host"); host != "" {
			hu.Put("host", host)
		}
		stream.Put("httpupgradeSettings", hu)
	}

	if network == "tcp" && str(input, "tcp_header_type") == "http" {
		reqPath := str(input, "tcp_header_path")
		if reqPath == "" {
			reqPath = "/"
		}
		request := phpjson.New("version", "1.1", "method", "GET", "path", phpjson.Array{reqPath})
		if host := str(input, "tcp_header_host"); host != "" {
			request.Put("headers", phpjson.New("Host", phpjson.Array{host}))
		}
		stream.Put("tcpSettings", phpjson.New("header", phpjson.New("type", "http", "request", request)))
	}
}

// XHTTPSettings 组装 xhttpSettings 段（出站用）。入站侧的 xhttp 组装另有更复杂逻辑，未并入此函数。
func XHTTPSettings(input map[string]any) *phpjson.Object {
	x := phpjson.New()
	path := str(input, "xhttp_path")
	if path == "" {
		path = "/"
	}
	x.Put("path", path)
	if mode := str(input, "xhttp_mode"); mode != "" {
		x.Put("mode", mode)
	}
	if host := str(input, "xhttp_host"); host != "" {
		x.Put("host", host)
	}
	return x
}

func str(input map[string]any, key string) string {
	if v, ok := input[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
