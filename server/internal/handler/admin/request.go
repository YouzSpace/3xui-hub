package admin

import (
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// jsonNumber 保存 JSON 数字的原始字面量（如 "1"、"0.90"、"1e3"）。
//
// 为什么要保留原文而不是转成 float64：PHP 的 json_decode 会区分 int 与 float，
// 后续 `(string)` 转换与 is_numeric / filter_var 的结果都依赖这个区分
// （"10.00" 与 10.00 在 in / integer 规则下表现不同）。用原文表达能逐字节对齐。
type jsonNumber string

// float 取数值。
func (n jsonNumber) float() float64 {
	f, _ := strconv.ParseFloat(string(n), 64)
	return f
}

// isIntLiteral 判断字面量是否为整数写法（无小数点/指数）。
func (n jsonNumber) isIntLiteral() bool {
	s := string(n)
	return !strings.ContainsAny(s, ".eE")
}

// phpValue 转成「PHP 侧看到的值」：整数 → int64，其余 → float64。
func (n jsonNumber) phpValue() any {
	if n.isIntLiteral() {
		if v, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return v
		}
	}
	return n.float()
}

// jsonInput 读取 JSON 请求体并模拟 Laravel 的全局输入处理：
//
//	TrimStrings               → 字符串去首尾空白（password 系字段除外）
//	ConvertEmptyStringsToNull → 去空白后为空的字符串变成 null
//
// 这两步在 PHP 里发生在所有路由之前，控制器看到的输入永远是「已整理」的形态。
// 非 JSON 请求体按空对象处理（管理端接口全部发 JSON）。
func jsonInput(c *gin.Context) map[string]any {
	input := map[string]any{}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 8<<20))
	if err != nil || len(body) == 0 {
		return input
	}
	if !strings.Contains(c.GetHeader("Content-Type"), "json") {
		return input
	}

	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return input
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return input
	}
	for k, v := range obj {
		input[k] = normalizeInputValue(k, convertNumbers(v))
	}
	return input
}

// convertNumbers 把 encoding/json 的 json.Number 换成保留原文的 jsonNumber。
func convertNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		return jsonNumber(t.String())
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = convertNumbers(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = convertNumbers(item)
		}
		return out
	}
	return v
}

// normalizeInputValue 递归模拟 TrimStrings + ConvertEmptyStringsToNull。
func normalizeInputValue(key string, v any) any {
	switch t := v.(type) {
	case string:
		if neverTrimKey(key) {
			// password 系字段不 trim，但空串仍转 null（ConvertEmptyStringsToNull 不看 except）。
			if t == "" {
				return nil
			}
			return t
		}
		t = phpTrimSpaceOnly(t)
		if t == "" {
			return nil
		}
		return t
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = normalizeInputValue(key, item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = normalizeInputValue(key+"."+k, item)
		}
		return out
	}
	return v
}

// neverTrimKey 对齐 TrimStrings::$except（按最后一段匹配）。
func neverTrimKey(key string) bool {
	last := key
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		last = key[i+1:]
	}
	switch last {
	case "current_password", "password", "password_confirmation":
		return true
	}
	return false
}

// clientIP 对齐 Laravel Request::ip()（无 TrustProxies 时取直连地址）。
func clientIP(c *gin.Context) string {
	if ip := c.ClientIP(); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}

// queryString 取 query 参数（去空白后为空 → nil，对齐 Laravel 的 filled() 口径）。
func queryString(c *gin.Context, key string) *string {
	raw := phpTrimSpaceOnly(c.Query(key))
	if raw == "" {
		return nil
	}
	return &raw
}

// queryInt 取整型 query 参数；缺失/非法返回 def。
func queryInt(c *gin.Context, key string, def int64) int64 {
	raw := phpTrimSpaceOnly(c.Query(key))
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// inputString 取输入里的字符串值（非字符串标量按 PHP (string) 转换）。
func inputString(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case jsonNumber:
		return string(t), true
	case bool:
		if t {
			return "1", true
		}
		return "", true
	}
	return "", false
}

// inputInt 取整型输入（PHP 的 (int) 转换口径：数值截断，非数值为 0）。
func inputInt(v any) int64 {
	switch t := v.(type) {
	case jsonNumber:
		if t.isIntLiteral() {
			n, err := strconv.ParseInt(string(t), 10, 64)
			if err == nil {
				return n
			}
		}
		return int64(t.float())
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
	case int:
		return int64(t)
	// 面板内部自己往 input 里回填 id 时用的是 int64/uint64（例如 HY2 自动挑证书），
	// 少这两个分支就会被当成「没传」，症状是明明有值却走缺省逻辑。
	case int64:
		return t
	case uint64:
		return int64(t)
	}
	return 0
}

// inputBool 对齐 PHP 的 (bool) / $request->boolean()：
// true、"1"、1、1.0 为真；false、"0"、0、""、null 为假。
func inputBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "0"
	case jsonNumber:
		return t.float() != 0
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return false
}

// phpInArray 对齐 PHP in_array($needle, $options)（松散比较 + (string) 转换）。
func phpInArray(v any, options []string) bool {
	s := phpScalarString(v)
	for _, opt := range options {
		if s == opt {
			return true
		}
	}
	return false
}

// decimalString 把输入里的数值按 decimal(_,2) 入库口径规范成字符串（保留两位）。
//
// 为什么要这一步：Laravel 的 decimal cast 在读取时输出 "10.00"，写入时按
// 参数绑定交给 MySQL 处理。Go 侧先用字符串保留精度，再交给同一个 MySQL，
// 存储结果与 PHP 完全一致（禁止转 float64，见 data-contract §3）。
func decimalString(v any, scale int) (string, bool) {
	var raw string
	switch t := v.(type) {
	case string:
		raw = phpTrimSpaceOnly(t)
	case jsonNumber:
		raw = string(t)
	case int:
		raw = strconv.Itoa(t)
	case int64:
		raw = strconv.FormatInt(t, 10)
	case float64:
		raw = strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return "", false
	}
	if raw == "" {
		return "", false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(f, 'f', scale, 64), true
}
