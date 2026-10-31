package admin

// TLS 证书库（HY2 用）：/admin-api/xray-certificates。
//
// 存在理由：hysteria 强制 TLS，而 xray_inbounds.stream_settings 是明文 JSON 列，
// 把 PEM 私钥塞进去等于私钥明文落库。所以入站的 tlsSettings 只存证书库 id
// （tlsCertId），私钥加密放 xray_certificates.key_enc，渲染时按 id 解密注入。
//
// 这里只做「证书库自身」的增删查：不碰入站引用计数（删证书时被引用的入站会在
// 渲染时拿不到 certificates，行级校验由前端提示 + 渲染结果暴露）。

import (
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// xrayCertificateIndex 对齐 index()：返回证书库列表（私钥永不回显）。
func (d *NodeDomain) xrayCertificateIndex(c *gin.Context) {
	certs, err := d.store.XrayCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询证书库", err)
		return
	}
	// 来源标记不落库：现扫一遍本机目录，能对上的就是「目录同步来的」。
	local := localCertSourceSet()
	items := make(jsArr, 0, len(certs))
	for i := range certs {
		items = append(items, presentXrayCertificate(&certs[i], local))
	}
	d.success(c, jo("certificates", items))
}

// xrayCertificateStore 对齐 store()：新增一条证书。
func (d *NodeDomain) xrayCertificateStore(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("name", "required", "string", "max:190")
	v.field("domain", "nullable", "string", "max:190")
	v.field("cert_pem", "required", "string", "max:65535")
	v.field("key_pem", "required", "string", "max:65535")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	certPEM := strings.TrimSpace(nodeStringField(input, "cert_pem"))
	keyPEM := strings.TrimSpace(nodeStringField(input, "key_pem"))
	if !strings.Contains(certPEM, "BEGIN CERTIFICATE") {
		d.fail(c, "证书必须是 PEM 格式（以 -----BEGIN CERTIFICATE----- 开头）", 500)
		return
	}
	if !strings.Contains(keyPEM, "PRIVATE KEY") {
		d.fail(c, "私钥必须是 PEM 格式（含 PRIVATE KEY 段）", 500)
		return
	}
	// 名字唯一（对齐 xray_certificates_name_unique）：重复时给可读文案而不是 SQL 报错。
	existing, err := d.store.XrayCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询证书库", err)
		return
	}
	name := strings.TrimSpace(nodeStringField(input, "name"))
	for i := range existing {
		if existing[i].Name == name {
			d.fail(c, "证书名称 "+name+" 已存在", 500)
			return
		}
	}

	in := XrayCertificateInput{
		Name:    name,
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
	}
	if domain := strings.TrimSpace(nodeStringField(input, "domain")); domain != "" {
		in.Domain = &domain
	}
	created, err := d.store.CreateXrayCertificate(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "保存证书", err)
		return
	}
	d.success(c, jo("certificate", presentXrayCertificate(created, localCertSourceSet())))
}

// xrayCertificateSyncLocal 扫描本机证书目录（默认 /root/cert，XRAY_CERT_DIR 覆盖）并入库。
//
// 给面板机自己用：acme.sh 续签后点一下（或等每 6 小时的定时任务）就能把新 PEM 刷进库，
// 入站只存 tlsCertId，所以证书一刷新，下次下发配置就生效，入站不用改。
func (d *NodeDomain) xrayCertificateSyncLocal(c *gin.Context) {
	res, err := d.SyncLocalCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "同步本机证书", err)
		return
	}
	reasons := res.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	d.success(c, jo(
		"dir", res.Dir,
		"scanned", res.Scanned,
		"created", res.Created,
		"updated", res.Updated,
		"skipped", res.Skipped,
		"reasons", stringsToJSArr(reasons),
	))
}

// xrayCertificateDestroy 对齐 destroy()。
func (d *NodeDomain) xrayCertificateDestroy(c *gin.Context) {
	id, ok := parseUint(c.Param("certificate"))
	if !ok {
		d.notFound(c)
		return
	}
	if err := d.store.DeleteXrayCertificate(ctxOf(c), id); err != nil {
		d.failServer(c, "删除证书", err)
		return
	}
	d.success(c, jo("deleted", true))
}

// presentXrayCertificate 只输出元信息（私钥绝不回显，PEM 本体也不进列表载荷）。
//
// source 是派生字段（不落库）：能对上本机证书目录的标 local-dir，手工加的标 manual。
func presentXrayCertificate(c *XrayCertificate, local map[string]bool) *jsObj {
	return jo(
		"id", c.ID,
		"name", c.Name,
		"domain", nullableStr(c.Domain),
		"source", certSourceOf(c, local),
		"has_cert", c.CertPEM != "",
		"has_key", c.KeyEnc != "",
		"cert_pem_chars", utf8.RuneCountInString(c.CertPEM),
		"not_after", nullableISO(c.NotAfter),
	)
}
