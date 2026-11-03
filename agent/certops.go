package main

// cert_status / cert_issue（issue #98）：节点侧证书的「看一眼」和「签一张」。
//
// 固定落地路径 /root/cert/<domain>/{fullchain.pem,privkey.pem}（面板按同一约定读，
// 别改）。签发一律交给 acme.sh（--standalone HTTP-01），这里绝不手写 ACME 协议：
// 自己实现校验、重试、账户注册是给生产埋雷。
//
// 面板侧契约（字段名一字不差）：
//
//	cert_status 入参 {"domain":"..."}                         回执 {"exists":true,"not_after":"...","remaining_days":N}
//	cert_issue  入参 {"domain":"...","method":"http-01","email":"..."}
//	            回执 {"ok":true,"not_after":"...","cert_path":"...","key_path":"...","cert_pem":"...","key_pem":"..."}
//
// cert_pem / key_pem 是刚装好的 certFileName / keyFileName 的原文（面板读不到节点磁盘，
// 证书库里的 PEM 只能从这里来）。读不回来时给空串，ok 仍为 true —— 签发/落盘本身已成功。
//
// 失败一律 false + reason（acme.sh 原始输出摘录），不静默。

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 证书目录与 acme.sh 位置做成包级变量：单测改成临时目录，不碰真实节点、不跑真 ACME。
var (
	// certRootDir 证书落地根目录（面板按 /root/cert/<domain>/ 读）。
	certRootDir = "/root/cert"
	// acmeShBin acme.sh 主程序；它的同级目录就是 acme.sh 的 home（放 _ecc 子目录）。
	acmeShBin = "/root/.acme.sh/acme.sh"
)

const (
	// certIssueTimeout ACME 签发的整段上限。HTTP-01 要真的从外网回连 80 端口，
	// 正常 10-60s，排队/重试时更久，给 3 分钟。
	certIssueTimeout = 3 * time.Minute
	// certInstallTimeout --install-cert 只搬文件，很快。
	certInstallTimeout = 60 * time.Second
	// certCmdOutMax acme.sh 输出进回执时的截断长度（面板 ack 上限 40000，这里只留摘录）。
	certCmdOutMax = 1200

	certFileName = "fullchain.pem"
	keyFileName  = "privkey.pem"
)

// certCmdRunner 执行外部命令的缝：单测替换掉，避免真跑 acme.sh / 真连 Let's Encrypt。
type certCmdRunner func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)

var runCertCmd certCmdRunner = execCertCmd

// readCertFile 读证书文件的缝：单测换成必定失败，验证「PEM 读不回来」不会把
// 已经成功的签发拖成失败回执。
var readCertFile = os.ReadFile

// execCertCmd 生产实现：带超时执行并合并输出。
func execCertCmd(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Env = append(os.Environ(), "PATH="+systemPath())
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// certStatusResult cert_status 回执。字段名即面板契约。
type certStatusResult struct {
	Exists bool `json:"exists"`
	// NotAfter RFC3339（UTC）。文件读不出到期日时为空，原因在 reason。
	NotAfter string `json:"not_after,omitempty"`
	// RemainingDays 剩余整天数；已过期给负数（-1 = 已过期），面板据此标「已过期」。
	RemainingDays int    `json:"remaining_days"`
	Reason        string `json:"reason,omitempty"`
}

// certIssueResult cert_issue 回执。字段名即面板契约。
type certIssueResult struct {
	OK       bool   `json:"ok"`
	NotAfter string `json:"not_after,omitempty"`
	CertPath string `json:"cert_path,omitempty"`
	KeyPath  string `json:"key_path,omitempty"`
	// CertPEM / KeyPEM 是刚装好的两个文件的原文：面板读不到节点磁盘，证书库里的 PEM
	// 只能从这里来。刻意不带 omitempty —— 读不回来也明确回空串（且 ok 仍为 true），
	// 面板据此报「回执里没有 PEM」，而不是猜这个字段为什么不见了。
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
	Reason  string `json:"reason,omitempty"`
}

// domainRe 只放行正常域名（至少一个点、各级 1-63、不允许通配符/斜杠/空格）。
// 域名直接拼进 /root/cert/<domain>/ 当目录名，必须先过白名单，否则 ../ 就能出逃。
var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// normalizeDomain 校验并归一化域名。
func normalizeDomain(raw string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(raw))
	if d == "" {
		return "", fmt.Errorf("domain 缺失")
	}
	if len(d) > 253 {
		return "", fmt.Errorf("domain 过长（>253）")
	}
	if net.ParseIP(d) != nil {
		return "", fmt.Errorf("cert_issue/cert_status 只处理域名，不做 IP 证书: %s", d)
	}
	if !domainRe.MatchString(d) {
		return "", fmt.Errorf("domain 非法: %q", raw)
	}
	return d, nil
}

