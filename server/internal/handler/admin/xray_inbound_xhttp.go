package admin

// xhttp 传输的 settings 组装、header 行解析，以及入站 tag 建议/清洗。
// 从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// suggestInboundTag 对齐 suggestTag()：协议-安全-传输，冲突自动加序号。
func (d *NodeDomain) suggestInboundTag(c *gin.Context, node *NodeRecord, protocol, security, network string, excludeID *uint64) (string, bool) {
	base := protocol
	switch {
	case security == "reality":
		base += "-reality"
	case security == "tls" && network != "tcp":
		base += "-ws-tls"
	case network != "tcp" && network != "":
		base += "-" + network
	}
	base = sanitizeTagChars(base)
	if base == "" {
		base = "inbound"
	}

	existing, err := d.store.XrayInboundTags(ctxOf(c), node.ID, excludeID)
	if err != nil {
		d.failServer(c, "查询入站标识", err)
		return "", false
	}
	taken := map[string]bool{}
	for _, tag := range existing {
		taken[tag] = true
	}

	tag := base
	for i := 2; taken[tag]; i++ {
		tag = base + "-" + strconv.Itoa(i)
	}
	return tag, true
}

// sanitizeTagChars 复刻 preg_replace('/[^A-Za-z0-9_-]/', '', $base)。
func sanitizeTagChars(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_', ch == '-':
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// ---- xhttp ----

// xhttpStringFields 是 xray-core 键名 ⇒ 表单字段（入站与出站共用同一张表）。
var xhttpStringFields = [][2]string{
	{"xPaddingBytes", "xhttp_padding_bytes"},
	{"xPaddingKey", "xhttp_padding_key"},
	{"xPaddingHeader", "xhttp_padding_header"},
	{"xPaddingPlacement", "xhttp_padding_placement"},
	{"xPaddingMethod", "xhttp_padding_method"},
	{"sessionIDPlacement", "xhttp_session_id_placement"},
	{"sessionIDKey", "xhttp_session_id_key"},
	{"sessionIDTable", "xhttp_session_id_table"},
	{"sessionIDLength", "xhttp_session_id_length"},
	{"seqPlacement", "xhttp_seq_placement"},
	{"seqKey", "xhttp_seq_key"},
	{"uplinkDataPlacement", "xhttp_uplink_data_placement"},
	{"uplinkDataKey", "xhttp_uplink_data_key"},
	{"scMaxEachPostBytes", "xhttp_sc_max_each_post_bytes"},
	{"scMinPostsIntervalMs", "xhttp_sc_min_posts_interval_ms"},
	{"scStreamUpServerSecs", "xhttp_sc_stream_up_server_secs"},
	{"uplinkHTTPMethod", "xhttp_uplink_http_method"},
}

var xhttpBoolFields = [][2]string{
	{"xPaddingObfsMode", "xhttp_padding_obfs_mode"},
	{"noSSEHeader", "xhttp_no_sse_header"},
	{"noGRPCHeader", "xhttp_no_grpc_header"},
}

var xhttpIntFields = [][2]string{
	{"scMaxBufferedPosts", "xhttp_sc_max_buffered_posts"},
	{"serverMaxHeaderBytes", "xhttp_server_max_header_bytes"},
	{"uplinkChunkSize", "xhttp_uplink_chunk_size"},
}

// xhttpSettings 对齐 xhttpSettings()：空值/关/零不落库（未配置即内核默认）。
func xhttpSettings(input map[string]any) *jsObj {
	path := nodeStringField(input, "xhttp_path")
	if path == "" {
		path = "/"
	}
	mode := nodeStringField(input, "xhttp_mode")
	if mode == "" {
		mode = "auto"
	}
	// PHP: array_filter(['path'=>..., 'host'=>..., 'mode'=>...], fn($v) => $v !== '')
	// → 键序恒为 path → host → mode，空值只是被删掉，不会挪位。
	s := jo("path", path)
	if host := nodeStringField(input, "xhttp_host"); host != "" {
		s.put("host", host)
	}
	s.put("mode", mode)

	for _, pair := range xhttpStringFields {
		if v := strings.TrimSpace(nodeStringField(input, pair[1])); v != "" {
			s.put(pair[0], v)
		}
	}
	for _, pair := range xhttpBoolFields {
		if inputBool(input[pair[1]]) {
			s.put(pair[0], true)
		}
	}
	for _, pair := range xhttpIntFields {
		if v := inputInt(input[pair[1]]); v > 0 {
			s.put(pair[0], v)
		}
	}
	if headers := parseHeaderLines(nodeStringField(input, "xhttp_headers")); headers != nil {
		s.put("headers", headers)
	}

	xmux := jo()
	for _, pair := range [][2]string{
		{"maxConcurrency", "xhttp_xmux_max_concurrency"},
		{"maxConnections", "xhttp_xmux_max_connections"},
		{"cMaxReuseTimes", "xhttp_xmux_c_max_reuse_times"},
		{"hMaxRequestTimes", "xhttp_xmux_h_max_request_times"},
		{"hMaxReusableSecs", "xhttp_xmux_h_max_reusable_secs"},
		{"hKeepAlivePeriod", "xhttp_xmux_h_keep_alive_period"},
	} {
		if v := strings.TrimSpace(nodeStringField(input, pair[1])); v != "" {
			xmux.put(pair[0], v)
		}
	}
	if len(xmux.keys) > 0 {
		s.put("xmux", xmux)
	}
	return s
}

// parseHeaderLines 对齐 parseHeaderLines()："Key: Value" 行文本 → header map。
func parseHeaderLines(text string) *jsObj {
	out := jo()
	for _, line := range splitLines(text) {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}
		idx := strings.Index(line, ":")
		k := strings.TrimSpace(line[:idx])
		if k == "" {
			continue
		}
		out.put(k, strings.TrimSpace(line[idx+1:]))
	}
	if len(out.keys) == 0 {
		return nil
	}
	return out
}

// headerLinesFrom 对齐 headerLinesFrom()：header map → "Key: Value" 行文本（值数组取首个）。
func headerLinesFrom(raw any) string {
	obj, ok := raw.(*jsObj)
	if !ok {
		return ""
	}
	lines := make([]string, 0, len(obj.keys))
	for i, k := range obj.keys {
		val := obj.vals[i]
		if arr, isArr := val.(jsArr); isArr {
			first := ""
			if len(arr) > 0 {
				first = nodeScalarString(arr[0])
			}
			lines = append(lines, k+": "+first)
			continue
		}
		lines = append(lines, k+": "+nodeScalarString(val))
	}
	return strings.Join(lines, "\n")
}

// splitLines 复刻 preg_split('/\r\n|\r|\n/', $text)。
func splitLines(text string) []string {
	if text == "" {
		return []string{""}
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	return strings.Split(normalized, "\n")
}

// ---- 展示 ----
