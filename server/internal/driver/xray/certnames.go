package xray

import (
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"time"
)

// Reality serverNames 与 dest 的一致性判定。
//
// 历史实现（SameSite）只比注册域：samsung.com 与 samsung.cn 判为跨站，直接拦。
// 但真实证书常跨注册域（www.samsung.com 的证书里有 50 条 SAN，含 .cn 的两个），
// 于是正确配置被误拦。这里改为「以 dest 实际出示的证书为准」：能连上就按证书 SAN 判，
// 连不上才退回注册域猜测。
//
// 已知限制：agent 回执的 server_names 被截断为 8 条（agent 侧 bug，面板无法修），
// 所以巡检比对仍会误报；本文件只负责保存前的静态拦截。

const (
	// certProbeTimeout 是保存入站时探测 dest 证书的超时。面板侧连不通 dest 时必须快速退回。
	certProbeTimeout = 5 * time.Second
	// certCacheTTL 是证书缓存时长，避免连续保存同一 dest 反复握手。
	certCacheTTL = 10 * time.Minute
)

// certProbe 是可替换的证书探测函数（测试注入用）。
var certProbe = probeCertNames

type certCacheEntry struct {
	names   []string
	expires time.Time
}

var (
	certCacheMu sync.Mutex
	certCache   = map[string]certCacheEntry{}
)

// CertServerNames 返回 dest 实际出示的证书里可用的 serverName（非通配符、去重、小写）。
//
// 连不上或解析失败返回 nil，调用方应退回静态判定。
func CertServerNames(dest string) []string {
	host := HostOf(dest)
	if host == "" || isIPLiteral(host) {
		return nil
	}
	key := host + "|" + destPort(dest)

	certCacheMu.Lock()
	if e, ok := certCache[key]; ok && time.Now().Before(e.expires) {
		certCacheMu.Unlock()
		return e.names
	}
	certCacheMu.Unlock()

	names := certProbe(dest)

	certCacheMu.Lock()
	certCache[key] = certCacheEntry{names: names, expires: time.Now().Add(certCacheTTL)}
	certCacheMu.Unlock()
	return names
}

// destPort 取 dest 的端口，缺省 443。
func destPort(dest string) string {
	dest = strings.TrimSpace(dest)
	if strings.HasPrefix(dest, "[") {
		if i := strings.LastIndex(dest, "]"); i >= 0 && i+1 < len(dest) && dest[i+1] == ':' {
			return dest[i+2:]
		}
		return "443"
	}
	if i := strings.LastIndex(dest, ":"); i >= 0 {
		if tail := dest[i+1:]; isAllDigitsASCII(tail) {
			return tail
		}
	}
	return "443"
}

// probeCertNames 对 dest 做一次 TLS 握手，取叶子证书的 DNS SAN。
//
// 证书本身不校验链（REALITY 只关心 dest 会给哪张证书），但要求握手成功。
func probeCertNames(dest string) []string {
	host := HostOf(dest)
	if host == "" {
		return nil
	}
	addr := net.JoinHostPort(host, destPort(dest))

	dialer := &net.Dialer{Timeout: certProbeTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, // 只取证书，不判信任；信任由调用方的业务口径决定
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		return nil
	}
	defer conn.Close()

	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return nil
	}
	return usableCertNames(state.PeerCertificates[0].DNSNames)
}

// usableCertNames 过滤成 REALITY 能用的 serverName：去通配符、去空、去重、小写。
func usableCertNames(dnsNames []string) []string {
	out := make([]string, 0, len(dnsNames))
	seen := make(map[string]bool, len(dnsNames))
	for _, raw := range dnsNames {
		v := strings.ToLower(strings.TrimSpace(raw))
		if v == "" || strings.Contains(v, "*") || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// CoveredByCert 判断 name 是否被 dest 出示的证书覆盖。
//
// 证书里有该名字即通过；证书取不到（面板连不上 dest）时退回 SameSite 猜测。
// ok=false 表示「取到了证书但名字不在里面」——这才是确定的问题。
func CoveredByCert(name, dest string) (covered bool, certain bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false, false
	}
	names := CertServerNames(dest)
	if names == nil {
		// 证书不可得：退回注册域猜测，且不算「确定」
		return SameSite(name, HostOf(dest)), false
	}
	for _, n := range names {
		if n == name {
			return true, true
		}
	}
	return false, true
}
