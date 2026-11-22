package xray

// 订阅分享链接（入站 → 客户端 URI）。
//
// 2026-11-22 去重：本文件原先自带一份与 server/internal/subscription/xrayuri.go
// 等价的 URI 拼装（vlessURI/vmessURI/trojanURI/ssURI/hysteriaURI…）。两份逐字节对拍
// 证实完全一致后，改为**委托** subscription.XrayInboundURI —— 链接规则只保留一个真相源，
// 避免「管理端预览」与「用户下载订阅」两处悄悄漂移。
//
// 本文件只保留：
//   - SubscriptionURIs：把 driver 的 Inbound/User 适配成 subscription 的类型后调用；
//   - HostOf / SameSite：导出给包外复用（handler/admin 用 HostOf），与链接无关，保留；
//   - 下方「内部小工具」区：被本包其它文件共用（streamObject/jsonFieldString/coalesceString…）。
//
// 刻意不做：不把 subscription 反向依赖 driver/xray（会有 import 环）。委托方向只能是 driver → subscription。

import (
	"strings"

	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
	"controlhub-server/internal/subscription"
)

// secondLevelSuffixes 是常见两级公共后缀（与 PHP 侧同一张表，不引 PSL 依赖）。
var secondLevelSuffixes = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"co.jp": true, "ne.jp": true, "or.jp": true, "ac.jp": true,
	"com.cn": true, "net.cn": true, "org.cn": true, "gov.cn": true, "edu.cn": true,
	"com.hk": true, "com.tw": true, "com.sg": true, "com.au": true,
	"co.kr": true, "co.nz": true, "com.br": true,
}

// SubscriptionURIs 复刻 subscriptionUris()：生成用户在节点上的全部订阅 URI
// （每个协议匹配的启用入站一条）。协议不支持时该入站不出链接。
//
// 适配层：把 driver 的 Node/Inbound/User 映射成 subscription 的同名结构体，
// 再逐入站调用 subscription.XrayInboundURI（唯一真相源）。字段映射与旧 switch 的入参一致。
func SubscriptionURIs(node Node, inbounds []Inbound, user User) []string {
	host := strings.TrimSpace(node.Host)
	if host == "" || user.UUID == "" {
		return []string{}
	}

	snode := subscription.Node{Name: node.Name}
	suser := &subscription.User{ID: user.ID, UUID: user.UUID}
	uris := make([]string, 0, len(inbounds))
	for _, ib := range inbounds {
		remark := ""
		if ib.Remark != nil {
			remark = *ib.Remark
		}
		sib := subscription.XrayInbound{
			Tag:            ib.Tag,
			Remark:         remark,
			Protocol:       ib.Protocol,
			Port:           ib.Port,
			Settings:       string(ib.Settings),
			StreamSettings: string(ib.StreamSettings),
		}
		// XrayInboundURI 对不支持的协议返回空串；与旧 switch 的 default: continue 等价。
		if uri := subscription.XrayInboundURI(snode, sib, suser, host); uri != "" {
			uris = append(uris, uri)
		}
	}
	return uris
}

// HostOf 复刻 hostOf()：dest 串 → 主机名。
// 支持 "host:443"、"host"、"[2001:db8::1]:443"、"[2001:db8::1]"；解析不出返回空串。
func HostOf(dest string) string {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return ""
	}

	// IPv6 字面量：[v6] 或 [v6]:port
	if strings.HasPrefix(dest, "[") {
		end := strings.IndexByte(dest, ']')
		if end < 0 {
			return ""
		}
		return strings.ToLower(dest[1:end])
	}

	if i := strings.LastIndex(dest, ":"); i >= 0 {
		tail := dest[i+1:]
		if isAllDigitsASCII(tail) {
			dest = dest[:i]
		}
	}
	return strings.ToLower(strings.Trim(dest, " \t\n\r\x00\x0B."))
}

// SameSite 复刻 sameSite()：相等、互为父域，或同属一个注册域。
//
// 用途：REALITY 的 serverNames 必须被 dest 证书覆盖。同一张证书通常覆盖一个注册域
// 及其子域，而跨注册域（aws.amazon.com 与 www.cloudflare.com）几乎不可能同证书。
// 通配符、IP 字面量与空串一律 false。
func SameSite(a, b string) bool {
	a = strings.ToLower(strings.Trim(a, " \t\n\r\x00\x0B."))
	b = strings.ToLower(strings.Trim(b, " \t\n\r\x00\x0B."))

	if a == "" || b == "" || strings.Contains(a, "*") || strings.Contains(b, "*") {
		return false
	}
	if a == b {
		return true
	}
	// IP 字面量之间没有「站点」概念，只认完全相等（上面已判过）
	if isIPLiteral(a) || isIPLiteral(b) {
		return false
	}
	if strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a) {
		return true
	}
	return registeredDomain(a) == registeredDomain(b)
}

