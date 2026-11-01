package admin

// xray 节点入站管理（/admin-api/nodes/{node}/xray-inbounds）。
// 逐条对齐 backend/app/Http/Controllers/Admin/XrayInboundController.php。
//
// 入站协议面板自建：vless / vmess / trojan / shadowsocks / hysteria
// （含 tcp/ws/grpc/xhttp/httpupgrade 传输、none/tls/reality 安全层；reality 仅 vless），
// 另有传统代理与自有 UDP 栈的 socks / http / tunnel / wireguard。
// 任何变更后 bump 该节点 config_version → 节点秒级拉取。

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/curve25519"
)

// xrayInboundIndex 对齐 index()。
func (d *NodeDomain) xrayInboundIndex(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	inbounds, err := d.store.XrayInboundsByNode(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询入站", err)
		return
	}
	counts, err := d.store.EligibleProtocolCounts(ctxOf(c))
	if err != nil {
		d.failServer(c, "统计可用用户", err)
		return
	}

	items := make(jsArr, 0, len(inbounds))
	for i := range inbounds {
		items = append(items, presentXrayInbound(&inbounds[i], counts))
	}
	d.success(c, jo(
		"inbounds", items,
		"config_version", node.ConfigVersion,
		// issue #94：端口跳跃要在节点上配防火墙，只有 agent ≥ 1.0.7 才有那几个 action。
		// 前端据此禁用「端口跳跃」区块并提示「需升级 agent」（与入队门槛同一判定）。
		"hop_supported", agentSupportsHop(node),
		"hop_min_agent_version", minHopAgentVersion,
	))
}

// xrayInboundStore 对齐 store()。
func (d *NodeDomain) xrayInboundStore(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateInboundPayload(c, input) {
		return
	}
	if !d.guardServerNames(c, node, input, nil) {
		return
	}

	// HY2 自动端口跳跃：用户没填区间时后端先探测一段空闲区间写进 payload。
	// 失败只降级成「无跳跃」，不挡建入站（原因随回执的 hop 段回给前端）。
	hopAuto, hopReason := d.autoAssignHysteriaHop(c, node, input, nil)

	in, ok := d.buildInboundInput(c, node, input, nil)
	if !ok {
		return
	}
	created, err := d.store.CreateXrayInbound(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建入站", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}

	counts, err := d.store.EligibleProtocolCounts(ctxOf(c))
	if err != nil {
		d.failServer(c, "统计可用用户", err)
		return
	}
	resp := jo(
		"inbound", presentXrayInbound(created, counts),
		"config_version", version,
	)
	// 入站已落库，防火墙这一步只报状态：下发失败不能让创建算失败。
	if hop := d.applyHysteriaHopAfterSave(c, node, created, hopAuto, hopReason); hop != nil {
		resp.put("hop", hop)
	}
	d.success(c, resp)
}

// xrayInboundUpdate 对齐 update()。
func (d *NodeDomain) xrayInboundUpdate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedInbound(c, node)
	if !ok {
		return
	}

	input := jsonInput(c)
	if !d.validateInboundPayload(c, input) {
		return
	}
	if !d.guardServerNames(c, node, input, existing) {
		return
	}

	// HY2 自动端口跳跃（编辑场景）：原配置已有区间就沿用，没有才重新探测。
	hopAuto, hopReason := d.autoAssignHysteriaHop(c, node, input, existing)

	in, ok := d.buildInboundInput(c, node, input, existing)
	if !ok {
		return
	}
	updated, err := d.store.UpdateXrayInbound(ctxOf(c), existing.ID, in)
	if err != nil {
		d.failServer(c, "保存入站", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}

	counts, err := d.store.EligibleProtocolCounts(ctxOf(c))
	if err != nil {
		d.failServer(c, "统计可用用户", err)
		return
	}
	resp := jo(
		"inbound", presentXrayInbound(updated, counts),
		"config_version", version,
	)
	// 入站已落库，防火墙这一步只报状态：下发失败不能让保存算失败。
	if hop := d.applyHysteriaHopAfterSave(c, node, updated, hopAuto, hopReason); hop != nil {
		resp.put("hop", hop)
	}
	d.success(c, resp)
}

// xrayInboundDestroy 对齐 destroy()。
func (d *NodeDomain) xrayInboundDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedInbound(c, node)
	if !ok {
		return
	}
	if err := d.store.DeleteXrayInbound(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "删除入站", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo("config_version", version))
}

// xrayInboundBatchDestroy 对齐 batchDestroy()：逐条处理、按 id 回报，只 bump 一次版本。
func (d *NodeDomain) xrayInboundBatchDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("ids", "required", "array", "min:1", "max:100")
	v.field("ids.*", "integer", "min:1")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	rawIDs, _ := input["ids"].([]any)
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id := inputInt(raw)
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	deleted := jsArr{}
	failed := jsArr{}
	anyDeleted := false
	for _, id := range ids {
		if id <= 0 {
			failed = append(failed, jo("id", id, "reason", "not_found"))
			continue
		}
		target, err := d.store.XrayInboundByID(ctxOf(c), node.ID, uint64(id))
		if err != nil {
			if d.noRowsToNotFoundQuiet(err) {
				failed = append(failed, jo("id", id, "reason", "not_found"))
				continue
			}
			failed = append(failed, jo("id", id, "reason", err.Error()))
			continue
		}
		_ = target
		if err := d.store.DeleteXrayInbound(ctxOf(c), uint64(id)); err != nil {
			failed = append(failed, jo("id", id, "reason", err.Error()))
			continue
		}
		deleted = append(deleted, id)
		anyDeleted = true
	}

	if anyDeleted {
		if _, err := d.store.BumpConfigVersion(ctxOf(c), node.ID); err != nil {
			d.failServer(c, "递增配置版本", err)
			return
		}
	}
	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	d.success(c, jo(
		"deleted", deleted,
		"failed", failed,
		"config_version", fresh.ConfigVersion,
	))
}

// noRowsToNotFoundQuiet 判断「查无此行」且不写响应（batch 里要按 id 回报）。
func (d *NodeDomain) noRowsToNotFoundQuiet(err error) bool {
	return err != nil && d.noRowsToNotFoundSilent(err)
}

// realityKeypair 对齐 realityKeypair()：生成一组 Reality 密钥材料（不落库）。
func (d *NodeDomain) realityKeypair(c *gin.Context) {
	if _, ok := d.loadXrayNode(c); !ok {
		return
	}

	keypair, err := xray.GenerateKeypair()
	if err != nil {
		d.failServer(c, "生成 Reality 密钥", err)
		return
	}
	shortID, err := xray.ShortID(4)
	if err != nil {
		d.failServer(c, "生成 shortId", err)
		return
	}

	d.success(c, jo(
		"private_key", keypair[0],
		"public_key", keypair[1],
		"short_id", shortID,
		"dest", xray.DefaultDest,
		"server_name", xray.DefaultSNI,
	))
}

// realityScan 对齐 realityScan()：探测在节点侧跑（复用 node_api_commands 指令通道）。
func (d *NodeDomain) realityScan(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("targets", "nullable", "array", "max:20")
	v.field("targets.*", "nullable", "string", "max:255")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	rawTargets := []string{}
	if arr, ok := input["targets"].([]any); ok {
		for _, item := range arr {
			rawTargets = append(rawTargets, nodeScalarString(item))
		}
	}
	targets := xray.SanitizeTargets(rawTargets)
	if len(targets) == 0 {
		targets = xray.DefaultTargets()
	}

	payload := jo("targets", stringsToJSArr(targets))
	command, err := d.store.CreateCommand(ctxOf(c), node.ID, "reality_scan", payload)
	if err != nil {
		d.failServer(c, "入队 reality 扫描", err)
		return
	}

	wait := d.cfg.RealityScanWait
	deadline := d.now().Add(timeDurationSeconds(wait))
	for d.now().Before(deadline) {
		fresh, err := d.store.CommandByID(ctxOf(c), command.ID)
		if err != nil {
			d.failServer(c, "回读指令状态", err)
			return
		}
		switch fresh.Status {
		case "failed":
			output := ""
			if fresh.Result != nil {
				output = *fresh.Result
			}
			if strings.Contains(output, "unknown action") {
				output = "该节点的 agent 版本过旧，请先在「节点」页升级 agent 后重试"
			} else if output == "" {
				output = "节点执行失败"
			}
			d.success(c, jo("results", jsArr{}, "error", output))
			return
		case "success":
			raw := ""
			if fresh.Result != nil {
				raw = *fresh.Result
			}
			parsed, err := parseOrderedJS([]byte(raw))
			if err != nil {
				d.success(c, jo("results", jsArr{}, "error", "节点回执解析失败"))
				return
			}
			if arr, ok := parsed.(jsArr); ok {
				d.success(c, jo("results", arr))
				return
			}
			// 回执不是数组：PHP 的 is_array(json_decode(..., true)) 对对象也为真，
			// 此时原样塞进 results（前端按「不是数组」显示失败），这里保持同口径。
			if obj, ok := parsed.(*jsObj); ok {
				d.success(c, jo("results", obj))
				return
			}
			d.success(c, jo("results", jsArr{}, "error", "节点回执解析失败"))
			return
		}
		d.sleep(d.pollInterval)
	}

	d.success(c, jo(
		"pending", true,
		"message", "节点未在 "+strconv.Itoa(wait)+" 秒内回执（可能离线），稍后可重试",
	))
}

