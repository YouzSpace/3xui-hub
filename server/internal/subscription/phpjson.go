package subscription

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 本文件是 PHP json_encode(JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES) 的手写复刻。
//
// 为什么不用 encoding/json：
//   - Go 的 map 键会被排序，PHP 保持插入顺序（订阅 outbounds 的字段顺序对客户端无影响，
//     但对「byte 级对拍」是硬约束）；
//   - Go 默认把 '<' '>' '&' 转义成 < 形态，PHP 不转；
//   - Go 默认输出 UTF-8 原文，PHP 默认转 \uXXXX（singbox 输出**没有** JSON_UNESCAPED_UNICODE）；
//   - 数组 vs 对象的空值与嵌套缩进规则要与 PHP 一致（空数组 [] / 空对象 {}）。
//
// 数据结构用 phpMap（有序键值对）与 []any 表达，与 PHP 的 array 语义一一对应。

// phpMap 是一个保持插入顺序的「PHP 关联数组」。
type phpMap []phpKV

// phpKV 是 phpMap 的一个键值对。
type phpKV struct {
	Key   string
	Value any
}

// obj 便捷构造：obj(k1, v1, k2, v2, ...)。
func obj(kv ...any) phpMap {
	m := make(phpMap, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m = append(m, phpKV{Key: kv[i].(string), Value: kv[i+1]})
	}
	return m
}

// set 追加或覆盖一个键（PHP $a['k'] = $v 的语义：覆盖时保持原位置）。
func (m *phpMap) set(key string, value any) {
	for i := range *m {
		if (*m)[i].Key == key {
			(*m)[i].Value = value
			return
		}
	}
	*m = append(*m, phpKV{Key: key, Value: value})
}

// get 取键值。
func (m phpMap) get(key string) (any, bool) {
	for _, kv := range m {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// jsonNumber 保存一个「已经是合法 JSON 数字文本」的值（PHP 里 int/float 的原始写法）。
type jsonNumber string

// String 返回原始数字文本（实现 fmt.Stringer 之外，也给 phpshim 的 (string) 转换用）。
func (n jsonNumber) String() string { return string(n) }

// Int64 按 PHP 的 (int) 语义解析（先试整数，失败再按浮点截断）。
func (n jsonNumber) Int64() (int64, error) {
	return strconv.ParseInt(string(n), 10, 64)
}

// Float64 按 PHP 的 (float) 语义解析。
func (n jsonNumber) Float64() (float64, error) {
	return strconv.ParseFloat(string(n), 64)
}

// jsonPretty 等价于 PHP json_encode($v, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES)。
//
// 失败（不可编码的值）时返回 ""，与 PHP 返回 false 的调用侧表现一致（订阅链路里
// 生成内容都是受控结构，不会走到这里）。
func jsonPretty(v any) string {
	var b strings.Builder
	if !writeJSON(&b, v, 0, true, true) {
		return ""
	}
	return b.String()
}

// jsonCompactUnicode 等价于 PHP json_encode($v, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE)。
//
// vmess:// 的 payload 与 xhttp 的 extra 用的就是这一组 flag（中文节点名保持原文）。
func jsonCompactUnicode(v any) string {
	var b strings.Builder
	if !writeJSON(&b, v, 0, false, false) {
		return ""
	}
	return b.String()
}

// jsonCompact 等价于 PHP json_encode($v, JSON_UNESCAPED_SLASHES)（无缩进）。
func jsonCompact(v any) string {
	var b strings.Builder
	if !writeJSON(&b, v, 0, false, true) {
		return ""
	}
	return b.String()
}

// writeJSON 递归写出 JSON；pretty 为 true 时按 PHP 的 4 空格缩进规则。
//
// 返回 false 表示遇到不可编码的值（对应 PHP 的 json_encode 返回 false）。
func writeJSON(b *strings.Builder, v any, depth int, pretty, escapeUnicode bool) bool {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		if !writeJSONString(b, t, escapeUnicode) {
			return false
		}
	case jsonNumber:
		if t == "" {
			b.WriteString("0")
			return true
		}
		b.WriteString(string(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(t, 10))
	case float64:
		s, ok := phpJSONFloat(t)
		if !ok {
			return false
		}
		b.WriteString(s)
	case []any:
		return writeJSONArray(b, t, depth, pretty, escapeUnicode)
	case phpMap:
		return writeJSONObject(b, t, depth, pretty, escapeUnicode)
	case map[string]string:
		// 只有单键或顺序无关的场景使用；为避免「Go map 键被排序」的隐性差异，
		// 这里按 PHP 的插入顺序无法还原，调用方应改用 phpMap。
		return writeJSONObject(b, sortedStringMap(t), depth, pretty, escapeUnicode)
	default:
		return false
	}
	return true
}

func sortedStringMap(m map[string]string) phpMap {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 稳定排序：仅作为兜底路径，不参与对拍断言。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	out := make(phpMap, 0, len(keys))
	for _, k := range keys {
		out = append(out, phpKV{Key: k, Value: m[k]})
	}
	return out
}

func writeJSONArray(b *strings.Builder, arr []any, depth int, pretty, escapeUnicode bool) bool {
	if len(arr) == 0 {
		b.WriteString("[]")
		return true
	}
	b.WriteByte('[')
	for i, item := range arr {
		if i > 0 {
			b.WriteByte(',')
		}
		if pretty {
			b.WriteByte('\n')
			writeIndent(b, depth+1)
		}
		if !writeJSON(b, item, depth+1, pretty, escapeUnicode) {
			return false
		}
	}
	if pretty {
		b.WriteByte('\n')
		writeIndent(b, depth)
	}
	b.WriteByte(']')
	return true
}

func writeJSONObject(b *strings.Builder, m phpMap, depth int, pretty, escapeUnicode bool) bool {
	if len(m) == 0 {
		b.WriteString("{}")
		return true
	}
	b.WriteByte('{')
	for i, kv := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		if pretty {
			b.WriteByte('\n')
			writeIndent(b, depth+1)
		}
		if !writeJSONString(b, kv.Key, escapeUnicode) {
			return false
		}
		b.WriteByte(':')
		if pretty {
			b.WriteByte(' ')
		}
		if !writeJSON(b, kv.Value, depth+1, pretty, escapeUnicode) {
			return false
		}
	}
	if pretty {
		b.WriteByte('\n')
		writeIndent(b, depth)
	}
	b.WriteByte('}')
	return true
}

// writeIndent 输出 PHP pretty-print 的 4 空格缩进。
func writeIndent(b *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		b.WriteString("    ")
	}
}

