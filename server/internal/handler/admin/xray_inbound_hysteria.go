package admin

// hysteria（HY2）入站的 streamSettings 组装：QUIC 调优、端口跳跃、
// salamander 混淆、masquerade。从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"strconv"
	"strings"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

// defaultHopInterval 是端口跳跃的缺省重拨间隔（秒）：缺省 30 秒；用户不需要手填。
// 取 30 与两主流客户端对齐：mihomo defaultHopInterval=30（min 5）；v2rayN 订阅导入兜底
// Hysteria2DefaultHopInt=30（Global.cs）。面板填别的值客户端也不认（URI scheme 无此键），
// 与客户端默认一致可保证「面板所见 = 客户端所行」。
const defaultHopInterval = "30"

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
		// interval 只在配了跳跃区间时才有意义；缺省 30 秒（defaultHopInterval）。
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
	hy.Put("udpIdleTimeout", idle)
	if masq := hysteriaMasquerade(input); masq != nil {
		hy.Put("masquerade", masq)
	}
	stream.Put("hysteriaSettings", hy)

	tls := jo()
	sni := strings.TrimSpace(nodeStringField(input, "tls_server_name"))
	if sni == "" {
		// 同证书引用：编辑时没带 SNI 就沿用原值。
		sni = jsonGetString(prevStream, "tlsSettings", "serverName")
	}
	if sni != "" {
		tls.Put("serverName", sni)
	}
	// HY2 走 QUIC，协商的 ALPN 固定是 h3；不写这一项时部分客户端/内核组合握手失败。
	// 只对 hysteria 生效，通用 TLS 入站分支不碰。
	tls.Put("alpn", jsArr{"h3"})
	// 高级 TLS 开关（3x-ui tlsSettings 同名字段）：缺省 false = 内核默认。
	// 编辑时没带字段就沿用原值，避免一次改备注把开关清零。
	tls.Put("rejectUnknownSni", hyBoolField(input, "hy_reject_unknown_sni", prevStream, "tlsSettings", "rejectUnknownSni"))
	tls.Put("enableSessionResumption", hyBoolField(input, "hy_session_resumption", prevStream, "tlsSettings", "enableSessionResumption"))
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
		tls.Put(xray.CertRefKey, certID)
	}
	stream.Put("tlsSettings", tls)

	// finalmask：congestion、brutal 带宽、QUIC 窗口与端口跳跃都在 quicParams 下。
	quic := jo()
	cong := strings.TrimSpace(nodeStringField(input, "hy_congestion"))
	if cong == "" {
		// 编辑时没带拥塞控制 → 沿用原值（brutal 带宽是否写也取决于它）。
		cong = jsonGetString(prevStream, "finalmask", "quicParams", "congestion")
	}
	if cong != "" {
		quic.Put("congestion", cong)
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
				quic.Put(b.wireKey, strconv.FormatInt(mbps, 10)+" Mbps")
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
		quic.Put(w.wireKey, n)
	}
	if hopPorts != "" {
		quic.Put("udpHop", jo("ports", hopPorts, "interval", hopInterval))
	}
	if len(quic.Keys()) > 0 {
		stream.Put("finalmask", jo("quicParams", quic))
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
	finalmask, _ := stream.Get("finalmask")
	fm, ok := finalmask.(*jsObj)
	if !ok {
		fm = jo()
	}
	fm.Put("udp", udp)
	stream.Put("finalmask", fm)
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
		m.Put("url", url)
	case "file":
		m.Put("dir", dir)
	case "string":
		m.Put("content", content)
		if status != 0 {
			m.Put("statusCode", status)
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