// ---- 内部 ----

// loadOwnedInbound 取 URL 里的入站并校验归属（PHP ensureOwned → abort(404)）。
func (d *NodeDomain) loadOwnedInbound(c *gin.Context, node *NodeRecord) (*XrayInboundRecord, bool) {
	id, ok := parseUint(c.Param("inbound"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	ib, err := d.store.XrayInboundByID(ctxOf(c), node.ID, id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询入站", err)
		return nil, false
	}
	return ib, true
}

// validateInboundPayload 对齐 validatePayload()。
func (d *NodeDomain) validateInboundPayload(c *gin.Context, input map[string]any) bool {
	v := newNodeValidator(ctxOf(c), input)
	// 入站协议白名单：与 XrayInbounds.vue 的协议下拉一一对应，多一项前端提交会被打回、
	// 少一项协议建不出来。socks / http / tunnel / wireguard 是 P2 补回来的全协议适配：
	//   socks     传统 SOCKS5 代理入站（auth 用户密码）
	//   http      传统 HTTP 正向代理入站（accounts 用户密码）
	//   tunnel    dokodemo-door（内核 26.x 的协议名就叫 tunnel）：目标地址+端口原样转发
	//   wireguard 内核自带 WireGuard 服务端（UDP 监听，server 私钥 + 对端公钥清单）
	v.field("protocol", "required", "in:vless,vmess,trojan,shadowsocks,hysteria,socks,http,tunnel,wireguard")
	v.field("tag", "nullable", "string", "max:64", "regex:/^[A-Za-z0-9_-]+$/")
	v.field("remark", "nullable", "string", "max:128")
	v.field("port", "required", "integer", "between:1,65535")
	v.field("listen", "nullable", "string", "max:64")
	// hysteria 也在白名单里：HY2 的传输层由 buildInboundInput 强制归一成 hysteria
	// （内核只认这个名字），前端建 HY2 时固定提交 network=hysteria —— 缺了它这条会被
	// 这里拦成 422「The selected network is invalid.」，整个 HY2 入站根本建不出来。
	v.field("network", "nullable", "in:tcp,ws,grpc,xhttp,httpupgrade,hysteria")
	v.field("security", "nullable", "in:none,tls,reality")
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

// buildInboundInput 对齐 fillFromPayload()（含全部业务校验）。
func (d *NodeDomain) buildInboundInput(c *gin.Context, node *NodeRecord, input map[string]any, existing *XrayInboundRecord) (XrayInboundInput, bool) {
	protocol := nodeStringField(input, "protocol")
	network := "tcp"
	if s, ok := inputString(input["network"]); ok && s != "" {
		network = s
	}
	security := "none"
	if s, ok := inputString(input["security"]); ok && s != "" {
		security = s
	}

	// 组合合法性（前后端同守）。
	if security == "reality" && protocol != "vless" {
		d.fail(c, "Reality 仅支持 VLESS 协议", 500)
		return XrayInboundInput{}, false
	}
	if security == "reality" && !phpInArray(network, []string{"tcp", "xhttp", "grpc"}) {
		d.fail(c, "Reality 仅支持 tcp / xhttp / grpc 传输", 500)
		return XrayInboundInput{}, false
	}
	if protocol == "shadowsocks" && (security != "none" || network != "tcp") {
		d.fail(c, "Shadowsocks 仅支持 tcp + 无附加安全层", 500)
		return XrayInboundInput{}, false
	}
	if protocol == "hysteria" {
		// HY2 的传输层固定：内核只认 network=hysteria + 强制 TLS（QUIC 自带加密）。
		// 前端不选传输/安全层，这里统一归一，避免历史负载带进非法组合。
		network = "hysteria"
		security = "tls"
	}
	if protocol == "wireguard" {
		// WireGuard 自带加密、固定 UDP：传输层与安全层都不存在，归一成空
		// （后面按空值跳过所有传输分支，stream_settings 落库就是 {}）。
		network = ""
		security = "none"
	}

	// Reality 后量子：seed 与 verify 成对。
	if security == "reality" {
		seed := strings.TrimSpace(nodeStringField(input, "mldsa65_seed"))
		verify := strings.TrimSpace(nodeStringField(input, "mldsa65_verify"))
		if (seed == "") != (verify == "") {
			d.fail(c, "mldsa65Seed 与 mldsa65Verify 必须成对设置（同时填写或同时留空）", 500)
			return XrayInboundInput{}, false
		}
	}

	// tag：缺省自动建议；唯一性预检（同节点内唯一）。
	var excludeID *uint64
	if existing != nil {
		excludeID = ptrUintOf(existing.ID)
	}
	tag := nodeStringField(input, "tag")
	if tag == "" {
		suggested, ok := d.suggestInboundTag(c, node, protocol, security, network, excludeID)
		if !ok {
			return XrayInboundInput{}, false
		}
		tag = suggested
	}
	dup, err := d.store.XrayInboundTagTaken(ctxOf(c), node.ID, tag, excludeID)
	if err != nil {
		d.failServer(c, "检查入站标识", err)
		return XrayInboundInput{}, false
	}
	if dup {
		d.fail(c, "标识 "+tag+" 已存在", 500)
		return XrayInboundInput{}, false
	}

	// 端口冲突：同节点启用入站不可复用端口（xray 会启动失败）。
	// 按传输族比对：tunnel 的 L4 网络在 settings.network（tcp / udp / tcp,udp），
	// 其余协议看协议本身（wireguard / hysteria 是 UDP）。
	port := inputInt(input["port"])
	// tunnel 的 L4 网络非法时这里按缺省算，真正的报错留给 settings 构造（文案更具体）。
	tunNetwork, _ := tunnelNetwork(input)
	portTaken, err := d.store.XrayInboundPortTaken(ctxOf(c), node.ID, port, inboundFamiliesOf(protocol, tunNetwork), excludeID)
	if err != nil {
		d.failServer(c, "检查入站端口", err)
		return XrayInboundInput{}, false
	}
	willEnable := true
	if existing != nil {
		willEnable = existing.Enabled
	}
	if v, present := input["enabled"]; present {
		willEnable = inputBool(v)
	}
	if willEnable && portTaken {
		d.fail(c, "端口已被该节点其他启用入站占用", 500)
		return XrayInboundInput{}, false
	}

	// settings
	var settings *jsObj
	switch protocol {
	case "vless":
		settings = jo("decryption", "none")
	case "shadowsocks":
		method := "aes-256-gcm"
		if s, ok := inputString(input["ss_method"]); ok && s != "" {
			method = s
		}
		settings = jo("method", method)
		if pw, ok := inputString(input["ss_password"]); ok && pw != "" {
			settings.put("password", pw)
		} else {
			d.fail(c, "Shadowsocks 必须设置密码", 500)
			return XrayInboundInput{}, false
		}
	case "hysteria":
		// 内核只接受 version=2；写 1 会让**整份节点配置**被拒（不是只挂这个入站）。
		settings = jo("version", 2)
	case "socks", "http":
		// 传统代理入站。落库形状（xray 26.3.27 实测可跑通真实代理流量）：
		//   socks = {auth: "password", accounts: [{user, pass}], udp: false}
		//   http  = {accounts: [{user, pass}], allowTransparent: false}
		user := strings.TrimSpace(nodeStringField(input, "auth_user"))
		pass := nodeStringField(input, "auth_pass")
		if pass == "" && existing != nil {
			// 密码不回显：编辑时留空 = 沿用原值（重填才改）。
			prev, _ := parseOrderedJS(existing.Settings)
			pass = jsonGetString(prev, "accounts", "0", "pass")
		}
		if user == "" || pass == "" {
			d.fail(c, "SOCKS / HTTP 入站必须填写用户名与密码（内核默认 noauth，公网裸代理会被扫端口滥用）", 500)
			return XrayInboundInput{}, false
		}
		account := jo("user", user, "pass", pass)
		if protocol == "socks" {
			// udp=false：只占 TCP，与端口族判定（inboundFamiliesOf）同口径。
			settings = jo("auth", "password", "accounts", jsArr{account}, "udp", false)
		} else {
			settings = jo("accounts", jsArr{account}, "allowTransparent", false)
		}
	case "tunnel":
		// dokodemo-door：把收到的流量原样转给 address:port。
		// 注意 L4 网络是 settings.network（tcp/udp/tcp,udp），写进 streamSettings.network
		// 会被内核直接拒（unknown transport protocol: udp）。
		tunAddr := strings.TrimSpace(nodeStringField(input, "tunnel_address"))
		tunPort := int64(0)
		if v, present := input["tunnel_port"]; present && v != nil {
			tunPort = inputInt(v)
		}
		if tunAddr == "" || tunPort <= 0 {
			d.fail(c, "Tunnel 入站必须填写转发目标（地址 + 端口）", 500)
			return XrayInboundInput{}, false
		}
		tunNet, netOK := tunnelNetwork(input)
		if !netOK {
			d.fail(c, "Tunnel 转发协议只支持 tcp / udp / tcp,udp", 500)
			return XrayInboundInput{}, false
		}
		settings = jo("address", tunAddr, "port", tunPort, "network", tunNet)
	case "wireguard":
		// 内核自带的 WireGuard 服务端：UDP 监听，服务端私钥 + 对端公钥清单。
		// secretKey 是服务端身份私钥（等同于节点密码），只在库里与下发给节点的配置里出现，
		// 读接口只回 wg_public_key。
		secret := ""
		if existing != nil {
			prev, _ := parseOrderedJS(existing.Settings)
			secret = jsonGetString(prev, "secretKey")
		}
		if secret == "" {
			kp, err := xray.WireGuardKeypair()
			if err != nil {
				d.failServer(c, "生成 WireGuard 密钥", err)
				return XrayInboundInput{}, false
			}
			secret = kp[0]
		}
		peers, ok := wireGuardPeers(nodeStringField(input, "wg_peers"))
		if !ok {
			d.fail(c, "WireGuard 对端格式错误：每行「公钥 [允许IP,...]」，公钥为 44 字符 base64", 500)
			return XrayInboundInput{}, false
		}
		mtu := int64(1420)
		if v, present := input["wg_mtu"]; present && inputInt(v) > 0 {
			mtu = inputInt(v)
		}
		// noKernelTun=true：内核目前只有 gVisor TUN 支持 WireGuard 入站（自带告警），
		// 显式写上，免得以后内核默认值变化把入站搞挂。
		settings = jo("secretKey", secret, "mtu", mtu, "noKernelTun", true)
		if len(peers) > 0 {
			settings.put("peers", peers)
		}
	default:
		settings = jo()
	}

	// stream_settings
	//
	// wireguard 自带加密、固定 UDP，没有可选的传输层与安全层：整段不写
	// （写入 network 虽然会被内核忽略，但列表页会把它显示成 TCP 入站，误导管理员）。
	var stream *jsObj
	if protocol == "wireguard" {
		stream = jo()
	} else {
		stream = jo("network", network, "security", security)
	}

	in := XrayInboundInput{
		NodeID:   node.ID,
		Tag:      tag,
		Protocol: protocol,
		Port:     port,
		Settings: phpEncode(settings),
		Sniffing: sniffingSettings(input),
	}

	if security == "reality" {
		privateKey := nodeStringField(input, "private_key")
		publicKey := nodeStringField(input, "public_key")
		if privateKey == "" {
			// 新建且未提供材料 → 面板自动生成完整一套。
			keypair, err := xray.GenerateKeypair()
			if err != nil {
				d.failServer(c, "生成 Reality 密钥", err)
				return XrayInboundInput{}, false
			}
			privateKey, publicKey = keypair[0], keypair[1]
		}
		if publicKey == "" && existing != nil {
			prev, _ := parseOrderedJS(existing.StreamSettings)
			publicKey = jsonGetString(prev, "realitySettings", "publicKey")
		}

		stream.put("realitySettings", d.realitySettings(input, publicKey))
		in.RealityPrivateKey = &privateKey
		in.RealityKeySet = true
	} else if existing != nil {
		// 从 reality 改成别的安全层：清掉旧私钥（对齐 setRealityPrivateKey(null)）。
		in.RealityKeySet = true
	}

	// certPickReason 是「自动挑证书为什么没挑到」的原因（"" = 挑到了，或压根没走自动挑）。
	// 只用来把下面的报错文案说准：证书库空 / 全过期，用户要做的操作不一样。
	certPickReason := ""
	if protocol == "hysteria" {
		var prevStream any
		if existing != nil {
			prevStream, _ = parseOrderedJS(existing.StreamSettings)
		}
		// 证书自动引用：请求没带 tls_cert_id、编辑时原配置也没引用时，从证书库挑一张，
		// 免得「建一个 HY2 入站先去手抄一个证书 id」。必须放在 buildHysteriaStream 之前
		// ——它读的就是 input 里的 tls_cert_id。
		certPickReason = d.autoPickHysteriaCert(c, node, input, prevStream)
		if !d.buildHysteriaStream(c, input, stream, prevStream) {
			return XrayInboundInput{}, false
		}
		// salamander 混淆密码：密码写独立加密列，stream_settings 只留 {"type":"salamander"} 占位
		// （issue #94 硬指标：库里不允许出现密码原文）。
		applyHysteriaSalamander(input, stream, prevStream, existing, &in)
	} else if existing != nil && existing.SalamanderPassword != nil {
		// 协议从 hysteria 改走别的：清掉遗留的混淆密码，别把密文留在没人读的列里。
		in.SalamanderPasswordSet = true
	}

	if network == "ws" {
		// PHP: array_filter(['path'=>..., 'host'=>...], fn($v) => $v !== '')
		// → path 恒在，host 为空时被删掉。
		ws := jo()
		path := nodeStringField(input, "ws_path")
		if path == "" {
			path = "/"
		}
		ws.put("path", path)
		if host := nodeStringField(input, "ws_host"); host != "" {
			ws.put("host", host)
		}
		stream.put("wsSettings", ws)
	} else if network == "xhttp" {
		stream.put("xhttpSettings", xhttpSettings(input))
	} else if network == "httpupgrade" {
		hu := jo()
		path := nodeStringField(input, "httpupgrade_path")
		if path == "" {
			path = "/"
		}
		hu.put("path", path)
		if host := nodeStringField(input, "httpupgrade_host"); host != "" {
			hu.put("host", host)
		}
		stream.put("httpupgradeSettings", hu)
	}

	// TCP header 伪装（type=http 时构造 request；响应段用内核默认）。
	if network == "tcp" && nodeStringField(input, "tcp_header_type") == "http" {
		reqPath := nodeStringField(input, "tcp_header_path")
		if reqPath == "" {
			reqPath = "/"
		}
		request := jo("version", "1.1", "method", "GET", "path", jsArr{reqPath})
		if host := nodeStringField(input, "tcp_header_host"); host != "" {
			request.put("headers", jo("Host", jsArr{host}))
		}
		stream.put("tcpSettings", jo("header", jo("type", "http", "request", request)))
	}

	// hysteria 的 tlsSettings 已在 buildHysteriaStream 里按证书引用写好
	// （只存 tlsCertId，私钥留在证书库），不能再走下面这段内联 PEM 分支。
	if security == "tls" && protocol != "hysteria" {
		tls := jo()
		if sni := nodeStringField(input, "tls_server_name"); sni != "" {
			tls.put("serverName", sni)
		}
		cert := nodeStringField(input, "tls_certificate")
		key := nodeStringField(input, "tls_key")
		if cert == "" || key == "" {
			// TLS 入站没有证书时 xray 无法启动；空 tlsSettings 会编码成 [] 让整份配置加载失败。
			d.fail(c, "TLS 入站必须提供证书与私钥", 500)
			return XrayInboundInput{}, false
		}
		tls.put("certificates", jsArr{jo("certificate", jsArr{cert}, "key", jsArr{key})})
		stream.put("tlsSettings", tls)
	}

	if protocol == "hysteria" {
		// HY2 强制 TLS：没引用证书时内核起不来（与 TLS 入站同理）。
		// 自动挑证书没成功（库空 / 全过期）时，文案要直接指向用户下一步该做什么。
		if jsonGet(stream, "tlsSettings", xray.CertRefKey) == nil {
			d.fail(c, hysteriaCertMissingMessage(certPickReason), 500)
			return XrayInboundInput{}, false
		}
	}
	in.StreamSettings = phpEncode(stream)

	remark := strings.TrimSpace(nodeStringField(input, "remark"))
	if remark != "" {
		in.Remark = &remark
	}
	listen := nodeStringField(input, "listen")
	if listen == "" {
		listen = "0.0.0.0"
	}
	in.Listen = listen

	if v, present := input["enabled"]; present {
		b := inputBool(v)
		in.Enabled = &b
	}
	if v, present := input["sort"]; present && v != nil {
		n := inputInt(v)
		in.Sort = &n
	}
	return in, true
}

// ---- hysteria 的 TLS 证书自动引用 ----

// 自动挑证书的未命中原因（只影响报错文案：库空 / 全过期 / 缺私钥，用户要做的事不一样）。
const (
	certPickEmpty   = "empty"
	certPickExpired = "expired"
	certPickNoKey   = "nokey"
	certPickDBError = "dberr"
	certPickNoMatch = "nomatch"
)

// hysteriaCertMissingMessage 把「自动挑不到证书」的原因翻成给管理员看的一句话。
func hysteriaCertMissingMessage(reason string) string {
	switch reason {
	case certPickExpired:
		return "Hysteria 入站需要 TLS 证书：证书库里的证书都已过期，请先到『TLS 证书库』更新一张再回来"
	case certPickNoKey:
		return "Hysteria 入站需要 TLS 证书：证书库里的证书缺少证书或私钥，请先到『TLS 证书库』补全一张再回来"
	case certPickDBError:
		return "Hysteria 入站需要 TLS 证书：读取证书库失败，请稍后重试或到『TLS 证书库』手动选一张"
	case certPickNoMatch:
		return "Hysteria 入站需要 TLS 证书：证书库里没有与该域名匹配的证书（域名不一致的证书握手会失败，故不自动选用）。请先到『TLS 证书库』为该域名签/导一张，或在下方手动指定"
	}
	return "Hysteria 入站需要 TLS 证书：证书库为空，请先到『TLS 证书库』添加一张再回来"
}

// autoPickHysteriaCert 在「没指定证书」时从证书库自动挑一张，把 tlsCertId 写回请求。
//
// 为什么要有这一步：前端建 HY2 时不带 tls_cert_id，而内核没有证书起不来 —— 于是
// 「证书库里明明有证书」也照样卡在「必须选择 TLS 证书」，用户得先去别处手抄一个 id。
//
// 沿用优先：请求带了 tls_cert_id、或编辑时原配置已经引用了证书 → 一个字都不改
// （避免每次改备注都换证书）。
//
// 返回非空 = 没挑到，值是原因常量（只用于报错文案）。
func (d *NodeDomain) autoPickHysteriaCert(c *gin.Context, node *NodeRecord, input map[string]any, prevStream any) string {
	if v, present := input["tls_cert_id"]; present && v != nil && inputInt(v) > 0 {
		return ""
	}
	if jsonInt(jsonGet(prevStream, "tlsSettings", xray.CertRefKey)) > 0 {
		return ""
	}

	certs, err := d.store.XrayCertificates(ctxOf(c))
	if err != nil {
		// 读库失败不能说成「库是空的」：用户会跑去建证书而白忙一场。
		return certPickDBError
	}
	if len(certs) == 0 {
		return certPickEmpty
	}

	sni := strings.TrimSpace(nodeStringField(input, "tls_server_name"))
	if sni == "" {
		sni = strings.TrimSpace(jsonGetString(prevStream, "tlsSettings", "serverName"))
	}
	id, live, material := pickCertForHysteria(certs, sni, nodeHostName(node), d.now())
	if id > 0 {
		// 必须写成 inputInt 认识的类型：它只处理 jsonNumber / string / bool / int，
		// 直接塞 uint64 会被读成 0（= 没选证书），于是「库里有证书也报证书库为空」。
		input["tls_cert_id"] = int(id)
		return ""
	}
	switch {
	case live == 0 && material == 0:
		return certPickNoKey
	case live == 0:
		return certPickExpired
	case live > 0 && (strings.TrimSpace(sni) != "" || strings.TrimSpace(nodeHostName(node)) != ""):
		// 库里有未过期的证书，但没有一张能覆盖要用的域名 —— 不能拿别的域名凑数。
		return certPickNoMatch
	}
	return certPickEmpty
}

// pickCertForHysteria 按优先级挑一张证书：① 域名与 SNI 一致 ② 域名与节点主机名一致
// ③ 未过期里到期最晚的一张（兜底，先让它跑得起来，用户之后可到表单里换）。
//
// 返回 (证书 id, 未过期张数, 齐材料张数)：id=0 时调用方按后两个数选报错文案。
// 过期判定用 not_after <= now；not_after 读不到（PEM 解析失败）按「不知道」处理 ——
// 不排除它，但优先级排在已知未过期之后（不知道的先别押）。
func pickCertForHysteria(certs []XrayCertificate, sni, nodeHost string, now time.Time) (uint64, int, int) {
	type candidate struct {
		id       uint64
		domain   string
		notAfter *time.Time
	}
	live := make([]candidate, 0, len(certs))
	material := 0
	for i := range certs {
		c := &certs[i]
		// 没有证书或没有私钥的行 xray 用不了：不算候选，也不该被选上。
		if strings.TrimSpace(c.CertPEM) == "" || strings.TrimSpace(c.KeyEnc) == "" {
			continue
		}
		material++
		if c.NotAfter != nil && !c.NotAfter.After(now) {
			continue
		}
		dom := ""
		if c.Domain != nil {
			dom = strings.ToLower(strings.TrimSpace(*c.Domain))
		}
		live = append(live, candidate{id: c.ID, domain: dom, notAfter: c.NotAfter})
	}
	if len(live) == 0 {
		return 0, 0, material
	}

	sni = strings.ToLower(strings.TrimSpace(sni))
	host := strings.ToLower(strings.TrimSpace(nodeHost))

	// ① 与入站 SNI 匹配
	if sni != "" {
		for _, c := range live {
			if certDomainMatches(c.domain, sni) {
				return c.id, len(live), material
			}
		}
	}
	// ② 与节点域名匹配
	if host != "" && host != sni {
		for _, c := range live {
			if certDomainMatches(c.domain, host) {
				return c.id, len(live), material
			}
		}
	}
	// 知道该用哪个域名却一张都不匹配 → 拒绝兜底，让调用方报错。
	//
	// 这里**故意不做「到期最晚的一张」兜底**：证书域名不匹配时客户端按 SNI 握手必失败，
	// 面板却显示建站成功，比直接报错危险得多。线上真踩过：给 KR01 建 HY2 时库里没有
	// kr1.wujiecloud.top，兜底挑了 jk.wujiecloud.top，入站看着正常、实际连不上。
	if sni != "" || host != "" {
		return 0, len(live), material
	}

	// 两个域名都不知道（例如节点没填 host）：只能退到到期最晚的一张。
	best := live[0]
	for _, c := range live[1:] {
		switch {
		case best.notAfter == nil && c.notAfter != nil:
			best = c
		case best.notAfter != nil && c.notAfter != nil && c.notAfter.After(*best.notAfter):
			best = c
		}
	}
	return best.id, len(live), material
}

// certDomainMatches 判断证书登记的域名是否覆盖某个主机名：完全一致，或通配符
// （*.example.com 覆盖 a.example.com 与 example.com 本身）。
func certDomainMatches(certDomain, host string) bool {
	if certDomain == "" || host == "" {
		return false
	}
	if certDomain == host {
		return true
	}
	if strings.HasPrefix(certDomain, "*.") {
		base := certDomain[2:]
		return host == base || strings.HasSuffix(host, "."+base)
	}
	return false
}

// nodeHostName 把节点记录的 host 剥成小写裸主机名（去 scheme / 路径 / 端口 / IPv6 方括号）。
func nodeHostName(node *NodeRecord) string {
	h := strings.TrimSpace(node.Host)
	if i := strings.Index(h, "//"); i >= 0 {
		h = h[i+2:]
	}
	if i := strings.IndexAny(h, "/?"); i >= 0 {
		h = h[:i]
	}
	if strings.HasPrefix(h, "[") { // [::1]:443
		if j := strings.Index(h, "]"); j > 0 {
			h = h[1:j]
		}
	} else if i := strings.LastIndex(h, ":"); i > 0 && strings.Count(h, ":") == 1 {
		h = h[:i] // host:port（有多个冒号的是裸 IPv6，不能截）
	}
	return strings.ToLower(h)
}

// defaultHopInterval 是端口跳跃的缺省重拨间隔（秒）：缺省 5 秒；用户不需要手填。
const defaultHopInterval = "5"

// buildHysteriaStream 组装 hysteria 入站的 streamSettings 专有段。
//
// 落库形状（对齐 3x-ui / xray-core 的 hysteria 传输）：
//
//	network          = "hysteria"（内核只认这一个传输名）
//	security         = "tls"
//	hysteriaSettings = {version: 2, udpIdleTimeout, masquerade?}
//	tlsSettings      = {serverName?, alpn: [h3], rejectUnknownSni, enableSessionResumption, tlsCertId}
//	                   ← 只存引用，私钥不进这张表
//	finalmask.quicParams = {congestion?, brutalUp?, brutalDown?, 4 个 QUIC 窗口?, udpHop?: {ports, interval}}
//	finalmask.udp       = [{type: "salamander"}]  ← 只有占位，混淆密码在独立加密列（见 applyHysteriaSalamander）
//
// 硬约束：主端口不许落在跳跃区间内 —— 否则已经到主端口的包会被 REDISECT 规则
// 再绕一圈，连接直接断。
func (d *NodeDomain) buildHysteriaStream(c *gin.Context, input map[string]any, stream *jsObj, prevStream any) bool {
	port := inputInt(input["port"])

	hopPorts := strings.TrimSpace(nodeStringField(input, "hy_hop_ports"))
	hopInterval := strings.TrimSpace(nodeStringField(input, "hy_hop_interval"))
	if hopPorts != "" {
		// interval 只在配了跳跃区间时才有意义；缺省 5 秒（defaultHopInterval）。
		if hopInterval == "" {
			hopInterval = defaultHopInterval
		}
		lo, hi, ok := parsePortSpan(hopPorts)
		if !ok {
			d.fail(c, "跳跃区间格式错误：应形如 20000-30000（端口 1-65535，左端不大于右端）", 500)
			return false
		}
		if port >= lo && port <= hi {
			d.fail(c, "主端口不能落在跳跃区间内（"+hopPorts+"）：主端口自身的流量会被转发规则误绕，连接会断", 500)
			return false
		}
		if _, _, ok := parsePortSpan(hopInterval); !ok {
			d.fail(c, "跳跃间隔格式错误：应形如 5-10", 500)
			return false
		}
	}

	// 主端口不能与区间外的其它启用入站撞车（由调用方统一判定），这里只校验区间自身。
	hy := jo("version", 2)
	idle := int64(60)
	if v, present := input["hy_udp_idle_timeout"]; present && inputInt(v) != 0 {
		idle = inputInt(v)
	}
	hy.put("udpIdleTimeout", idle)
	if masq := hysteriaMasquerade(input); masq != nil {
		hy.put("masquerade", masq)
	}
	stream.put("hysteriaSettings", hy)

	tls := jo()
	sni := strings.TrimSpace(nodeStringField(input, "tls_server_name"))
	if sni == "" {
		// 同证书引用：编辑时没带 SNI 就沿用原值。
		sni = jsonGetString(prevStream, "tlsSettings", "serverName")
	}
	if sni != "" {
		tls.put("serverName", sni)
	}
	// HY2 走 QUIC，协商的 ALPN 固定是 h3；不写这一项时部分客户端/内核组合握手失败。
	// 只对 hysteria 生效，通用 TLS 入站分支不碰。
	tls.put("alpn", jsArr{"h3"})
	// 高级 TLS 开关（3x-ui tlsSettings 同名字段）：缺省 false = 内核默认。
	// 编辑时没带字段就沿用原值，避免一次改备注把开关清零。
	tls.put("rejectUnknownSni", hyBoolField(input, "hy_reject_unknown_sni", prevStream, "tlsSettings", "rejectUnknownSni"))
	tls.put("enableSessionResumption", hyBoolField(input, "hy_session_resumption", prevStream, "tlsSettings", "enableSessionResumption"))
	// 证书走引用：只存证书库 id，渲染时解密注入 certificates。
	certID := int64(0)
	if v, present := input["tls_cert_id"]; present && v != nil {
		certID = inputInt(v)
	}
	if certID <= 0 {
		// 编辑时没带证书字段 → 沿用原引用（避免一次改备注就把证书清掉）。
		certID = jsonInt(jsonGet(prevStream, "tlsSettings", xray.CertRefKey))
	}
	if certID > 0 {
		tls.put(xray.CertRefKey, certID)
	}
	stream.put("tlsSettings", tls)

	// finalmask：congestion、brutal 带宽、QUIC 窗口与端口跳跃都在 quicParams 下。
	quic := jo()
	cong := strings.TrimSpace(nodeStringField(input, "hy_congestion"))
	if cong == "" {
		// 编辑时没带拥塞控制 → 沿用原值（brutal 带宽是否写也取决于它）。
		cong = jsonGetString(prevStream, "finalmask", "quicParams", "congestion")
	}
	if cong != "" {
		quic.put("congestion", cong)
	}
	// 内核硬约束（实测 26.3.27）：force-brutal 必须带上行带宽，否则整份节点配置被
	// 「force-brutal requires up」拒掉 —— 面板要在提交时就拦住。
	if cong == "force-brutal" && hyMbpsField(input, "hy_brutal_up", prevStream, "brutalUp") <= 0 {
		d.fail(c, "force-brutal 必须填上行带宽（Mbps）：内核要求 force-brutal 限定上行，留空会让整份节点配置起不来", 500)
		return false
	}
	// brutalUp/brutalDown 只对 brutal / force-brutal 有意义：其它拥塞算法下内核直接忽略，
	// 写进去只会让表单显示与实际不符。值是带宽字符串（内核口径 `<数字> <单位>`，
	// 单位 bps/Kbps/Mbps/Gbps 都是 bit，裸数字按 bit/s），面板统一按整数 Mbps 收、写成 "N Mbps"。
	if cong == "brutal" || cong == "force-brutal" {
		for _, b := range []struct{ inputKey, wireKey string }{
			{"hy_brutal_up", "brutalUp"},
			{"hy_brutal_down", "brutalDown"},
		} {
			if mbps := hyMbpsField(input, b.inputKey, prevStream, b.wireKey); mbps > 0 {
				quic.put(b.wireKey, strconv.FormatInt(mbps, 10)+" Mbps")
			}
		}
	}
	// QUIC 接收窗口（字节）：0/未填不写（内核默认）；非 0 时内核要求至少 16384，先在这里拦。
	for _, w := range []struct{ inputKey, wireKey, label string }{
		{"hy_init_stream_window", "initStreamReceiveWindow", "初始流接收窗口"},
		{"hy_max_stream_window", "maxStreamReceiveWindow", "最大流接收窗口"},
		{"hy_init_conn_window", "initConnectionReceiveWindow", "初始连接接收窗口"},
		{"hy_max_conn_window", "maxConnectionReceiveWindow", "最大连接接收窗口"},
	} {
		n := hyIntField(input, w.inputKey, prevStream, "finalmask", "quicParams", w.wireKey)
		if n == 0 {
			continue
		}
		if n < minQuicWindow {
			d.fail(c, "HY2 的 "+w.label+"至少 16384 字节，或留空/填 0 用内核默认（内核会拒掉更小的值）", 500)
			return false
		}
		quic.put(w.wireKey, n)
	}
	if hopPorts != "" {
		quic.put("udpHop", jo("ports", hopPorts, "interval", hopInterval))
	}
	if len(quic.keys) > 0 {
		stream.put("finalmask", jo("quicParams", quic))
	}
	return true
}

// minQuicWindow 是 xray-core 对 QUIC 接收窗口的下限（非 0 时必须 ≥ 16384，否则整份节点
// 配置被拒：`InitStreamReceiveWindow must be at least 16384`）。
const minQuicWindow = 16384

// salamanderMaskPlaceholder 是 finalmask.udp 里 salamander 段的**落库形态**：
// 只有 type，没有 settings —— 真密码在 xray_inbounds.salamander_password（AES 密文），
// 渲染时才解密注入成 {"type":"salamander","settings":{"password":"..."}}。
//
// 为什么不写 `"settings":{"password":"***"}` 之类的占位值：占位一旦被谁当真配置下发，
// 内核不报错（实测 26.3.27 空密码照样 Configuration OK），客户端却会拿占位当 key，
// 表现成「握手成功但连不通」，排查成本极高。宁可不写键。
func salamanderMaskPlaceholder() *jsObj { return jo("type", "salamander") }

// applyHysteriaSalamander 决定 HY2 入站的 salamander 混淆状态：写/清/沿用。
//
//   - 请求带 hy_salamander_password 且非空 → 密码进独立加密列，stream_settings 只留占位；
//   - 带字段但为空 → 清除（对齐「清空=清除」），占位一并摘掉，内核就不启用混淆；
//   - 没带字段 → 沿用落库状态（只改备注/改别的参数不能把混淆静默关掉）。
func applyHysteriaSalamander(input map[string]any, stream *jsObj, prevStream any, existing *XrayInboundRecord, in *XrayInboundInput) {
	if _, present := input["hy_salamander_password"]; present {
		password := strings.TrimSpace(nodeStringField(input, "hy_salamander_password"))
		in.SalamanderPasswordSet = true
		if password == "" {
			// 清除：不写 finalmask.udp（stream 是新建的，本来也没有键要删）。
			in.SalamanderPassword = nil
			return
		}
		in.SalamanderPassword = &password
		setSalamanderUDP(stream, jsArr{salamanderMaskPlaceholder()})
		return
	}

	// 未带字段：库里已有密码 → 回写占位（buildHysteriaStream 每次都是重建 finalmask，
	// 不回写就等于把混淆关掉）。
	if existing != nil && existing.SalamanderPassword != nil && *existing.SalamanderPassword != "" {
		setSalamanderUDP(stream, jsArr{salamanderMaskPlaceholder()})
		return
	}
	// 兼容手写进库的存量形态（udp 段里自带密码）：原样搬过去，别在保存别的参数时弄丢。
	if udp, ok := jsonGet(prevStream, "finalmask", "udp").(jsArr); ok && len(udp) > 0 {
		setSalamanderUDP(stream, udp)
	}
}

// setSalamanderUDP 把 udp 段写进 stream.finalmask，保留同层的 quicParams 等键。
func setSalamanderUDP(stream *jsObj, udp jsArr) {
	finalmask, _ := stream.get("finalmask")
	fm, ok := finalmask.(*jsObj)
	if !ok {
		fm = jo()
	}
	fm.put("udp", udp)
	stream.put("finalmask", fm)
}

// hyBoolField 取布尔开关：请求里带了就按请求；没带则沿用落库原值；都没有 → false（内核默认）。
func hyBoolField(input map[string]any, key string, prev any, path ...string) bool {
	if v, present := input[key]; present && v != nil {
		return inputBool(v)
	}
	return nodeTruthy(jsonGet(prev, path...))
}

// hyIntField 取整型字段（窗口字节数）：请求带了就按请求，没带沿用落库原值。
func hyIntField(input map[string]any, key string, prev any, path ...string) int64 {
	if v, present := input[key]; present && v != nil {
		return inputInt(v)
	}
	return jsonInt(jsonGet(prev, path...))
}

// hyMbpsField 取「整数 Mbps」字段：请求带了就按请求，没带则把落库的带宽字符串换算回来。
func hyMbpsField(input map[string]any, key string, prev any, wireKey string) int64 {
	if v, present := input[key]; present && v != nil {
		return inputInt(v)
	}
	return bandwidthMbps(jsonGetString(prev, "finalmask", "quicParams", wireKey))
}

// bandwidthMbps 把内核的带宽字符串换算成整数 Mbps（"100 Mbps" → 100）。
//
// 内核口径（实测 xray 26.3.27）：`<数字><单位>`，单位 bps/Kbps/Mbps/Gbps 都按 bit 算，
// 裸数字也按 bit/s；单位不认识（如 "12MB/s"）时内核直接「unsupported unit」拒绝。
// 换算不出来 → 0（表单留空重填，避免把原值猜成错的）。
func bandwidthMbps(raw string) int64 {
	s := strings.ToLower(strings.Join(strings.Fields(raw), ""))
	if s == "" {
		return 0
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	switch s[i:] {
	case "", "bps", "bit/s":
		num /= 1e6
	case "kbps":
		num /= 1e3
	case "mbps":
	case "gbps":
		num *= 1e3
	default:
		return 0
	}
	return int64(num + 0.5)
}

// hysteriaMasquerade 组装 masquerade 段；全部为空 → nil（不落库，用内核默认）。
func hysteriaMasquerade(input map[string]any) *jsObj {
	typ := strings.TrimSpace(nodeStringField(input, "hy_masquerade_type"))
	url := strings.TrimSpace(nodeStringField(input, "hy_masquerade_url"))
	dir := strings.TrimSpace(nodeStringField(input, "hy_masquerade_dir"))
	content := nodeStringField(input, "hy_masquerade_content")
	status := int64(0)
	if v, present := input["hy_masquerade_status_code"]; present && v != nil {
		status = inputInt(v)
	}
	if typ == "" && url == "" && dir == "" && content == "" && status == 0 {
		return nil
	}
	m := jo("type", typ)
	switch typ {
	case "proxy":
		m.put("url", url)
	case "file":
		m.put("dir", dir)
	case "string":
		m.put("content", content)
		if status != 0 {
			m.put("statusCode", status)
		}
	}
	return m
}

// parsePortSpan 解析 "A-B" 形式的端口区间；单端口 "A" 视作 A-A。
func parsePortSpan(raw string) (int64, int64, bool) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) > 2 || len(parts) == 0 {
		return 0, 0, false
	}
	lo, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || lo < 1 || lo > 65535 {
		return 0, 0, false
	}
	hi := lo
	if len(parts) == 2 {
		hi, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || hi < 1 || hi > 65535 {
			return 0, 0, false
		}
	}
	if lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// ---- socks / http / tunnel / wireguard 的构造小工具 ----

// defaultTunnelNetwork 是 dokodemo-door 的缺省 L4 网络（3x-ui 表单同款）。
const defaultTunnelNetwork = "tcp,udp"

// tunnelNetwork 读 tunnel 入站的 L4 网络（落 settings.network 的那个值）。
// 第二个返回值 false = 值非法（合法值只有 tcp / udp / tcp,udp，缺省 tcp,udp）。
func tunnelNetwork(input map[string]any) (string, bool) {
	raw, _ := inputString(input["tunnel_network"])
	switch strings.TrimSpace(raw) {
	case "":
		return defaultTunnelNetwork, true
	case "tcp", "udp", "tcp,udp":
		return strings.TrimSpace(raw), true
	}
	return "", false
}

// wireGuardPeers 把「每行一条：公钥 [允许IP,...]」解析成内核的 peers 数组。
// 公钥必须是 32 字节 base64（WireGuard key）；允许IP 可省（服务端仍能握手，
// 只是没有回程路由，实际用不起来）。
func wireGuardPeers(raw string) (jsArr, bool) {
	peers := jsArr{}
	for _, line := range splitLines(raw) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !isWireGuardKey(fields[0]) {
			return nil, false
		}
		peer := jo("publicKey", fields[0])
		allowed := jsArr{}
		for _, chunk := range fields[1:] {
			for _, ip := range strings.Split(chunk, ",") {
				if ip = strings.TrimSpace(ip); ip != "" {
					allowed = append(allowed, ip)
				}
			}
		}
		if len(allowed) > 0 {
			peer.put("allowedIPs", allowed)
		}
		peers = append(peers, peer)
	}
	return peers, true
}

// isWireGuardKey 判断是否为 32 字节的 base64 密钥（带/不带填充都收，
// 客户端导出的多为带填充的 44 字符形态）。
func isWireGuardKey(s string) bool {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if raw, err := enc.DecodeString(s); err == nil && len(raw) == 32 {
			return true
		}
	}
	return false
}

// wireGuardPublicKey 由 WireGuard 私钥推导公钥（同格式：标准 base64 带填充）。
// 服务端私钥只落库、读接口只回这个公钥 —— 管理员拿它配客户端。
func wireGuardPublicKey(privateKey string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil || len(raw) != 32 {
		if raw, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(privateKey)); err != nil || len(raw) != 32 {
			return ""
		}
	}
	pk, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pk)
}

