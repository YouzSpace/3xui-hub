package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reality 目标探测（面板入站表单「检测目标」用）。
//
// 面板把候选目标下发到这里，由**节点**发起真实 TLS 握手——REALITY 的伪装回源就是节点去连
// dest，所以只有节点视角的延迟才是有效指标（面板视角在国内场景会完全失真）。
//
// 判定口径与 3x-ui 的 REALITY 目标扫描器一致，四项全过才算可用：
//  1. 协商到 TLS 1.3（REALITY 只支持 1.3）
//  2. ALPN 协商出 h2
//  3. 密钥交换为 X25519 / X25519MLKEM768
//  4. 证书链受信任（按 SNI 验链，含服务端下发的中间证书）
//
// 延迟 = 从开始拨号到握手完成的耗时。内网/保留地址一律跳过（跟随 3x-ui 的 netsafe 口径），
// 避免被当成内网跳板。额外信息：证书链总字节数 >8192 时标 warn——这是面板侧 P1 真机踩坑
// 记录（TLS 记录过大会破坏 REALITY），但字节数与记录大小只是近似关系，故只提示、不否决。

const (
	// realityScanMaxTargets 单次扫描目标上限（面板侧同样校验，双保险）。
	realityScanMaxTargets = 20
	// realityScanConcurrency 并发探测数（20 目标最多两轮，5s 内出结果）。
	realityScanConcurrency = 10
	// realityScanTimeout 单目标拨号+握手总超时。
	realityScanTimeout = 5 * time.Second
	// realityScanChainWarnSize 证书链总字节数告警阈值（近似口径，仅提示）。
	realityScanChainWarnSize = 8192
	// realityScanOutputMax 回执 JSON 上限（面板侧校验放宽到 40000）。
	realityScanOutputMax = 30000
	// realityScanSANMax 从证书 SAN 回填的 serverNames 上限。
	//
	// 以前这里是 8，注释写「xray reality 上限 8」——这是错的：Xray 官方文档
	// （xtls.github.io/config/transports/reality.html）里 serverNames 是普通字符串数组，
	// 没有条数上限，只有 shortIds 有长度限制。8 条的上限会把 SAN 多的大站（如
	// www.amazon.com 的 24 条）截断，面板拿截断后的列表做覆盖比对就必然误报。
	// 这里与面板侧的校验上限对齐（server/xray_inbound.go：server_names max:64）。
	// 回执仍可能被 realityScanOutputMax 砍短，故另有 san_total 记录截断前的真实条数。
	realityScanSANMax = 64
)

// realityScanResult 单目标探测结果（字段名即面板/前端 JSON 契约，snake_case）。
type realityScanResult struct {
	Target      string   `json:"target"`
	Host        string   `json:"host"`
	IP          string   `json:"ip,omitempty"`
	Port        int      `json:"port"`
	Feasible    bool     `json:"feasible"`
	TLSVersion  string   `json:"tls_version,omitempty"`
	ALPN        string   `json:"alpn,omitempty"`
	Curve       string   `json:"curve,omitempty"`
	CertSubject string   `json:"cert_subject,omitempty"`
	CertIssuer  string   `json:"cert_issuer,omitempty"`
	NotAfter    string   `json:"not_after,omitempty"`
	CertValid   bool     `json:"cert_valid"`
	ChainBytes  int      `json:"chain_bytes,omitempty"`
	ChainWarn   bool     `json:"chain_warn,omitempty"`
	ServerNames []string `json:"server_names,omitempty"`
	// SANTotal 是证书里可用 DNS SAN 的总数（截断前）。server_names 可能被
	// realityScanSANMax / 回执体积上限截断，面板靠这个数判断列表是否完整——
	// 只有 len(server_names) >= san_total 时才敢做「不在证书覆盖范围」的判定。
	SANTotal  int    `json:"san_total,omitempty"`
	LatencyMs int    `json:"latency_ms"`
	Reason    string `json:"reason,omitempty"`
}

// runRealityScan 执行一批 Reality 候选目标探测。
// payload = {"targets":["host:port", ...]}；targets 为空时直接失败（候选清单由面板侧维护）。
func runRealityScan(_ Paths, cmd Command) AckResult {
	var payload struct {
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}

	targets := normalizeRealityTargets(payload.Targets)
	if len(targets) == 0 {
		return AckResult{ID: cmd.ID, Success: false, Output: "没有可扫描的目标"}
	}

	results := scanRealityTargets(targets)

	raw, err := json.Marshal(results)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "结果序列化失败: " + err.Error()}
	}
	// 极端情况下（目标数 × 字段全满）超限：丢尾部条目重编，保证回执始终是合法 JSON
	for len(raw) > realityScanOutputMax && len(results) > 1 {
		results = results[:len(results)-1]
		raw, _ = json.Marshal(results)
	}

	return AckResult{ID: cmd.ID, Success: true, Output: string(raw)}
}

