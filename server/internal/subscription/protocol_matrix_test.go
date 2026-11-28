package subscription

// 协议 × 传输 × 安全层 的全组合矩阵测试。
//
// 目的：把「入站能配出来的所有组合」穷举一遍，锁死三件事：
//   1. 订阅链接（vless / vmess / trojan / ss / hysteria2）必须带全传输层与安全层参数；
//   2. 节点侧渲染（nodeapi）对 flow 的口径必须与链接一致（vless + tcp + reality/tls）；
//   3. 链接转 Clash / sing-box 后关键字段不丢（sing-box 明确不支持 xhttp，属已知跳过）。
//
// 组合数量按 internal/protocol/*/spec.go 的 Transports × Securities 展开：
// vless 13（tcp/ws/grpc/xhttp/httpupgrade × none/tls/reality，reality 仅 tcp/grpc/xhttp）、
// vmess 10、trojan 10、shadowsocks 2（2022 多用户 / 传统单凭据）、hysteria2 2（带/不带 obfs），
// 另加 vless/vmess/trojan 的 tcp + HTTP 伪装 3 例。

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"controlhub-server/internal/nodeapi"
)

const (
	matrixNodeName = "N1"
	matrixInbound  = "m"
	matrixUUID     = "11111111-2222-3333-4444-555555555555"
	matrixHost     = "1.2.3.4"
	matrixSNI      = "sni.example.com"
	matrixReality  = "www.cloudflare.com"
	// 一个真实可用的 X25519 公钥：假串会让 mihomo 报 "invalid REALITY public key"，
	// 用真串才能拿真实客户端（mihomo -t / sing-box check）验证生成的配置。
	matrixPBK      = "K5vrWEPbeiPCGFTMQGp6PQ8msfE84sjPV7AZJ8uvZHU"
	matrixSID      = "0123456789abcdef"
	matrixSsPass   = "MDEyMzQ1Njc4OWFiY2RlZg=="
	matrixSalamand = "salamander-pass"
)

// matrixCombo 是一个「入站组合」。
type matrixCombo struct {
	name       string // 显示名：协议/传输/安全层
	protocol   string
	transport  string
	security   string
	stream     string
	settings   string
	salamander string // 仅 hysteria：obfs 密码

	expectFlow     bool
	expectClashNet string // Clash network 值（httpupgrade 折成 ws）
	expectClashSkip bool  // mihomo 不支持该协议 × 传输（xhttp 仅 vless；trojan 无 HTTP 伪装）
	expectSboxSkip  bool  // sing-box 不支持该传输（xhttp / TCP HTTP 伪装）
}

