package admin

// xray 节点出站管理（/admin-api/nodes/{node}/xray-outbounds）。
// 逐条对齐 backend/app/Http/Controllers/Admin/XrayOutboundController.php。
//
// - 结构化 CRUD（协议 / 服务器 / 凭据 / 传输 + 高级 JSON 透传兜底）
// - 目标来源选「本站服务器」→ 自动中转出站（Panel-side 建承接凭据 + 本地 upsert 出站）
// - 协议选 WARP → 走 WARP 注册 + 应用
// - URI 一键导入（不落库，返回预填结构）
// - 连通测试（下发 test_outbound 指令，节点侧 TCP 探活回执）
// - 列表附出站流量（agent 上报的快照）

import (
	"context"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

// 系统保留 tag 与不可作「借道出站」的协议（与 PHP 常量一致）。
var (
	outboundReservedTags       = map[string]bool{"direct": true, "blocked": true, "api": true}
	outboundUnsupportedProxy   = []string{"freedom", "blackhole"}
	outboundLinkSupportedProto = []string{"vless", "vmess", "trojan", "shadowsocks", "socks", "http"}
)

// xrayOutboundIndex 对齐 index()。
func (d *NodeDomain) xrayOutboundIndex(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	outbounds, err := d.store.XrayOutboundsByNode(ctxOf(c), node.ID, false)
	if err != nil {
		d.failServer(c, "查询出站", err)
		return
	}
	traffic, err := d.store.OutboundTraffic(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询出站流量", err)
		return
	}
	targets, err := d.relayTargets(c, node)
	if err != nil {
		return
	}

	items := make(jsArr, 0, len(outbounds))
	names := map[uint64]*string{}
	for i := range outbounds {
		ob := &outbounds[i]
		var tr *OutboundTraffic
		if v, ok := traffic[ob.Tag]; ok {
			tv := v
			tr = &tv
		}
		var targetName *string
		if ob.RelayTargetNodeID != nil {
			name, ok := names[*ob.RelayTargetNodeID]
			if !ok {
				name, err = d.store.NodeNameByID(ctxOf(c), *ob.RelayTargetNodeID)
				if err != nil {
					d.failServer(c, "查询中转目标节点", err)
					return
				}
				names[*ob.RelayTargetNodeID] = name
			}
			targetName = name
		}
		items = append(items, presentXrayOutbound(ob, tr, targetName))
	}

	d.success(c, jo(
		"outbounds", items,
		"relay_targets", targets,
		"config_version", node.ConfigVersion,
	))
}

// xrayOutboundStore 对齐 store()：relay / warp / 普通三条分支。
func (d *NodeDomain) xrayOutboundStore(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)

	switch nodeStringField(input, "target_source") {
	case "relay":
		d.storeRelayOutbound(c, node, input)
		return
	case "":
		// 往下走普通分支
	}
	if nodeStringField(input, "protocol") == "warp" {
		d.storeWarpOutbound(c, node)
		return
	}

	if !d.validateOutboundPayload(c, input) {
		return
	}
	in, ok := d.buildOutboundInput(c, node, input, nil)
	if !ok {
		return
	}
	created, err := d.store.CreateXrayOutbound(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建出站", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo(
		"outbound", presentXrayOutbound(created, nil, nil),
		"config_version", version,
	))
}

// xrayOutboundUpdate 对齐 update()。
func (d *NodeDomain) xrayOutboundUpdate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedOutbound(c, node)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateOutboundPayload(c, input) {
		return
	}
	in, ok := d.buildOutboundInput(c, node, input, existing)
	if !ok {
		return
	}
	updated, err := d.store.UpdateXrayOutbound(ctxOf(c), existing.ID, in)
	if err != nil {
		d.failServer(c, "保存出站", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	var targetName *string
	if updated.RelayTargetNodeID != nil {
		targetName, err = d.store.NodeNameByID(ctxOf(c), *updated.RelayTargetNodeID)
		if err != nil {
			d.failServer(c, "查询中转目标节点", err)
			return
		}
	}
	d.success(c, jo(
		"outbound", presentXrayOutbound(updated, nil, targetName),
		"config_version", version,
	))
}

// xrayOutboundDestroy 对齐 destroy()：WARP 出站走 WARP 面板；中转出站连带清理对方凭据。
func (d *NodeDomain) xrayOutboundDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedOutbound(c, node)
	if !ok {
		return
	}
	if existing.Tag == "warp" {
		d.fail(c, "WARP 出站由 WARP 功能管理，请到 WARP 面板操作", 500)
		return
	}

	if existing.RelayTargetNodeID != nil {
		if !d.deleteRelayOutbound(c, node, existing) {
			return
		}
	} else if err := d.store.DeleteXrayOutbound(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "删除出站", err)
		return
	}

	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo("config_version", version))
}

