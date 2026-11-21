// Package phpjson 是「值 → JSON 字节」按 PHP json_encode 默认口径的唯一实现。
//
// 为什么要独立成包（2026-11-22）：协议相关逻辑（入站/出站 settings 组装、分享链接）
// 要按协议拆进 internal/protocol/<proto>/ 独立包，而那些函数都依赖这套有序 JSON
// （键序保留、中文 \uXXXX 转义、斜杠转义…）。原先编码器是 handler/admin 的包内实现
// （handler/api、driver/internal/phpjson 另有两份近似副本），协议包无法 import。
// 本包作为「handler 侧协议逻辑」的共享编码器，供 admin（类型别名接入）与后续
// internal/protocol/* 复用。
//
// 行为与抽取前 handler/admin/json.go 的编码器逐字节一致（由既有 golden 测试守护）。
package phpjson

import (
	"encoding/json"
	"strconv"
	"time"
	"unicode/utf8"
)

// Object 是键序可控的 JSON 对象（对应 PHP 的关联数组）。
type Object struct {
	keys []string
	vals []any
}

// jo 按参数顺序构造对象：jo("code", 0, "msg", "ok")。
func New(pairs ...any) *Object {
	if len(pairs)%2 != 0 {
		panic("admin: jo() 需要偶数个参数")
	}
	o := &Object{keys: make([]string, 0, len(pairs)/2), vals: make([]any, 0, len(pairs)/2)}
	for i := 0; i < len(pairs); i += 2 {
		o.keys = append(o.keys, pairs[i].(string))
		o.vals = append(o.vals, pairs[i+1])
	}
	return o
}

// put 追加或覆盖一个键；覆盖时保持原位置（与 PHP 数组赋值语义一致）。
func (o *Object) Put(key string, val any) *Object {
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

// Get 读一个键（对应 PHP 数组的 `$arr[$k] ?? null` 里的取值部分）。
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	for i, k := range o.keys {
		if k == key {
			return o.vals[i], true
		}
	}
	return nil, false
}

// Keys 返回对象键序副本（供需要转换到其它保序结构的调用方读取）。
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}

// Vals 返回对象值序副本（与 Keys 一一对应）。
func (o *Object) Vals() []any {
	if o == nil {
		return nil
	}
	out := make([]any, len(o.vals))
	copy(out, o.vals)
	return out
}

// Array 是 JSON 数组。
type Array []any

// Float 标记「按 PHP 的 (float) 输出」的数值：
// strconv 的 'f' + -1 与 PHP 的 serialize_precision=-1 同为最短往返表示（4.0→4，4.9→4.9）。
type Float float64

// EloqTime 是 Eloquent 模型 toJSON 的时间口径：2026-07-01T15:42:57.000000Z。
type EloqTime time.Time

// ISOTime 是 Carbon::toIso8601String() 的口径：2026-11-07T04:29:24+00:00。
type ISOTime time.Time

// DateTimeStr 是 Carbon::toDateTimeString() 的口径：2026-10-24 09:01:59。
type DateTimeStr time.Time

// phpEncode 生成与 PHP json_encode（默认选项）字节兼容的输出。
func Encode(v any) []byte {
	buf := make([]byte, 0, 256)
	return appendPHP(buf, v)
}

func appendPHP(dst []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(dst, "null"...)
	case *Object:
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
	case Object:
		return appendPHP(dst, &t)
	case Array:
		dst = append(dst, '[')
		for i, item := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendPHP(dst, item)
		}
		return append(dst, ']')
	case []any:
		return appendPHP(dst, Array(t))
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
	case int64:
		return strconv.AppendInt(dst, t, 10)
	case uint64:
		return strconv.AppendUint(dst, t, 10)
	case float64:
		return appendPHPFloat(dst, t)
	case json.Number:
		// decodeScheduleConfig 用 UseNumber 解析数字为 json.Number；转回数字避免 default 分支输出 null
		// （applyWarp merge 已有 settings 时，mtu/reserved 等数字字段会踩到）。
		if f, err := t.Float64(); err == nil {
			return appendPHPFloat(dst, f)
		}
		return append(dst, "null"...)
	case Float:
		return appendPHPFloat(dst, float64(t))
	case EloqTime:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02T15:04:05.000000Z")...)
		return append(dst, '"')
	case ISOTime:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02T15:04:05+00:00")...)
		return append(dst, '"')
	case DateTimeStr:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02 15:04:05")...)
		return append(dst, '"')
	case time.Time:
		return appendPHP(dst, DateTimeStr(t))
	default:
		return append(dst, "null"...)
	}
}

// appendPHPFloat 输出 PHP json_encode 的浮点表示（业务里只有金额与倍率，2 位小数）。
func appendPHPFloat(dst []byte, f float64) []byte {
	if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
		return append(dst, "null"...)
	}
	return strconv.AppendFloat(dst, f, 'f', -1, 64)
}

// appendPHPString 复刻 PHP json_encode 默认选项的字符串转义：
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