// matrixCombos 穷举全部合法组合。
func matrixCombos() []matrixCombo {
	tcpHeaderStream := "{" +
		`"network":"tcp","security":"tls",` +
		`"tlsSettings":{"serverName":"` + matrixSNI + `"},` +
		`"tcpSettings":{"header":{"type":"http","request":{"path":["/tp-path"],"headers":{"Host":["tp.example.com"]}}}}` +
		"}"

	combos := []matrixCombo{
		// tcp + HTTP 伪装（三种协议各一例）：Clash 支持，sing-box 无法忠实表达（跳过）
		{name: "vless/tcp+tls+http", protocol: "vless", transport: "tcp", security: "tls", stream: tcpHeaderStream, settings: vlessSettings(), expectFlow: true, expectClashNet: "http", expectSboxSkip: true},
		{name: "vmess/tcp+tls+http", protocol: "vmess", transport: "tcp", security: "tls", stream: tcpHeaderStream, settings: "{}", expectClashNet: "http", expectSboxSkip: true},
		{name: "trojan/tcp+tls+http", protocol: "trojan", transport: "tcp", security: "tls", stream: tcpHeaderStream, settings: "{}", expectClashNet: "http", expectClashSkip: true, expectSboxSkip: true},
		// shadowsocks
		{name: "shadowsocks/2022", protocol: "shadowsocks", transport: "tcp", security: "none", stream: `{"network":"tcp","security":"none"}`, settings: `{"method":"2022-blake3-aes-128-gcm","password":"` + matrixSsPass + `"}`, expectClashNet: "tcp"},
		{name: "shadowsocks/legacy", protocol: "shadowsocks", transport: "tcp", security: "none", stream: `{"network":"tcp","security":"none"}`, settings: `{"method":"aes-256-gcm","password":"legacy-pass"}`, expectClashNet: "tcp"},
		// hysteria2
		{name: "hysteria/tls", protocol: "hysteria", transport: "hysteria", security: "tls", stream: hysteriaStream(), settings: `{"version":2}`, expectClashNet: "tcp"},
		{name: "hysteria/tls+obfs", protocol: "hysteria", transport: "hysteria", security: "tls", stream: hysteriaStream(), settings: `{"version":2}`, salamander: matrixSalamand, expectClashNet: "tcp"},
	}

	specs := []struct {
		protocol   string
		transports []string
		securities []string
	}{
		{"vless", []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, []string{"none", "tls", "reality"}},
		{"vmess", []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, []string{"none", "tls"}},
		{"trojan", []string{"tcp", "ws", "grpc", "xhttp", "httpupgrade"}, []string{"none", "tls"}},
	}

	for _, spec := range specs {
		for _, transport := range spec.transports {
			for _, security := range spec.securities {
				// reality 仅 vless，且仅 tcp / grpc / xhttp（spec.RealityTransports）
				if security == "reality" && (spec.protocol != "vless" || (transport != "tcp" && transport != "grpc" && transport != "xhttp")) {
					continue
				}
				settings := "{}"
				if spec.protocol == "vless" {
					settings = vlessSettings()
				}
				combos = append(combos, matrixCombo{
					name:           spec.protocol + "/" + transport + "/" + security,
					protocol:       spec.protocol,
					transport:      transport,
					security:       security,
					stream:         matrixStream(transport, security),
					settings:       settings,
					expectFlow:     spec.protocol == "vless" && transport == "tcp" && (security == "tls" || security == "reality"),
					expectClashNet: clashNetworkFor(transport),
					expectClashSkip: transport == "xhttp" && spec.protocol != "vless",
					expectSboxSkip:  transport == "xhttp",
				})
			}
		}
	}
	return combos
}

func vlessSettings() string { return `{"decryption":"none"}` }

func hysteriaStream() string {
	return `{"network":"hysteria","security":"tls","tlsSettings":{"serverName":"` + matrixSNI + `"},` +
		`"hysteriaSettings":{"version":2,"udpHop":{"ports":"20000-30000"}},` +
		`"finalmask":{"quicParams":{"udpHop":{"ports":"20000-30000"}}}}`
}

