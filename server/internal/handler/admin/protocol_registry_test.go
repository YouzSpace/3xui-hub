package admin

// 协议能力表的基准（golden）测试（issue #137 协议扩展点收敛）。
//
// 这不是「新功能测试」，是**行为冻结**用的：把「协议 → 支持项 / 必填项 / 缺省值 /
// 渲染入口」收敛进 protocol_registry.go 的过程中，任何一个字节的输出变化都必须红灯。
// 断言范围是每个协议的完整链路：
//
//	validateInboundPayload（校验通过的规则白名单 + 报错文案）
//	→ buildInboundInput（settings / stream_settings / listen / tag / 密钥落库形态）
//	→ presentXrayInbound（列表/编辑弹窗的回显字段与顺序）
//
// 金样本怎么来的：改造前先用同一份用例跑一遍
// `go test ./internal/handler/admin -run TestProtocolRegistryGolden -update-protocol-golden`，
// 把当时的输出原样落进 testdata/protocol_inbound.golden；之后运行时不带 -update 就是只读比对。
//
// 维护方式：这些是**行为锁定**用例，断言的是当前语义。只有**有意**修改某个协议的输出时
// 才重录金样本，并说明原因。

import (
	"context"
	"database/sql"
	"encoding/base64"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

var updateProtocolGolden = flag.Bool("update-protocol-golden", false, "重录协议金样本")

// ---- 桩 store ----

// protocolStore 只实现构造入站真正调用到的方法：
//   - 标识/端口占用：恒不冲突（这里不测唯一性，测的是协议分支）；
//   - 证书库：XrayCertificates 恒空（让 hysteria 自动挑证书走「库为空」文案），
//     XrayCertificatePEM 给一条 id=5 / 域名 example.com 的证书（TLS 与 HY2 引用它）。
type protocolStore struct {
	NodeStore
	pem map[uint64]*XrayCertificatePEM
}

func newProtocolStore() *protocolStore {
	domain := "example.com"
	notAfter := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	return &protocolStore{pem: map[uint64]*XrayCertificatePEM{
		5: {
			ID:       5,
			Name:     "example.com",
			Domain:   &domain,
			CertPEM:  "-----BEGIN CERTIFICATE-----\nTUlJQg==\n-----END CERTIFICATE-----",
			KeyPEM:   "-----BEGIN PRIVATE KEY-----\nTUlJQg==\n-----END PRIVATE KEY-----",
			NotAfter: &notAfter,
		},
	}}
}

func (s *protocolStore) XrayInboundTagTaken(context.Context, uint64, string, *uint64) (bool, error) {
	return false, nil
}

func (s *protocolStore) XrayInboundPortTaken(context.Context, uint64, int64, []string, *uint64) (bool, error) {
	return false, nil
}

func (s *protocolStore) XrayCertificates(context.Context) ([]XrayCertificate, error) {
	return nil, nil
}

func (s *protocolStore) XrayCertificatePEM(_ context.Context, id uint64) (*XrayCertificatePEM, error) {
	if c, ok := s.pem[id]; ok {
		return c, nil
	}
	return nil, sql.ErrNoRows
}

// ---- 用例 ----

// wireGuardKey 生成一把合法的 32 字节 base64 密钥（WireGuard 公钥形状）。
func wireGuardKey(fill byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = fill
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// ss2022Key 生成一把合法的 SS-2022 服务端 PSK（aes-256 要求 32 字节）。
func ss2022Key(fill byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = fill
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// inboundInput 是所有用例共用的底稿（字段取值固定，避免随机/时间带来的漂移）。
func inboundInput(protocol string) map[string]any {
	return map[string]any{
		"protocol":    protocol,
		"tag":         "in-" + protocol,
		"port":        8443,
		"remark":      "备注",
		"listen":      "0.0.0.0",
		"client_host": "cdn.example.com",
		"sniffing": map[string]any{
			"enabled":      true,
			"destOverride": []any{"http", "tls"},
			"routeOnly":    true,
		},
		"enabled": true,
		"sort":    3,
	}
}

// protocolCase 是一条用例：payload（含可选 existing 表示「编辑已有入站」）。
//
// 新建 wireguard / 新建 reality 会现场生成随机密钥，金样本只能用编辑路径锁
// （这些用例都带 existing），随机那一段不在金样本范围内。
type protocolCase struct {
	name     string
	input    map[string]any
	existing *XrayInboundRecord
}

func protocolCases() []protocolCase {
	cases := []protocolCase{}

	// ---- vless ----
	in := inboundInput("vless")
	cases = append(cases, protocolCase{name: "vless-plain", input: in})

	in = inboundInput("vless")
	in["security"] = "reality"
	in["network"] = "tcp"
	in["private_key"] = "REALITY-PRIVATE-KEY-FIXED"
	in["public_key"] = "REALITY-PUBLIC-KEY-FIXED"
	in["server_names"] = []any{"a.example.com", "*.b.example.com", " "}
	in["short_ids"] = []any{"ab12", "CDEF"}
	in["dest"] = "www.cloudflare.com:443"
	in["show"] = true
	in["xver"] = 1
	in["min_client_ver"] = "1.8.0"
	in["max_client_ver"] = "26.3.27"
	in["max_timediff"] = 30000
	in["fingerprint"] = "chrome"
	in["spider_x"] = "/"
	in["master_key_log"] = "/tmp/vless.log"
	in["limit_fallback_upload"] = map[string]any{"after_bytes": 1024, "bytes_per_sec": 2097152, "burst_bytes_per_sec": 4194304}
	cases = append(cases, protocolCase{name: "vless-reality", input: in})

	in = inboundInput("vless")
	in["security"] = "tls"
	in["network"] = "ws"
	in["tls_cert_id"] = 5
	in["tls_server_name"] = "example.com"
	in["ws_path"] = "/ws"
	in["ws_host"] = "cdn.example.com"
	cases = append(cases, protocolCase{name: "vless-tls-ws", input: in})

	in = inboundInput("vless")
	in["network"] = "xhttp"
	in["xhttp_path"] = "/xhttp"
	in["xhttp_host"] = "cdn.example.com"
	in["xhttp_mode"] = "auto"
	in["xhttp_padding_bytes"] = "100-1000"
	in["xhttp_padding_obfs_mode"] = true
	in["xhttp_padding_key"] = "pad-key"
	in["xhttp_padding_header"] = "X-Padding"
	in["xhttp_padding_placement"] = "header"
	in["xhttp_padding_method"] = "repeat-x"
	in["xhttp_session_id_placement"] = "path"
	in["xhttp_session_id_key"] = "sid"
	in["xhttp_session_id_table"] = "hex"
	in["xhttp_session_id_length"] = "16"
	in["xhttp_seq_placement"] = "query"
	in["xhttp_seq_key"] = "seq"
	in["xhttp_uplink_data_placement"] = "body"
	in["xhttp_uplink_data_key"] = "data"
	in["xhttp_uplink_chunk_size"] = 4096
	in["xhttp_sc_max_each_post_bytes"] = "1000-2000"
	in["xhttp_sc_max_buffered_posts"] = 100
	in["xhttp_sc_min_posts_interval_ms"] = "10-20"
	in["xhttp_sc_stream_up_server_secs"] = "20-40"
	in["xhttp_server_max_header_bytes"] = 8192
	in["xhttp_uplink_http_method"] = "PUT"
	in["xhttp_no_sse_header"] = true
	in["xhttp_no_grpc_header"] = true
	in["xhttp_headers"] = "X-A: 1"
	in["xhttp_xmux_max_concurrency"] = "8-16"
	in["xhttp_xmux_max_connections"] = "4"
	in["xhttp_xmux_c_max_reuse_times"] = "128"
	in["xhttp_xmux_h_max_request_times"] = "600-900"
	in["xhttp_xmux_h_max_reusable_secs"] = "1800-3000"
	in["xhttp_xmux_h_keep_alive_period"] = "30"
	cases = append(cases, protocolCase{name: "vless-xhttp", input: in})

	in = inboundInput("vless")
	in["network"] = "httpupgrade"
	in["httpupgrade_path"] = "/hu"
	in["httpupgrade_host"] = "cdn.example.com"
	cases = append(cases, protocolCase{name: "vless-httpupgrade", input: in})

	in = inboundInput("vless")
	in["network"] = "tcp"
	in["tcp_header_type"] = "http"
	in["tcp_header_path"] = "/raw"
	in["tcp_header_host"] = "www.example.com"
	cases = append(cases, protocolCase{name: "vless-tcp-header", input: in})

	// ---- vmess / trojan ----
	in = inboundInput("vmess")
	in["network"] = "ws"
	in["ws_path"] = "/vm"
	cases = append(cases, protocolCase{name: "vmess-ws", input: in})

	in = inboundInput("trojan")
	in["security"] = "tls"
	in["tls_cert_id"] = 5
	in["tls_server_name"] = "example.com"
	in["network"] = "grpc"
	cases = append(cases, protocolCase{name: "trojan-tls", input: in})

	// ---- shadowsocks ----
	in = inboundInput("shadowsocks")
	in["ss_method"] = "2022-blake3-aes-256-gcm"
	in["ss_password"] = ss2022Key(7)
	cases = append(cases, protocolCase{name: "shadowsocks-2022", input: in})

	in = inboundInput("shadowsocks")
	in["ss_method"] = "aes-256-gcm"
	in["ss_password"] = "legacy-pass"
	cases = append(cases, protocolCase{name: "shadowsocks-legacy", input: in})

	// 编辑时留空 = 沿用原值（2022 的多用户形态）。密钥必须是该 method 的合法长度
	// （aes-256 = 32 字节），否则面板会现场重新生成一把 → 金样本就不稳定了。
	in = inboundInput("shadowsocks")
	in["ss_method"] = "2022-blake3-aes-256-gcm"
	cases = append(cases, protocolCase{
		name:  "shadowsocks-edit-inherit-password",
		input: in,
		existing: &XrayInboundRecord{
			ID:       11,
			NodeID:   42,
			Tag:      "in-shadowsocks",
			Protocol: "shadowsocks",
			Port:     8443,
			Settings: []byte(`{"method":"2022-blake3-aes-256-gcm","password":"` + ss2022Key(9) + `"}`),
		},
	})

	// ---- hysteria ----
	in = inboundInput("hysteria")
	in["tls_cert_id"] = 5
	in["tls_server_name"] = "example.com"
	cases = append(cases, protocolCase{name: "hysteria-basic", input: in})

	in = inboundInput("hysteria")
	in["tls_cert_id"] = 5
	in["hy_hop_ports"] = "20000-30000"
	in["hy_hop_interval"] = "30-60"
	in["hy_udp_idle_timeout"] = 0 // 0 = 用内核默认：归一成「不带这个字段」
	in["hy_masquerade_type"] = "string"
	in["hy_masquerade_content"] = "<h1>hi</h1>"
	in["hy_masquerade_status_code"] = 200
	in["hy_congestion"] = "brutal"
	in["hy_brutal_up"] = 100
	in["hy_brutal_down"] = 50
	in["hy_init_stream_window"] = 16384
	in["hy_max_stream_window"] = 8388608
	in["hy_init_conn_window"] = 32768
	in["hy_max_conn_window"] = 16777216
	in["hy_reject_unknown_sni"] = true
	in["hy_session_resumption"] = true
	in["hy_salamander_password"] = "sal-pass"
	cases = append(cases, protocolCase{name: "hysteria-full", input: in})

	in = inboundInput("hysteria")
	in["tls_cert_id"] = 5
	in["hy_masquerade_type"] = "proxy"
	in["hy_masquerade_url"] = "https://example.com/"
	in["hy_congestion"] = "bbr"
	cases = append(cases, protocolCase{name: "hysteria-masquerade-proxy", input: in})

	// ---- socks / http ----
	in = inboundInput("socks")
	in["auth_user"] = "socks-user"
	in["auth_pass"] = "socks-pass"
	cases = append(cases, protocolCase{name: "socks", input: in})

	in = inboundInput("http")
	in["auth_user"] = "http-user"
	in["auth_pass"] = "http-pass"
	cases = append(cases, protocolCase{name: "http", input: in})

	// 编辑时密码留空 = 沿用原值。
	in = inboundInput("socks")
	in["auth_user"] = "socks-user"
	cases = append(cases, protocolCase{
		name:  "socks-edit-inherit-pass",
		input: in,
		existing: &XrayInboundRecord{
			ID:       12,
			NodeID:   42,
			Tag:      "in-socks",
			Protocol: "socks",
			Port:     8443,
			Settings: []byte(`{"auth":"password","accounts":[{"user":"old","pass":"old-pass"}],"udp":false}`),
		},
	})

	// ---- tunnel ----
	in = inboundInput("tunnel")
	in["tunnel_address"] = "127.0.0.1"
	in["tunnel_port"] = 8080
	in["tunnel_network"] = "udp"
	cases = append(cases, protocolCase{name: "tunnel-udp", input: in})

	in = inboundInput("tunnel")
	in["tunnel_address"] = "10.0.0.2"
	in["tunnel_port"] = 443
	cases = append(cases, protocolCase{name: "tunnel-default-network", input: in})

	// ---- wireguard ----
	in = inboundInput("wireguard")
	in["wg_peers"] = wireGuardKey(1) + " 10.0.0.2/32,fd00::2/128\n" + wireGuardKey(2)
	in["wg_mtu"] = 1400
	cases = append(cases, protocolCase{
		name:  "wireguard-edit",
		input: in,
		existing: &XrayInboundRecord{
			ID:       13,
			NodeID:   42,
			Tag:      "in-wireguard",
			Protocol: "wireguard",
			Port:     8443,
			Settings: []byte(`{"secretKey":"` + wireGuardKey(3) + `","mtu":1420,"noKernelTun":true}`),
		},
	})

	// ---- 失败路径（文案也是行为的一部分，同样入金样本）----
	in = inboundInput("amneziawg")
	cases = append(cases, protocolCase{name: "fail-unknown-protocol", input: in})

	in = inboundInput("vless")
	in["network"] = "kcp"
	cases = append(cases, protocolCase{name: "fail-unknown-network", input: in})

	in = inboundInput("vmess")
	in["security"] = "reality"
	cases = append(cases, protocolCase{name: "fail-reality-on-vmess", input: in})

	in = inboundInput("vless")
	in["security"] = "reality"
	in["network"] = "ws"
	in["private_key"] = "REALITY-PRIVATE-KEY-FIXED"
	in["short_ids"] = []any{"ab12"}
	cases = append(cases, protocolCase{name: "fail-reality-on-ws", input: in})

	in = inboundInput("vless")
	in["security"] = "reality"
	in["private_key"] = "REALITY-PRIVATE-KEY-FIXED"
	in["short_ids"] = []any{"ab12"}
	in["mldsa65_seed"] = "seed-only"
	cases = append(cases, protocolCase{name: "fail-mldsa-not-paired", input: in})

	in = inboundInput("shadowsocks")
	in["network"] = "ws"
	cases = append(cases, protocolCase{name: "fail-shadowsocks-ws", input: in})

	in = inboundInput("shadowsocks")
	in["ss_method"] = "aes-256-gcm"
	cases = append(cases, protocolCase{name: "fail-shadowsocks-no-password", input: in})

	in = inboundInput("vless")
	in["security"] = "tls"
	cases = append(cases, protocolCase{name: "fail-tls-without-cert", input: in})

	in = inboundInput("vless")
	in["security"] = "tls"
	in["tls_cert_id"] = 5
	in["tls_server_name"] = "other.example.com"
	cases = append(cases, protocolCase{name: "fail-tls-cert-domain-mismatch", input: in})

	in = inboundInput("hysteria")
	cases = append(cases, protocolCase{name: "fail-hysteria-no-cert", input: in})

	in = inboundInput("hysteria")
	in["tls_cert_id"] = 5
	in["hy_hop_ports"] = "8000-9000"
	cases = append(cases, protocolCase{name: "fail-hysteria-hop-covers-main", input: in})

	in = inboundInput("hysteria")
	in["tls_cert_id"] = 5
	in["hy_congestion"] = "force-brutal"
	cases = append(cases, protocolCase{name: "fail-hysteria-force-brutal-no-up", input: in})

	in = inboundInput("hysteria")
	in["tls_cert_id"] = 5
	in["hy_init_stream_window"] = 4096
	cases = append(cases, protocolCase{name: "fail-hysteria-window-too-small", input: in})

	in = inboundInput("socks")
	in["auth_user"] = "only-user"
	cases = append(cases, protocolCase{name: "fail-socks-no-pass", input: in})

	in = inboundInput("tunnel")
	in["tunnel_port"] = 8080
	cases = append(cases, protocolCase{name: "fail-tunnel-no-address", input: in})

	in = inboundInput("tunnel")
	in["tunnel_address"] = "127.0.0.1"
	in["tunnel_port"] = 8080
	in["tunnel_network"] = "icmp"
	cases = append(cases, protocolCase{name: "fail-tunnel-bad-network", input: in})

	in = inboundInput("wireguard")
	in["wg_peers"] = "not-a-key 10.0.0.2/32"
	cases = append(cases, protocolCase{
		name:  "fail-wireguard-bad-peers",
		input: in,
		existing: &XrayInboundRecord{
			ID:       14,
			NodeID:   42,
			Tag:      "in-wireguard",
			Protocol: "wireguard",
			Port:     8443,
			Settings: []byte(`{"secretKey":"` + wireGuardKey(3) + `","mtu":1420,"noKernelTun":true}`),
		},
	})

	return cases
}

// ---- 执行 ----

func protocolGoldenNode() *NodeRecord {
	return &NodeRecord{ID: 42, Name: "MY-TEST", Host: "node.example.com", DriverType: "xray"}
}

// runProtocolCase 跑一条用例并返回可逐字节比对的文本快照。
func runProtocolCase(t *testing.T, d *NodeDomain, tc protocolCase) string {
	t.Helper()
	var out strings.Builder
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/nodes/42/xray-inbounds", nil)

	if !d.validateInboundPayload(c, tc.input) {
		fmt.Fprintf(&out, "validate=fail\nbody=%s\n", rec.Body.String())
		return out.String()
	}
	out.WriteString("validate=ok\n")

	rec.Body.Reset()
	in, ok := d.buildInboundInput(c, protocolGoldenNode(), tc.input, tc.existing)
	if !ok {
		fmt.Fprintf(&out, "build=fail\nbody=%s\n", rec.Body.String())
		return out.String()
	}

	fmt.Fprintf(&out, "tag=%s\nport=%d\nlisten=%s\n", in.Tag, in.Port, in.Listen)
	fmt.Fprintf(&out, "settings=%s\n", in.Settings)
	fmt.Fprintf(&out, "stream=%s\n", in.StreamSettings)
	fmt.Fprintf(&out, "sniffing=%s\n", in.Sniffing)
	fmt.Fprintf(&out, "reality_private_key=%s\n", ptrText(in.RealityPrivateKey))
	fmt.Fprintf(&out, "salamander_password=%s\n", ptrText(in.SalamanderPassword))
	fmt.Fprintf(&out, "client_host=%s\n", ptrText(in.ClientHost))

	// 回显：模拟落库后的一行（id / 创建时间是库给的，这里固定）。
	createdAt := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	record := &XrayInboundRecord{
		ID:                 7,
		NodeID:             in.NodeID,
		Tag:                in.Tag,
		Remark:             in.Remark,
		Protocol:           in.Protocol,
		Port:               in.Port,
		Listen:             in.Listen,
		ClientHost:         in.ClientHost,
		Settings:           in.Settings,
		StreamSettings:     in.StreamSettings,
		Sniffing:           in.Sniffing,
		RealityPrivateKey:  in.RealityPrivateKey,
		SalamanderPassword: in.SalamanderPassword,
		CreatedAt:          &createdAt,
	}
	if in.Enabled != nil {
		record.Enabled = *in.Enabled
	}
	if in.Sort != nil {
		record.Sort = *in.Sort
	}
	counts := map[string]int64{"vless": 3, "hysteria": 105, "shadowsocks": 12}
	fmt.Fprintf(&out, "present=%s\n", phpEncode(presentXrayInbound(record, counts)))
	return out.String()
}

// snapshotProtocolInbounds 拼出全部用例的快照（金样本内容）。
func snapshotProtocolInbounds(t *testing.T, d *NodeDomain) string {
	t.Helper()
	var b strings.Builder
	for _, tc := range protocolCases() {
		b.WriteString("=== " + tc.name + " ===\n")
		b.WriteString(runProtocolCase(t, d, tc))
		b.WriteString("\n")
	}
	return b.String()
}

// TestProtocolRegistryGolden 逐字节锁定「校验 → 组装 → 回显」三段输出。
func TestProtocolRegistryGolden(t *testing.T) {
	d := NewNodeDomain(NodeDeps{Store: newProtocolStore()})
	got := snapshotProtocolInbounds(t, d)

	path := filepath.Join("testdata", "protocol_inbound.golden")
	if *updateProtocolGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("建 testdata 目录失败: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("写金样本失败: %v", err)
		}
		t.Logf("已重录金样本 %s（%d 字节）", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读金样本失败（首次请带 -update-protocol-golden 生成）: %v", err)
	}
	if got == string(want) {
		return
	}
	t.Fatalf("协议输出与金样本不一致（第一处差异）:\n%s", firstDiff(string(want), got))
}

// firstDiff 定位第一处行级差异，比整段 diff 好读。
func firstDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) && i < len(gotLines); i++ {
		if wantLines[i] != gotLines[i] {
			return fmt.Sprintf("第 %d 行:\n  金样本: %s\n  当前值: %s", i+1, wantLines[i], gotLines[i])
		}
	}
	return fmt.Sprintf("行数不同：金样本 %d 行，当前 %d 行", len(wantLines), len(gotLines))
}

// ptrText 把可选字符串渲染成快照文本。
func ptrText(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