// guardServerNames 对齐 guardServerNames()：REALITY serverNames 与 dest 的静态一致性拦截。
func (d *NodeDomain) guardServerNames(c *gin.Context, node *NodeRecord, input map[string]any, existing *XrayInboundRecord) bool {
	var prevStream any
	if existing != nil {
		prevStream, _ = parseOrderedJS(existing.StreamSettings)
	}
	security := nodeStringField(input, "security")
	if security == "" {
		security = jsonGetString(prevStream, "security")
	}
	if security != "reality" {
		return true
	}

	dest := nodeStringField(input, "dest")
	if dest == "" {
		dest = jsonGetString(prevStream, "realitySettings", "dest")
	}
	host := xray.HostOf(dest)
	if host == "" {
		return true
	}

	names := usableServerNames(stringListInput(input["server_names"], nil))
	suspect, evidence := xray.ServerNameSuspects(dest, names)

	if len(suspect) == 0 || inputBool(input["confirm_server_names"]) {
		return true
	}

	if evidence == xray.CertEvidenceSameSite {
		// 证书取不到 → 退回旧的注册域猜测，文案说明依据是「猜的」。
		d.abort422(c, "这些 serverNames 与 dest（"+host+"）不是同一站点的域名："+
			strings.Join(suspect, "、")+
			"。REALITY 会按客户端 SNI 向 dest 取证书，dest 给不出证书的域名会让被分到的用户全部连不上。"+
			"未能取到 dest 的真实证书，以上按注册域推测；若确认这些域名由 dest 的同一张证书覆盖（CDN 多 SAN），请确认后再保存。")
		return false
	}

	d.abort422(c, "这些 serverNames 不在 dest（"+host+"）出示的证书里："+
		strings.Join(suspect, "、")+
		"。REALITY 会按客户端 SNI 向 dest 取证书，dest 给不出证书的域名会让被分到的用户全部连不上。"+
		"若确认这些域名由 dest 的同一张证书覆盖（CDN 多 SAN），请确认后再保存。")
	return false
}

