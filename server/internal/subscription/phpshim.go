// Package subscription 复刻 PHP 的订阅生成链路（GET /api/sub/{token} 三格式），
// 目标是与 PHP 输出 **byte 级一致**。
//
// 迁移对象（backend/）：
//   - app/Http/Controllers/Api/SubscriptionController.php（token 校验、格式选择、响应头）
//   - app/Services/SubscriptionService.php（取链 + 三格式生成，1486 行，核心）
//   - app/Drivers/Xray/XrayConfigService.php（xray 节点订阅 URI 生成）
//   - app/Support/NodeRegion.php（地区识别 / 旗帜）
//   - app/Services/ThirdPartyService.php（第三方订阅注入 + 中性改名）
//
// 与 PHP 对齐的三条硬约束：
//  1. 字符串处理逐字复刻 PHP 的 urlencode/urldecode/parse_str/trim/base64 语义，
//     不"按 Go 习惯"重写（差异都会体现在订阅正文里）。
//  2. 数值（round/number_format/formatBytes/浮点转字符串）按 PHP 8.4 的口径实现并
//     由对拍测试钉死。
//  3. JSON 手写有序编码器：PHP 数组保序，`json_encode(JSON_PRETTY_PRINT|JSON_UNESCAPED_SLASHES)`
//     的缩进/转义/浮点形态必须逐字一致（Go 的 encoding/json 会把 map 键排序、并对 / 与
//     非 ASCII 做不同处理）。
package subscription

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// phpTrimChars 是 PHP trim() 的默认字符集（注意：不含 \f）。
const phpTrimChars = " \t\n\r\x00\x0B"

// phpTrim 等价于 PHP 的 trim($s)（默认字符集）。
func phpTrim(s string) string { return strings.Trim(s, phpTrimChars) }

// phpTrimSet 等价于 PHP 的 trim($s, $charlist)，charlist 为字符集合（不是前缀/后缀串）。
func phpTrimSet(s string, cutset string) string { return strings.Trim(s, cutset) }

// phpEmpty 等价于 PHP 的 empty() 对标量："" 与 "0" 都算空（0 / 0.0 / false / null 由调用方处理）。
func phpEmptyString(s string) bool { return s == "" || s == "0" }

// rawurlencode 等价于 PHP rawurlencode()：除 A-Za-z0-9 - _ . ~ 外全部 %XX（大写）。
func rawurlencode(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0F])
		}
	}
	return b.String()
}

// urlencode 等价于 PHP urlencode()：空格转 '+'，其余同 rawurlencode 但没有 '~'。
func urlencode(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0F])
		}
	}
	return b.String()
}

// urldecode 等价于 PHP urldecode()：'+' 转空格，%XX 解码（非法转义原样保留）。
func urldecode(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			b.WriteByte(' ')
		case '%':
			if i+2 < len(s) {
				if hi, ok := hexVal(s[i+1]); ok {
					if lo, ok2 := hexVal(s[i+2]); ok2 {
						b.WriteByte(hi<<4 | lo)
						i += 2
						continue
					}
				}
			}
			b.WriteByte('%')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// parseStr 复刻 PHP parse_str($query, $out) 在当前用法下需要的语义：
//   - 以 '&' 分隔（arg_separator.input 默认值）；
//   - 键与值都做 urldecode；
//   - 顶层键里的 '.' 与 ' ' 转成 '_'（PHP 的历史行为）；
//   - 无 '=' 的片段值为空串；
//   - 同名键后者覆盖前者（PHP 是覆盖，不是追加）；
//   - 'k[]' 这种数组写法本项目链接里不出现，按普通键处理（已在文档中标注）。
func parseStr(query string) map[string]string {
	out := map[string]string{}
	if query == "" {
		return out
	}
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		key, val := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			key, val = part[:i], part[i+1:]
		}
		key = urldecode(key)
		// PHP 把顶层键中的 '.' 和空格统一替换成 '_'。
		key = strings.Map(func(r rune) rune {
			if r == '.' || r == ' ' {
				return '_'
			}
			return r
		}, key)
		out[key] = urldecode(val)
	}
	return out
}

// decodeBase64URL 等价于 PHP 侧 decodeBase64Url()：标准/URL-safe 均可、允许缺 padding。
// 失败返回 ("", false)。
func decodeBase64URL(value string) (string, bool) {
	value = strings.NewReplacer("-", "+", "_", "/").Replace(value)
	switch len(value) % 4 {
	case 1:
		return "", false
	case 2:
		value += "=="
	case 3:
		value += "="
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

// b64encode 等价于 PHP base64_encode()（标准字母表 + padding，不换行）。
func b64encode(s []byte) string { return base64.StdEncoding.EncodeToString(s) }

// phpRound 等价于 PHP 8.4 的 round($value, $places)（places >= 0）。
//
// PHP 8.4 起 round() 的舍入口径是「对最短往返十进制表示（serialize_precision=-1）做
// 十进制的 half-away-from-zero」。已用 40 万组取值（含 x.xx5 边界）与 PHP 8.5 实测对拍，
// 零差异；对比直接用 math.Round(x*100)/100（二进制误差会让 1.005 变成 1.00）。
func phpRound(value float64, places int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	if places < 0 {
		places = 0
	}
	// 'f' + -1：给出最短往返、且不带指数的最短十进制表示（如 1e15 → "1000000000000000"）。
	s := strconv.FormatFloat(value, 'f', -1, 64)
	rounded := decRoundHalfAway(s, places)
	out, err := strconv.ParseFloat(rounded, 64)
	if err != nil {
		return value
	}
	return out
}

// decRoundHalfAway 对十进制字符串按位做「四舍五入、远离零」。
func decRoundHalfAway(s string, places int) string {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}

	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}

	digits := intPart + fracPart
	pointPos := len(intPart) // 小数点在 digits 中的位置
	if pointPos < 0 {
		pointPos = 0
	}

	keep := pointPos + places
	if keep < 0 {
		keep = 0
	}
	if keep >= len(digits) {
		digits += strings.Repeat("0", keep-len(digits))
	} else {
		roundUp := digits[keep] >= '5'
		digits = digits[:keep]
		if roundUp {
			var carry byte = 1
			buf := []byte(digits)
			for i := len(buf) - 1; i >= 0 && carry == 1; i-- {
				if buf[i] == '9' {
					buf[i] = '0'
				} else {
					buf[i]++
					carry = 0
				}
			}
			digits = string(buf)
			if carry == 1 {
				digits = "1" + digits
				pointPos++
			}
		}
	}

	if pointPos <= 0 {
		intPart = "0"
		fracPart = strings.Repeat("0", -pointPos) + digits
	} else if pointPos >= len(digits) {
		intPart = digits + strings.Repeat("0", pointPos-len(digits))
		fracPart = ""
	} else {
		intPart, fracPart = digits[:pointPos], digits[pointPos:]
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}

	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg && strings.Trim(out, "0.") != "" {
		out = "-" + out
	}
	return out
}

