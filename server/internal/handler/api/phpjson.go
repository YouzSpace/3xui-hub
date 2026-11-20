package api

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 本文件把「值 → JSON 字节」按 PHP json_encode 的默认口径复刻，而不是用 Go 的 encoding/json。
//
// 为什么值得自己写（前端零改动 + 收口要做双跑 JSON 对拍）：
//   - 中文：PHP 默认 \uXXXX 转义（未加 JSON_UNESCAPED_UNICODE），Go 输出原始 UTF-8；
//   - 斜杠：PHP 转义 "/" 为 "\/"，Go 不转义；
//   - <>&：Go 会转义成 <，PHP 不会；
//   - 键序：PHP 关联数组保留插入顺序，Go 的 map 按字典序。
//
// 三者叠加会让「语义相同的两份 JSON」字节完全不同，双跑对拍直接失败。
// 因此本包所有响应都走这里的编码器。

// jsObj 是键序可控的 JSON 对象（对应 PHP 的关联数组）。
type jsObj struct {
	keys []string
	vals []any
}

// jo 按参数顺序构造对象：jo("code", 0, "msg", "ok")。
func jo(pairs ...any) *jsObj {
	if len(pairs)%2 != 0 {
		panic("api: jo() 需要偶数个参数")
	}
	o := &jsObj{
		keys: make([]string, 0, len(pairs)/2),
		vals: make([]any, 0, len(pairs)/2),
	}
	for i := 0; i < len(pairs); i += 2 {
		o.keys = append(o.keys, pairs[i].(string))
		o.vals = append(o.vals, pairs[i+1])
	}
	return o
}

// put 追加或覆盖一个键；覆盖时保持原位置（与 PHP 数组赋值语义一致）。
func (o *jsObj) put(key string, val any) *jsObj {
	for i, k := range o.keys {
		if k == key {
			o.vals[i] = val
			return o
		}
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, val)
	return o
}

// jsArr 是 JSON 数组。
type jsArr []any

// phpFloat 标记「按 PHP 的 (float) 输出」的数值。
// strconv 的 'f' + -1 与 PHP 的 serialize_precision=-1 同为最短往返表示（4.0→4，4.9→4.9）。
type phpFloat float64

// eloqTime 是 Eloquent 模型 toJSON 的时间口径：2026-07-01T15:42:57.000000Z。
// 用于「直接输出模型属性」的端点（tutorials）。
type eloqTime time.Time

// isoTime 是 Carbon::toIso8601String() 的口径：2026-11-07T04:29:24+00:00。
// 用于 /api/me 里显式调用 toIso8601String() 的字段。
type isoTime time.Time

// phpEncode 生成与 PHP json_encode（默认选项）字节兼容的输出。
func phpEncode(v any) []byte {
	buf := make([]byte, 0, 256)
	return appendPHP(buf, v)
}

func appendPHP(dst []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(dst, "null"...)
	case *jsObj:
		dst = append(dst, '{')
		for i, k := range t.keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendPHPString(dst, k)
			dst = append(dst, ':')
			dst = appendPHP(dst, t.vals[i])
		}
		return append(dst, '}')
	case jsObj:
		return appendPHP(dst, &t)
	case jsArr:
		dst = append(dst, '[')
		for i, item := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendPHP(dst, item)
		}
		return append(dst, ']')
	case []any:
		return appendPHP(dst, jsArr(t))
	case []string:
		dst = append(dst, '[')
		for i, s := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendPHPString(dst, s)
		}
		return append(dst, ']')
	case string:
		return appendPHPString(dst, t)
	case bool:
		if t {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int:
		return strconv.AppendInt(dst, int64(t), 10)
	case int32:
		return strconv.AppendInt(dst, int64(t), 10)
	case int64:
		return strconv.AppendInt(dst, t, 10)
	case uint64:
		return strconv.AppendUint(dst, t, 10)
	case float64:
		return appendPHPFloat(dst, t)
	case phpFloat:
		return appendPHPFloat(dst, float64(t))
	case eloqTime:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02T15:04:05.000000Z")...)
		return append(dst, '"')
	case isoTime:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02T15:04:05+00:00")...)
		return append(dst, '"')
	case time.Time:
		// 兜底：未显式标注的时间按 Eloquent 口径输出，避免出现 RFC3339 的 +08:00 之类。
		return appendPHP(dst, eloqTime(t))
	default:
		return append(dst, "null"...)
	}
}

// appendPHPFloat 输出 PHP json_encode 的浮点表示。
//
// 业务里只会出现金额（2 位小数）与倍率（2 位小数），'f' 最短表示与 PHP 完全一致；
// 极端量级（|x| ≥ 1e21 或为 0 附近的次正规数）PHP 会切成 1.0e+21 形式，这里不做模拟 ——
// 出现即说明字段口径错了，宁可输出可读的最短十进制。
func appendPHPFloat(dst []byte, f float64) []byte {
	if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
		return append(dst, "null"...)
	}
	return strconv.AppendFloat(dst, f, 'f', -1, 64)
}