// registeredDomain 取注册域（example.com / example.co.uk）；取不出时返回原串。
func registeredDomain(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	tail2 := strings.Join(parts[len(parts)-2:], ".")
	if secondLevelSuffixes[tail2] {
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return tail2
}

// ── 内部小工具 ───────────────────────────────────────────────

func streamObject(ib Inbound) phpjson.Object {
	val, _ := phpjson.ParseOrdered(ib.StreamSettings)
	return phpval.ToObject(val)
}

func securityOf(stream phpjson.Object) string {
	security := phpval.String(mustGet(stream, "security"))
	if security == "" {
		return "none"
	}
	return security
}

func networkOf(stream phpjson.Object) string {
	network := phpval.String(mustGet(stream, "network"))
	if network == "" {
		return "tcp"
	}
	return network
}

// jsonField 从保序对象里取嵌套字段（PHP 的 $arr['a']['b'] 读取，缺任何一层都是 null）。
func jsonField(m any, path ...string) any {
	cur := m
	for _, key := range path {
		obj, ok := cur.(phpjson.Object)
		if !ok {
			return nil
		}
		next, ok := obj.Get(key)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// jsonFieldString 等价于 PHP 的 `(string)($arr['a']['b'] ?? ”)`。
func jsonFieldString(m any, path ...string) string { return phpval.String(jsonField(m, path...)) }

// jsonFieldArray 取嵌套数组；不是数组时返回 nil。
func jsonFieldArray(m any, path ...string) []any {
	v := jsonField(m, path...)
	if arr, ok := v.(phpjson.Array); ok {
		return arr
	}
	return nil
}

// toAnySlice 把 JSON 数组转成 []any（非数组 → nil）。
func toAnySlice(v any) []any {
	if arr, ok := v.(phpjson.Array); ok {
		return arr
	}
	return nil
}

// coalesceString 对应 PHP 的 `$x ?? 'default'`：键不存在 / 值为 null 才回落，
// 空串是**有效值**（PHP 的 `??` 不是 `?:`，这个区别直接影响链接字节）。
func coalesceString(v any, def string) string {
	switch t := v.(type) {
	case nil:
		return def
	case string:
		return t
	case phpjson.Object, phpjson.Array:
		// PHP 里对数组做字符串拼接会得到 "Array"（并告警），链接里不会出现这种配置；
		// 保守回落到默认值，避免产出 "Array" 这种脏参数。
		return def
	default:
		return phpval.String(t)
	}
}

// firstStringValue 取数组第一个元素的字符串（PHP 的 (string)($arr[0] ?? ”)）。
func firstStringValue(v any) string {
	s, _ := firstStringOK(toAnySlice(v))
	return s
}

// firstStringOK 严格对应 PHP 的 `$arr[0] ?? null` 判定：
// 数组为空或元素不是字符串时返回 ("", false)，**空串本身是有效值**。
func firstStringOK(items []any) (string, bool) {
	if len(items) == 0 {
		return "", false
	}
	if s, ok := items[0].(string); ok {
		return s, true
	}
	return "", false
}

// isAllDigitsASCII 判断字符串是否全为 ASCII 数字（空串返回 false）。
func isAllDigitsASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isIPLiteral 近似 PHP 的 filter_var($s, FILTER_VALIDATE_IP)（订阅链路只需要 v4/v6 字面量判定）。
func isIPLiteral(s string) bool {
	if !strings.Contains(s, ".") && !strings.Contains(s, ":") {
		return false
	}
	return isIPv4Literal(s) || isIPv6Literal(s)
}

func isIPv4Literal(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 3 || !isAllDigitsASCII(part) {
			return false
		}
		if phpval.IntFromString(part) > 255 {
			return false
		}
	}
	return true
}

func isIPv6Literal(s string) bool {
	// 只需要「像 IPv6」：必须含 ':'、只由十六进制/冒号/点组成、冒号数量合理。
	if !strings.Contains(s, ":") {
		return false
	}
	colons := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ':':
			colons++
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F', c == '.':
		default:
			return false
		}
	}
	return colons >= 2
}
