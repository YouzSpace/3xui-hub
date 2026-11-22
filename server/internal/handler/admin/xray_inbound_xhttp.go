package admin

// xhttp 传输的 settings 组装、header 行解析，以及入站 tag 建议/清洗。
// 从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	prototrans "controlhub-server/internal/protocol/transport"
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
// xhttpSettings 组装入站 xhttp settings —— 委托 internal/protocol/transport（单一真相源），
// 转换回 admin 的 jsObj（protocol.Object 与 admin jsObj 是不同类型，用 parseOrderedJS 桥接）。
func xhttpSettings(input map[string]any) *jsObj {
	parsed, err := parseOrderedJS(prototrans.EncodeXHTTP(transportFriendlyInput(input)))
	if err != nil {
		return jo()
	}
	if o, ok := parsed.(*jsObj); ok {
		return o
	}
	return jo()
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