// certPaths 固定落地路径。<domain> 已过白名单，可安全拼接。
func certPaths(domain string) (certPath, keyPath string) {
	dir := filepath.Join(certRootDir, domain)
	return filepath.Join(dir, certFileName), filepath.Join(dir, keyFileName)
}

// readCertNotAfter 读 PEM 证书的到期时间（取第一个 CERTIFICATE 块）。
func readCertNotAfter(path string) (time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return time.Time{}, fmt.Errorf("PEM 里没有 CERTIFICATE 块: %s", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, fmt.Errorf("证书解析失败: %w", err)
		}
		return cert.NotAfter, nil
	}
}

// remainingDays 剩余整天数（向下取整；已过期返回负数）。
func remainingDays(notAfter time.Time, now time.Time) int {
	return int(math.Floor(notAfter.Sub(now).Hours() / 24))
}

// loadCertPEM 读回刚装好的 cert/key 原文（原样，不做裁剪）。
//
// 读失败不返回 error：签发和落盘都已经成功，只是这次回执带不回去 —— 调用方仍报
// ok=true，原因拼进 reason，面板看到空 PEM 会明确提示「回执里没有 PEM」。
func loadCertPEM(certPath, keyPath string) (certPEM, keyPEM, reason string) {
	var problems []string
	if raw, err := readCertFile(certPath); err != nil {
		problems = append(problems, certFileName+" 读取失败: "+err.Error())
	} else {
		certPEM = string(raw)
	}
	if raw, err := readCertFile(keyPath); err != nil {
		problems = append(problems, keyFileName+" 读取失败: "+err.Error())
	} else {
		keyPEM = string(raw)
	}
	return certPEM, keyPEM, strings.Join(problems, "；")
}

// runCertStatus 只读：报节点上有没有这张证书、什么时候到期。
func runCertStatus(_ Paths, cmd Command) AckResult {
	var payload struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}
	domain, err := normalizeDomain(payload.Domain)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: err.Error()}
	}

	certPath, keyPath := certPaths(domain)
	var missing []string
	if !fileExists(certPath) {
		missing = append(missing, certFileName)
	}
	if !fileExists(keyPath) {
		missing = append(missing, keyFileName)
	}
	if len(missing) > 0 {
		res := certStatusResult{
			Exists: false,
			Reason: fmt.Sprintf("证书不存在：%s 下缺少 %s", filepath.Dir(certPath), strings.Join(missing, " / ")),
		}
		return marshalCertAck(cmd.ID, true, res)
	}

	notAfter, err := readCertNotAfter(certPath)
	if err != nil {
		// 文件在但读不出来：如实说「在」，到期日留空 + 原因，别假装没有证书。
		res := certStatusResult{Exists: true, Reason: err.Error()}
		return marshalCertAck(cmd.ID, true, res)
	}

	res := certStatusResult{
		Exists:        true,
		NotAfter:      notAfter.UTC().Format(time.RFC3339),
		RemainingDays: remainingDays(notAfter, time.Now()),
	}
	return marshalCertAck(cmd.ID, true, res)
}