// xrayOutboundImport 对齐 import()：解析分享链接（不落库）。
func (d *NodeDomain) xrayOutboundImport(c *gin.Context) {
	if _, ok := d.loadXrayNode(c); !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("link", "required", "string", "max:4096")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	parsed, err := xrayParseOutboundLink(nodeStringField(input, "link"))
	if err != nil {
		d.fail(c, err.Error(), 400)
		return
	}
	d.success(c, jo("parsed", parsed))
}

// xrayOutboundLink 对齐 shareLink()：优先取 3x 面板回执，回退本地生成。
func (d *NodeDomain) xrayOutboundLink(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	ob, ok := d.loadOwnedOutbound(c, node)
	if !ok {
		return
	}

	// 1) 中转出站：优先取对方面板生成的完整链接（同 UserAdminService 的 email 口径）。
	if ob.RelayTargetNodeID != nil && ob.RelayTargetInboundID != nil {
		target, err := d.store.NodeByID(ctxOf(c), *ob.RelayTargetNodeID)
		if err != nil && !d.noRowsToNotFoundSilent(err) {
			d.failServer(c, "查询中转目标节点", err)
			return
		}
		if target != nil && !target.IsXray() {
			email := truncateRunes("relay_"+uInt64String(node.ID)+"_"+uInt64String(*ob.RelayTargetInboundID), 128)
			links, err := d.ops.ThreeXClientLinks(ctxOf(c), target, email)
			if err != nil {
				d.logger.Printf("admin shareLink：读取 3x 面板 links 失败，回退本地生成: %v", err)
			} else if len(links) > 0 {
				d.success(c, jo(
					"supported", true,
					"link", links[0],
					"links", stringsToJSArr(links),
					"source", "3x",
				))
				return
			}
		}
	}

	// 2) 本地按协议生成；不支持的协议返回「不支持」说明（前端禁用按钮）。
	link, err := xrayGenerateOutboundLink(ob)
	if err != nil {
		d.fail(c, err.Error(), 400)
		return
	}
	d.success(c, jo(
		"supported", true,
		"link", link,
		"links", jsArr{link},
		"source", "local",
	))
}

// xrayOutboundTest 对齐 test()：下发连通测试并同步等待回执（≤8s）。
func (d *NodeDomain) xrayOutboundTest(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	ob, ok := d.loadOwnedOutbound(c, node)
	if !ok {
		return
	}
	host, port := outboundTarget(ob)
	if host == "" {
		d.fail(c, "该出站没有可测试的目标地址", 400)
		return
	}

	status, output, pending := d.ops.RelayTestTarget(ctxOf(c), node.ID, host, port)
	if pending {
		d.success(c, jo(
			"pending", true,
			"message", "节点未在 8 秒内回执（可能离线），稍后可重试",
		))
		return
	}
	d.success(c, jo("success", status == "success", "output", output))
}

// ---- 内部 ----

