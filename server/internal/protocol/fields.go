// Package protocol 是「代理协议」的聚合包：有序 JSON 编解码 + 协议元数据类型 + 注册表。
// 每个协议一个子包（protocol/<proto>），子包在 init 中 Register 自己，本包不强依赖子包。
package protocol

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Fields 是协议构造所需的字段读取最小接口。
type Fields interface { Str(key string) string; Int(key string) int64 }

// FieldDefault / Param / ClientKind / Spec：协议元数据。
type FieldDefault struct { Key string; Value any }
type Param struct { Key string; Options []string; Default string }
type ClientKind string

const (
	ClientNone ClientKind = ""
	ClientUUID ClientKind = "uuid"
	ClientPassword ClientKind = "password"
	ClientAuth ClientKind = "auth"
	ClientSS2022 ClientKind = "ss2022"
)

// OutboundBuilder 构造出站 settings（边界：字段+地址端口+必填校验→JSON字节）。
type OutboundBuilder func(f Fields, address string, port int64, require func(string) (string, bool)) ([]byte, bool)

// InboundCtx 是入站 settings 装配的纯协议上下文：协议包只按「协议规则」读字段、
// 产出有序对象；带副作用的业务动作（写失败响应、现场生成密钥、读编辑前旧值）由
// admin 以回调/数据注入，协议包不碰 gin、DB、driver。
type InboundCtx struct {
	// Protocol 是当前协议名（dispatcher 自动填），供 socks/http 等共用实现区分。
	Protocol string
	// Input 读字符串字段（缺失返回空串）。
	Input func(key string) string
	// Int 读整数字段（第二值 = 字段「存在且非 nil」，与改动前 present&&v!=nil 口径一致）。
	Int func(key string) (int64, bool)
	// DefaultStr / DefaultInt 取该协议 Spec.Defaults 的缺省（dispatcher 按注册表填）。
	DefaultStr func(key string) string
	DefaultInt func(key string) int64
	// Prev 是编辑前的 settings（有序对象）；新建为 nil。PrevProtocol 是编辑前记录的协议名。
	Prev         any
	PrevProtocol string
	// Fail 写业务校验失败响应（已写响应，返回 false 即终止）。
	Fail func(msg string)
	// FailServer 写服务器错误响应。
	FailServer func(what string, err error)
	// GenWireGuardKey 生成服务端 WireGuard 私钥（带副作用，必须由 admin 注入）。
	GenWireGuardKey func() (privateKey string, err error)
}

// PrevStr 取编辑前 settings 里的字符串（缺失/无旧值返回空串）。
func (c *InboundCtx) PrevStr(path ...string) string { return GetDefaultString(c.Prev, "", path...) }

// InboundBuilder 构造入站 settings（纯装配；业务副作用经 InboundCtx 回调）。
type InboundBuilder func(*InboundCtx) (*Object, bool)

type Spec struct {
	Name string
	Transports []string
	Securities []string
	RealityTransports []string
	RejectMessage string
	Normalize bool
	ForcedNetwork, ForcedSecurity string
	Stream bool
	PresentNetwork string
	Required []string
	Defaults []FieldDefault
	Client ClientKind
	RelayKey string
	Flow bool
	Params []Param
	BuildOutbound OutboundBuilder
	BuildInbound  InboundBuilder
}

var (
	regMu sync.RWMutex
	registry = map[string]Spec{}
)

// Register 由各协议子包 init 调用。
func Register(s Spec) { regMu.Lock(); registry[s.Name] = s; regMu.Unlock() }

// Lookup 查协议。
func Lookup(name string) (Spec, bool) { regMu.RLock(); s, ok := registry[name]; regMu.RUnlock(); return s, ok }

// BuildOutboundSettings 分发到协议的出站构造器。handled=false 表示未注册。
func BuildOutboundSettings(name string, f Fields, address string, port int64, require func(string) (string, bool)) (out []byte, ok bool, handled bool) {
	s, found := Lookup(name)
	if !found || s.BuildOutbound == nil { return nil, false, false }
	b, k := s.BuildOutbound(f, address, port, require)
	return b, k, true
}

// BuildInboundSettings 分发到协议的入站构造器，并自动填好 c.Protocol。
// handled=false 表示该协议未注册入站构造器（调用方保留 admin 兜底）。
func BuildInboundSettings(name string, c *InboundCtx) (out *Object, ok bool, handled bool) {
	s, found := Lookup(name)
	if !found || s.BuildInbound == nil {
		return nil, false, false
	}
	c.Protocol = name
	b, k := s.BuildInbound(c)
	return b, k, true
}

// AllSpecs 返回全部协议（按名排序稳定）。
func AllSpecs() []Spec {
	regMu.RLock(); defer regMu.RUnlock()
	out := make([]Spec, 0, len(registry))
	for _, s := range registry { out = append(out, s) }
	return out
}

// -------- 有序 JSON 编解码（PHP json_encode 口径）--------

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
// Get 读键；Keys/Vals 取副本。
func (o *Object) Get(key string) (any, bool) {
	if o == nil { return nil, false }
	for i, k := range o.keys { if k == key { return o.vals[i], true } }
	return nil, false
}
func (o *Object) Keys() []string { if o == nil { return nil }; out := make([]string, len(o.keys)); copy(out, o.keys); return out }
func (o *Object) Vals() []any { if o == nil { return nil }; out := make([]any, len(o.vals)); copy(out, o.vals); return out }


// RawURLEncode 等价 PHP rawurlencode（空格 → %20）。
func RawURLEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// InArray 判断 v 的字符串形式是否在 options 内。
func InArray(v any, options []string) bool {
	s := ScalarString(v)
	for _, o := range options {
		if s == o {
			return true
		}
	}
	return false
}

// GetDefaultString 取嵌套字符串，缺失（nil）回默认值。
func GetDefaultString(v any, def string, path ...string) string {
	x := Get(v, path...)
	if x == nil {
		return def
	}
	return ScalarString(x)
}

// GetBool 取嵌套真值。
func GetBool(v any, path ...string) bool {
	return Truthy(Get(v, path...))
}

// Get 按路径取嵌套值（Object/Array）；缺失返回 nil。
func Get(v any, path ...string) any {
	cur := v
	for _, key := range path {
		switch t := cur.(type) {
		case *Object:
			n, ok := t.Get(key)
			if !ok { return nil }
			cur = n
		case Array:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(t) { return nil }
			cur = t[idx]
		default:
			return nil
		}
	}
	return cur
}

// ScalarString 把标量按 PHP 口径转字符串。
func ScalarString(v any) string {
	switch t := v.(type) {
	case nil: return ""
	case string: return t
	case bool: if t { return "1" }; return ""
	case int64: return strconv.FormatInt(t, 10)
	case int: return strconv.Itoa(t)
	case float64: return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		if i, err := t.Int64(); err == nil { return strconv.FormatInt(i, 10) }
		return t.String()
	}
	return ""
}

// GetString 取嵌套字符串。
func GetString(v any, path ...string) string { return ScalarString(Get(v, path...)) }

// GetInt 取嵌套整数。
func GetInt(v any, path ...string) int64 {
	switch t := Get(v, path...).(type) {
	case int64: return t
	case int: return int64(t)
	case float64: return int64(t)
	case string: n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64); return n
	case bool: if t { return 1 }; return 0
	}
	return 0
}

// Truthy 判断 PHP 真值。
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil: return false
	case bool: return t
	case string: return t != "" && t != "0"
	case int64: return t != 0
	case int: return t != 0
	case float64: return t != 0
	case Array: return len(t) > 0
	case *Object: return len(t.keys) > 0
	}
	return false
}
