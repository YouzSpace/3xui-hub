package admin

import "fmt"

// itoaUint / itoaInt / truncateRunes 是管理端各域共用的纯格式化辅助。
//
// 原文件（provision.go / webshare.go）承载代理商城的开号逻辑，该产品线已下线；
// 这三个函数仍被用户域（clientEmail）、节点域（编号前缀）与订阅域（日志截断）使用，
// 因此单独保留在这里。

// itoaUint 是 uint64 → 十进制字符串（含 0，不产生空串）。
func itoaUint(v uint64) string { return fmt.Sprintf("%d", v) }

// itoaInt 是 int → 十进制字符串。
func itoaInt(v int) string { return fmt.Sprintf("%d", v) }

// truncateRunes 按「字符」截断（不是字节），超长补省略号；日志/提示里用。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
