package admin

// hysteria 入站的 TLS 证书引用：自动挑证书、PEM 去重落库、SNI 匹配与
// 挑不到证书的原因文案。从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

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

// certificateIDForPEM 把「手贴的证书 PEM」落到证书库并返回行 id：
// 先按内容匹配（去空白后一字不差），没有就新建一条（名字优先取 serverName）。
// 这样老的「只贴 PEM」用法也会进入引用链，续签统一走证书库。
func (d *NodeDomain) certificateIDForPEM(c *gin.Context, certPEM, keyPEM, sni string) (uint64, bool) {
	certs, err := d.store.XrayCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "读取证书库", err)
		return 0, false
	}
	want := normalizePEMText(certPEM)
	if want != "" {
		for i := range certs {
			if normalizePEMText(certs[i].CertPEM) == want {
				return certs[i].ID, true
			}
		}
	}

	name := sni
	if name == "" {
		sum := sha256.Sum256([]byte(want))
		name = "tls-" + hex.EncodeToString(sum[:4])
	}
	if len([]rune(name)) > certNameMaxLen {
		name = string([]rune(name)[:certNameMaxLen])
	}
	taken := make(map[string]bool, len(certs))
	for i := range certs {
		taken[certs[i].Name] = true
	}
	base := name
	for n := 2; taken[name]; n++ {
		suffix := "-" + strconv.Itoa(n)
		trimmed := base
		if runes := []rune(trimmed); len(runes)+len(suffix) > certNameMaxLen {
			trimmed = string(runes[:certNameMaxLen-len(suffix)])
		}
		name = trimmed + suffix
	}

	in := XrayCertificateInput{Name: name, CertPEM: certPEM, KeyPEM: keyPEM}
	if validCertDomain(name) {
		dm := name
		in.Domain = &dm
	}
	created, err := d.store.CreateXrayCertificate(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "写入证书库", err)
		return 0, false
	}
	return created.ID, true
}

// normalizePEMText 把 PEM 压成「无空白」串用于内容比对（换行/缩进差异不影响判断）。
func normalizePEMText(s string) string { return strings.Join(strings.Fields(s), "") }

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