// runCertIssue 用 acme.sh 走 HTTP-01 签发，成功后装到固定路径。
func runCertIssue(_ Paths, cmd Command) AckResult {
	var payload struct {
		Domain string `json:"domain"`
		Method string `json:"method"`
		Email  string `json:"email"`
		// Force = 面板要求「立即重签」，即使 acme.sh 认为还没到续期时间。
		// 手工续签（--renew-certs-once / 后台点「立即续签」）会带上；定时续签不带。
		Force bool `json:"force"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}

	domain, err := normalizeDomain(payload.Domain)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: err.Error()}
	}

	// 只做 HTTP-01（--standalone 占 80）；dns-01 需要 DNS API 凭据，是后续的事。
	method := strings.ToLower(strings.TrimSpace(payload.Method))
	if method == "" {
		method = "http-01"
	}
	if method != "http-01" {
		return AckResult{ID: cmd.ID, Success: false, Output: "method 只支持 http-01，收到: " + payload.Method}
	}

	email := strings.TrimSpace(payload.Email)
	if email != "" && !strings.Contains(email, "@") {
		return AckResult{ID: cmd.ID, Success: false, Output: "email 非法: " + email}
	}

	// acme.sh 不存在就明确报错：不自动下载第三方脚本（面板密钥泄露时这就是个远程执行口）。
	if !fileExists(acmeShBin) {
		return marshalCertAck(cmd.ID, false, certIssueResult{
			Reason: "acme.sh 不存在：" + acmeShBin + "（本 action 不自动安装，请先在节点上装 acme.sh）",
		})
	}

	certPath, keyPath := certPaths(domain)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return marshalCertAck(cmd.ID, false, certIssueResult{Reason: "创建证书目录失败: " + err.Error()})
	}

	ctx := context.Background()

	// 1) 签发。已有证书且没到续期时间时 acme.sh 会直接跳过（非 0 退出），
	//    这不算失败 —— 下面 --install-cert 把已有证书装到固定路径同样可用。
	issueArgs := []string{"--issue", "-d", domain, "--standalone", "--httpport", "80", "--server", "letsencrypt"}
	if email != "" {
		issueArgs = append(issueArgs, "-m", email)
	}
	if payload.Force {
		// 强制重签：acme.sh 默认会在到期前 30 天才续，面板手工触发时不受这个限制
		// （否则会回 "Domains not changed ... Add '--force'" 直接跳过）。
		issueArgs = append(issueArgs, "--force")
	}
	issueOut, issueErr := runCertCmd(ctx, certIssueTimeout, acmeShBin, issueArgs...)

	// 2) 找准 acme.sh 自己那份证书的目录：默认 ec-256 落在 <domain>_ecc，
	//    老账户/RSA 落在 <domain>。--install-cert 必须带上对应的 --ecc。
	ecc, hasCert := acmeCertDir(domain)
	if !hasCert && issueErr != nil {
		return marshalCertAck(cmd.ID, false, certIssueResult{
			Reason: "acme.sh 签发失败: " + truncate(strings.TrimSpace(issueOut), certCmdOutMax),
		})
	}

	// 3) 装到固定路径（--key-file / --fullchain-file 由 acme.sh 负责写）。
	installArgs := []string{"--install-cert", "-d", domain}
	if ecc {
		installArgs = append(installArgs, "--ecc")
	}
	installArgs = append(installArgs, "--key-file", keyPath, "--fullchain-file", certPath)
	installOut, installErr := runCertCmd(ctx, certInstallTimeout, acmeShBin, installArgs...)

	if installErr != nil || !fileExists(certPath) || !fileExists(keyPath) {
		reason := strings.TrimSpace(issueOut + "\n" + installOut)
		return marshalCertAck(cmd.ID, false, certIssueResult{
			Reason: "acme.sh 安装证书失败: " + truncate(reason, certCmdOutMax),
		})
	}

	// 内核以 root 读，证书公开、私钥收紧。
	_ = os.Chmod(certPath, 0o644)
	_ = os.Chmod(keyPath, 0o600)

	// 回执带上两个文件的原文：面板读不到节点磁盘，证书库里的 PEM 只能从这里来。
	// 读不回来只把原因记进 reason，success / ok 仍为 true。
	certPEM, keyPEM, pemReason := loadCertPEM(certPath, keyPath)
	res := certIssueResult{OK: true, CertPath: certPath, KeyPath: keyPath, CertPEM: certPEM, KeyPEM: keyPEM}

	var notes []string
	if notAfter, err := readCertNotAfter(certPath); err == nil {
		res.NotAfter = notAfter.UTC().Format(time.RFC3339)
	} else {
		notes = append(notes, "读取到期日失败: "+err.Error())
	}
	if pemReason != "" {
		notes = append(notes, "读取 PEM 失败（"+pemReason+"）")
	}
	if len(notes) > 0 {
		res.Reason = "证书已安装，但" + strings.Join(notes, "；")
	}
	return marshalCertAck(cmd.ID, true, res)
}

// acmeCertDir 判断 acme.sh 里这个域名的证书目录（_ecc 优先）。
func acmeCertDir(domain string) (ecc bool, ok bool) {
	home := filepath.Dir(acmeShBin)
	if st, err := os.Stat(filepath.Join(home, domain+"_ecc")); err == nil && st.IsDir() {
		return true, true
	}
	if st, err := os.Stat(filepath.Join(home, domain)); err == nil && st.IsDir() {
		return false, true
	}
	return false, false
}

// marshalCertAck 把证书回执序列化进 ack output（与 port_probe 同一写法）。
func marshalCertAck(id int64, success bool, res any) AckResult {
	raw, err := json.Marshal(res)
	if err != nil {
		return AckResult{ID: id, Success: false, Output: "结果序列化失败: " + err.Error()}
	}
	return AckResult{ID: id, Success: success, Output: string(raw)}
}