// loadOwnedOutbound 取 URL 里的出站并校验归属（PHP ensureOwned → abort(404)）。
func (d *NodeDomain) loadOwnedOutbound(c *gin.Context, node *NodeRecord) (*XrayOutboundRecord, bool) {
	id, ok := parseUint(c.Param("outbound"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	ob, err := d.store.XrayOutboundByID(ctxOf(c), node.ID, id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询出站", err)
		return nil, false
	}
	return ob, true
}

// validateOutboundPayload 对齐 validatePayload()。
func (d *NodeDomain) validateOutboundPayload(c *gin.Context, input map[string]any) bool {
	v := newNodeValidator(ctxOf(c), input)
	// 标识允许中文等任意非空字符（实测 xray 26.3.27 支持中文 tag）。
	v.field("tag", "required", "string", "max:64", "regex:/\\S/")
	v.field("protocol", "required", "in:freedom,blackhole,vmess,vless,trojan,shadowsocks,socks,http,wireguard")
	v.field("remark", "nullable", "string", "max:128")
	v.field("enabled", "nullable", "boolean")
	v.field("sort", "nullable", "integer", "min:0", "max:65535")
	v.field("address", "nullable", "string", "max:255")
	v.field("port", "nullable", "integer", "between:1,65535")
	v.field("user_id", "nullable", "string", "max:255")
	v.field("password", "nullable", "string", "max:255")
	v.field("flow", "nullable", "string", "max:64")
	v.field("encryption", "nullable", "string", "max:64")
	v.field("method", "nullable", "string", "max:64")
	v.field("username", "nullable", "string", "max:255")
	v.field("network", "nullable", "in:tcp,ws,grpc,xhttp,httpupgrade")
	v.field("security", "nullable", "in:none,tls,reality")
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
	v.field("reality_server_name", "nullable", "string", "max:255")
	v.field("reality_public_key", "nullable", "string", "max:255")
	v.field("reality_short_id", "nullable", "string", "max:32")
	v.field("reality_fingerprint", "nullable", "string", "max:32")
	v.field("reality_spider_x", "nullable", "string", "max:255")
	v.field("dialer_proxy", "nullable", "string", "max:64")
	v.field("wg_secret_key", "nullable", "string", "max:255")
	v.field("wg_address", "nullable", "string", "max:512")
	v.field("wg_peer_public_key", "nullable", "string", "max:255")
	v.field("wg_peer_endpoint", "nullable", "string", "max:255")
	v.field("wg_reserved", "nullable", "string", "max:64")
	v.field("advanced_settings", "nullable", "string", "max:65535")
	v.field("advanced_stream", "nullable", "string", "max:65535")

	if !v.passes() {
		d.failNodeValidation(c, v)
		return false
	}
	return true
}

// buildOutboundInput 对齐 fillFromPayload()。
func (d *NodeDomain) buildOutboundInput(c *gin.Context, node *NodeRecord, input map[string]any, existing *XrayOutboundRecord) (XrayOutboundInput, bool) {
	protocol := nodeStringField(input, "protocol")
	tag := nodeStringField(input, "tag")

	lowerTag := strings.ToLower(tag)
	if outboundReservedTags[lowerTag] {
		d.fail(c, "标识 "+tag+" 为系统保留", 500)
		return XrayOutboundInput{}, false
	}
	if lowerTag == "warp" && protocol != "wireguard" {
		d.fail(c, "warp 标识预留给 WARP 出站", 500)
		return XrayOutboundInput{}, false
	}

	var excludeID *uint64
	if existing != nil {
		excludeID = ptrUintOf(existing.ID)
	}
	dup, err := d.store.XrayOutboundTagTaken(ctxOf(c), node.ID, tag, excludeID)
	if err != nil {
		d.failServer(c, "检查出站标识", err)
		return XrayOutboundInput{}, false
	}
	if dup {
		d.fail(c, "标识 "+tag+" 已存在", 500)
		return XrayOutboundInput{}, false
	}

	// 高级 JSON 透传（优先）。
	settingsRaw, ok := parseAdvancedJSON(nodeStringField(input, "advanced_settings"), "advanced_settings")
	if !ok {
		d.fail(c, "JSON 格式不正确", 500)
		return XrayOutboundInput{}, false
	}
	streamRaw, ok := parseAdvancedJSON(nodeStringField(input, "advanced_stream"), "advanced_stream")
	if !ok {
		d.fail(c, "JSON 格式不正确", 500)
		return XrayOutboundInput{}, false
	}

	if settingsRaw == nil {
		settingsRaw = d.buildOutboundSettings(c, protocol, input)
		if settingsRaw == nil {
			return XrayOutboundInput{}, false
		}
	}
	if streamRaw == nil {
		var okStream bool
		streamRaw, okStream = d.buildOutboundStream(c, node, protocol, input, existing)
		if !okStream {
			return XrayOutboundInput{}, false
		}
	}

	in := XrayOutboundInput{
		NodeID:         node.ID,
		Tag:            tag,
		Protocol:       protocol,
		Settings:       settingsRaw,
		StreamSettings: streamRaw,
	}
	// wireguard 私钥分离存储（加密列）。
	if protocol == "wireguard" {
		if sk := nodeStringField(input, "wg_secret_key"); sk != "" {
			in.SecretKeySet = true
			in.SecretKey = &sk
		}
	}
	if remark := nodeStringField(input, "remark"); remark != "" {
		in.Remark = &remark
	}
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

// buildOutboundSettings 对齐 buildSettings()：按协议组装 settings。
//
// 返回 nil 表示已写错误响应。
func (d *NodeDomain) buildOutboundSettings(c *gin.Context, protocol string, input map[string]any) []byte {
	address := nodeStringField(input, "address")
	port := inputInt(input["port"])

	requireField := func(value, field string) (string, bool) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			d.fail(c, "该字段必填", 500)
			_ = field
			return "", false
		}
		return trimmed, true
	}
	requirePort := func(p int64) (int64, bool) {
		if p < 1 || p > 65535 {
			d.fail(c, "端口必填（1-65535）", 500)
			return 0, false
		}
		return p, true
	}

	switch protocol {
	case "freedom":
		return phpEncode(jo("domainStrategy", "AsIs"))
	case "blackhole":
		return phpEncode(jo())
	case "vmess", "vless", "trojan", "shadowsocks", "socks", "http":
	default:
		return phpEncode(jo())
	}

	addr, ok := requireField(address, "address")
	if !ok {
		return nil
	}
	p, ok := requirePort(port)
	if !ok {
		return nil
	}

	switch protocol {
	case "vmess":
		id, ok := requireField(nodeStringField(input, "user_id"), "user_id")
		if !ok {
			return nil
		}
		security := nodeStringField(input, "encryption")
		if security == "" {
			security = "auto"
		}
		return phpEncode(jo("vnext", jsArr{jo(
			"address", addr,
			"port", p,
			"users", jsArr{jo("id", id, "alterId", int64(0), "security", security)},
		)}))
	case "vless":
		id, ok := requireField(nodeStringField(input, "user_id"), "user_id")
		if !ok {
			return nil
		}
		user := jo("id", id, "encryption", "none")
		if flow := nodeStringField(input, "flow"); flow != "" {
			user.put("flow", flow)
		}
		return phpEncode(jo("vnext", jsArr{jo(
			"address", addr,
			"port", p,
			"users", jsArr{user},
		)}))
	case "trojan":
		password, ok := requireField(nodeStringField(input, "password"), "password")
		if !ok {
			return nil
		}
		return phpEncode(jo("servers", jsArr{jo("address", addr, "port", p, "password", password)}))
	case "shadowsocks":
		method, ok := requireField(nodeStringField(input, "method"), "method")
		if !ok {
			return nil
		}
		password, ok := requireField(nodeStringField(input, "password"), "password")
		if !ok {
			return nil
		}
		return phpEncode(jo("servers", jsArr{jo(
			"address", addr, "port", p, "method", method, "password", password,
		)}))
	case "socks", "http":
		return phpEncode(jo("servers", jsArr{jo(
			"address", addr,
			"port", p,
			"users", jsArr{jo(
				"user", nodeStringField(input, "username"),
				"pass", nodeStringField(input, "password"),
			)},
		)}))
	}
	return phpEncode(jo())
}

// buildOutboundStream 对齐 buildStream()：按协议组装 streamSettings。
func (d *NodeDomain) buildOutboundStream(c *gin.Context, node *NodeRecord, protocol string, input map[string]any, existing *XrayOutboundRecord) ([]byte, bool) {
	dialerProxy := strings.TrimSpace(nodeStringField(input, "dialer_proxy"))
	if dialerProxy != "" {
		if !d.validateDialerProxy(c, node, dialerProxy, existing) {
			return nil, false
		}
	}

	if protocol == "freedom" || protocol == "blackhole" {
		return phpEncode(jo()), true
	}

	network := "tcp"
	if s, ok := inputString(input["network"]); ok && s != "" {
		network = s
	}
	security := "none"
	if s, ok := inputString(input["security"]); ok && s != "" {
		security = s
	}

	// 组合合法性（与入站同守）：Reality 仅 VLESS。
	if security == "reality" && protocol != "vless" {
		d.fail(c, "Reality 仅支持 VLESS 协议", 500)
		return nil, false
	}
	if security == "reality" && !phpInArray(network, []string{"tcp", "xhttp", "grpc"}) {
		d.fail(c, "Reality 仅支持 tcp / xhttp / grpc 传输", 500)
		return nil, false
	}

	stream := jo("network", network, "security", security)

	if network == "ws" {
		ws := jo()
		path := nodeStringField(input, "ws_path")
		if path == "" {
			path = "/"
		}
		ws.put("path", path)
		if host := nodeStringField(input, "ws_host"); host != "" {
			ws.put("headers", jo("Host", host))
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

	if security == "tls" {
		tls := jo()
		if sni := nodeStringField(input, "tls_server_name"); sni != "" {
			tls.put("serverName", sni)
		}
		stream.put("tlsSettings", tls)
	} else if security == "reality" {
		reality := jo()
		if v := nodeStringField(input, "reality_server_name"); v != "" {
			reality.put("serverName", v)
		}
		if v := nodeStringField(input, "reality_public_key"); v != "" {
			reality.put("publicKey", v)
		}
		if v := nodeStringField(input, "reality_short_id"); v != "" {
			reality.put("shortId", v)
		}
		fp := nodeStringField(input, "reality_fingerprint")
		if fp == "" {
			fp = "chrome"
		}
		reality.put("fingerprint", fp)
		if v := nodeStringField(input, "reality_spider_x"); v != "" {
			reality.put("spiderX", v)
		}
		stream.put("realitySettings", reality)
	}

	if dialerProxy != "" {
		stream.put("sockopt", jo("dialerProxy", dialerProxy))
	}
	return phpEncode(stream), true
}

// validateDialerProxy 对齐 validateDialerProxy()：必须是同节点内存在的、可作出口的出站。
func (d *NodeDomain) validateDialerProxy(c *gin.Context, node *NodeRecord, tag string, existing *XrayOutboundRecord) bool {
	if existing != nil && tag == existing.Tag {
		d.fail(c, "借道出站不能指向自身", 500)
		return false
	}
	target, err := d.store.XrayOutboundByTag(ctxOf(c), node.ID, tag)
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		d.failServer(c, "查询借道出站", err)
		return false
	}
	if target == nil || (existing != nil && target.ID == existing.ID) {
		d.fail(c, "借道出站 "+tag+" 不存在", 500)
		return false
	}
	if phpInArray(target.Protocol, outboundUnsupportedProxy) {
		d.fail(c, "借道出站 "+tag+"（"+target.Protocol+"）不能作为出口", 500)
		return false
	}
	return true
}

// parseAdvancedJSON 对齐 parseAdvanced()：空 → nil（走结构化组装）；非对象数组 → 报错。
func parseAdvancedJSON(text, field string) ([]byte, bool) {
	if strings.TrimSpace(text) == "" {
		return nil, true
	}
	parsed, err := parseOrderedJS([]byte(text))
	if err != nil {
		return nil, false
	}
	switch parsed.(type) {
	case *jsObj, jsArr:
		return phpEncode(parsed), true
	}
	return nil, false
}

// ---- 中转（本站服务器） ----

// storeRelayOutbound 对齐 storeRelay()：源节点建中转出站 + 目标节点建承接凭据。
func (d *NodeDomain) storeRelayOutbound(c *gin.Context, node *NodeRecord, input map[string]any) {
	v := newNodeValidator(ctxOf(c), input)
	v.field("target_node_id", "required", "integer")
	v.field("target_inbound_id", "required", "integer")
	v.field("tag", "nullable", "string", "max:64")
	v.field("remark", "nullable", "string", "max:128")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	targetNodeID := uint64(inputInt(input["target_node_id"]))
	targetInboundID := uint64(inputInt(input["target_inbound_id"]))

	target, err := d.store.NodeByID(ctxOf(c), targetNodeID)
	if err != nil {
		if d.noRowsToNotFoundSilent(err) {
			d.fail(c, "目标节点不存在", 500)
			return
		}
		d.failServer(c, "查询目标节点", err)
		return
	}

	customTag := strings.TrimSpace(nodeStringField(input, "tag"))
	customRemark := strings.TrimSpace(nodeStringField(input, "remark"))

	// 幂等命中：同一「源节点 × 目标入站」已有出站时不校验自定义标识。
	existingRelay, err := d.store.RelayOutboundByTarget(ctxOf(c), node.ID, targetNodeID, targetInboundID)
	if err != nil {
		d.failServer(c, "查询中转出站", err)
		return
	}
	if customTag != "" && existingRelay == nil {
		tag := sanitizeRelayCustomTag(customTag)
		if tag == "" {
			d.fail(c, "标识格式不正确", 500)
			return
		}
		taken, err := d.store.XrayOutboundTagTaken(ctxOf(c), node.ID, tag, nil)
		if err != nil {
			d.failServer(c, "检查出站标识", err)
			return
		}
		if taken {
			d.fail(c, "标识 "+tag+" 已存在", 500)
			return
		}
		customTag = tag
	}

	ob, err := d.ensureRelay(c, node, target, targetInboundID, customTag, customRemark, existingRelay)
	if err != nil {
		var relayErr *relayFailure
		if asRelayFailure(err, &relayErr) {
			d.fail(c, relayErr.Message, 500)
			return
		}
		d.failServer(c, "建立中转出站", err)
		return
	}

	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	var targetName *string
	name := target.Name
	targetName = &name
	d.success(c, jo(
		"outbound", presentXrayOutbound(ob, nil, targetName),
		"config_version", version,
	))
}

// storeWarpOutbound 对齐 storeWarp()：注册（若未注册）并 apply。
func (d *NodeDomain) storeWarpOutbound(c *gin.Context, node *NodeRecord) {
	if existing, err := d.store.XrayOutboundByTag(ctxOf(c), node.ID, "warp"); err == nil && existing != nil {
		d.fail(c, "该节点已有 WARP 出站，请到 WARP 面板管理（换 IP / 重新应用）", 400)
		return
	}

	account, err := d.store.WarpAccountByNode(ctxOf(c), node.ID)
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		d.failServer(c, "查询 WARP 账户", err)
		return
	}
	newAccount, err := d.ops.WarpRegister(ctxOf(c), node.ID, node.Name, account)
	if err != nil {
		d.fail(c, "WARP 注册失败："+err.Error(), 400)
		return
	}
	if err := d.store.SaveWarpAccount(ctxOf(c), *newAccount); err != nil {
		d.failServer(c, "保存 WARP 账户", err)
		return
	}

	ob, err := d.applyWarp(c, node, newAccount)
	if err != nil {
		d.fail(c, "WARP 应用失败："+err.Error(), 400)
		return
	}

	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	d.success(c, jo(
		"outbound", presentXrayOutbound(ob, nil, nil),
		"config_version", fresh.ConfigVersion,
	))
}

// applyWarp 组装 wireguard 出站并 upsert（tag=warp），bump 版本。
func (d *NodeDomain) applyWarp(c *gin.Context, node *NodeRecord, account *WarpAccountRecord) (*XrayOutboundRecord, error) {
	return d.applyWarpContext(ctxOf(c), node, account)
}

// applyWarpContext 是 applyWarp 的无 HTTP 版本：定时换 IP 的调用方没有 gin.Context。
func (d *NodeDomain) applyWarpContext(ctx context.Context, node *NodeRecord, account *WarpAccountRecord) (*XrayOutboundRecord, error) {
	if account == nil {
		return nil, errRelay("尚未注册 WARP，请先注册")
	}
	if account.PrivateKey == "" {
		return nil, errRelay("WARP 账户缺少私钥，请重新注册")
	}

	// 读已有 warp 出站的 settings，保留管理员在高级 JSON 里改过的参数（对齐 PHP mergeWarpRotation）。
	// 整体重写会把 mtu/domainStrategy/noKernelTun 等冲掉，且每次轮换都重置一次。
	previous := jo()
	if existing, err := d.store.XrayOutboundByTag(ctx, node.ID, "warp"); err == nil && existing != nil && len(existing.Settings) > 0 {
		previous = decodeScheduleConfig(string(existing.Settings))
	}

	// previousPeers = previous.peers[0]（换 IP 时保留 peers 里除账号字段外的自定义项）
	previousPeers := jo()
	if pv, ok := previous.get("peers"); ok {
		if arr, ok := pv.(jsArr); ok && len(arr) > 0 {
			if po, ok := arr[0].(*jsObj); ok {
				previousPeers = po
			}
		}
	}

	// 新 settings = previous（去掉 secretKey/peers）+ 账号相关字段覆盖
	settings := jo()
	for _, k := range previous.keys {
		if k == "secretKey" || k == "peers" {
			continue
		}
		v, _ := previous.get(k)
		settings.put(k, v)
	}
	if len(account.Addresses) > 0 {
		settings.put("address", stringsToJSArr(account.Addresses))
	}
	peerObj := jo()
	for _, k := range previousPeers.keys {
		v, _ := previousPeers.get(k)
		peerObj.put(k, v)
	}
	peerObj.put("publicKey", account.PeerPublicKey)
	peerObj.put("endpoint", account.PeerEndpoint)
	settings.put("peers", jsArr{peerObj})
	if len(account.Reserved) > 0 {
		reserved := make(jsArr, len(account.Reserved))
		for i, v := range account.Reserved {
			reserved[i] = v
		}
		settings.put("reserved", reserved)
	}
	// mtu / domainStrategy / noKernelTun：保留已有或补默认（对齐 PHP 的 ?? 语义）。
	// 不写 domainStrategy 即 FORCE_IP：v4/v6 同时启用，隧道内目标解析随机落两族之一（IPv4-only 节点会不通）。
	if _, ok := settings.get("mtu"); !ok {
		settings.put("mtu", int64(1280))
	}
	if _, ok := settings.get("domainStrategy"); !ok {
		settings.put("domainStrategy", "ForceIPv4v6")
	}
	if _, ok := settings.get("noKernelTun"); !ok {
		settings.put("noKernelTun", true)
	}

	privateKey := account.PrivateKey
	in := XrayOutboundInput{
		NodeID:       node.ID,
		Tag:          "warp",
		Protocol:     "wireguard",
		Settings:     phpEncode(settings),
		SecretKey:    &privateKey,
		SecretKeySet: true,
		Remark:       ptrStringOf("Cloudflare WARP"),
		Enabled:      ptrBoolOf(true),
	}
	ob, err := d.store.UpsertRelayOutbound(ctx, in)
	if err != nil {
		return nil, err
	}

	enabled := true
	if err := d.store.PatchWarpAccount(ctx, node.ID, nil, nil, nil, &enabled); err != nil {
		return nil, err
	}
	if _, err := d.store.BumpConfigVersion(ctx, node.ID); err != nil {
		return nil, err
	}
	return ob, nil
}

// RotateWarp 是「换 IP」的无 HTTP 入口：重新注册设备（保留 license）→ 已应用则重组出站。
// 定时任务 xray:warp-rotate 走这里（PHP 的 WarpService::rotate()）。
func (d *NodeDomain) RotateWarp(ctx context.Context, nodeID uint64) error {
	node, err := d.store.NodeByID(ctx, nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("节点 #%d 不存在", nodeID)
	}
	if !node.IsXray() {
		return fmt.Errorf("节点 #%d 不是 xray 节点", nodeID)
	}

	existing, err := d.store.WarpAccountByNode(ctx, nodeID)
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		return err
	}
	wasEnabled := existing != nil && existing.Enabled

	account, err := d.ops.WarpRegister(ctx, node.ID, node.Name, existing)
	if err != nil {
		return err
	}
	if err := d.store.SaveWarpAccount(ctx, *account); err != nil {
		return err
	}
	if wasEnabled {
		if _, err := d.applyWarpContext(ctx, node, account); err != nil {
			return err
		}
	}

	now := d.now().UTC()
	return d.store.PatchWarpAccount(ctx, nodeID, nil, nil, &now, nil)
}