// realitySettings 对齐 realitySettings()：核心四字段 + 3x-ui 高级字段（显式提交才落库）。
func (d *NodeDomain) realitySettings(input map[string]any, publicKey string) *jsObj {
	dest := nodeStringField(input, "dest")
	if dest == "" {
		dest = xray.DefaultDest
	}
	serverNames := usableServerNames(stringListInput(input["server_names"], nil))
	shortIDs := stringListInput(input["short_ids"], nil)
	if len(shortIDs) == 0 {
		if sid, err := xray.ShortID(4); err == nil {
			shortIDs = []string{sid}
		}
	}

	settings := jo(
		"dest", dest,
		"serverNames", stringsToJSArr(serverNames),
		"shortIds", stringsToJSArr(shortIDs),
		"publicKey", publicKey,
	)

	if v, present := input["xver"]; present && inputInt(v) != 0 {
		settings.put("xver", inputInt(v))
	}
	if v, present := input["show"]; present && inputBool(v) {
		settings.put("show", true)
	}
	if v := strings.TrimSpace(nodeStringField(input, "min_client_ver")); v != "" {
		settings.put("minClientVer", v)
	}
	if v := strings.TrimSpace(nodeStringField(input, "max_client_ver")); v != "" {
		settings.put("maxClientVer", v)
	}
	if v, present := input["max_timediff"]; present && inputInt(v) != 0 {
		settings.put("maxTimediff", inputInt(v))
	}
	if v := strings.TrimSpace(nodeStringField(input, "mldsa65_seed")); v != "" {
		settings.put("mldsa65Seed", v)
	}
	if v := strings.TrimSpace(nodeStringField(input, "master_key_log")); v != "" {
		settings.put("masterKeyLog", v)
	}
	if v, ok := limitFallback(input["limit_fallback_upload"]); ok {
		settings.put("limitFallbackUpload", v)
	}
	if v, ok := limitFallback(input["limit_fallback_download"]); ok {
		settings.put("limitFallbackDownload", v)
	}

	// 客户端段（订阅链接：fp / spx / pqv）
	client := jo()
	if fp := strings.TrimSpace(nodeStringField(input, "fingerprint")); fp != "" {
		client.put("fingerprint", fp)
	}
	if spx, present := input["spider_x"]; present && nodeScalarString(spx) != "" {
		client.put("spiderX", nodeScalarString(spx))
	}
	if pqv := strings.TrimSpace(nodeStringField(input, "mldsa65_verify")); pqv != "" {
		client.put("mldsa65Verify", pqv)
	}
	if len(client.keys) > 0 {
		settings.put("settings", client)
	}
	return settings
}

