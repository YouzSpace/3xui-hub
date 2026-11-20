package nodeapi

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// PHP 侧 `$request->validate([...])` 的行为复刻（含通配符展开、nullable、
// required 短路与英文文案）。
//
// 为什么手写而不是用 gin binding：契约要求失败语义与 PHP 逐字对齐。Laravel 的
// 全局异常渲染把 ValidationException 转成 **http 200 + {"code":500,"msg":首条文案}**
// （见 bootstrap/app.php），用 gin binding 会得到完全不同的响应体，agent 的错误
// 日志与运维排查会对不上。

// ValidationError 承载一次校验失败的全部文案（对应 ValidationException）。
type ValidationError struct {
	Messages []string
}

func (e *ValidationError) Error() string { return e.Summary() }

// Summary 复刻 ValidationException::summarize()：
// 首条文案 +（还有 N 条时）" (and N more error|errors)"。
func (e *ValidationError) Summary() string {
	if len(e.Messages) == 0 {
		return "The given data was invalid."
	}
	msg := e.Messages[0]
	if rest := len(e.Messages) - 1; rest > 0 {
		noun := "errors"
		if rest == 1 {
			noun = "error"
		}
		msg += fmt.Sprintf(" (and %d more %s)", rest, noun)
	}
	return msg
}

// ── 规则模型 ─────────────────────────────────────────────────

// ruleKind 区分 implicit 规则（required 等：值缺失也要校验）与普通规则。
type ruleKind int

const (
	kindNormal ruleKind = iota
	kindImplicit
	kindNullable // 语义标记：值恰为 null 时跳过后续规则
)

// ruleCtx 是执行一条规则时的上下文（max/min 需要知道该属性是否带 numeric 规则，
// 以复刻 Laravel 的 getSize() 口径）。
type ruleCtx struct {
	hasNumericRule bool
	display        string // 报错文案里 :attribute 的替换值
}

type rule struct {
	kind  ruleKind
	apply func(attribute string, value any, ctx ruleCtx) (ok bool, message string)
	// numeric 表示这条规则属于 Laravel 的 numericRules（numeric/integer/decimal）。
	numeric bool
}

func normal(r func(string, any) (bool, string)) rule {
	return rule{kind: kindNormal, apply: func(a string, v any, _ ruleCtx) (bool, string) { return r(a, v) }}
}

// sized 构造 max/min 这类依赖数值/字符/数组口径的规则。
func sized(r func(string, any, ruleCtx) (bool, string)) rule {
	return rule{kind: kindNormal, apply: r}
}

// fieldSpec 是「模式 → 规则链」，模式可含 *。
type fieldSpec struct {
	pattern string
	rules   []rule
}

// validator 复刻 Laravel Validator::passes()：
//   - 通配符规则按数据出现顺序展开；
//   - 属性按首次出现顺序校验；
//   - 值缺失或为空串时只跑 implicit 规则；
//   - nullable 且值为 null 时跳过普通规则；
//   - implicit 规则失败后短路本属性剩余规则。
type validator struct {
	data   any // ParseJSON 的结果（Object / Array / json.Number）
	specs  []fieldSpec
	order  []string // 展开后的属性顺序
	byAttr map[string][]rule
	// wildcard 记录由 * 展开出来的属性：Laravel 对这类「implicit attribute」
	// 直接显示原始路径（stats.ch_user_1.up），不做下划线转空格。
	wildcard map[string]bool
}

func newValidator(data any) *validator {
	return &validator{data: data, byAttr: map[string][]rule{}, wildcard: map[string]bool{}}
}

// field 追加一条规则链（链式书写，与 PHP 规则数组顺序一致）。
func (v *validator) field(pattern string, rules ...rule) *validator {
	v.specs = append(v.specs, fieldSpec{pattern: pattern, rules: rules})
	return v
}

func (v *validator) expand() {
	for _, spec := range v.specs {
		if strings.Contains(spec.pattern, "*") {
			for _, attr := range expandWildcard(v.data, spec.pattern) {
				v.wildcard[attr] = true
				v.append(attr, spec.rules...)
			}
			continue
		}
		v.append(spec.pattern, spec.rules...)
	}
}

