package api

// 注册邮箱域名白名单 / 一次性邮箱拦截。
//
// 背景：register_email_verify=1 时确实校验 6 位邮箱验证码，但 euploading.com、
// mailinator.com、smtp.is 这类「公共 / 一次性收信服务」本身就能收到验证码 ——
// 验证码对它零门槛，等于邮箱验证形同虚设。实测：226 个用户里唯一一个非主流域名
// 就是 euploading.com，且时间线是「先发码 → 再注册成功」。
//
// 这里在注册与发码两个入口各加一道域名判定。两个站点配置（均逗号分隔）：
//
//	register_email_allowed_domains  白名单。**子域自动匹配**：填 qq.com 覆盖
//	                                a@qq.com 与 a@vip.qq.com，不必逐个子域列。
//	register_email_blocked_domains  黑名单（一次性邮箱兜底）。**优先于白名单**：
//	                                命中即拒，防止「误把一次性域名加进白名单」时放行。
//
// 向后兼容（很重要）：两个键都为空 = 不启用，行为与升级前完全一致。
// 只要配了白名单，不在名单内的一律拒；黑名单任何时候都生效。

import "strings"

const (
	// cfgRegisterEmailAllowedDomains / cfgRegisterEmailBlockedDomains 是站点配置键。
	cfgRegisterEmailAllowedDomains = "register_email_allowed_domains"
	cfgRegisterEmailBlockedDomains = "register_email_blocked_domains"
)

// msgEmailDomainDenied 是域名被拒时的对外文案（前端直接展示，需可读、可操作）。
const msgEmailDomainDenied = "请使用常用邮箱注册（QQ / 163 / Gmail / Outlook 等），不支持一次性或临时邮箱"

// domainKey 定义「域名相等 / 互为子域」的规范化口径。
func normalizeDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

// domainOf 取邮箱域名：小写、去首尾空白与尾部点。无 @ 或 @ 在首尾返回空串。
func domainOf(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	return normalizeDomain(email[at+1:])
}

// splitDomainList 把逗号/分号/空白分隔的配置拆成规范化域名列表：
// 去掉误写的前导 '@'、'*.'、尾部点，丢弃空项。
func splitDomainList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		d := normalizeDomain(f)
		d = strings.TrimPrefix(d, "@")
		d = strings.TrimPrefix(d, "*")
		d = strings.TrimPrefix(d, ".")
		d = normalizeDomain(d)
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// domainMatches 判断 emailDomain 是否命中某条域名规则，**含子域**：
// 规则 example.com 命中 a@example.com 与 a@mail.example.com。
// 这样「填 qq.com 覆盖 vip.qq.com」不必逐个列举。
func domainMatches(emailDomain, rule string) bool {
	if emailDomain == "" || rule == "" {
		return false
	}
	if emailDomain == rule {
		return true
	}
	return strings.HasSuffix(emailDomain, "."+rule)
}

// emailDomainAllowed 判定该邮箱域名是否放行。cfg 是站点配置键值。
//
// 返回 false 表示应当拒绝注册/发码（调用方回 msgEmailDomainDenied）。
// 邮箱格式非法（取不到域名）时返回 true —— 交给格式校验去报更准确的文案，
// 不要让这里抢先给出「用常用邮箱」的误导提示。
func emailDomainAllowed(cfg map[string]string, email string) bool {
	domain := domainOf(email)
	if domain == "" {
		return true
	}

	for _, rule := range splitDomainList(cfg[cfgRegisterEmailBlockedDomains]) {
		if domainMatches(domain, rule) {
			return false
		}
	}

	allow := splitDomainList(cfg[cfgRegisterEmailAllowedDomains])
	if len(allow) == 0 {
		return true // 未启用白名单 → 保持升级前行为
	}
	for _, rule := range allow {
		if domainMatches(domain, rule) {
			return true
		}
	}
	return false
}

// defaultAllowedEmailDomains 是大厂邮箱基线（前端「恢复默认」按钮与文档共用此口径）。
//
// 不含企业邮箱与自建域名 —— 需要时由运维在白名单里追加。
var defaultAllowedEmailDomains = []string{
	"qq.com", "vip.qq.com", "foxmail.com",
	"163.com", "126.com", "yeah.net", "188.com",
	"sina.com", "sina.cn", "sohu.com", "139.com", "189.cn", "aliyun.com",
	"gmail.com", "googlemail.com",
	"outlook.com", "hotmail.com", "live.com", "msn.com",
	"icloud.com", "me.com",
	"yahoo.com", "protonmail.com", "proton.me",
}

// DefaultAllowedEmailDomainsCSV 返回默认白名单的逗号串（前端文档/恢复默认用）。
func DefaultAllowedEmailDomainsCSV() string { return strings.Join(defaultAllowedEmailDomains, ",") }