// deleteRelayOutbound 对齐 deleteRelayOutbound()：清理对方凭据 + 删本地出站。
func (d *NodeDomain) deleteRelayOutbound(c *gin.Context, node *NodeRecord, ob *XrayOutboundRecord) bool {
	targetNodeID := ob.RelayTargetNodeID
	targetInboundID := ob.RelayTargetInboundID
	if targetNodeID != nil && targetInboundID != nil {
		target, err := d.store.NodeByID(ctxOf(c), *targetNodeID)
		if err != nil && !d.noRowsToNotFoundSilent(err) {
			d.failServer(c, "查询中转目标节点", err)
			return false
		}
		if target != nil {
			if target.IsXray() {
				deleted, err := d.store.DeleteRelayClientsBySource(ctxOf(c), target.ID, *targetInboundID, node.ID)
				if err != nil {
					d.failServer(c, "清理中转凭据", err)
					return false
				}
				if deleted > 0 {
					if _, err := d.store.BumpConfigVersion(ctxOf(c), target.ID); err != nil {
						d.failServer(c, "递增目标节点配置版本", err)
						return false
					}
					if _, err := d.store.BumpConfigVersion(ctxOf(c), node.ID); err != nil {
						d.failServer(c, "递增配置版本", err)
						return false
					}
				}
			} else {
				email := truncateRunes("relay_"+uInt64String(node.ID)+"_"+uInt64String(*targetInboundID), 128)
				if err := d.ops.ThreeXDeleteClient(ctxOf(c), target, email); err != nil {
					d.logger.Printf("admin 清理 3x-ui 中转凭据失败（不阻断本地删除）: %v", err)
				}
			}
		}
	}

	if err := d.store.DeleteXrayOutbound(ctxOf(c), ob.ID); err != nil {
		d.failServer(c, "删除出站", err)
		return false
	}
	return true
}

