package admin

// 远程证书自动续签：把「快到期、且有入站引用」的证书交给该域名落地的节点重签，
// 回执 PEM 覆盖证书库的**同一行**，再 bump 引用它的节点配置版本让它重拉配置。
//
// 为什么必须在面板侧做：
//   - 真源是证书库那一行（入站只存 tlsCertId，渲染时把 PEM 内联下发）；
//     在节点上装 acme.sh 自己续签对入站**无效** —— 节点本地换文件不改下发内容。
//   - 面板也不能自己签：域名解析在各台节点上，HTTP-01 得由那台节点完成。
//   ⇒ 面板调度 + 节点签发 + 回执入库 + bump 重拉，四步缺一不可。
//
// 幂等：只在「not_after 落在续签窗口内」时动作；同一轮每张证书最多签一次；
// 失败只记原因不抛（避免整条定时任务标红），下次触发再试。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

const (
	// certRenewBeforeDaysEnv / certRenewBeforeDaysDefault 是续签窗口（天）。
	// 默认 21：LE 证书 90 天有效，提前三周留足重试与人工介入的余量。
	certRenewBeforeDaysEnv     = "XRAY_CERT_RENEW_BEFORE_DAYS"
	certRenewBeforeDaysDefault = 21
	// certRenewMethod 是自动续签使用的 ACME 验证方式（节点 80 端口做 HTTP-01）。
	certRenewMethod = "http-01"
	// certRenewMaxReasons 是汇总里最多回几条原因（避免一次刷屏）。
	certRenewMaxReasons = 20
)

// CertRenewResult 是一次自动续签的结果口径（接口回执与定时任务日志共用）。
type CertRenewResult struct {
	Checked int // 库里证书总数
	Due     int // 进入续签窗口的张数
	Renewed int // 实际重签成功
	Failed  int // 重签失败
	Skipped int // 跳过（无到期时间 / 无入站引用 / 找不到可签节点）
	Bumped  int // 因续签而 bump 配置版本的节点数
	Reasons []string
}

// certRenewBeforeDays 读续签窗口（天），非法值回默认。
func certRenewBeforeDays() int {
	raw := strings.TrimSpace(os.Getenv(certRenewBeforeDaysEnv))
	if raw == "" {
		return certRenewBeforeDaysDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return certRenewBeforeDaysDefault
	}
	return n
}

// certRenewDue 判断一张证书是否进入续签窗口（now 之后 window 天内到期）。
// 抽成纯函数便于单测：窗口边界、无到期时间、已过期都要能明确判定。
func certRenewDue(notAfter *time.Time, now time.Time, windowDays int) bool {
	if notAfter == nil {
		return false
	}
	return !notAfter.After(now.AddDate(0, 0, windowDays))
}

// RenewDueCertificates 扫证书库，对进入续签窗口的证书逐个续签（定时任务入口）。
func (d *NodeDomain) RenewDueCertificates(ctx context.Context) (CertRenewResult, error) {
	return d.renewCertificates(ctx, certRenewBeforeDays(), false)
}

// RenewAllReferencedCertificates 忽略续签窗口，立刻把所有「有入站引用、且不由面板机 acme.sh
// 维护」的证书重签一遍。
//
// 用途：把「手工导入」的证书一次性切到自动续签轨道（运维入口 --renew-certs-once）。
func (d *NodeDomain) RenewAllReferencedCertificates(ctx context.Context) (CertRenewResult, error) {
	return d.renewCertificates(ctx, 0, true)
}

// renewCertificates 是续签主流程；force=true 时忽略窗口（立刻把这批证书重签一遍）。
func (d *NodeDomain) renewCertificates(ctx context.Context, windowDays int, force bool) (CertRenewResult, error) {
	res := CertRenewResult{}
	certs, err := d.store.XrayCertificates(ctx)
	if err != nil {
		return res, err
	}
	refs, err := d.certRefNodes(ctx)
	if err != nil {
		return res, err
	}
	nodes, err := d.nodeRecordsByID(ctx)
	if err != nil {
		return res, err
	}

	// 面板机 acme.sh 自己维护的域名（界面标「本机目录」）不归这条任务管：
	// 它们由 root crontab 的 `acme.sh --cron` 续签 + 每 6 小时的目录同步刷库，
	// 这里再签一次属于重复签发，而且面板机的 80 端口被 nginx 占着，HTTP-01 多半直接失败。
	local := localCertSourceSet()
	now := d.now()
	for i := range certs {
		c := certs[i]
		res.Checked++

		domain := certDomain(&c)
		if domain != "" && local[strings.ToLower(domain)] {
			res.Skipped++
			res.note("证书「%s」由面板机 acme.sh 维护（本机目录），跳过以免重复签发", c.Name)
			continue
		}
		if !force && !certRenewDue(c.NotAfter, now, windowDays) {
			continue
		}
		res.Due++

		ids := refs[c.ID]
		if len(ids) == 0 {
			res.Skipped++
			res.note("证书「%s」到期前 %d 天内，但没有任何入站引用它 —— 不知去哪台节点续签，跳过",
				c.Name, windowDays)
			continue
		}
		target := d.pickCertRenewNode(&c, ids, nodes)
		if target == nil {
			res.Skipped++
			res.note("证书「%s」引用的节点都不在线/不是 xray 节点，跳过", c.Name)
			continue
		}

		if domain == "" {
			domain = normalizeCertDomain(target.Host)
		}

		if err := d.renewCertificateOnNode(ctx, target, &c, domain); err != nil {
			res.Failed++
			res.note("证书「%s」在节点 %s 续签失败：%v", c.Name, target.Name, err)
			continue
		}
		res.Renewed++

		// 续签只换了库里的 PEM：必须让引用它的节点重新拉配置，否则内核还用旧证书直到下次无关变更。
		for _, nid := range ids {
			if _, err := d.store.BumpConfigVersion(ctx, nid); err != nil {
				res.note("续签后 bump 节点 #%d 配置版本失败：%v", nid, err)
				continue
			}
			res.Bumped++
		}
	}
	return res, nil
}