// appendPHPString 按 PHP json_encode 的默认转义规则输出字符串：
// 转义 " \ / 与全部控制字符；所有非 ASCII 字符输出 \uXXXX（BMP 之外用 UTF-16 代理对）。
func appendPHPString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			var esc byte
			switch c {
			case '"':
				esc = '"'
			case '\\':
				esc = '\\'
			case '/':
				esc = '/'
			case '\n':
				esc = 'n'
			case '\r':
				esc = 'r'
			case '\t':
				esc = 't'
			case '\b':
				esc = 'b'
			case '\f':
				esc = 'f'
			default:
				if c < 0x20 {
					dst = append(dst, s[start:i]...)
					dst = append(dst, '\\', 'u', '0', '0', hexDigit(c>>4), hexDigit(c&0xF))
					i++
					start = i
					continue
				}
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', esc)
			i++
			start = i
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// 非法 UTF-8：PHP 的 json_encode 会整体失败；这里用 U+FFFD 兜底，
			// 保证响应始终是合法 JSON。
			r = utf8.RuneError
		}
		dst = append(dst, s[start:i]...)
		dst = appendUnicodeEscape(dst, r)
		i += size
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func appendUnicodeEscape(dst []byte, r rune) []byte {
	if r > 0xFFFF {
		r -= 0x10000
		dst = appendU16Escape(dst, uint16(0xD800+(r>>10)))
		return appendU16Escape(dst, uint16(0xDC00+(r&0x3FF)))
	}
	return appendU16Escape(dst, uint16(r))
}

func appendU16Escape(dst []byte, v uint16) []byte {
	// 每个半字节都要 &0xF：byte(v>>4) 会带上高位，hexDigit 就会吐出 ASCII 之外的字符。
	return append(dst, '\\', 'u',
		hexDigit(byte(v>>12)&0xF), hexDigit(byte(v>>8)&0xF),
		hexDigit(byte(v>>4)&0xF), hexDigit(byte(v)&0xF))
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

// phpFloatOf 把内部 float64 字段标成 PHP float 输出。
func phpFloatOf(f float64) phpFloat { return phpFloat(f) }

// ---- 与 JSON 无关、但同属「PHP 值语义」的小工具 ----

// phpTrim 对齐 PHP trim() 的默认字符集：" \t\n\r\0\x0B"。
func phpTrim(s string) string { return strings.Trim(s, " \t\n\r\x00\x0B") }

// phpTruthy 对齐 PHP 的布尔判定（字符串 "0" 与 "" 为假）。
func phpTruthy(s string) bool { return s != "" && s != "0" }

// phpStrlen 对齐 PHP strlen()：按字节数。注册的「密码至少 6 位」用的就是它。
func phpStrlen(s string) int { return len(s) }

// phpMbStrlen 对齐 PHP mb_strlen(UTF-8)：按字符数。验证码 size:6 用的是它。
func phpMbStrlen(s string) int { return utf8.RuneCountInString(s) }

// phpStrToLower 对齐 PHP strtolower()：只转 ASCII A-Z。
// Go 的 strings.ToLower 会把 'Ä' 也转小写，与 PHP 不一致 —— 验证码比较依赖这一差异。
func phpStrToLower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// phpStrToUpper 对齐 PHP strtoupper()：只转 ASCII a-z。
func phpStrToUpper(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// phpInArray 对齐 PHP in_array($needle, $options)（松散比较 + (string) 转换）。
func phpInArray(v any, options []string) bool {
	s, ok := phpStringValue(v)
	if !ok {
		return false
	}
	for _, opt := range options {
		if s == opt {
			return true
		}
	}
	return false
}

// phpStringValue 对齐 PHP 的 (string) 转换（校验器与 in_array 会遇到的类型）。
func phpStringValue(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		if t {
			return "1", true
		}
		return "", true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	}
	return "", false
}

// phpValueIsString 对齐 PHP is_string()：JSON 里只有真正的字符串才算（123456 不算）。
func phpValueIsString(v any) bool {
	_, ok := v.(string)
	return ok
}

// phpFalsy 对齐 PHP 的布尔判定：null / false / 0 / 0.0 / "" / "0" / 空数组为假。
func phpFalsy(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == "" || t == "0"
	case float64:
		return t == 0
	case int:
		return t == 0
	case int64:
		return t == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// inputString 把 JSON 输入按 PHP 的弱类型口径转成字符串（供 SQL 比较/日志）。
// 非标量（数组/对象）返回空串与 false。
func inputString(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	return phpStringValue(v)
}

// phpURLDecode 对齐 PHP urldecode()：'+' 转空格，%XX 解码，非法转义原样保留。
func phpURLDecode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			b.WriteByte(' ')
			continue
		}
		if c == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
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
