package admin

// 回归测试：面板 input 里的数字是 jsonNumber（jsonInput 用 UseNumber + convertNumbers），
// 而 internal/protocol/transport 只认原生类型。PR #149 把 xhttp/传输段委托给 transport 后，
// 不经过 transportFriendlyInput 就会静默丢字段 —— 这里用真实请求形态锁住。

import (
	"testing"
)

// TestXHTTPSettingsKeepsNumericInput 数字形式提交的 xhttp 高级字段必须落库。
func TestXHTTPSettingsKeepsNumericInput(t *testing.T) {
	in := map[string]any{
		"xhttp_path":                  "/x",
		"xhttp_mode":                  "auto",
		"xhttp_padding_bytes":         jsonNumber("1024"),
		"xhttp_padding_obfs_mode":     jsonNumber("1"),
		"xhttp_sc_max_buffered_posts": jsonNumber("100"),
		"xhttp_session_id_length":     jsonNumber("8"),
		"xhttp_uplink_http_method":    jsonNumber("123"),
	}
	got := string(phpEncode(xhttpSettings(in)))
	want := `{"path":"\/x","mode":"auto","xPaddingBytes":"1024","sessionIDLength":"8","uplinkHTTPMethod":"123","xPaddingObfsMode":true,"scMaxBufferedPosts":100}`
	if got != want {
		t.Fatalf("xhttp 数字字段丢失\n got=%s\nwant=%s", got, want)
	}
}

// TestOutboundStreamKeepsNumericInput 出站 stream 的传输段同样要吃数字输入。
func TestOutboundStreamKeepsNumericInput(t *testing.T) {
	d := &NodeDomain{}
	c := zzCtx()
	in := map[string]any{
		"network":                     "xhttp",
		"security":                    "none",
		"xhttp_path":                  "/s",
		"xhttp_session_id_length":     jsonNumber("8"),
		"xhttp_sc_max_buffered_posts": jsonNumber("64"),
	}
	got, ok := d.buildOutboundStream(c, &NodeRecord{}, "vless", in, nil)
	if !ok {
		t.Fatalf("buildOutboundStream 失败")
	}
	want := `{"network":"xhttp","security":"none","xhttpSettings":{"path":"\/s","mode":"auto","sessionIDLength":"8","scMaxBufferedPosts":64}}`
	if string(got) != want {
		t.Fatalf("出站 stream 数字字段丢失\n got=%s\nwant=%s", got, want)
	}
}
