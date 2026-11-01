package admin

// 远程证书签发：面板对裸内核节点下发 cert_issue，成功后把 PEM 写进 TLS 证书库。
//
//	POST /admin-api/xray-certificates/issue
//	body {node_id: 3, domain?: "node.example.com", method?: "http-01",
//	      email?: "ops@example.com", name?: "证书库里的名字"}
//	→ {code:0,data:{certificate:{id,name,domain,not_after}, mode:"created|updated",
//	                cert_path:"...", key_path:"..."}}
//
// 为什么要面板来触发：裸内核节点的证书是节点本机 acme 申请的（也支持手动），
// 面板这边只存「引用」。签发动作放在节点上（它有 80/443、也在目标域名解析上），
// 面板只负责发指令 + 把结果收进证书库 —— 这样入站仍然只写 tlsCertId，
// 私钥永远不进 xray_inbounds.stream_settings。
//
// 通道复用 node_api_commands（与 firewall_hop_apply / sysctl_apply 同一条），不另开队列。
// 老 agent 不认识这个 action 时只会回 `unknown action: ...`，runNodeCommandSync 已把它
// 翻成「请先升级 agent」的可读文案，所以这里不做版本门槛（版本号未知，硬卡会误伤）。

import (
	"strings"

	"controlhub-server/internal/nodeapi"

	"github.com/gin-gonic/gin"
)

const (
	// certIssueMethodDefault 是签发方式的默认值（HTTP-01：节点上 80 端口可直接验证）。
	certIssueMethodDefault = "http-01"
	// certNameMaxLen 与 xray_certificates.name varchar(190) 对齐。
	certNameMaxLen = 190
)

// certIssueMethods 是允许下发的 ACME 验证方式（白名单：面板只转发它认识的方式）。
var certIssueMethods = map[string]bool{
	"http-01":     true,
	"dns-01":      true,
	"tls-alpn-01": true,
}