// writeJSONString 复刻 PHP json_encode 的字符串转义：
//   - 双引号、反斜杠转义；
//   - \b \t \n \f \r 用短转义，其余 < 0x20 用 \u00XX；
//   - '/' 不转义（JSON_UNESCAPED_SLASHES）；
//   - 非 ASCII 一律 \uXXXX（PHP 默认行为），BMP 外字符输出代理对；
//   - 非法 UTF-8 返回 false（PHP json_encode 返回 false）。
func writeJSONString(b *strings.Builder, s string, escapeUnicode bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			switch {
			case r < 0x20:
				b.WriteString(`\u00`)
				b.WriteByte(hexLower(byte(r) >> 4))
				b.WriteByte(hexLower(byte(r) & 0x0F))
			case r < 0x80:
				b.WriteByte(byte(r))
			case !escapeUnicode:
				b.WriteRune(r)
			case r <= 0xFFFF:
				writeUnicodeEscape(b, uint16(r))
			default:
				r -= 0x10000
				hi := uint16(0xD800 + (r >> 10))
				lo := uint16(0xDC00 + (r & 0x3FF))
				writeUnicodeEscape(b, hi)
				writeUnicodeEscape(b, lo)
			}
		}
	}
	b.WriteByte('"')
	return true
}

func writeUnicodeEscape(b *strings.Builder, u uint16) {
	const hexdigits = "0123456789abcdef"
	b.WriteString(`\u`)
	b.WriteByte(hexdigits[(u>>12)&0xF])
	b.WriteByte(hexdigits[(u>>8)&0xF])
	b.WriteByte(hexdigits[(u>>4)&0xF])
	b.WriteByte(hexdigits[u&0xF])
}

func hexLower(v byte) byte {
	const hexdigits = "0123456789abcdef"
	return hexdigits[v&0xF]
}

// phpJSONFloat 复刻 PHP json_encode 对 float 的输出：
//   - serialize_precision=-1 → 最短往返表示；
//   - 整数值输出为 "1"（不带 .0）；
//   - 指数表示用大写 E 且带符号（1.0E+15）。
func phpJSONFloat(f float64) (string, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", false // PHP json_encode 对 NAN/INF 返回 false
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10), true
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	s = strings.ReplaceAll(s, "e", "E")
	if i := strings.IndexByte(s, 'E'); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		if !strings.HasPrefix(exp, "-") && !strings.HasPrefix(exp, "+") {
			exp = "+" + exp
		}
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		s = mant + "E" + exp
	}
	return s, true
}

// decodeJSONAny 解析 JSON 文本为 PHP 语义的 any（对象→phpMap、数组→[]any、数字→jsonNumber）。
//
// 失败返回 nil：PHP 侧 json_decode(..., true) 失败同样得到 null，调用方都按「没取到」处理。
func decodeJSONAny(text string) any {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil
	}
	return fromRawJSON(raw)
}

func fromRawJSON(raw any) any {
	switch t := raw.(type) {
	case map[string]any:
		// encoding/json 的 map 丢失原始顺序；这里按 Go 迭代顺序无法还原 PHP 顺序。
		// 订阅链路只从 stream_settings 里「按键取值」，不回写整块 JSON，故顺序无关。
		m := make(phpMap, 0, len(t))
		for k, v := range t {
			m = append(m, phpKV{Key: k, Value: fromRawJSON(v)})
		}
		return m
	case []any:
		out := make([]any, 0, len(t))
		for _, v := range t {
			out = append(out, fromRawJSON(v))
		}
		return out
	case json.Number:
		return jsonNumber(t.String())
	default:
		return raw
	}
}

// jsonField 从 phpMap 里取嵌套字段（PHP 的 $arr['a']['b'] 读取，缺任何一层都是 null）。
func jsonField(m any, path ...string) any {
	cur := m
	for _, key := range path {
		obj, ok := cur.(phpMap)
		if !ok {
			return nil
		}
		next, ok := obj.get(key)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// jsonFieldString 等价于 PHP 的 (string)($arr['a']['b'] ?? ”)。
func jsonFieldString(m any, path ...string) string { return phpString(jsonField(m, path...)) }

// jsonFieldMap 取嵌套对象；不是对象时返回 nil。
func jsonFieldMap(m any, path ...string) phpMap {
	v := jsonField(m, path...)
	if obj, ok := v.(phpMap); ok {
		return obj
	}
	return nil
}

// jsonFieldSlice 取嵌套数组；不是数组时返回 nil。
func jsonFieldSlice(m any, path ...string) []any {
	v := jsonField(m, path...)
	if arr, ok := v.([]any); ok {
		return arr
	}
	return nil
}