// limitFallback 对齐 limitFallback()：三字段全 0（或未提交）→ 不落库。
func limitFallback(raw any) (*jsObj, bool) {
	block, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	after := inputInt(block["after_bytes"])
	rate := inputInt(block["bytes_per_sec"])
	burst := inputInt(block["burst_bytes_per_sec"])
	if after == 0 && rate == 0 && burst == 0 {
		return nil, false
	}
	return jo("afterBytes", after, "bytesPerSec", rate, "burstBytesPerSec", burst), true
}

// presentLimitFallback 对齐 presentLimitFallback()：无则三键全 0（供表单绑定）。
func presentLimitFallback(raw any) *jsObj {
	block := jsonObject(raw)
	return jo(
		"after_bytes", jsonInt(valueOrNil(block, "afterBytes")),
		"bytes_per_sec", jsonInt(valueOrNil(block, "bytesPerSec")),
		"burst_bytes_per_sec", jsonInt(valueOrNil(block, "burstBytesPerSec")),
	)
}

// sniffingSettings 对齐 sniffingSettings()：未提交时保持现状默认；默认值不落库。
func sniffingSettings(input map[string]any) []byte {
	raw, present := input["sniffing"]
	s, isObj := raw.(map[string]any)
	if !present || !isObj {
		return phpEncode(jo("enabled", true, "destOverride", jsArr{"http", "tls", "quic"}))
	}

	// `$s['enabled'] ?? true`：缺失与显式 null 都回落 true。
	enabled := true
	if v, ok := s["enabled"]; ok && v != nil {
		enabled = inputBool(v)
	}
	// `array_values(array_filter((array) ($s['destOverride'] ?? [...]), 'is_string'))`
	// —— (array) 把标量包成单元素数组，array_filter 只保留字符串（含空串）。
	destOverride := jsArr{"http", "tls", "quic"}
	if v, ok := s["destOverride"]; ok && v != nil {
		destOverride = jsArr{}
		for _, item := range phpArrayCast(v) {
			if str, isStr := item.(string); isStr {
				destOverride = append(destOverride, str)
			}
		}
	}
	config := jo("enabled", enabled, "destOverride", destOverride)
	if v, ok := s["routeOnly"]; ok && inputBool(v) {
		config.put("routeOnly", true)
	}
	return phpEncode(config)
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

// presentXrayInbound 对齐 present()（字段顺序即 PHP 数组顺序）。
func presentXrayInbound(ib *XrayInboundRecord, userCounts map[string]int64) *jsObj {
	stream, _ := parseOrderedJS(ib.StreamSettings)
	settings, _ := parseOrderedJS(ib.Settings)
	reality := jsonGet(stream, "realitySettings")
	client := jsonGet(reality, "settings")
	xs := jsonGet(stream, "xhttpSettings")

	// 用户全协议通用：任何入站上的 client 都是「全部有套餐的可用用户」，不再按
	// 入站协议去匹配 users.protocol（那样 hy2 入站永远显示 0，而实际它上面有 105 个）。
	// shadowsocks 例外（历史行为：它的人数在订阅里走另一条路）。
	clientCount := int64(0)
	if ib.Protocol != "shadowsocks" {
		for _, n := range userCounts {
			clientCount += n
		}
	}

	var sniffing any
	if parsed, err := parseOrderedJS(ib.Sniffing); err == nil && parsed != nil {
		sniffing = parsed
	} else {
		sniffing = jo("enabled", true, "destOverride", jsArr{"http", "tls", "quic"})
	}

	// wireguard 的 streamSettings 是空的（内核固定 UDP 监听），列表页要显示 udp 而不是默认 tcp。
	network := jsonDefaultString(stream, "tcp", "network")
	if ib.Protocol == "wireguard" {
		network = "udp"
	}

	return jo(
		"id", ib.ID,
		"tag", ib.Tag,
		"remark", nullableStr(ib.Remark),
		"protocol", ib.Protocol,
		"port", ib.Port,
		"listen", ib.Listen,
		"network", network,
		"security", jsonDefaultString(stream, "none", "security"),
		"sniffing", sniffing,
		"dest", nullableValue(reality, "dest"),
		"server_names", arrayValues(jsonGet(reality, "serverNames")),
		"short_ids", arrayValues(jsonGet(reality, "shortIds")),
		"public_key", nullableValue(reality, "publicKey"),
		"has_private_key", ib.RealityPrivateKey != nil,
		"show", jsonBoolAt(reality, "show"),
		"xver", jsonIntAt(reality, "xver"),
		"min_client_ver", jsonDefaultString(reality, "", "minClientVer"),
		"max_client_ver", jsonDefaultString(reality, "", "maxClientVer"),
		"max_timediff", jsonIntAt(reality, "maxTimediff"),
		"mldsa65_seed", jsonDefaultString(reality, "", "mldsa65Seed"),
		"mldsa65_verify", jsonDefaultString(client, "", "mldsa65Verify"),
		"master_key_log", jsonDefaultString(reality, "", "masterKeyLog"),
		"fingerprint", jsonDefaultString(client, "", "fingerprint"),
		"spider_x", stringFirstNonEmpty(jsonGetString(client, "spiderX"), jsonGetString(reality, "spiderX")),
		"limit_fallback_upload", presentLimitFallback(jsonGet(reality, "limitFallbackUpload")),
		"limit_fallback_download", presentLimitFallback(jsonGet(reality, "limitFallbackDownload")),
		"ws_path", nullablePath(stream, "wsSettings", "path"),
		"ws_host", nullablePath(stream, "wsSettings", "host"),
		"httpupgrade_path", nullablePath(stream, "httpupgradeSettings", "path"),
		"httpupgrade_host", nullablePath(stream, "httpupgradeSettings", "host"),
		"tcp_header_type", jsonFirstNonEmpty(stream, "none", "tcpSettings", "header", "type"),
		"tcp_header_path", jsonDefaultString(stream, "", "tcpSettings", "header", "request", "path", "0"),
		"tcp_header_host", tcpHeaderHost(stream),
		"xhttp_path", nullableValue(xs, "path"),
		"xhttp_host", nullableValue(xs, "host"),
		"xhttp_mode", nullableValue(xs, "mode"),
		"xhttp_padding_bytes", jsonDefaultString(xs, "", "xPaddingBytes"),
		"xhttp_padding_obfs_mode", jsonBoolAt(xs, "xPaddingObfsMode"),
		"xhttp_padding_key", jsonDefaultString(xs, "", "xPaddingKey"),
		"xhttp_padding_header", jsonDefaultString(xs, "", "xPaddingHeader"),
		"xhttp_padding_placement", jsonDefaultString(xs, "", "xPaddingPlacement"),
		"xhttp_padding_method", jsonDefaultString(xs, "", "xPaddingMethod"),
		"xhttp_session_id_placement", jsonDefaultString(xs, "", "sessionIDPlacement"),
		"xhttp_session_id_key", jsonDefaultString(xs, "", "sessionIDKey"),
		"xhttp_session_id_table", jsonDefaultString(xs, "", "sessionIDTable"),
		"xhttp_session_id_length", jsonDefaultString(xs, "", "sessionIDLength"),
		"xhttp_seq_placement", jsonDefaultString(xs, "", "seqPlacement"),
		"xhttp_seq_key", jsonDefaultString(xs, "", "seqKey"),
		"xhttp_uplink_data_placement", jsonDefaultString(xs, "", "uplinkDataPlacement"),
		"xhttp_uplink_data_key", jsonDefaultString(xs, "", "uplinkDataKey"),
		"xhttp_uplink_chunk_size", jsonIntAt(xs, "uplinkChunkSize"),
		"xhttp_sc_max_each_post_bytes", jsonDefaultString(xs, "", "scMaxEachPostBytes"),
		"xhttp_sc_max_buffered_posts", jsonIntAt(xs, "scMaxBufferedPosts"),
		"xhttp_sc_min_posts_interval_ms", jsonDefaultString(xs, "", "scMinPostsIntervalMs"),
		"xhttp_sc_stream_up_server_secs", jsonDefaultString(xs, "", "scStreamUpServerSecs"),
		"xhttp_server_max_header_bytes", jsonIntAt(xs, "serverMaxHeaderBytes"),
		"xhttp_uplink_http_method", jsonDefaultString(xs, "", "uplinkHTTPMethod"),
		"xhttp_no_sse_header", jsonBoolAt(xs, "noSSEHeader"),
		"xhttp_no_grpc_header", jsonBoolAt(xs, "noGRPCHeader"),
		"xhttp_headers", headerLinesFrom(jsonGet(xs, "headers")),
		"xhttp_xmux_max_concurrency", jsonDefaultString(xs, "", "xmux", "maxConcurrency"),
		"xhttp_xmux_max_connections", jsonDefaultString(xs, "", "xmux", "maxConnections"),
		"xhttp_xmux_c_max_reuse_times", jsonDefaultString(xs, "", "xmux", "cMaxReuseTimes"),
		"xhttp_xmux_h_max_request_times", jsonDefaultString(xs, "", "xmux", "hMaxRequestTimes"),
		"xhttp_xmux_h_max_reusable_secs", jsonDefaultString(xs, "", "xmux", "hMaxReusableSecs"),
		"xhttp_xmux_h_keep_alive_period", jsonDefaultString(xs, "", "xmux", "hKeepAlivePeriod"),
		"tls_server_name", nullablePath(stream, "tlsSettings", "serverName"),
		"has_tls_cert", jsonArrayLen(jsonGet(stream, "tlsSettings", "certificates")) > 0,
		"ss_method", ssMethod(ib, settings),
		"ss_password_set", ssPasswordSet(ib, settings),
		// socks / http（传统代理）：用户名回显，密码只回「已设置」
		"auth_user", accountUser(ib, settings),
		"auth_password_set", accountPasswordSet(ib, settings),
		// tunnel（dokodemo-door）：转发目标与 L4 网络
		"tunnel_address", nullableValue(settings, "address"),
		"tunnel_port", jsonIntAt(settings, "port"),
		"tunnel_network", jsonDefaultString(settings, "", "network"),
		// wireguard：服务端私钥永不出接口，只回公钥 + 对端清单（公钥不是密钥）
		"wg_mtu", jsonIntAt(settings, "mtu"),
		"wg_public_key", wgPublicKeyOf(ib, settings),
		"wg_peers", wireGuardPeersText(settings),
		"wg_peer_count", jsonArrayLen(jsonGet(settings, "peers")),
		"hy_hop_ports", jsonDefaultString(stream, "", "finalmask", "quicParams", "udpHop", "ports"),
		"hy_hop_interval", jsonDefaultString(stream, "", "finalmask", "quicParams", "udpHop", "interval"),
		"hy_udp_idle_timeout", nullableValue(jsonGet(stream, "hysteriaSettings"), "udpIdleTimeout"),
		"hy_masquerade", nullableValue(jsonGet(stream, "hysteriaSettings"), "masquerade"),
		"hy_congestion", jsonDefaultString(stream, "", "finalmask", "quicParams", "congestion"),
		"hy_reject_unknown_sni", jsonBoolAt(stream, "tlsSettings", "rejectUnknownSni"),
		"hy_session_resumption", jsonBoolAt(stream, "tlsSettings", "enableSessionResumption"),
		"hy_brutal_up", bandwidthMbps(jsonDefaultString(stream, "", "finalmask", "quicParams", "brutalUp")),
		"hy_brutal_down", bandwidthMbps(jsonDefaultString(stream, "", "finalmask", "quicParams", "brutalDown")),
		"hy_init_stream_window", jsonIntAt(stream, "finalmask", "quicParams", "initStreamReceiveWindow"),
		"hy_max_stream_window", jsonIntAt(stream, "finalmask", "quicParams", "maxStreamReceiveWindow"),
		"hy_init_conn_window", jsonIntAt(stream, "finalmask", "quicParams", "initConnectionReceiveWindow"),
		"hy_max_conn_window", jsonIntAt(stream, "finalmask", "quicParams", "maxConnectionReceiveWindow"),
		// salamander 混淆密码只回显「已设置/未设置」，原文永不出接口。
		"hy_salamander_password_set", ib.SalamanderPassword != nil && *ib.SalamanderPassword != "",
		"tls_cert_id", nullableValue(jsonGet(stream, "tlsSettings"), xray.CertRefKey),
		"enabled", ib.Enabled,
		"sort", ib.Sort,
		"client_count", clientCount,
		"created_at", nullableISO(ib.CreatedAt),
	)
}

// ---- 展示小工具 ----

// nullableValue 对应 `$obj['k'] ?? null`。
func nullableValue(obj any, path ...string) any {
	return jsonGet(obj, path...)
}

// nullablePath 取嵌套值（缺失 → null）。
func nullablePath(obj any, path ...string) any {
	return nullableValue(obj, path...)
}

// jsonDefaultString 对应 `(string) ($obj['a']['b'] ?? $def)`。
func jsonDefaultString(obj any, def string, path ...string) string {
	v := jsonGet(obj, path...)
	if v == nil {
		return def
	}
	return nodeScalarString(v)
}

// jsonFirstNonEmpty 取嵌套字符串；缺失/空 → def（PHP 的 `?? 'none'` 只在缺失时兜底，
// 但入站表单里 tcpSettings 缺失与 type 为空等价，这里按「缺失才兜底」精确实现）。
func jsonFirstNonEmpty(obj any, def string, path ...string) string {
	v := jsonGet(obj, path...)
	if v == nil {
		return def
	}
	return nodeScalarString(v)
}

// jsonIntAt 对应 `(int) ($obj['k'] ?? 0)`。
func jsonIntAt(obj any, path ...string) int64 {
	return jsonInt(jsonGet(obj, path...))
}

// jsonBoolAt 对应 `(bool) ($obj['k'] ?? false)`。
func jsonBoolAt(obj any, path ...string) bool {
	return nodeTruthy(jsonGet(obj, path...))
}

// jsonArrayLen 取数组长度（非数组 → 0，对应 `!empty(...)` 的判定）。
func jsonArrayLen(v any) int {
	if arr, ok := v.(jsArr); ok {
		return len(arr)
	}
	if obj, ok := v.(*jsObj); ok {
		return len(obj.keys)
	}
	return 0
}

// arrayValues 对应 `array_values((array) ($obj['k'] ?? []))`。
func arrayValues(v any) jsArr {
	return jsonArrayValues(v)
}

// stringFirstNonEmpty 返回第一个非空字符串。
func stringFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// tcpHeaderHost 对齐 present 里的 response 优先、request 兜底取值。
func tcpHeaderHost(stream any) string {
	if v := jsonGetString(stream, "tcpSettings", "header", "response", "headers", "Host", "0"); v != "" {
		return v
	}
	return jsonGetString(stream, "tcpSettings", "header", "request", "headers", "Host", "0")
}

func ssMethod(ib *XrayInboundRecord, settings any) any {
	if ib.Protocol != "shadowsocks" {
		return nil
	}
	return nullableValue(settings, "method")
}

func ssPasswordSet(ib *XrayInboundRecord, settings any) bool {
	if ib.Protocol != "shadowsocks" {
		return false
	}
	return nodeTruthy(jsonGet(settings, "password"))
}

// accountUser 回显 socks / http 入站的代理账号（其余协议 null）。
func accountUser(ib *XrayInboundRecord, settings any) any {
	if ib.Protocol != "socks" && ib.Protocol != "http" {
		return nil
	}
	return nullableValue(settings, "accounts", "0", "user")
}

// accountPasswordSet 只回「已设置密码」，原文永不出接口（编辑时留空 = 不改）。
func accountPasswordSet(ib *XrayInboundRecord, settings any) bool {
	if ib.Protocol != "socks" && ib.Protocol != "http" {
		return false
	}
	return nodeTruthy(jsonGet(settings, "accounts", "0", "pass"))
}

// wgPublicKeyOf 由落库的服务端私钥现算 WireGuard 公钥（公钥不是密钥，可以回显）。
func wgPublicKeyOf(ib *XrayInboundRecord, settings any) any {
	if ib.Protocol != "wireguard" {
		return nil
	}
	pub := wireGuardPublicKey(jsonGetString(settings, "secretKey"))
	if pub == "" {
		return nil
	}
	return pub
}

// wireGuardPeersText 把 peers 还原成「每行一条：公钥 允许IP,...」，
// 让编辑弹窗能原样回填（对端公钥不是密钥）。
func wireGuardPeersText(settings any) string {
	peers, ok := jsonGet(settings, "peers").(jsArr)
	if !ok {
		return ""
	}
	lines := make([]string, 0, len(peers))
	for _, p := range peers {
		key := jsonGetString(p, "publicKey")
		if key == "" {
			continue
		}
		line := key
		if arr, ok := jsonGet(p, "allowedIPs").(jsArr); ok && len(arr) > 0 {
			ips := make([]string, 0, len(arr))
			for _, ip := range arr {
				ips = append(ips, nodeScalarString(ip))
			}
			line += " " + strings.Join(ips, ",")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// stringsToJSArr 把 []string 转成 jsArr。
func stringsToJSArr(in []string) jsArr {
	out := make(jsArr, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