// relayTargets 对齐 relayTargets()：启用服务器 × 启用入站（排除自身）。
func (d *NodeDomain) relayTargets(c *gin.Context, node *NodeRecord) (jsArr, error) {
	targets, err := d.store.RelayTargets(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询中转候选", err)
		return nil, err
	}

	out := jsArr{}
	for _, t := range targets {
		if t.Node.IsXray() {
			for _, in := range t.XrayInbounds {
				if !in.Enabled {
					continue
				}
				out = append(out, jo(
					"node_id", t.Node.ID,
					"node_name", t.Node.Name,
					"inbound_id", in.ID,
					"port", in.Port,
					"protocol", in.Protocol,
					"label", relayLabel(t.Node.Name, in.Port, in.Tag, in.Protocol),
					"supported", in.Protocol != "shadowsocks",
				))
			}
			continue
		}

		// 3x-ui 服务器：node_inbounds 只有 inbound_id/protocol，端口/入站名实时读面板。
		meta := map[int64][2]any{}
		inbounds, err := d.ops.ThreeXInbounds(ctxOf(c), &t.Node)
		if err != nil {
			d.logger.Printf("admin relay 候选：读取 3x 面板入站清单失败，降级为服务器名·协议: %v", err)
			inbounds = nil
		}
		for _, in := range inbounds {
			id := jsonInt(in["id"])
			if id == 0 {
				continue
			}
			remark := strings.TrimSpace(nodeScalarString(in["remark"]))
			if remark == "" {
				remark = nodeScalarString(in["tag"])
			}
			meta[id] = [2]any{jsonInt(in["port"]), remark}
		}

		for _, row := range t.Inbounds {
			var port any
			label := relayLabel(t.Node.Name, 0, "", row.Protocol)
			if info, ok := meta[row.InboundID]; ok {
				port = info[0]
				label = relayLabel(t.Node.Name, jsonInt(info[0]), nodeScalarString(info[1]), row.Protocol)
			}
			out = append(out, jo(
				"node_id", t.Node.ID,
				"node_name", t.Node.Name,
				"inbound_id", row.InboundID,
				"port", port,
				"protocol", row.Protocol,
				"label", label,
				"supported", row.Protocol != "shadowsocks",
			))
		}
	}
	return out, nil
}

