// Package xhttp 是 xhttp 传输的 streamSettings 段构造（入站/出站共用同一张字段表）。
package xhttp

import (
	"strings"

	p "controlhub-server/internal/protocol"
	tu "controlhub-server/internal/protocol/transport/util"
)

var stringFields = [][2]string{
	{"xPaddingBytes", "xhttp_padding_bytes"}, {"xPaddingKey", "xhttp_padding_key"},
	{"xPaddingHeader", "xhttp_padding_header"}, {"xPaddingPlacement", "xhttp_padding_placement"},
	{"xPaddingMethod", "xhttp_padding_method"}, {"sessionIDPlacement", "xhttp_session_id_placement"},
	{"sessionIDKey", "xhttp_session_id_key"}, {"sessionIDTable", "xhttp_session_id_table"},
	{"sessionIDLength", "xhttp_session_id_length"}, {"seqPlacement", "xhttp_seq_placement"},
	{"seqKey", "xhttp_seq_key"}, {"uplinkDataPlacement", "xhttp_uplink_data_placement"},
	{"uplinkDataKey", "xhttp_uplink_data_key"}, {"scMaxEachPostBytes", "xhttp_sc_max_each_post_bytes"},
	{"scMinPostsIntervalMs", "xhttp_sc_min_posts_interval_ms"}, {"scStreamUpServerSecs", "xhttp_sc_stream_up_server_secs"},
	{"uplinkHTTPMethod", "xhttp_uplink_http_method"},
}
var boolFields = [][2]string{{"xPaddingObfsMode", "xhttp_padding_obfs_mode"}, {"noSSEHeader", "xhttp_no_sse_header"}, {"noGRPCHeader", "xhttp_no_grpc_header"}}
var intFields = [][2]string{{"scMaxBufferedPosts", "xhttp_sc_max_buffered_posts"}, {"serverMaxHeaderBytes", "xhttp_server_max_header_bytes"}, {"uplinkChunkSize", "xhttp_uplink_chunk_size"}}

// Settings 组装 xhttpSettings 对象（入站/出站共用）。
func Settings(input map[string]any) *p.Object {
	path := tu.OrSlash(tu.Str(input, "xhttp_path"))
	mode := tu.Str(input, "xhttp_mode")
	if mode == "" {
		mode = "auto"
	}
	o := p.New("path", path)
	if h := tu.Str(input, "xhttp_host"); h != "" {
		o.Put("host", h)
	}
	o.Put("mode", mode)
	for _, x := range stringFields {
		if v := tu.Str(input, x[1]); v != "" {
			o.Put(x[0], v)
		}
	}
	for _, x := range boolFields {
		if tu.Bool(input, x[1]) {
			o.Put(x[0], true)
		}
	}
	for _, x := range intFields {
		if v := tu.Int(input, x[1]); v > 0 {
			o.Put(x[0], v)
		}
	}
	if h := ParseHeaderLines(tu.Str(input, "xhttp_headers")); h != nil {
		o.Put("headers", h)
	}
	xmux := p.New()
	for _, x := range [][2]string{{"maxConcurrency", "xhttp_xmux_max_concurrency"}, {"maxConnections", "xhttp_xmux_max_connections"}, {"cMaxReuseTimes", "xhttp_xmux_c_max_reuse_times"}, {"hMaxRequestTimes", "xhttp_xmux_h_max_request_times"}, {"hMaxReusableSecs", "xhttp_xmux_h_max_reusable_secs"}, {"hKeepAlivePeriod", "xhttp_xmux_h_keep_alive_period"}} {
		if v := tu.Str(input, x[1]); v != "" {
			xmux.Put(x[0], v)
		}
	}
	if len(xmux.Keys()) > 0 {
		o.Put("xmux", xmux)
	}
	return o
}

// Build 返回 xhttpSettings 段 JSON。
func Build(input map[string]any) []byte { return p.Encode(p.New("xhttpSettings", Settings(input))) }

// ParseHeaderLines "Key: Value" 行 → header 对象（无 → nil）。
func ParseHeaderLines(text string) *p.Object {
	out := p.New()
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, ":")
		if line == "" || i < 0 {
			continue
		}
		if k := strings.TrimSpace(line[:i]); k != "" {
			out.Put(k, strings.TrimSpace(line[i+1:]))
		}
	}
	if len(out.Keys()) == 0 {
		return nil
	}
	return out
}