// normalizeRealityTargets 规整目标串：去空白、补默认端口、去重、截上限。
func normalizeRealityTargets(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		switch {
		case strings.Contains(t, "["):
			// 已带方括号的 IPv6（可能自带端口），原样
		case strings.Count(t, ":") >= 2:
			t = "[" + t + "]:443" // 裸 IPv6
		case !strings.Contains(t, ":"):
			t += ":443" // 裸域名 / 裸 IPv4
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= realityScanMaxTargets {
			break
		}
	}

	return out
}

// scanRealityTargets 并发探测全部目标，按「可用 → 延迟升序 → 目标名」排序。
func scanRealityTargets(targets []string) []realityScanResult {
	results := make([]realityScanResult, len(targets))
	sem := make(chan struct{}, realityScanConcurrency)
	var wg sync.WaitGroup

	for i, target := range targets {
		host, port, err := splitRealityTarget(target)
		if err != nil {
			results[i] = realityScanResult{Target: target, Reason: err.Error()}
			continue
		}
		// IP 直连目标不带 SNI（跟随 3x-ui：无 SNI 探测，服务名从证书里发现）
		sni := host
		if net.ParseIP(host) != nil {
			sni = ""
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, tgt, dialHost, serverName string, p int) {
			defer wg.Done()
			defer func() { <-sem }()
			r := probeRealityAddr(dialHost, p, serverName, realityScanTimeout, false)
			r.Target = tgt
			results[idx] = r
		}(i, target, host, sni, port)
	}

	wg.Wait()
	sortRealityResults(results)

	return results
}

// splitRealityTarget 拆 "host:port"（缺端口补 443）。
func splitRealityTarget(target string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host = strings.Trim(strings.TrimSpace(target), "[]")
		if host == "" {
			return "", 0, fmt.Errorf("目标为空")
		}
		return host, 443, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("端口非法: %s", portStr)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", 0, fmt.Errorf("主机为空")
	}

	return host, port, nil
}

// probeRealityAddr 探测单个地址：解析 → 内网拦截 → TLS 握手 → 采集判定。
// allowPrivate=false 时拒绝拨向内网/保留地址（测试用例传 true 以便对本地测试服务探测）。
func probeRealityAddr(dialHost string, port int, sni string, timeout time.Duration, allowPrivate bool) realityScanResult {
	res := realityScanResult{Host: dialHost, Port: port}
	if ip := net.ParseIP(dialHost); ip != nil {
		res.IP = ip.String()
	}
	if sni != "" {
		res.Host = sni
	}
	res.Target = net.JoinHostPort(res.Host, strconv.Itoa(port))

	dialIP, err := resolvePublicIP(dialHost, allowPrivate)
	if err != nil {
		res.Reason = err.Error()
		return res
	}
	if res.IP == "" {
		res.IP = dialIP.String()
	}
	addr := net.JoinHostPort(dialIP.String(), strconv.Itoa(port))

	// 密钥交换：REALITY 只认 X25519 系列。Go 1.24 的 ConnectionState 不暴露 CurveID
	// （1.25 才有），故改用「只提供一种曲线」两段式反推 —— 能握手即证明是它协商的，
	// 曲线标签因此是确定的，不是猜的。X25519 不被支持时才多拨一次（当日几乎不存在）。
	var st *tls.ConnectionState
	var latencyMs int64
	curveName := ""
	firstErr := error(nil)
	for i, cid := range []tls.CurveID{tls.X25519, tls.X25519MLKEM768} {
		state, ms, herr := tlsHandshake(addr, sni, cid, timeout)
		if herr != nil {
			if i == 0 {
				firstErr = herr
			}
			latencyMs = ms
			continue
		}
		st = state
		latencyMs = ms
		curveName = realityCurveName(cid)
		break
	}
	if st == nil {
		res.LatencyMs = int(latencyMs)
		res.Reason = "TLS 握手失败: " + firstErr.Error()
		return res
	}

	res.LatencyMs = int(latencyMs)
	res.TLSVersion = tlsVersionName(st.Version)
	res.ALPN = st.NegotiatedProtocol
	res.Curve = curveName

	tls13 := st.Version == tls.VersionTLS13
	h2 := st.NegotiatedProtocol == "h2"

	if len(st.PeerCertificates) == 0 {
		res.Reason = "服务端未提供证书"
		return res
	}

	leaf := st.PeerCertificates[0]
	res.CertSubject = leaf.Subject.CommonName
	if res.CertSubject == "" && len(leaf.DNSNames) > 0 {
		res.CertSubject = leaf.DNSNames[0]
	}
	if len(leaf.Issuer.Organization) > 0 {
		res.CertIssuer = leaf.Issuer.Organization[0]
	} else {
		res.CertIssuer = leaf.Issuer.CommonName
	}
	res.NotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)
	res.ServerNames, res.SANTotal = usableSANs(leaf)

	chainBytes := 0
	for _, c := range st.PeerCertificates {
		chainBytes += len(c.Raw)
	}
	res.ChainBytes = chainBytes
	res.ChainWarn = chainBytes > realityScanChainWarnSize

	verifyHost := sni
	if verifyHost == "" {
		verifyHost = res.CertSubject
	}
	if verifyHost != "" {
		opts := x509.VerifyOptions{
			DNSName:       verifyHost,
			Intermediates: x509.NewCertPool(),
			Roots:         realityScanTestRoots, // nil = 系统根证书（生产路径）
		}
		for _, c := range st.PeerCertificates[1:] {
			opts.Intermediates.AddCert(c)
		}
		if _, verr := leaf.Verify(opts); verr == nil {
			res.CertValid = true
		} else {
			res.Reason = "证书链不受信任"
		}
	} else {
		res.Reason = "证书里没有可用域名"
	}

	// 曲线只允许 X25519 系列（握手成功即已保证），故判据里不再单列
	res.Feasible = tls13 && h2 && res.CertValid
	if !res.Feasible && res.Reason == "" {
		switch {
		case !tls13:
			res.Reason = "服务端未协商 TLS 1.3"
		case !h2:
			res.Reason = "服务端未协商 HTTP/2 (h2)"
		}
	}

	return res
}

