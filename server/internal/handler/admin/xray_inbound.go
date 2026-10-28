package admin

// xray 节点入站管理（/admin-api/nodes/{node}/xray-inbounds）。
// 逐条对齐 backend/app/Http/Controllers/Admin/XrayInboundController.php。
//
// 入站协议面板自建：vless / vmess / trojan / shadowsocks
// （含 tcp/ws/grpc/xhttp/httpupgrade 传输、none/tls/reality 安全层；reality 仅 vless）。
// 任何变更后 bump 该节点 config_version → 节点秒级拉取。

import (
	"strconv"
	"strings"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
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
	d.success(c, jo("inbounds", items, "config_version", node.ConfigVersion))
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
	d.success(c, jo(
		"inbound", presentXrayInbound(created, counts),
		"config_version", version,
	))
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
	d.success(c, jo(
		"inbound", presentXrayInbound(updated, counts),
		"config_version", version,
	))
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
	v.field("protocol", "required", "in:vless,vmess,trojan,shadowsocks")
	v.field("tag", "nullable", "string", "max:64", "regex:/^[A-Za-z0-9_-]+$/")
	v.field("remark", "nullable", "string", "max:128")
	v.field("port", "required", "integer", "between:1,65535")
	v.field("listen", "nullable", "string", "max:64")
	v.field("network", "nullable", "in:tcp,ws,grpc,xhttp,httpupgrade")
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
	port := inputInt(input["port"])
	portTaken, err := d.store.XrayInboundPortTaken(ctxOf(c), node.ID, port, excludeID)
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
	default:
		settings = jo()
	}

	// stream_settings
	stream := jo("network", network, "security", security)

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

	if security == "tls" {
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

// guardServerNames 校验 REALITY serverNames 是否被 dest 出示的证书覆盖。
//
// 判据是 dest 的真实证书 SAN，而不是注册域：同一张证书常跨注册域
// （www.samsung.com 的证书含 samsung.cn 的两个域名），按注册域判会把正确配置拦下。
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
	suspect := []string{}
	for _, name := range names {
		if covered, _ := xray.CoveredByCert(name, dest); !covered {
			suspect = append(suspect, name)
		}
	}

	if len(suspect) == 0 || inputBool(input["confirm_server_names"]) {
		return true
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
	case network != "tcp":
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

	clientCount := int64(0)
	if ib.Protocol != "shadowsocks" {
		clientCount = userCounts[ib.Protocol]
	}

	var sniffing any
	if parsed, err := parseOrderedJS(ib.Sniffing); err == nil && parsed != nil {
		sniffing = parsed
	} else {
		sniffing = jo("enabled", true, "destOverride", jsArr{"http", "tls", "quic"})
	}

	return jo(
		"id", ib.ID,
		"tag", ib.Tag,
		"remark", nullableStr(ib.Remark),
		"protocol", ib.Protocol,
		"port", ib.Port,
		"listen", ib.Listen,
		"network", jsonDefaultString(stream, "tcp", "network"),
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

// stringsToJSArr 把 []string 转成 jsArr。
func stringsToJSArr(in []string) jsArr {
	out := make(jsArr, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