func (v *validator) append(attr string, rules ...rule) {
	if _, ok := v.byAttr[attr]; !ok {
		v.order = append(v.order, attr)
	}
	v.byAttr[attr] = append(v.byAttr[attr], rules...)
}

// run 执行校验，返回文案列表（空 = 通过）。
func (v *validator) run() []string {
	v.expand()
	var msgs []string

	for _, attr := range v.order {
		rules := v.byAttr[attr]
		value, present := v.lookup(attr)
		emptyString := false
		if s, ok := value.(string); ok && strings.TrimSpace(s) == "" {
			emptyString = true
		}
		nullable := hasNullable(rules)

		display := attr
		if !v.wildcard[attr] {
			display = strings.ReplaceAll(attr, "_", " ")
		}
		ctx := ruleCtx{display: display}
		for _, r := range rules {
			if r.numeric {
				ctx.hasNumericRule = true
			}
		}

		for _, r := range rules {
			if r.kind == kindNullable {
				continue
			}
			// presentOrRuleIsImplicit：缺失/空串时只让 implicit 规则继续。
			if (!present || emptyString) && r.kind != kindImplicit {
				continue
			}
			// isNotNullIfMarkedAsNullable：nullable 且值为 null → 跳过普通规则。
			if nullable && value == nil && r.kind != kindImplicit {
				continue
			}
			ok, message := r.apply(attr, value, ctx)
			if ok {
				continue
			}
			msgs = append(msgs, message)
			if r.kind == kindImplicit {
				// required 失败后不再跑本属性剩余规则（Laravel 的 implicit 短路）。
				break
			}
		}
	}
	return msgs
}

func hasNullable(rules []rule) bool {
	for _, r := range rules {
		if r.kind == kindNullable {
			return true
		}
	}
	return false
}

// lookup 按点路径取原始值；缺键返回 (nil,false)。
func (v *validator) lookup(attr string) (any, bool) {
	return lookupPath(v.data, strings.Split(attr, "."))
}