// numberFormat2 等价于 PHP number_format($v, 2, '.', ”)：先 round 到两位，再固定两位小数的文本。
func numberFormat2(v float64) string {
	return strconv.FormatFloat(phpRound(v, 2), 'f', 2, 64)
}

// phpFloatToString 等价于 PHP 的 (string)$float（precision=14）。
//
// 订阅链路里只用于 round()/ceil() 的结果：量级不超过 1e8、小数位不超过 2，
// 此范围内与 PHP 的 php_gcvt 输出完全一致（整数值不带 ".0"，如 1024 → "1024"）。
// 超出该范围（|v| >= 1e14）时按 PHP 的 %.*G 形态给出指数表示，并保留 14 位有效数字。
func phpFloatToString(v float64) string {
	if math.IsNaN(v) {
		return "NAN"
	}
	if math.IsInf(v, 0) {
		if v > 0 {
			return "INF"
		}
		return "-INF"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e14 {
		return strconv.FormatInt(int64(v), 10)
	}
	if math.Abs(v) >= 1e14 {
		// PHP: printf('%.13E', v) 形态，尾数至少保留一位小数（1.0E+15）。
		s := strconv.FormatFloat(v, 'E', 13, 64)
		// 去掉尾数多余的 0，但至少留一位小数；指数去掉多余前导 0。
		mant, exp, _ := strings.Cut(s, "E")
		mant = strings.TrimRight(mant, "0")
		if strings.HasSuffix(mant, ".") {
			mant += "0"
		}
		sign := ""
		if strings.HasPrefix(exp, "-") || strings.HasPrefix(exp, "+") {
			sign, exp = exp[:1], exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		return mant + "E" + sign + exp
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// formatBytes 逐字复刻 SubscriptionService::formatBytes()（含 round 的两位小数目视）。
func formatBytes(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	v := math.Abs(float64(bytes))
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return phpFloatToString(phpRound(v, 2)) + " " + units[i]
}

// phpDate 等价于 PHP date('Y-m-d', $ts)，时区取 config/app.php 的 UTC。
func phpDate(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}

// ceilDays 等价于 PHP 的 max(0, ceil(($at - time()) / 86400)) 并转成文本。
func ceilDays(at, now int64) string {
	days := math.Ceil(float64(at-now) / 86400)
	if days < 0 {
		days = 0
	}
	return phpFloatToString(days)
}

// phpString 把 JSON 解码出来的 any 转成 PHP 的 (string) 行为（用于 stream_settings 取值）。
func phpString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return ""
	case jsonNumber:
		return t.String()
	case float64:
		return phpFloatToString(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

// phpBool 等价于 PHP 的 (bool) 转换（空串与 "0" 为 false）。
func phpBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return !phpEmptyString(t)
	case jsonNumber:
		f, _ := t.Float64()
		return f != 0
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	}
	return true
}

// phpInt 等价于 PHP 的 (int) 转换（字符串取前缀数字，非数字为 0）。
func phpInt(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		return parseIntPrefix(t)
	case jsonNumber:
		if n, err := t.Int64(); err == nil {
			return n
		}
		f, _ := t.Float64()
		return int64(f)
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	}
	return 0
}

// parseIntPrefix 复刻 PHP 的字符串转 int：跳过空白、取可选符号与连续数字。
func parseIntPrefix(s string) int64 {
	i := 0
	for i < len(s) && strings.IndexByte(" \t\n\r\v\f", s[i]) >= 0 {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if start == i {
		return 0
	}
	n, err := strconv.ParseInt(s[start:i], 10, 64)
	if err != nil {
		return 0
	}
	if neg {
		return -n
	}
	return n
}

// asciiUpper 复刻 PHP strtoupper()：只把 ASCII 小写字母转大写，多字节序列原样保留。
func asciiUpper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// asciiLower 复刻 PHP strtolower()：只处理 ASCII。
func asciiLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// phpSprintfInt 用于 URL 拼接时的整数格式化（等价于 %d）。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// mustParseFloat 解析十进制字符串为 float64；失败返回 0（调用方只在有值场景使用）。
func mustParseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// errUnsupported 表示「PHP 侧会跳过/降级」的处理分支，调用方据此保持原样。
var errUnsupported = fmt.Errorf("unsupported")
