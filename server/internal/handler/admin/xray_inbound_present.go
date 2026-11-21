package admin

// 入站记录 → 前端响应体的整形（对齐 present*），以及配套的 JSON 取值小工具。
// 从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"strings"

	"controlhub-server/internal/driver/xray"
)

// presentXrayInbound 对齐 present()（字段顺序即 PHP 数组顺序）。
func presentXrayInbound(ib *XrayInboundRecord, userCounts map[string]int64) *jsObj {
	stream, _ := parseOrderedJS(ib.StreamSettings)
	settings, _ := parseOrderedJS(ib.Settings)
	reality := jsonGet(stream, "realitySettings")
	client := jsonGet(reality, "settings")
	xs := jsonGet(stream, "xhttpSettings")

	// 用户全协议通用：任何入站上的 client 都是「全部有套餐的可用用户」，不再按
	// 入站协议去匹配 users.protocol（那样 hy2 入站永远显示 0，而实际它上面有 105 个）。
	// shadowsocks 以前恒 0（它只有入站级单凭据、没有逐用户 client）；SS-2022 多用户后
	// ss 入站上也逐用户注入 client，那层特判已经没有依据，一并去掉。
	clientCount := int64(0)
	for _, n := range userCounts {
		clientCount += n
	}

	var sniffing any
	if parsed, err := parseOrderedJS(ib.Sniffing); err == nil && parsed != nil {
		sniffing = parsed
	} else {
		sniffing = jo("enabled", true, "destOverride", jsArr{"http", "tls", "quic"})
	}

	// 列表页显示的传输层：wireguard 的 streamSettings 是空的（内核固定 UDP 监听），
	// 要显示 udp 而不是默认 tcp —— 取值来自能力表（PresentNetwork）。
	network := jsonDefaultString(stream, "tcp", "network")
	if presentNetwork := inboundProtocolOf(ib.Protocol).PresentNetwork; presentNetwork != "" {
		network = presentNetwork
	}

	return jo(
		"id", ib.ID,
		"tag", ib.Tag,
		"remark", nullableStr(ib.Remark),
		// 入站归属地区（#入站级分组）：null = 跟随节点地区。
		// region_name 由调用方补（presentXrayInboundWithRegion），这里只出 id。
		"region_id", nullableUint(ib.RegionID),
		"protocol", ib.Protocol,
		"port", ib.Port,
		"listen", ib.Listen,
		"client_host", nullableStr(ib.ClientHost),
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
		// ss 的服务端 PSK 回显原文：它不是「管理员私有」的密钥 —— 每个用户的订阅链接里
		// 都带着它（ss://method:服务端PSK:用户PSK），编辑时要能看见、2022 自动生成的
		// 那一把也要能看见，否则管理员没法核对/复制。其余协议（socks/http 账号密码、
		// wireguard 私钥、salamander）仍然只回「已设置」。
		"ss_password", ssPassword(ib, settings),
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

// presentXrayInboundWithRegion 在 presentXrayInbound 的基础上补 region_name
// （#入站级分组：列表/详情要同时给出归属地区的 id 与名称）。
func presentXrayInboundWithRegion(ib *XrayInboundRecord, userCounts map[string]int64, regionNames map[uint64]string) *jsObj {
	out := presentXrayInbound(ib, userCounts)
	var name any
	if ib.RegionID != nil {
		if n, ok := regionNames[*ib.RegionID]; ok {
			name = n
		}
	}
	out.Put("region_name", name)
	return out
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
		return len(obj.Keys())
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

// ssPassword 回显 ss 入站的服务端 PSK 原文（其余协议 null）。
//
// 为什么不藏着：ss 的 password 是**入站级**凭据，会明文出现在每个用户的订阅链接里
// （"ss://method:服务端PSK:用户PSK"），只回「已设置」既挡不住谁，又让管理员在
// 2022 自动生成密钥后看不见落库的那一把（改一次 method 就没法核对）。
func ssPassword(ib *XrayInboundRecord, settings any) any {
	if ib.Protocol != "shadowsocks" {
		return nil
	}
	return nullableValue(settings, "password")
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