func lookupPath(cur any, parts []string) (any, bool) {
	for _, p := range parts {
		switch t := cur.(type) {
		case Object:
			var found bool
			for _, m := range t {
				if m.Key == p {
					cur, found = m.Value, true
					break
				}
			}
			if !found {
				return nil, false
			}
		case map[string]any:
			v, ok := t[p]
			if !ok {
				return nil, false
			}
			cur = v
		case Array:
			idx, err := strconv.Atoi(p)
			if err != nil || idx < 0 || idx >= len(t) {
				return nil, false
			}
			cur = t[idx]
		case []any:
			idx, err := strconv.Atoi(p)
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

// expandWildcard 复刻 ValidationRuleParser::explodeWildcardRules：
// 按数据出现顺序，把 pattern 里的每个 * 替换为实际键名。
func expandWildcard(data any, pattern string) []string {
	parts := strings.Split(pattern, ".")
	results := []string{""}
	for i, seg := range parts {
		next := []string{}
		if seg == "*" {
			for _, prefix := range results {
				var container any
				container = data
				if prefix != "" {
					container, _ = lookupPath(data, strings.Split(prefix, "."))
				}
				switch t := container.(type) {
				case Object:
					for _, m := range t {
						next = append(next, joinPath(prefix, m.Key))
					}
				case map[string]any:
					// Go 的 map 无插入顺序；这里按 PHP 侧同一批数据的键序确定性排序，
					// 保证报错顺序稳定（见 sortKeys 注释）。
					for _, k := range sortedKeys(t) {
						next = append(next, joinPath(prefix, k))
					}
				case Array:
					for idx := range t {
						next = append(next, joinPath(prefix, strconv.Itoa(idx)))
					}
				case []any:
					for idx := range t {
						next = append(next, joinPath(prefix, strconv.Itoa(idx)))
					}
				}
				// 容器缺失 → 不产生任何属性（Laravel 同样展开为空）
			}
		} else {
			for _, prefix := range results {
				next = append(next, joinPath(prefix, seg))
			}
		}
		results = next
		if len(results) == 0 {
			break
		}
		_ = i
	}
	return results
}

func joinPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

// ── 规则实现（文案逐字对齐 lang/en/validation.php）───────────

func attrName(attribute string) string {
	return strings.ReplaceAll(attribute, "_", " ")
}

func msg(attribute, template string) string {
	return strings.ReplaceAll(template, ":attribute", attrName(attribute))
}

// msgCtx 用上下文里的显示名拼文案（通配符展开的属性保留原始路径）。
func msgCtx(ctx ruleCtx, attribute, template string) string {
	name := ctx.display
	if name == "" {
		name = attrName(attribute)
	}
	return strings.ReplaceAll(template, ":attribute", name)
}

// Required 对应 Laravel 的 required（implicit）。
//
// 文案里的属性名对通配符展开的属性保留原始路径（stats.ch_user_1.up），
// 与 Laravel 的 getDisplayableAttribute 对 implicit attributes 的处理一致。
func Required() rule {
	return rule{kind: kindImplicit, apply: func(attribute string, value any, ctx ruleCtx) (bool, string) {
		if requiredPresent(value) {
			return true, ""
		}
		return false, msgCtx(ctx, attribute, "The :attribute field is required.")
	}}
}

// Nullable 只是语义标记。
func Nullable() rule { return rule{kind: kindNullable} }

// String 对应 string。
func String() rule {
	return normal(func(attribute string, value any) (bool, string) {
		if _, ok := value.(string); ok {
			return true, ""
		}
		return false, msg(attribute, "The :attribute field must be a string.")
	})
}

// ArrayRule 对应 array（PHP 关联数组与列表都算）。
func ArrayRule() rule {
	return normal(func(attribute string, value any) (bool, string) {
		switch value.(type) {
		case Object, Array, map[string]any, []any:
			return true, ""
		}
		return false, msg(attribute, "The :attribute field must be an array.")
	})
}

// Integer 对应 integer（filter_var FILTER_VALIDATE_INT）。
func Integer() rule {
	return rule{kind: kindNormal, numeric: true, apply: func(attribute string, value any, ctx ruleCtx) (bool, string) {
		if isRuleInteger(value) {
			return true, ""
		}
		return false, msgCtx(ctx, attribute, "The :attribute field must be an integer.")
	}}
}

// Numeric 对应 numeric（PHP is_numeric）。
func Numeric() rule {
	return rule{kind: kindNormal, numeric: true, apply: func(attribute string, value any, ctx ruleCtx) (bool, string) {
		if isRuleNumeric(value) {
			return true, ""
		}
		return false, msgCtx(ctx, attribute, "The :attribute field must be a number.")
	}}
}

// Boolean 对应 boolean（true/false/0/1/"0"/"1"）。
func Boolean() rule {
	return normal(func(attribute string, value any) (bool, string) {
		if isRuleBoolean(value) {
			return true, ""
		}
		return false, msg(attribute, "The :attribute field must be true or false.")
	})
}

// Max 对应 max:N（数组按元素数、numeric 规则下按数值、否则按字符数）。
func Max(limit int) rule {
	return sized(func(attribute string, value any, ctx ruleCtx) (bool, string) {
		switch sizeKind(value, ctx.hasNumericRule) {
		case sizeArray:
			if lenContainer(value) > limit {
				return false, msgCtx(ctx, attribute, fmt.Sprintf("The :attribute field must not have more than %d items.", limit))
			}
		case sizeNumeric:
			if numericValue(value) > float64(limit) {
				return false, msgCtx(ctx, attribute, fmt.Sprintf("The :attribute field must not be greater than %d.", limit))
			}
		default:
			if runeLen(stringOf(value)) > limit {
				return false, msgCtx(ctx, attribute, fmt.Sprintf("The :attribute field must not be greater than %d characters.", limit))
			}
		}
		return true, ""
	})
}

// Min 对应 min:N。
func Min(limit int) rule {
	return sized(func(attribute string, value any, ctx ruleCtx) (bool, string) {
		switch sizeKind(value, ctx.hasNumericRule) {
		case sizeArray:
			if lenContainer(value) < limit {
				return false, msgCtx(ctx, attribute, fmt.Sprintf("The :attribute field must have at least %d items.", limit))
			}
		case sizeNumeric:
			if numericValue(value) < float64(limit) {
				return false, msgCtx(ctx, attribute, fmt.Sprintf("The :attribute field must be at least %d.", limit))
			}
		default:
			if runeLen(stringOf(value)) < limit {
				return false, msgCtx(ctx, attribute, fmt.Sprintf("The :attribute field must be at least %d characters.", limit))
			}
		}
		return true, ""
	})
}

type sizeClass int

const (
	sizeString sizeClass = iota
	sizeNumeric
	sizeArray
)

// sizeKind 复刻 getSize() + hasRule(numericRules) 的选择逻辑。
// hasNumericRule=true 表示该属性带 integer/numeric 规则（PHP 的 numericRules）。
func sizeKind(value any, hasNumericRule bool) sizeClass {
	switch value.(type) {
	case Object, Array, map[string]any, []any:
		return sizeArray
	}
	if hasNumericRule && isRuleNumeric(value) {
		return sizeNumeric
	}
	return sizeString
}

func lenContainer(v any) int {
	switch t := v.(type) {
	case Array:
		return len(t)
	case Object:
		return len(t)
	case []any:
		return len(t)
	case map[string]any:
		return len(t)
	}
	return 0
}

// numericValue 取数值（用于 max:0 这类比较）。
func numericValue(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	}
	return 0
}

func runeLen(s string) int { return len([]rune(s)) }

// requiredPresent 复刻 validateRequired 的空值判定，并按 PHP 数组处理 Object。
func requiredPresent(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case Array:
		return len(t) > 0
	case Object:
		return len(t) > 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// isRuleInteger 复刻 filter_var($v, FILTER_VALIDATE_INT) !== false。
func isRuleInteger(v any) bool {
	switch t := v.(type) {
	case bool:
		return t // PHP: true → 1；false → false
	case json.Number:
		return phpNumberIsInt(t)
	case float64:
		return t == math.Trunc(t) && !math.IsInf(t, 0) &&
			t >= -9223372036854775808 && t <= 9223372036854775807
	case int, int64:
		return true
	case string:
		return phpStrictIntString(t)
	}
	return false
}

func phpNumberIsInt(n json.Number) bool {
	if _, err := n.Int64(); err == nil {
		return true
	}
	f, err := n.Float64()
	if err != nil {
		return false
	}
	return f == math.Trunc(f) && !math.IsInf(f, 0) &&
		f >= -9223372036854775808 && f <= 9223372036854775807
}

// phpStrictIntString 复刻 FILTER_VALIDATE_INT 的字符串口径：
// 允许首尾空白、正负号；拒绝前导 0（"01"）、小数、指数与溢出。
func phpStrictIntString(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	body := trimmed
	if body[0] == '+' || body[0] == '-' {
		body = body[1:]
	}
	if body == "" || (len(body) > 1 && body[0] == '0') {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(trimmed, 10, 64)
	return err == nil
}

// isRuleNumeric 复刻 PHP is_numeric()。
func isRuleNumeric(v any) bool {
	switch t := v.(type) {
	case json.Number, float64, int, int64:
		return true
	case string:
		return isNumericString(t)
	}
	return false
}

func isNumericString(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if _, err := strconv.ParseFloat(t, 64); err == nil {
		// 排除 "NaN"/"Inf" 这类 Go 接受但 PHP 不接受的字面量
		lower := strings.ToLower(t)
		if strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
			return false
		}
		return true
	}
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		_, err := strconv.ParseUint(t[2:], 16, 64)
		return err == nil
	}
	return false
}

// isRuleBoolean 复刻 validateBoolean 的默认（非 strict）口径。
func isRuleBoolean(v any) bool {
	switch t := v.(type) {
	case bool:
		return true
	case json.Number:
		return t.String() == "0" || t.String() == "1"
	case float64:
		return t == 0 || t == 1
	case int, int64:
		n := intOf(v)
		return n == 0 || n == 1
	case string:
		return t == "0" || t == "1"
	}
	return false
}

// sortedKeys 返回 map 的确定性键序。
//
// PHP 的 json_decode 关联数组保留**文档顺序**，Go 的 map 没有顺序。校验报错顺序
// 依赖属性展开顺序（如 stats 下多个用户），因此这里按键名排序固定下来 ——
// 对 agent 的排障体验等价（同一条错误先出现），且保证测试可重复。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