// certDomain 取证书的域名（去空白；没有则空串）。
func certDomain(c *XrayCertificate) string {
	if c == nil || c.Domain == nil {
		return ""
	}
	return strings.TrimSpace(*c.Domain)
}

// note 追加一条原因（有条数上限）。
func (r *CertRenewResult) note(format string, args ...any) {
	if len(r.Reasons) >= certRenewMaxReasons {
		return
	}
	r.Reasons = append(r.Reasons, fmt.Sprintf(format, args...))
}

// certRefNodes 汇总「证书 id → 引用它的节点 id 列表」。
//
// 引用关系藏在入站的 stream_settings.tlsSettings.tlsCertId 里（JSON），
// 逐节点读入站解析：节点只有个位数，不做额外 SQL 抽象更省事。
func (d *NodeDomain) certRefNodes(ctx context.Context) (map[uint64][]uint64, error) {
	nodes, _, err := d.store.NodesPage(ctx, true, 1, 500)
	if err != nil {
		return nil, err
	}
	out := map[uint64][]uint64{}
	for _, n := range nodes {
		inbounds, err := d.store.XrayInboundsByNode(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		seen := map[uint64]bool{}
		for _, ib := range inbounds {
			id := certIDOfInbound(ib.StreamSettings)
			if id == 0 || seen[id] {
				continue
			}
			seen[id] = true
			out[id] = append(out[id], n.ID)
		}
	}
	return out, nil
}

// nodeRecordsByID 读全量节点，按 id 建索引（选目标节点用）。
func (d *NodeDomain) nodeRecordsByID(ctx context.Context) (map[uint64]*NodeRecord, error) {
	nodes, _, err := d.store.NodesPage(ctx, true, 1, 500)
	if err != nil {
		return nil, err
	}
	out := make(map[uint64]*NodeRecord, len(nodes))
	for i := range nodes {
		out[nodes[i].ID] = &nodes[i]
	}
	return out, nil
}

// pickCertRenewNode 选「这张证书该在哪台节点签」。
func (d *NodeDomain) pickCertRenewNode(cert *XrayCertificate, ids []uint64, nodes map[uint64]*NodeRecord) *NodeRecord {
	domain := ""
	if cert.Domain != nil {
		domain = normalizeCertDomain(*cert.Domain)
	}
	// 先找域名对得上的那台（HTTP-01 要求域名解析到发起签发的机器）。
	if domain != "" {
		for _, id := range ids {
			n := nodes[id]
			if n != nil && n.IsXray() && normalizeCertDomain(n.Host) == domain {
				return n
			}
		}
	}
	// 退而取第一台可用的 xray 节点：域名可能挂在别的接入地址上，但证书是这台在用。
	for _, id := range ids {
		if n := nodes[id]; n != nil && n.IsXray() {
			return n
		}
	}
	return nil
}

// renewCertificateOnNode 让指定节点重签证书，并把 PEM 覆盖进证书库同一行。
//
// 与 certIssue 处理器同一条指令通道（node_api_commands），区别只在于：
//   - 用 context 而非 gin 上下文；
//   - 失败以 error 返回给调用方汇总，不写 HTTP 响应；
//   - 同样要求回执带 cert_pem / key_pem（面板读不到节点磁盘）。
func (d *NodeDomain) renewCertificateOnNode(ctx context.Context, node *NodeRecord, cert *XrayCertificate, domain string) error {
	payload := jo("domain", domain, "method", certRenewMethod)
	out := d.runNodeCommandWait(ctx, node, "cert_issue", payload, d.cfg.CertIssueWait)
	switch {
	case out.FailErr != nil:
		return fmt.Errorf("%s: %w", out.FailOp, out.FailErr)
	case out.Pending:
		return fmt.Errorf("节点 %d 秒内未回执（签发还在跑或节点离线）", d.cfg.CertIssueWait)
	case out.ErrMsg != "":
		return fmt.Errorf("%s", out.ErrMsg)
	}

	certPEM := strings.TrimSpace(jsonGetString(out.Result, "cert_pem"))
	keyPEM := strings.TrimSpace(jsonGetString(out.Result, "key_pem"))
	if !strings.Contains(certPEM, "BEGIN CERTIFICATE") || !strings.Contains(keyPEM, "PRIVATE KEY") {
		return fmt.Errorf("回执里没有 PEM（只回了路径）—— 该 agent 版本不支持把证书交给面板，需先升级 agent")
	}

	in := XrayCertificateInput{Name: cert.Name, CertPEM: certPEM, KeyPEM: keyPEM}
	if domain != "" {
		dm := domain
		in.Domain = &dm
	}
	if err := d.store.UpdateXrayCertificate(ctx, cert.ID, in); err != nil {
		return fmt.Errorf("写回证书库失败: %w", err)
	}
	if d.logger != nil {
		d.logger.Printf("证书自动续签：%s（id=%d）已由节点 %s 重签并覆盖入库", cert.Name, cert.ID, node.Name)
	}
	return nil
}

// xrayCertificateRenew 手动触发自动续签（与每日定时任务同一套实现）。
//
//	POST /admin-api/xray-certificates/renew
//	body {"dry_run": true} → 只报告到期情况与目标节点，不真签、不入库
//
// 留这个口子是为了「上线前先空跑一遍」和「到期前人工催一次」。
func (d *NodeDomain) xrayCertificateRenew(c *gin.Context) {
	input := jsonInput(c)
	dryRun := inputBool(input["dry_run"])

	if dryRun {
		plan, err := d.planCertRenew(ctxOf(c))
		if err != nil {
			d.failServer(c, "生成续签计划", err)
			return
		}
		d.success(c, jo(
			"dry_run", true,
			"window_days", certRenewBeforeDays(),
			"plan", plan.items,
			"checked", plan.result.Checked,
			"due", plan.result.Due,
			"skipped", plan.result.Skipped,
			"reasons", plan.result.Reasons,
		))
		return
	}

	res, err := d.RenewDueCertificates(ctxOf(c))
	if err != nil {
		d.failServer(c, "自动续签", err)
		return
	}
	d.success(c, jo(
		"dry_run", false,
		"window_days", certRenewBeforeDays(),
		"checked", res.Checked,
		"due", res.Due,
		"renewed", res.Renewed,
		"failed", res.Failed,
		"skipped", res.Skipped,
		"bumped_nodes", res.Bumped,
		"reasons", res.Reasons,
	), "续签完成")
}

// certRenewPlan / certRenewPlanItem 是空跑（dry_run）的返回口径。
type certRenewPlan struct {
	result CertRenewResult
	items  []*jsObj
}

// planCertRenew 只做判定，不签发：列出每张到期证书「会由哪台节点续签」。
func (d *NodeDomain) planCertRenew(ctx context.Context) (*certRenewPlan, error) {
	plan := &certRenewPlan{}
	certs, err := d.store.XrayCertificates(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := d.certRefNodes(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := d.nodeRecordsByID(ctx)
	if err != nil {
		return nil, err
	}
	window := certRenewBeforeDays()
	now := d.now()
	local := localCertSourceSet()
	for i := range certs {
		c := certs[i]
		plan.result.Checked++
		domain := certDomain(&c)
		if domain != "" && local[strings.ToLower(domain)] {
			plan.result.Skipped++
			plan.result.note("证书「%s」由面板机 acme.sh 维护（本机目录），不在本任务范围内", c.Name)
			continue
		}
		if !certRenewDue(c.NotAfter, now, window) {
			continue
		}
		plan.result.Due++
		ids := refs[c.ID]
		target := d.pickCertRenewNode(&c, ids, nodes)
		nodeName := ""
		if target != nil {
			nodeName = target.Name
		} else {
			plan.result.Skipped++
			if len(ids) == 0 {
				plan.result.note("证书「%s」没有任何入站引用，无法确定在哪台节点续签", c.Name)
			} else {
				plan.result.note("证书「%s」引用的节点不可用（离线/非 xray）", c.Name)
			}
		}
		plan.items = append(plan.items, jo(
			"id", c.ID,
			"name", c.Name,
			"not_after", nullableISO(c.NotAfter),
			"ref_nodes", ids,
			"target_node", nodeName,
		))
	}
	return plan, nil
}

// certIDOfInbound 从入站的 stream_settings 里读 tlsSettings.tlsCertId。
// 读不到（没引用证书 / JSON 坏了）返回 0 —— 调用方据此跳过。
func certIDOfInbound(stream []byte) uint64 {
	if len(stream) == 0 {
		return 0
	}
	parsed, err := parseOrderedJS(stream)
	if err != nil {
		return 0
	}
	raw := jsonGet(parsed, "tlsSettings", xray.CertRefKey)
	if raw == nil {
		return 0
	}
	if n := inputInt(raw); n > 0 {
		return uint64(n)
	}
	return 0
}
