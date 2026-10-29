package admin

// 本文件给节点域补两样 P8d 共享文件里没有的东西：
//   1) 保序 JSON 解析（jsObj/jsArr 形态）—— xray 的 settings / stream_settings /
//      driver_config.agent 都是「原样回显」的 JSON 列，键序必须与 PHP 落库时一致；
//   2) 一组值转换小工具（PHP 的 (int)/(string)/json_decode 数字口径）。
//
// 不放进 json.go（P8d 共享文件，任务边界禁止修改），因此以 node_ 前缀独立存放。

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// parseOrderedJS 把 JSON 文本解析成 jsObj / jsArr（保序）。
//
// 数字保留「PHP json_decode 后的类型」：整数字面量 → int64，其余 → float64
// （与 PHP 的 json_decode 一致；后续 phpEncode 输出才能逐字节一致）。
func parseOrderedJS(raw []byte) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	v, err := parseOrderedJSValue(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("admin: JSON 尾部存在多余内容")
	}
	return v, nil
}

func parseOrderedJSValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			obj := jo()
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("admin: JSON 对象键不是字符串")
				}
				val, err := parseOrderedJSValue(dec)
				if err != nil {
					return nil, err
				}
				obj.put(key, val)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return obj, nil
		case '[':
			arr := jsArr{}
			for dec.More() {
				val, err := parseOrderedJSValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, errors.New("admin: 非法的 JSON 分隔符")
	}
	return convertJSONNumber(tok), nil
}

// convertJSONNumber 复刻 PHP json_decode 的数字类型选择（整数字面量 → int，其余 → float）。
func convertJSONNumber(tok any) any {
	n, ok := tok.(json.Number)
	if !ok {
		return tok
	}
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return f
}

// get 读一个键（对应 PHP 数组的 `$arr[$k] ?? null` 里的取值部分）。
//
// 写在 node_json.go 而不是 json.go：P8d 的共享文件在本任务边界内不可改。
func (o *jsObj) get(key string) (any, bool) {
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

// phpArrayCast 复刻 PHP 的 `(array) $value`：数组原样；map 取 values；标量包成单元素；null 空。
func phpArrayCast(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			out = append(out, val)
		}
		return out
	case nil:
		return nil
	default:
		return []any{t}
	}
}

// valueOrNil 取一个键的值（缺失 → nil），对应 PHP 的 `$arr[$k] ?? null`。
func valueOrNil(obj *jsObj, key string) any {
	if v, ok := obj.get(key); ok {
		return v
	}
	return nil
}

// i64String 输出 int64 的十进制（中转 tag / 订阅 tag 拼接用）。
func i64String(v int64) string { return strconv.FormatInt(v, 10) }

// sha1HexString 输出字符串的 sha1 小写 hex（订阅 content_hash 口径与 PHP sha1() 一致）。
func sha1HexString(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// strconvFormatUint 是 strconv.FormatUint 的薄包装（避免各文件重复 import strconv）。
func strconvFormatUint(v uint64) string { return strconv.FormatUint(v, 10) }

// jsonObject 把任意输入当作关联数组读取（PHP 的 `$x ?? []`：非对象 → 空对象）。
func jsonObject(v any) *jsObj {
	if o, ok := v.(*jsObj); ok {
		return o
	}
	return jo()
}

// jsonArrayValues 复刻 `(array) $value`：数组原样；标量包成单元素；nil → 空。
func jsonArrayValues(v any) jsArr {
	switch t := v.(type) {
	case jsArr:
		return t
	case []any:
		return jsArr(t)
	case nil:
		return jsArr{}
	default:
		return jsArr{t}
	}
}

// jsonGet 复刻 `$arr['a']['b'] ?? null`（任意一层缺失 → nil）。
//
// 支持数组下标：路径段是纯数字时按 jsArr 下标取值（PHP 的 `$arr[0]`）。
func jsonGet(v any, path ...string) any {
	cur := v
	for _, key := range path {
		switch t := cur.(type) {
		case *jsObj:
			next, ok := t.get(key)
			if !ok {
				return nil
			}
			cur = next
		case jsArr:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(t) {
				return nil
			}
			cur = t[idx]
		default:
			return nil
		}
	}
	return cur
}

// jsonGetOK 同 jsonGet，但额外返回「键/下标是否存在」（对应 PHP 的 isset / array_key_exists）。
func jsonGetOK(v any, path ...string) (any, bool) {
	cur := v
	for _, key := range path {
		switch t := cur.(type) {
		case *jsObj:
			next, ok := t.get(key)
			if !ok {
				return nil, false
			}
			cur = next
		case jsArr:
			idx, err := strconv.Atoi(key)
			if err != nil || idx < 0 || idx >= len(t) {
				return nil, false
			}
			cur = t[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// jsonGetString 复刻 `(string) ($arr['a']['b'] ?? '')`。
func jsonGetString(v any, path ...string) string {
	return nodeScalarString(jsonGet(v, path...))
}

// nodeScalarString 复刻 PHP 的 (string) 转换（本域只用到的标量）。
//
// ⚠️ 必须能吃 json.Number：本仓的 phpjson.Decode 用 `dec.UseNumber()` 解码，
// 3x-ui 返回的所有数字（id / port / total / expiryTime …）都是 json.Number 而不是
// float64。以前这里只有 int64/float64/int 分支，数字一律落到 return "" ——
// 表现就是入站选择器里端口显示成 `JP02 (vless :?)`（见 node_crud.probeInbounds
// 里 `port := nodeScalarString(in["port"]); if port == "" { port = "?" }`）。
func nodeScalarString(v any) string {
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
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case json.Number:
		// 整数形态（3x-ui 的 id/port 都是整数）直接按整数输出，避免出现 "30159.0"。
		if i, err := t.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		return t.String()
	}
	return ""
}

// jsonInt 复刻 PHP 的 (int) 转换。
func jsonInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case json.Number:
		n, _ := strconv.ParseInt(t.String(), 10, 64)
		return n
	case string:
		n, err := strconv.ParseInt(phpTrimSpaceOnly(t), 10, 64)
		if err != nil {
			return 0
		}
		return n
	case bool:
		if t {
			return 1
		}
		return 0
	}
	return 0
}

// nodeTruthy 复刻 PHP 的布尔语义（用于 array_filter 的 `!empty`）。
func nodeTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "0"
	case int64:
		return t != 0
	case float64:
		return t != 0
	case jsArr:
		return len(t) > 0
	case *jsObj:
		return len(t.keys) > 0
	}
	return false
}

// jsonNonEmptyString 复刻 `trim((string) $v) !== ''`。
func jsonNonEmptyString(v any) bool {
	return phpTrimSpaceOnly(nodeScalarString(v)) != ""
}

// rawJSONOrNil 把已构造的 jsObj/jsArr 编码成落库 JSON 文本；nil 保持 nil（PHP 的 null 列）。
func rawJSONOrNil(v any) *string {
	if v == nil {
		return nil
	}
	s := string(phpEncode(v))
	return &s
}