// certIssue 对裸内核节点签发证书，并把回执里的 PEM 写进证书库。
//
// 节点用请求体里的 node_id 指定（而不是路由里的 :node）：这个接口挂在证书库路径下，
// 前端「签发证书」按钮在入站表单里就能直接调，不必再套一层节点路由。
func (d *NodeDomain) certIssue(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("node_id", "required", "integer", "min:1", "max:2000000000")
	v.field("domain", "nullable", "string", "max:190")
	v.field("method", "nullable", "string", "max:16")
	v.field("email", "nullable", "string", "max:190")
	v.field("name", "nullable", "string", "max:190")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	node, err := d.store.NodeByID(ctxOf(c), uint64(inputInt(input["node_id"])))
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "查询节点", err)
		return
	}
	if node == nil {
		d.notFound(c)
		return
	}
	if !node.IsXray() {
		d.abort422(c, "仅裸内核（xray）节点支持面板签发证书：3x-ui 节点的证书请在 3x-ui 面板里配置")
		return
	}

	// 域名默认取节点 host：证书签给节点自己的域名，入站的 serverName 也就对得上。
	domain := normalizeCertDomain(nodeStringField(input, "domain"))
	if domain == "" {
		domain = normalizeCertDomain(node.Host)
	}
	if !validCertDomain(domain) {
		d.abort422(c, "域名不合法："+domain+"（应为 a.example.com 这样的纯域名，不带协议/端口/路径）")
		return
	}

	method := strings.ToLower(strings.TrimSpace(nodeStringField(input, "method")))
	if method == "" {
		method = certIssueMethodDefault
	}
	if !certIssueMethods[method] {
		d.abort422(c, "不支持的验证方式："+method+"（只接受 http-01 / dns-01 / tls-alpn-01）")
		return
	}

	// 证书库里的名字默认用域名：同一个域名重复签发就覆盖同一行，
	// 入站的 tlsCertId 不会因为重建行而悬空。
	name := strings.TrimSpace(nodeStringField(input, "name"))
	if name == "" {
		name = domain
	}
	if len([]rune(name)) > certNameMaxLen {
		d.abort422(c, "证书名称过长（上限 190 字）")
		return
	}

	payload := jo("domain", domain, "method", method)
	if email := strings.TrimSpace(nodeStringField(input, "email")); email != "" {
		payload.put("email", email)
	}
	out := d.runNodeCommandSync(c, node, nodeapi.ActionCertIssue, payload, d.cfg.CertIssueWait)
	switch {
	case out.FailErr != nil:
		d.failServer(c, out.FailOp, out.FailErr)
		return
	case out.Pending:
		d.fail(c, "节点未在 "+itoaInt(d.cfg.CertIssueWait)+" 秒内回执（签发还在跑或节点离线）：稍后刷新证书库确认，别连续重点（会撞重复签发限流）", 500)
		return
	case out.ErrMsg != "":
		d.fail(c, "证书签发失败："+out.ErrMsg, 500)
		return
	}

	certPath := strings.TrimSpace(jsonGetString(out.Result, "cert_path"))
	keyPath := strings.TrimSpace(jsonGetString(out.Result, "key_path"))
	certPEM := strings.TrimSpace(jsonGetString(out.Result, "cert_pem"))
	keyPEM := strings.TrimSpace(jsonGetString(out.Result, "key_pem"))
	if !strings.Contains(certPEM, "BEGIN CERTIFICATE") || !strings.Contains(keyPEM, "PRIVATE KEY") {
		// 协议里回执只有 cert_path / key_path（节点上的文件路径），面板读不到节点磁盘。
		// 没有 PEM 就写不了证书库 —— 明确说出来，不要存一条只有路径的假记录
		// （入站按 tlsCertId 引用它，渲染时会拿不到证书，hysteria 直接起不来）。
		d.fail(c, "节点已签发但回执里没有 PEM（只回了路径：cert="+orDash(certPath)+" key="+orDash(keyPath)+
			"）。面板读不到节点磁盘，需要 agent 的 cert_issue 回执带上 cert_pem / key_pem 才能写入证书库", 500)
		return
	}

	// 同名行已存在 = 续签：就地覆盖 PEM（与「本机目录同步」同一套语义），
	// 不删了重建 —— 入站的 tlsCertId 指着这一行的 id。
	existing, err := d.store.XrayCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询证书库", err)
		return
	}
	var hit *XrayCertificate
	for i := range existing {
		if existing[i].Name == name {
			hit = &existing[i]
			break
		}
	}
	in := XrayCertificateInput{Name: name, CertPEM: certPEM, KeyPEM: keyPEM}
	if domain != "" {
		dm := domain
		in.Domain = &dm
	}

	mode := "created"
	var saved *XrayCertificatePEM
	if hit != nil {
		if err := d.store.UpdateXrayCertificate(ctxOf(c), hit.ID, in); err != nil {
			d.failServer(c, "更新证书", err)
			return
		}
		mode = "updated"
		saved, err = d.store.XrayCertificatePEM(ctxOf(c), hit.ID)
	} else {
		var created *XrayCertificate
		created, err = d.store.CreateXrayCertificate(ctxOf(c), in)
		if err == nil {
			saved, err = d.store.XrayCertificatePEM(ctxOf(c), created.ID)
		}
	}
	if err != nil {
		d.failServer(c, "保存证书", err)
		return
	}

	// certificate 是给前端直接用的（id → 填进入站的 tls_cert_id；not_after → 提示文案）。
	d.success(c, jo(
		"certificate", jo(
			"id", saved.ID,
			"name", saved.Name,
			"domain", nullableStr(saved.Domain),
			"not_after", nullableISO(saved.NotAfter),
		),
		"mode", mode,
		"cert_path", certPath,
		"key_path", keyPath,
	))
}

// normalizeCertDomain 去掉管理员常粘进来的协议前缀与末尾斜杠（https://a.example.com/ → a.example.com）。
//
// 只做这一种归一化：其它形态（带端口、带路径）一律交给 validCertDomain 判非法报错 ——
// 静默改写一个带 :443 的串可能签出一张错域名的证书，代价远大于让管理员改一次输入。
func normalizeCertDomain(raw string) string {
	s := strings.TrimSpace(raw)
	for _, prefix := range []string{"https://", "http://"} {
		if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
			s = s[len(prefix):]
			break
		}
	}
	return strings.TrimRight(s, "/")
}

// validCertDomain 判定「纯域名」：允许 * 通配与 - / . ，不允许协议、端口、路径。
//
// 刻意不做到 RFC 级校验：证书签发失败时 agent 会回具体原因，面板这一层只需要挡住
// 明显是粘错东西的输入（带 https:// 或 :443 的 host 会被签成错域名）。
func validCertDomain(domain string) bool {
	if domain == "" || len(domain) > 190 {
		return false
	}
	if strings.ContainsAny(domain, "/ \\:@?#") {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	for _, r := range domain {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '*':
		default:
			return false
		}
	}
	return true
}

// orDash 是回执/日志里「空值显示成 -」的小工具。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