func matrixStream(transport, security string) string {
	parts := []string{`"network":"` + transport + `"`, `"security":"` + security + `"`}
	switch security {
	case "tls":
		parts = append(parts, `"tlsSettings":{"serverName":"`+matrixSNI+`"}`)
	case "reality":
		parts = append(parts,
			`"realitySettings":{"dest":"`+matrixReality+`:443","serverNames":["`+matrixReality+`"],"publicKey":"`+matrixPBK+`","shortIds":["`+matrixSID+`"],"settings":{"fingerprint":"chrome"}}`)
	}
	switch transport {
	case "ws":
		parts = append(parts, `"wsSettings":{"path":"/ws-path","host":"ws.example.com"}`)
	case "grpc":
		parts = append(parts, `"grpcSettings":{"serviceName":"svc-name"}`)
	case "xhttp":
		parts = append(parts, `"xhttpSettings":{"path":"/xh-path","host":"xh.example.com","mode":"auto","xPaddingBytes":"100-200"}`)
	case "httpupgrade":
		parts = append(parts, `"httpupgradeSettings":{"path":"/hu-path","host":"hu.example.com"}`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func clashNetworkFor(transport string) string {
	switch transport {
	case "httpupgrade":
		return "ws"
	case "hysteria":
		return "tcp"
	default:
		return transport
	}
}

func matrixInboundOf(c matrixCombo) XrayInbound {
	ib := XrayInbound{
		ID: 1, NodeID: 1, Tag: matrixInbound, Protocol: c.protocol, Port: 4432,
		StreamSettings: c.stream, Settings: c.settings, Enabled: true,
	}
	if c.salamander != "" {
		pwd := c.salamander
		ib.SalamanderPassword = pwd
	}
	return ib
}

func matrixLinkOf(t *testing.T, c matrixCombo) string {
	t.Helper()
	node := Node{Name: matrixNodeName, Host: matrixHost}
	user := &User{ID: 7, UUID: matrixUUID}
	link := XrayInboundURI(node, matrixInboundOf(c), user, matrixHost)
	if link == "" {
		t.Fatalf("[%s] 没有生成链接（协议 %s 未被 XrayInboundURI 支持？）", c.name, c.protocol)
	}
	return link
}

// TestProtocolMatrixLinkParams：逐组合核对链接参数（传输层 + 安全层 + flow）。
func TestProtocolMatrixLinkParams(t *testing.T) {
	for _, c := range matrixCombos() {
		t.Run(c.name, func(t *testing.T) {
			link := matrixLinkOf(t, c)
			checkLinkPrefix(t, link, c.protocol)

			switch c.protocol {
			case "vless", "trojan":
				q := mustQuery(t, link)
				if got := q.Get("type"); got != c.transport {
					t.Errorf("type=%q，期望 %q", got, c.transport)
				}
				if c.protocol == "trojan" || c.security != "none" {
					if got := q.Get("security"); got != c.security {
						t.Errorf("security=%q，期望 %q", got, c.security)
					}
				}
				checkTransportQuery(t, c, q)
				checkSecurityQuery(t, c, q)
				if got := q.Has("flow"); got != c.expectFlow {
					t.Errorf("flow 出现=%v，期望 %v（链接: %s）", got, c.expectFlow, link)
				}
				if c.protocol == "trojan" {
					if user := trojanUserOf(link); user != matrixUUID {
						t.Errorf("trojan 密码应为 uuid，实际 %q", user)
					}
				}
			case "vmess":
				doc := mustVmessJSON(t, link)
				if got := fmt.Sprint(doc["net"]); got != c.transport {
					t.Errorf("net=%q，期望 %q", got, c.transport)
				}
				wantTLS := ""
				if c.security == "tls" {
					wantTLS = "tls"
				}
				if got := fmt.Sprint(doc["tls"]); got != wantTLS {
					t.Errorf("tls=%q，期望 %q", got, wantTLS)
				}
				if c.security == "tls" && fmt.Sprint(doc["sni"]) != matrixSNI {
					t.Errorf("sni=%v，期望 %s", doc["sni"], matrixSNI)
				}
				checkVmessTransport(t, c, doc)
			case "shadowsocks":
				if !strings.HasPrefix(link, "ss://") {
					t.Fatalf("ss 链接前缀错误: %s", link)
				}
			case "hysteria":
				u, err := url.Parse(link)
				if err != nil {
					t.Fatalf("hysteria2 链接解析失败: %v", err)
				}
				q := u.Query()
				if q.Get("sni") != matrixSNI {
					t.Errorf("sni=%q，期望 %q", q.Get("sni"), matrixSNI)
				}
				if q.Get("mport") != "20000-30000" {
					t.Errorf("mport=%q，期望 20000-30000", q.Get("mport"))
				}
				if c.salamander != "" {
					if q.Get("obfs") != "salamander" || q.Get("obfs-password") != matrixSalamand {
						t.Errorf("obfs 参数缺失: %s", link)
					}
				}
			}
		})
	}
}

// TestProtocolMatrixServerFlow：节点渲染与链接对 flow 的判断必须一致。
func TestProtocolMatrixServerFlow(t *testing.T) {
	for _, c := range matrixCombos() {
		if c.protocol != "vless" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			link := matrixLinkOf(t, c)
			rendered := renderFlowFor(t, c)
			linkFlow := mustQuery(t, link).Get("flow")

			if c.expectFlow {
				if rendered != xrayFlow {
					t.Errorf("节点渲染 client.flow=%q，期望 %q（服务端会要求 vision）", rendered, xrayFlow)
				}
				if linkFlow != xrayFlow {
					t.Errorf("链接 flow=%q，期望 %q（客户端没开 vision → 连不上）", linkFlow, xrayFlow)
				}
				return
			}
			if rendered != "" {
				t.Errorf("节点渲染不该有 flow，实际 %q", rendered)
			}
			if linkFlow != "" {
				t.Errorf("链接不该有 flow，实际 %q", linkFlow)
			}
		})
	}
}

// TestProtocolMatrixClash：链接 → Clash YAML，关键字段不得丢。
func TestProtocolMatrixClash(t *testing.T) {
	svc := &Service{}
	user := &User{ID: 7, UUID: matrixUUID}
	cfg := &siteConfigReader{values: map[string]string{}}

	for _, c := range matrixCombos() {
		t.Run(c.name, func(t *testing.T) {
			link := matrixLinkOf(t, c)
			out := svc.generateClash(user, nil, []string{link}, cfg)
			if c.expectClashSkip {
				// mihomo 不支持的组合必须整条跳过：写进去会让整份配置加载失败。
				if strings.Contains(out, matrixNodeName+"-"+matrixInbound) {
					t.Fatalf("该组合 mihomo 不支持，应跳过，实际生成了节点\n--- 输出 ---\n%s", out)
				}
				return
			}
			block := clashNodeBlock(t, out, matrixNodeName+"-"+matrixInbound)

			checks := []string{"type: " + clashTypeOf(c.protocol)}
			if c.protocol == "shadowsocks" {
				checks = append(checks, "cipher: ")
			} else if c.protocol != "hysteria" {
				checks = append(checks, "network: "+c.expectClashNet)
			}
			if c.protocol == "vmess" {
				// mihomo 的 vmess 必填 uuid / cipher / alterId；password/encryption 是错键。
				checks = append(checks, "uuid: "+matrixUUID, "cipher: none", "alterId: 0")
				for _, bad := range []string{"password: " + matrixUUID, "encryption: "} {
					if strings.Contains(block, bad) {
						t.Errorf("vmess 出现 mihomo 不认的键 %q\n%s", bad, block)
					}
				}
			}
			switch {
			case c.protocol == "hysteria":
				checks = append(checks, "ports: 20000-30000")
			case c.expectFlow:
				checks = append(checks, "flow: "+xrayFlow)
			}
			if c.security == "tls" || c.security == "reality" {
				checks = append(checks, "tls: true")
				// mihomo：trojan / hysteria2 读 sni，vless / vmess 读 servername。
				if c.protocol == "trojan" || c.protocol == "hysteria" {
					checks = append(checks, "sni: ")
				} else {
					checks = append(checks, "servername: ")
				}
			}
			if c.security == "reality" {
				checks = append(checks, "reality-opts:", "public-key: \""+matrixPBK+"\"", "short-id: \""+matrixSID+"\"")
			}
			if c.salamander != "" {
				checks = append(checks, "obfs: salamander", "obfs-password: "+matrixSalamand)
			}
			for _, want := range checks {
				if !strings.Contains(block, want) {
					t.Errorf("Clash 节点缺 %q\n--- 节点 ---\n%s", want, block)
				}
			}
			checkClashTransport(t, c, block)
		})
	}
}

// TestProtocolMatrixSingbox：链接 → sing-box JSON（xhttp 为已知跳过）。
func TestProtocolMatrixSingbox(t *testing.T) {
	svc := &Service{}
	user := &User{ID: 7, UUID: matrixUUID}
	cfg := &siteConfigReader{values: map[string]string{}}

	for _, c := range matrixCombos() {
		t.Run(c.name, func(t *testing.T) {
			link := matrixLinkOf(t, c)
			out := svc.generateSingbox(user, nil, []string{link}, cfg)

			var doc struct {
				Outbounds []map[string]any `json:"outbounds"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("sing-box 输出不是合法 JSON: %v\n%s", err, out)
			}

			var ob map[string]any
			var hint bool
			for _, o := range doc.Outbounds {
				if fmt.Sprint(o["tag"]) == matrixNodeName+"-"+matrixInbound {
					ob = o
				}
				if strings.Contains(fmt.Sprint(o["tag"]), "不支持Sing-box") {
					hint = true
				}
			}

			if c.expectSboxSkip {
				// sing-box 1.14 没有 xhttp 传输：节点被跳过，只剩提示出站（产品已知行为）。
				if ob != nil {
					t.Errorf("xhttp 组合应被 sing-box 跳过，实际生成了 %v", ob)
				}
				if !hint {
					t.Errorf("全部节点被跳过时应注入提示出站")
				}
				return
			}
			if ob == nil {
				t.Fatalf("sing-box 丢了这条节点\n--- 输出 ---\n%s", out)
			}

			if got := fmt.Sprint(ob["type"]); got != sboxTypeOf(c.protocol) {
				t.Errorf("type=%q，期望 %q", got, sboxTypeOf(c.protocol))
			}
			if c.protocol == "vmess" {
				// sing-box 的 vmess 加密字段叫 security（不是 cipher）。
				if fmt.Sprint(ob["security"]) != "auto" {
					t.Errorf("sing-box vmess security=%v，期望 auto", ob["security"])
				}
				if _, bad := ob["cipher"]; bad {
					t.Errorf("sing-box vmess 出现错键 cipher：%v", ob["cipher"])
				}
			}
			if c.expectFlow && fmt.Sprint(ob["flow"]) != xrayFlow {
				t.Errorf("sing-box flow=%v，期望 %s", ob["flow"], xrayFlow)
			}
			checkSboxTransport(t, c, ob)
			checkSboxSecurity(t, c, ob)
		})
	}
}

// ───────────────────────── 断言辅助 ─────────────────────────

func checkLinkPrefix(t *testing.T, link, protocol string) {
	t.Helper()
	want := map[string]string{"vless": "vless://", "vmess": "vmess://", "trojan": "trojan://", "shadowsocks": "ss://", "hysteria": "hysteria2://"}[protocol]
	if !strings.HasPrefix(link, want) {
		t.Fatalf("链接前缀应为 %s，实际 %s", want, link)
	}
	if protocol == "vmess" {
		doc := mustVmessJSON(t, link)
		if fmt.Sprint(doc["add"]) != matrixHost || fmt.Sprint(doc["port"]) != "4432" {
			t.Errorf("vmess 的 add/port 不对：%v", doc)
		}
		return
	}
	if !strings.Contains(link, matrixHost+":4432") {
		t.Errorf("链接缺少 host:port：%s", link)
	}
}

func mustQuery(t *testing.T, link string) url.Values {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("链接解析失败: %v", err)
	}
	return u.Query()
}

func trojanUserOf(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return u.User.Username()
}

func mustVmessJSON(t *testing.T, link string) map[string]any {
	t.Helper()
	raw, ok := decodeBase64URL(strings.TrimPrefix(link, "vmess://"))
	if !ok {
		t.Fatalf("vmess payload 解不开: %s", link)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("vmess payload 不是 JSON: %v\n%s", err, raw)
	}
	return doc
}

// checkTransportQuery 核对 vless / trojan 的传输层参数。
func checkTransportQuery(t *testing.T, c matrixCombo, q url.Values) {
	t.Helper()
	if strings.Contains(c.name, "+http") {
		if q.Get("headerType") != "http" || q.Get("path") != "/tp-path" || q.Get("host") != "tp.example.com" {
			t.Errorf("tcp HTTP 伪装参数缺失：%v", q)
		}
		return
	}
	switch c.transport {
	case "ws":
		if q.Get("path") != "/ws-path" || q.Get("host") != "ws.example.com" {
			t.Errorf("ws 参数缺失：%v", q)
		}
	case "httpupgrade":
		if q.Get("path") != "/hu-path" || q.Get("host") != "hu.example.com" {
			t.Errorf("httpupgrade 参数缺失：%v", q)
		}
	case "grpc":
		if q.Get("serviceName") != "svc-name" {
			t.Errorf("grpc serviceName 缺失：%v", q)
		}
	case "xhttp":
		if q.Get("path") != "/xh-path" || q.Get("host") != "xh.example.com" || q.Get("mode") != "auto" {
			t.Errorf("xhttp path/host/mode 缺失：%v", q)
		}
		if !strings.Contains(q.Get("extra"), "xPaddingBytes") || q.Get("x_padding_bytes") != "100-200" {
			t.Errorf("xhttp 高级字段（extra/x_padding_bytes）缺失：%v", q)
		}
	}
}

func checkSecurityQuery(t *testing.T, c matrixCombo, q url.Values) {
	t.Helper()
	switch c.security {
	case "tls":
		if q.Get("sni") != matrixSNI {
			t.Errorf("tls sni=%q，期望 %q", q.Get("sni"), matrixSNI)
		}
	case "reality":
		if q.Get("sni") != matrixReality || q.Get("pbk") != matrixPBK || q.Get("sid") != matrixSID {
			t.Errorf("reality 参数缺失：%v", q)
		}
		if q.Get("fp") == "" {
			t.Errorf("reality 缺 fp（uTLS 指纹）")
		}
	}
}

// checkVmessTransport 核对 vmess 分享 JSON 里的传输字段。
func checkVmessTransport(t *testing.T, c matrixCombo, doc map[string]any) {
	t.Helper()
	if strings.Contains(c.name, "+http") {
		if fmt.Sprint(doc["type"]) != "http" || fmt.Sprint(doc["path"]) != "/tp-path" || fmt.Sprint(doc["host"]) != "tp.example.com" {
			t.Errorf("vmess tcp HTTP 伪装字段缺失：%v", doc)
		}
		return
	}
	switch c.transport {
	case "ws":
		if fmt.Sprint(doc["path"]) != "/ws-path" || fmt.Sprint(doc["host"]) != "ws.example.com" {
			t.Errorf("vmess ws 字段缺失：%v", doc)
		}
	case "httpupgrade":
		if fmt.Sprint(doc["path"]) != "/hu-path" || fmt.Sprint(doc["host"]) != "hu.example.com" {
			t.Errorf("vmess httpupgrade 字段缺失：%v", doc)
		}
	case "grpc":
		if fmt.Sprint(doc["path"]) != "svc-name" {
			t.Errorf("vmess grpc serviceName（path）缺失：%v", doc)
		}
	case "xhttp":
		if fmt.Sprint(doc["path"]) != "/xh-path" || fmt.Sprint(doc["host"]) != "xh.example.com" {
			t.Errorf("vmess xhttp 字段缺失：%v", doc)
		}
	}
}

// checkClashTransport 核对 Clash 传输段细节。
func checkClashTransport(t *testing.T, c matrixCombo, block string) {
	t.Helper()
	if strings.Contains(c.name, "+http") {
		for _, want := range []string{"network: http", "http-opts:", "- \"/tp-path\"", "Host:"} {
			if !strings.Contains(block, want) {
				t.Errorf("Clash tcp HTTP 伪装缺 %q\n%s", want, block)
			}
		}
		return
	}
	switch c.transport {
	case "ws":
		for _, want := range []string{"ws-opts:", "path: \"/ws-path\"", "Host: \"ws.example.com\""} {
			if !strings.Contains(block, want) {
				t.Errorf("Clash ws 缺 %q\n%s", want, block)
			}
		}
	case "httpupgrade":
		for _, want := range []string{"ws-opts:", "v2ray-http-upgrade: true", "path: \"/hu-path\"", "Host: \"hu.example.com\""} {
			if !strings.Contains(block, want) {
				t.Errorf("Clash httpupgrade 缺 %q\n%s", want, block)
			}
		}
	case "grpc":
		for _, want := range []string{"grpc-opts:", "grpc-service-name: \"svc-name\""} {
			if !strings.Contains(block, want) {
				t.Errorf("Clash grpc 缺 %q\n%s", want, block)
			}
		}
	case "xhttp":
		wants := []string{"xhttp-opts:", "path: \"/xh-path\"", "mode: \"auto\"", "host: \"xh.example.com\""}
		if c.protocol != "vmess" {
			// vmess 的分享 JSON 没有 extra 字段，xPaddingBytes 表达不出来（已知限制，非漏拼）。
			wants = append(wants, "x-padding-bytes: 100-200")
		}
		for _, want := range wants {
			if !strings.Contains(block, want) {
				t.Errorf("Clash xhttp 缺 %q\n%s", want, block)
			}
		}
	}
}

// checkSboxTransport 核对 sing-box transport 段。
func checkSboxTransport(t *testing.T, c matrixCombo, ob map[string]any) {
	t.Helper()
	if strings.Contains(c.name, "+http") {
		tr, _ := ob["transport"].(map[string]any)
		if tr == nil || fmt.Sprint(tr["type"]) != "http" || fmt.Sprint(tr["path"]) != "/tp-path" {
			t.Errorf("sing-box tcp HTTP 伪装 transport 缺失：%v", ob["transport"])
		}
		return
	}
	tr, _ := ob["transport"].(map[string]any)
	switch c.transport {
	case "tcp", "hysteria":
		if tr != nil && fmt.Sprint(tr["type"]) != "" {
			t.Errorf("不该有 transport：%v", tr)
		}
	case "ws":
		if tr == nil || fmt.Sprint(tr["type"]) != "ws" {
			t.Fatalf("sing-box transport 缺失：%v", ob)
		}
		if fmt.Sprint(tr["path"]) != "/ws-path" {
			t.Errorf("sing-box ws path 不对：%v", tr)
		}
		headers, _ := tr["headers"].(map[string]any)
		if headers == nil || fmt.Sprint(headers["Host"]) != "ws.example.com" {
			t.Errorf("sing-box ws headers.Host 缺失：%v", tr)
		}
	case "httpupgrade":
		if tr == nil || fmt.Sprint(tr["type"]) != "httpupgrade" {
			t.Fatalf("sing-box transport 缺失：%v", ob)
		}
		if fmt.Sprint(tr["path"]) != "/hu-path" {
			t.Errorf("sing-box httpupgrade path 不对：%v", tr)
		}
		// sing-box 的 httpupgrade Host 是独立字段（不是 headers.Host）。
		if fmt.Sprint(tr["host"]) != "hu.example.com" {
			t.Errorf("sing-box httpupgrade host 缺失：%v", tr)
		}
	case "grpc":
		if tr == nil || fmt.Sprint(tr["service_name"]) != "svc-name" {
			t.Errorf("sing-box grpc service_name 缺失：%v", ob["transport"])
		}
		// sing-box 对未知字段是致命错误（整份配置失败）：grpc transport 没有 path。
		if tr != nil {
			if _, bad := tr["path"]; bad {
				t.Errorf("sing-box grpc transport 不该有 path：%v", tr)
			}
		}
	}
}

// checkSboxSecurity 核对 sing-box tls / reality / 协议凭据段。
func checkSboxSecurity(t *testing.T, c matrixCombo, ob map[string]any) {
	t.Helper()
	switch c.security {
	case "tls":
		tlsDoc, _ := ob["tls"].(map[string]any)
		if tlsDoc == nil || fmt.Sprint(tlsDoc["server_name"]) != matrixSNI {
			t.Errorf("sing-box tls.server_name 缺失：%v", ob["tls"])
		}
	case "reality":
		tlsDoc, _ := ob["tls"].(map[string]any)
		reality, _ := tlsDoc["reality"].(map[string]any)
		if reality == nil || fmt.Sprint(reality["public_key"]) != matrixPBK || fmt.Sprint(reality["short_id"]) != matrixSID {
			t.Errorf("sing-box reality 段缺失：%v", ob["tls"])
		}
	}
	if c.salamander != "" {
		obfs, _ := ob["obfs"].(map[string]any)
		if obfs == nil || fmt.Sprint(obfs["password"]) != matrixSalamand {
			t.Errorf("sing-box obfs 段缺失：%v", ob["obfs"])
		}
	}
}

func clashTypeOf(protocol string) string {
	switch protocol {
	case "hysteria":
		return "hysteria2"
	case "shadowsocks":
		return "ss"
	default:
		return protocol
	}
}

func sboxTypeOf(protocol string) string {
	switch protocol {
	case "shadowsocks":
		return "shadowsocks"
	case "hysteria":
		return "hysteria2"
	default:
		return protocol
	}
}

// clashNodeBlock 从 YAML 里截出名字匹配的那个 proxy 段。
func clashNodeBlock(t *testing.T, yaml, name string) string {
	t.Helper()
	marker := "  - name: \"" + name + "\""
	idx := strings.Index(yaml, marker)
	if idx < 0 {
		t.Fatalf("Clash 输出里没有节点 %q\n--- 输出 ---\n%s", name, yaml)
	}
	rest := yaml[idx+len(marker):]
	if end := strings.Index(rest, "\n  - name: "); end >= 0 {
		rest = rest[:end]
	}
	if end := strings.Index(rest, "\nproxy-groups:"); end >= 0 {
		rest = rest[:end]
	}
	return marker + rest
}

// renderFlowFor 调真实渲染器（nodeapi）拿这个入站第一个 client 的 flow。
func renderFlowFor(t *testing.T, c matrixCombo) string {
	t.Helper()
	var realityKey *string
	if c.security == "reality" {
		key := "reality-private-key"
		realityKey = &key
	}
	rd := &nodeapi.RenderData{
		Node: nodeapi.RenderNode{ID: 1, Name: matrixNodeName, Host: matrixHost, DriverType: "xray"},
		Users: []nodeapi.RenderUser{
			{ID: 7, UUID: matrixUUID, Protocol: c.protocol},
		},
		Inbounds: []nodeapi.RenderInbound{{
			ID: 1, Tag: matrixInbound, Protocol: c.protocol, Port: 4432,
			Settings:          json.RawMessage(c.settings),
			StreamSettings:    json.RawMessage(c.stream),
			Sniffing:          json.RawMessage(`{"enabled":true}`),
			RealityPrivateKey: realityKey,
		}},
	}
	body, err := nodeapi.RenderJSON(rd, nil)
	if err != nil {
		t.Fatalf("节点渲染失败: %v", err)
	}
	var doc struct {
		Inbounds []struct {
			Settings struct {
				Clients []map[string]any `json:"clients"`
			} `json:"settings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("渲染结果不是 JSON: %v", err)
	}
	if len(doc.Inbounds) == 0 || len(doc.Inbounds[0].Settings.Clients) == 0 {
		t.Fatalf("渲染结果没有 clients：%s", body)
	}
	v := doc.Inbounds[0].Settings.Clients[0]["flow"]
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
