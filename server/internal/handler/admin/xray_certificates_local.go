package admin

// 本机证书目录同步：把面板机上 acme.sh 签好/续好的证书自动搬进证书库。
//
// 为什么需要它：HY2 强制 TLS，而入站只存 tlsCertId（私钥加密放 xray_certificates）。
// 面板机本机就有 acme.sh 维护的证书目录（root crontab 每 6 小时 --cron 续签），
// 但库是空的 —— 于是「建 HY2 入站」永远卡在「没有证书可选」。
//
// 约定：目录下一级子目录名 = 域名，子目录里放 fullchain.pem + privkey.pem：
//
//	/root/cert/mykl1.example.com/fullchain.pem
//	/root/cert/mykl1.example.com/privkey.pem
//
// 目录用环境变量 XRAY_CERT_DIR 覆盖，默认 /root/cert。
//
// 私钥一律走 store 的加密写入路径（key_enc），这里永远不直接写库、也不落明文。
// 幂等：同域名已存在就更新 PEM 与 not_after，不重复插行（入站引用的是 id，行不能重建）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

const (
	// localCertDirEnv / defaultLocalCertDir 是证书目录的覆盖项与缺省值。
	localCertDirEnv     = "XRAY_CERT_DIR"
	defaultLocalCertDir = "/root/cert"
	// 目录里约定的两个文件名（acme.sh 的默认产物名）。
	localCertFullchain = "fullchain.pem"
	localCertPrivkey   = "privkey.pem"
	// certSyncMaxReasons 是回执里最多回几条跳过原因（避免一次回执几千字）。
	certSyncMaxReasons = 20
)

// 证书来源标记（回显用，不落库：来源只是「当下能不能与本机目录对上」的派生信息，
// 为此加一列要改表，不值得）。
const (
	certSourceLocalDir = "local-dir"
	certSourceManual   = "manual"
)

// LocalCertSyncResult 是一次本机目录同步的结果（接口回执与定时任务日志共用）。
type LocalCertSyncResult struct {
	Dir     string
	Scanned int      // 扫到的子目录数
	Created int      // 新入库
	Updated int      // 已有同域名行被刷新
	Skipped int      // 跳过的子目录数
	Reasons []string // 跳过原因（最多 certSyncMaxReasons 条）
}

// localCertDir 读证书目录（环境变量优先，空则用缺省值）。
func localCertDir() string {
	if v := strings.TrimSpace(os.Getenv(localCertDirEnv)); v != "" {
		return v
	}
	return defaultLocalCertDir
}

// SyncLocalCertificates 扫描本机证书目录并 upsert 进证书库。
//
// 目录不存在 / 读不了不算错误：返回结果里带上原因，让调用方（接口回执 / 定时任务日志）
// 自己决定怎么展示 —— 一台没有 acme 目录的机器不该每 6 小时刷一条失败记录。
func (d *NodeDomain) SyncLocalCertificates(ctx context.Context) (LocalCertSyncResult, error) {
	res := LocalCertSyncResult{Dir: localCertDir()}
	entries, err := os.ReadDir(res.Dir)
	if err != nil {
		res.Skipped++
		res.addReason("读取证书目录失败（" + res.Dir + "）：" + err.Error())
		return res, nil
	}

	existing, err := d.store.XrayCertificates(ctx)
	if err != nil {
		return res, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		domain := strings.TrimSpace(entry.Name())
		if domain == "" || strings.HasPrefix(domain, ".") {
			continue
		}
		res.Scanned++

		certPEM, keyPEM, reason := readLocalCertPair(res.Dir, domain)
		if reason != "" {
			res.Skipped++
			res.addReason(reason)
			continue
		}

		// 同域名（或同名字）已存在 → 更新那一行：入站引用的是 id，重建行会让引用悬空。
		var hit *XrayCertificate
		for i := range existing {
			if certRowMatchesDomain(&existing[i], domain) {
				hit = &existing[i]
				break
			}
		}
		dom := domain
		in := XrayCertificateInput{
			Name:    domain,
			Domain:  &dom,
			CertPEM: certPEM,
			KeyPEM:  keyPEM,
		}
		if hit != nil {
			if err := d.store.UpdateXrayCertificate(ctx, hit.ID, in); err != nil {
				res.Skipped++
				res.addReason(domain + " 更新失败：" + err.Error())
				continue
			}
			res.Updated++
			continue
		}
		if _, err := d.store.CreateXrayCertificate(ctx, in); err != nil {
			res.Skipped++
			res.addReason(domain + " 入库失败：" + err.Error())
			continue
		}
		res.Created++
	}
	return res, nil
}

