package admin

// 入站入参校验：字段白名单（对齐 validatePayload）与 serverNames 过滤。
// 从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"strings"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

// validateInboundPayload 对齐 validatePayload()。
func (d *NodeDomain) validateInboundPayload(c *gin.Context, input map[string]any) bool {
	v := newNodeValidator(ctxOf(c), input)
	// 入站协议白名单：取自能力表（protocol_registry.go），与 XrayInbounds.vue 的协议下拉
	// 一一对应 —— 多一项前端提交会被打回、少一项协议建不出来。表里现有
	// vless / vmess / trojan / shadowsocks / hysteria / socks / http / tunnel / wireguard：
	//   socks     传统 SOCKS5 代理入站（auth 用户密码）
	//   http      传统 HTTP 正向代理入站（accounts 用户密码）
	//   tunnel    dokodemo-door（内核 26.x 的协议名就叫 tunnel）：目标地址+端口原样转发
	//   wireguard 内核自带 WireGuard 服务端（UDP 监听，server 私钥 + 对端公钥清单）
	v.field("protocol", "required", inRule(inboundProtocolNames()))
	v.field("tag", "nullable", "string", "max:64", "regex:/^[A-Za-z0-9_-]+$/")
	v.field("remark", "nullable", "string", "max:128")
	v.field("port", "required", "integer", "between:1,65535")
	v.field("listen", "nullable", "string", "max:64")
	// 传输层 / 安全层白名单：同样是能力表里的并集（表里受限的行只列它能用的项）。
	// hysteria 也在传输层白名单里：HY2 的传输层由 buildInboundInput 强制归一成 hysteria
	// （内核只认这个名字），前端建 HY2 时固定提交 network=hysteria —— 缺了它这条会被
	// 这里拦成 422「The selected network is invalid.」，整个 HY2 入站根本建不出来。
	v.field("network", "nullable", inRule(inboundProtocolNetworks()))
	v.field("security", "nullable", inRule(inboundProtocolSecurities()))
	v.field("sniffing", "nullable", "array")
	v.field("sniffing.enabled", "nullable", "boolean")
	v.field("sniffing.destOverride", "nullable", "array", "max:4")
	v.field("sniffing.destOverride.*", "string", "in:http,tls,quic,fakedns")
	v.field("sniffing.routeOnly", "nullable", "boolean")
	v.field("dest", "nullable", "string", "max:255")
	v.field("server_names", "nullable", "array", "max:64")
	v.field("server_names.*", "string", "max:255")
	v.field("short_ids", "nullable", "array", "max:8")
	v.field("short_ids.*", "string", "max:16", "regex:/^[0-9a-fA-F]+$/")
	v.field("private_key", "nullable", "string", "max:255")
	v.field("public_key", "nullable", "string", "max:255")
	v.field("show", "nullable", "boolean")
	v.field("xver", "nullable", "integer", "min:0", "max:2")
	v.field("min_client_ver", "nullable", "string", "max:32", "regex:/^\\d+(\\.\\d+)*$/")
	v.field("max_client_ver", "nullable", "string", "max:32", "regex:/^\\d+(\\.\\d+)*$/")
	v.field("max_timediff", "nullable", "integer", "min:0", "max:86400000")
	v.field("mldsa65_seed", "nullable", "string", "max:8192")
	v.field("mldsa65_verify", "nullable", "string", "max:8192")
	v.field("master_key_log", "nullable", "string", "max:255")
	v.field("fingerprint", "nullable", "string", "max:32")
	v.field("spider_x", "nullable", "string", "max:255")
	v.field("limit_fallback_upload", "nullable", "array")
	v.field("limit_fallback_upload.after_bytes", "nullable", "integer", "min:0")
	v.field("limit_fallback_upload.bytes_per_sec", "nullable", "integer", "min:0")
	v.field("limit_fallback_upload.burst_bytes_per_sec", "nullable", "integer", "min:0")
	v.field("limit_fallback_download", "nullable", "array")
	v.field("limit_fallback_download.after_bytes", "nullable", "integer", "min:0")
	v.field("limit_fallback_download.bytes_per_sec", "nullable", "integer", "min:0")
	v.field("limit_fallback_download.burst_bytes_per_sec", "nullable", "integer", "min:0")
	v.field("ws_path", "nullable", "string", "max:255")
	v.field("ws_host", "nullable", "string", "max:255")
	v.field("httpupgrade_path", "nullable", "string", "max:255")
	v.field("httpupgrade_host", "nullable", "string", "max:255")
	v.field("tcp_header_type", "nullable", "in:none,http")
	v.field("tcp_header_path", "nullable", "string", "max:255")
	v.field("tcp_header_host", "nullable", "string", "max:255")
	v.field("xhttp_path", "nullable", "string", "max:255")
	v.field("xhttp_host", "nullable", "string", "max:255")
	v.field("xhttp_mode", "nullable", "string", "max:32")
	v.field("xhttp_padding_bytes", "nullable", "string", "max:64")
	v.field("xhttp_padding_obfs_mode", "nullable", "boolean")
	v.field("xhttp_padding_key", "nullable", "string", "max:128")
	v.field("xhttp_padding_header", "nullable", "string", "max:128")
	v.field("xhttp_padding_placement", "nullable", "string", "max:32")
	v.field("xhttp_padding_method", "nullable", "string", "max:32")
	v.field("xhttp_session_id_placement", "nullable", "string", "max:32")
	v.field("xhttp_session_id_key", "nullable", "string", "max:128")
	v.field("xhttp_session_id_table", "nullable", "string", "max:64")
	v.field("xhttp_session_id_length", "nullable", "string", "max:32")
	v.field("xhttp_seq_placement", "nullable", "string", "max:32")
	v.field("xhttp_seq_key", "nullable", "string", "max:128")
	v.field("xhttp_uplink_data_placement", "nullable", "string", "max:32")
	v.field("xhttp_uplink_data_key", "nullable", "string", "max:128")
	v.field("xhttp_uplink_chunk_size", "nullable", "integer", "min:0", "max:1048576")
	v.field("xhttp_sc_max_each_post_bytes", "nullable", "string", "max:64")
	v.field("xhttp_sc_max_buffered_posts", "nullable", "integer", "min:0", "max:100000")
	v.field("xhttp_sc_min_posts_interval_ms", "nullable", "string", "max:64")
	v.field("xhttp_sc_stream_up_server_secs", "nullable", "string", "max:64")
	v.field("xhttp_server_max_header_bytes", "nullable", "integer", "min:0", "max:1048576")
	v.field("xhttp_uplink_http_method", "nullable", "string", "max:16")
	v.field("xhttp_no_sse_header", "nullable", "boolean")
	v.field("xhttp_no_grpc_header", "nullable", "boolean")
	v.field("xhttp_headers", "nullable", "string", "max:8192")
	v.field("xhttp_xmux_max_concurrency", "nullable", "string", "max:32")
	v.field("xhttp_xmux_max_connections", "nullable", "string", "max:32")
	v.field("xhttp_xmux_c_max_reuse_times", "nullable", "string", "max:32")
	v.field("xhttp_xmux_h_max_request_times", "nullable", "string", "max:32")
	v.field("xhttp_xmux_h_max_reusable_secs", "nullable", "string", "max:32")
	v.field("xhttp_xmux_h_keep_alive_period", "nullable", "string", "max:32")
	v.field("tls_server_name", "nullable", "string", "max:255")
	// 客户端连接地址（CDN 域名等）：订阅链接里用它替换节点地址；空 = 用节点地址。
	v.field("client_host", "nullable", "string", "max:255")
	// 入站归属地区（#入站级分组）：可选，null = 跟随节点地区（老数据行为）。
	// 存在性在 buildInboundInput 里用 RegionExists 查库校验（节点域校验器不查库）。
	v.field("region_id", "nullable", "integer", "min:1")
	v.field("tls_certificate", "nullable", "string", "max:65535")
	v.field("tls_key", "nullable", "string", "max:65535")
	v.field("ss_method", "nullable", "in:aes-256-gcm,aes-128-gcm,chacha20-poly1305,xchacha20-poly1305,2022-blake3-aes-256-gcm,2022-blake3-aes-128-gcm")
	v.field("ss_password", "nullable", "string", "max:128")
	// socks / http（传统代理入站）：单组账号密码。密码编辑时留空 = 沿用原值。
	v.field("auth_user", "nullable", "string", "max:64")
	v.field("auth_pass", "nullable", "string", "max:128")
	// tunnel（dokodemo-door）：转发目标 + L4 网络（settings.network，不是传输层）。
	v.field("tunnel_address", "nullable", "string", "max:255")
	v.field("tunnel_port", "nullable", "integer", "between:1,65535")
	// 逗号是校验规则的分隔符，「tcp,udp」这种值只能手工校验（见 tunnelNetwork）。
	v.field("tunnel_network", "nullable", "string", "max:16")
	// wireguard：服务端私钥由面板生成（读接口不回显），这里只收对端清单与 MTU。
	v.field("wg_peers", "nullable", "string", "max:8192")
	v.field("wg_mtu", "nullable", "integer", "min:576", "max:9000")
	// hysteria（HY2）：主端口复用 port；其余见 buildHysteriaStream。
	v.field("hy_hop_ports", "nullable", "string", "max:64")
	v.field("hy_hop_interval", "nullable", "string", "max:64")
	// hy_hop_auto：可选总开关。缺省（不带这个字段）按开启处理 —— HY2 默认自动分配跳跃区间；
	// 只有显式 false 才关掉（见 autoAssignHysteriaHop）。
	v.field("hy_hop_auto", "nullable", "boolean")
	// hy_udp_idle_timeout：**0 = 用内核默认 60**（见 buildHysteriaStream 的 `inputInt(v) != 0`
	// 判据），所以先把 0 归一成「不带这个字段」，再按真实取值校验。
	//
	// 为什么不直接把下界写成 0：那会放行 1 —— 内核不接受小于 2 的值，等于把非法配置下发到节点。
	// 也不能只留 min:2：前端历史上默认就提交 0，「不改动直接保存」会被拦成
	// 「must be at least 2」，与界面文案、渲染逻辑三方打架（2026-11-02 用户实测踩到）。
	if raw, ok := input["hy_udp_idle_timeout"]; ok && inputInt(raw) == 0 {
		delete(input, "hy_udp_idle_timeout")
	}
	v.field("hy_udp_idle_timeout", "nullable", "integer", "min:2", "max:600")
	v.field("hy_masquerade_type", "nullable", "in:,proxy,file,string")
	v.field("hy_masquerade_url", "nullable", "string", "max:255")
	v.field("hy_masquerade_dir", "nullable", "string", "max:255")
	v.field("hy_masquerade_content", "nullable", "string", "max:8192")
	v.field("hy_masquerade_status_code", "nullable", "integer", "min:0", "max:999")
	v.field("hy_congestion", "nullable", "in:reno,bbr,brutal,force-brutal")
	// hy2 高级 TLS 开关 + QUIC 调优（quicParams）：brutal 带宽是整数 Mbps，0 = 不写。
	v.field("hy_reject_unknown_sni", "nullable", "boolean")
	v.field("hy_session_resumption", "nullable", "boolean")
	v.field("hy_brutal_up", "nullable", "integer", "min:0", "max:1000000")
	v.field("hy_brutal_down", "nullable", "integer", "min:0", "max:1000000")
	v.field("hy_init_stream_window", "nullable", "integer", "min:0", "max:2147483647")
	v.field("hy_max_stream_window", "nullable", "integer", "min:0", "max:2147483647")
	v.field("hy_init_conn_window", "nullable", "integer", "min:0", "max:2147483647")
	v.field("hy_max_conn_window", "nullable", "integer", "min:0", "max:2147483647")
	// salamander 混淆密码：空串 = 清除（「清空=清除」），不带字段 = 不改。
	v.field("hy_salamander_password", "nullable", "string", "max:255")
	v.field("tls_cert_id", "nullable", "integer", "min:1")
	v.field("enabled", "nullable", "boolean")
	v.field("sort", "nullable", "integer", "min:0", "max:65535")

	if !v.passes() {
		d.failNodeValidation(c, v)
		return false
	}
	return true
}

// usableServerNames 对齐 usableServerNames()：剔除通配符与空串，全空回落默认 SNI。
func usableServerNames(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" || strings.Contains(v, "*") || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return []string{xray.DefaultSNI}
	}
	return out
}

// stringListInput 对齐 stringList()：非空字符串列表（空则用默认单元素）。
func stringListInput(v any, def *string) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, item := range arr {
			s := nodeScalarString(item)
			if strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 && def != nil {
		out = []string{*def}
	}
	return out
}
