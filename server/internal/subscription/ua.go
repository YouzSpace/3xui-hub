package subscription

// User-Agent 识别订阅格式（照 Xboard 的 flags 思路）。
//
// 目的：用户把订阅链接直接丢进 Clash Verge / FlClash / sing-box 客户端就能用，
// 不需要自己在链接后面加 ?clash / ?singbox。显式参数仍然优先（键存在即生效），
// 所以老链接、第三方脚本的行为完全不变。
//
// 只做格式选择，不涉及节点内容；认不出的 UA（浏览器、v2rayN、Shadowrocket 等）
// 一律返回 false，走原来的 base64 默认。

import "strings"

// clashUAHints 是 Clash 系客户端 UA 的特征串（小写包含匹配）。
// "clash" 已覆盖 clash-verge / clashx / clashmeta / clash-for-windows 等；
// mihomo / flclash / nekobox / nekoray / stash 是同一内核的其它外壳。
var clashUAHints = []string{
	"clash", "mihomo", "flclash", "nekobox", "nekoray", "stash",
}

// singboxUAHints 是 sing-box 官方客户端的 UA 特征串。
// 官方 App 的 UA 形如 "SFA/1.10.1 (Linux; Android 14)"、"SFI/..."、"SFM/..."、"SFP/..."，
// 带斜杠避免误伤普通单词；"sing-box" 覆盖直接写内核名的客户端。
var singboxUAHints = []string{
	"sing-box", "singbox", "sfa/", "sfi/", "sfm/", "sfp/",
}

// formatFromUA 按 User-Agent 推断订阅格式；认不出返回 ("", false)。
func formatFromUA(ua string) (Format, bool) {
	s := strings.ToLower(strings.TrimSpace(ua))
	if s == "" {
		return "", false
	}
	for _, hint := range clashUAHints {
		if strings.Contains(s, hint) {
			return FormatClash, true
		}
	}
	for _, hint := range singboxUAHints {
		if strings.Contains(s, hint) {
			return FormatSingbox, true
		}
	}
	return "", false
}