// addReason 记一条跳过原因（超上限就丢，避免回执过长）。
func (r *LocalCertSyncResult) addReason(reason string) {
	if len(r.Reasons) >= certSyncMaxReasons {
		return
	}
	r.Reasons = append(r.Reasons, reason)
}

// certRowMatchesDomain 判断证书库一行是否就是「这个域名」对应的那条：
// 先比 domain，再比 name（手工加的证书常常把名字写成域名）。
func certRowMatchesDomain(c *XrayCertificate, domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if c.Domain != nil && strings.ToLower(strings.TrimSpace(*c.Domain)) == d {
		return true
	}
	return strings.ToLower(strings.TrimSpace(c.Name)) == d
}

// readLocalCertPair 读一个域名的 fullchain.pem + privkey.pem；第二个返回值非空 = 跳过原因。
//
// PEM 合法性用与手工新增接口同一套判据（含 BEGIN CERTIFICATE / PRIVATE KEY），
// 免得把半个文件塞进库、到渲染时才炸。
func readLocalCertPair(root, domain string) (string, string, string) {
	certPath := filepath.Join(root, domain, localCertFullchain)
	keyPath := filepath.Join(root, domain, localCertPrivkey)

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return "", "", domain + " 缺少 " + localCertFullchain
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return "", "", domain + " 缺少 " + localCertPrivkey
	}
	certText := strings.TrimSpace(string(certPEM))
	keyText := strings.TrimSpace(string(keyPEM))
	if !strings.Contains(certText, "BEGIN CERTIFICATE") {
		return "", "", domain + " 的 " + localCertFullchain + " 不是 PEM 证书"
	}
	if !strings.Contains(keyText, "PRIVATE KEY") {
		return "", "", domain + " 的 " + localCertPrivkey + " 不是 PEM 私钥"
	}
	return certText, keyText, ""
}

// localCertSourceSet 返回「本机证书目录里现存的域名」集合（小写）。
//
// 读目录失败就当空集合（列表全部按 manual 显示）：来源只是展示信息，
// 不能因为它把证书列表接口搞挂。
func localCertSourceSet() map[string]bool {
	set := map[string]bool{}
	dir := localCertDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return set
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(entry.Name()))
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		// 只 stat 两个文件名就够（来源判定不需要读内容，别在列表接口里读一堆 PEM）。
		if localCertPairExists(dir, entry.Name()) {
			set[name] = true
		}
	}
	return set
}

// localCertPairExists 判断一个域名子目录里两个约定文件都在。
func localCertPairExists(root, domain string) bool {
	for _, name := range []string{localCertFullchain, localCertPrivkey} {
		if _, err := os.Stat(filepath.Join(root, domain, name)); err != nil {
			return false
		}
	}
	return true
}

// certSourceOf 判定证书来源：name / domain 能对上本机目录的子目录名 → local-dir，否则 manual。
func certSourceOf(c *XrayCertificate, local map[string]bool) string {
	if len(local) == 0 {
		return certSourceManual
	}
	if c.Domain != nil && local[strings.ToLower(strings.TrimSpace(*c.Domain))] {
		return certSourceLocalDir
	}
	if local[strings.ToLower(strings.TrimSpace(c.Name))] {
		return certSourceLocalDir
	}
	return certSourceManual
}