// tlsHandshake 单次 TCP 拨号 + TLS 握手（只提供指定曲线），返回连接状态与耗时（毫秒）。
func tlsHandshake(addr, sni string, curve tls.CurveID, timeout time.Duration) (*tls.ConnectionState, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	conn, err := netDialer().DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, time.Since(start).Milliseconds(), err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	cfg := &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true, // 自验链：证书不受信任要作为「一条结果」返回，而不是直接握手失败
		NextProtos:         []string{"h2", "http/1.1"},
		CurvePreferences:   []tls.CurveID{curve},
		MinVersion:         tls.VersionTLS12,
	}
	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, time.Since(start).Milliseconds(), err
	}
	st := tlsConn.ConnectionState()

	return &st, time.Since(start).Milliseconds(), nil
}

// realityScanTestRoots 验链用的根证书池。生产路径留 nil（= 用系统根证书）；
// 测试注入自签 CA 才能覆盖「可用」这条正向路径。
var realityScanTestRoots *x509.CertPool

// resolvePublicIP 解析出拨号 IP；allowPrivate=false 时拦截内网/回环/保留地址。
// 域名解析到任一非公网地址即整体跳过（防 DNS 轮流解析绕过）。
func resolvePublicIP(host string, allowPrivate bool) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && !isPublicIP(ip) {
			return nil, fmt.Errorf("内网/保留地址，已跳过")
		}
		return ip, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("DNS 解析失败: %v", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("DNS 无解析结果")
	}

	var first net.IP
	for _, a := range addrs {
		if !allowPrivate && !isPublicIP(a.IP) {
			return nil, fmt.Errorf("解析到内网/保留地址，已跳过")
		}
		if first == nil {
			first = a.IP
		}
	}

	return first, nil
}

// isPublicIP 是否公网可路由地址（内网/回环/链路本地/组播/文档/基准等一律 false）。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0: // 0.0.0.0/8
			return false
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // 100.64/10 CGNAT
			return false
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0: // 192.0.0.0/24
			return false
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // 198.18/15 基准测试
			return false
		case ip4[0] >= 240: // 240/4 保留
			return false
		}
		return true
	}

	// IPv6：2001:db8::/32 文档段
	if len(ip) >= 4 && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return false
	}

	return true
}

// usableSANs 从叶子证书 SAN 里挑可用域名（去通配符/IP/非法主机名）。
// 返回 (截断到 realityScanSANMax 的列表, 截断前的可用域名总数)：总数让面板能区分
// 「证书里真没有这个名」与「回执里没带全」——后者不能报成配置故障。
func usableSANs(leaf *x509.Certificate) ([]string, int) {
	seen := make(map[string]bool, len(leaf.DNSNames))
	out := make([]string, 0, len(leaf.DNSNames))
	total := 0
	for _, name := range leaf.DNSNames {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || strings.Contains(name, "*") || net.ParseIP(name) != nil || !validHostname(name) {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		total++
		if len(out) < realityScanSANMax {
			out = append(out, name)
		}
	}

	return out, total
}

// validHostname 简单主机名校验（小写字母/数字/连字符/点，不以连字符开头结尾）。
func validHostname(s string) bool {
	if s == "" || len(s) > 253 || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if r == '.' || r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return !strings.HasPrefix(s, "-") && !strings.HasSuffix(s, "-")
}

// sortRealityResults 可用优先 → 延迟升序 → 目标名（保证面板展示顺序稳定）。
func sortRealityResults(rs []realityScanResult) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Feasible != rs[j].Feasible {
			return rs[i].Feasible
		}
		if rs[i].Feasible && rs[i].LatencyMs != rs[j].LatencyMs {
			return rs[i].LatencyMs < rs[j].LatencyMs
		}
		return rs[i].Target < rs[j].Target
	})
}

// tlsVersionName TLS 版本可读名。
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS10:
		return "1.0"
	}

	return ""
}

// realityCurveName 密钥交换曲线可读名。
func realityCurveName(c tls.CurveID) string {
	switch c {
	case tls.X25519:
		return "X25519"
	case tls.X25519MLKEM768:
		return "X25519MLKEM768"
	case tls.CurveP256:
		return "P-256"
	case tls.CurveP384:
		return "P-384"
	case tls.CurveP521:
		return "P-521"
	}

	return ""
}