// relayLabel 对齐 relayLabel()：「服务器名 · 端口 · 入站名 · 协议」，空段自动跳过。
func relayLabel(nodeName string, port int64, name, protocol string) string {
	parts := []string{}
	if nodeName != "" {
		parts = append(parts, nodeName)
	}
	if port != 0 {
		parts = append(parts, uInt64String(uint64(port)))
	}
	if name != "" {
		parts = append(parts, name)
	}
	if protocol != "" {
		parts = append(parts, protocol)
	}
	return strings.Join(parts, " · ")
}

// presentXrayOutbound 对齐 present()（不吐凭据明文；流量来自快照表）。
func presentXrayOutbound(ob *XrayOutboundRecord, traffic *OutboundTraffic, targetName *string) *jsObj {
	settings, _ := parseOrderedJS(ob.Settings)
	stream, _ := parseOrderedJS(ob.StreamSettings)

	first := jsonGet(settings, "vnext", "0")
	if _, ok := first.(*jsObj); !ok {
		first = jsonGet(settings, "servers", "0")
	}

	var address, port any
	if obj, ok := first.(*jsObj); ok {
		address = valueOrNil(obj, "address")
		port = valueOrNil(obj, "port")
	}

	var relayPort any
	if ob.RelayTargetNodeID != nil {
		relayPort = int64(0)
		if v := jsonGet(settings, "vnext", "0", "port"); v != nil {
			relayPort = jsonInt(v)
		} else if v := jsonGet(settings, "servers", "0", "port"); v != nil {
			relayPort = jsonInt(v)
		}
	}

	dialer := jsonGetString(stream, "sockopt", "dialerProxy")
	var dialerProxy any
	if dialer != "" {
		dialerProxy = dialer
	}

	settingsOut := any(jo())
	if settings != nil {
		settingsOut = settings
	}
	streamOut := any(jo())
	if stream != nil {
		streamOut = stream
	}

	var upload, download int64
	var updatedAt any
	if traffic != nil {
		upload = traffic.Upload
		download = traffic.Download
		updatedAt = nullableStr(traffic.UpdatedAt)
	}

	return jo(
		"id", ob.ID,
		"tag", ob.Tag,
		"protocol", ob.Protocol,
		"remark", nullableStr(ob.Remark),
		"address", address,
		"port", port,
		"network", nullableNested(stream, "network"),
		"security", nullableNested(stream, "security"),
		"dialer_proxy", dialerProxy,
		"relay_target_node_id", nullableUint(ob.RelayTargetNodeID),
		"relay_target_port", relayPort,
		"relay_target_node_name", nullableStr(targetName),
		"enabled", ob.Enabled,
		"sort", ob.Sort,
		"link_supported", phpInArray(ob.Protocol, outboundLinkSupportedProto),
		"settings", settingsOut,
		"stream_settings", streamOut,
		"has_secret_key", ob.SecretKey != nil,
		"upload", upload,
		"download", download,
		"traffic_updated_at", updatedAt,
	)
}

// nullableNested 对应 `$stream['k'] ?? null`。
func nullableNested(obj any, path ...string) any {
	return jsonGet(obj, path...)
}

// outboundTarget 对齐 targetOf()：从出站配置提取测试目标（地址 + 端口）。
func outboundTarget(ob *XrayOutboundRecord) (string, int64) {
	settings, _ := parseOrderedJS(ob.Settings)
	for _, key := range []string{"vnext", "servers"} {
		addr := jsonGetString(settings, key, "0", "address")
		if addr != "" {
			port := jsonInt(jsonGet(settings, key, "0", "port"))
			if port == 0 {
				port = 443
			}
			return addr, port
		}
	}
	if ob.Protocol == "wireguard" {
		endpoint := jsonGetString(settings, "peers", "0", "endpoint")
		if idx := strings.LastIndex(endpoint, ":"); idx > 0 {
			return endpoint[:idx], jsonInt(endpoint[idx+1:])
		}
	}
	return "", 0
}

func ptrBoolOf(v bool) *bool { return &v }

// uInt64String 输出无符号整数的十进制（避免 strconv 到处 import）。
func uInt64String(v uint64) string {
	return strconvFormatUint(v)
}
