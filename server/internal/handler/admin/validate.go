package admin

// 本文件复刻 Laravel Validator 在**管理端商店域用到的规则**上的可观测行为。
//
// 为什么不用手写 if：PHP 侧的失败响应是
//
//	HTTP 200 + {"code":500,"msg":"<第一条文案> (and N more errors)"}
//
// （bootstrap/app.php 的 render 回调把 ValidationException 的 422 改写成 200 + code=500）。
// 前端直接展示 msg，所以文案、合并规则、字段名展示方式都必须逐字一致 ——
// 语料见 validate_test.go（全部取自真实 PHP 响应）。
//
// 已实现的规则：required / nullable / sometimes / string / integer / numeric / boolean /
// array / date / min / max / gt / lte / in / exists / regex / size，以及 `ids.*` 通配。
//
// 刻意不实现（本域控制器未用到，遇到会 panic 以便早暴露）：
// email / unique / confirmed / 自定义 Rule 对象等。

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ruleError 是一条校验失败（字段 + 文案）。
type ruleError struct {
	Field string
	Msg   string
}

// ruleField 是「字段 → 规则列表」，顺序与 PHP 里规则声明顺序一致（决定文案顺序）。
type ruleField struct {
	field string
	rules []string
}

// validator 持有待校验的输入（已做 trim / 空串转 null）与自定义文案。
type validator struct {
	data     map[string]any
	fields   []ruleField
	custom   map[string]string // 键形如 "folder.exists"
	errs     []ruleError
	exists   func(table, column string, value any) (bool, error)
	ctx      context.Context
	storeErr error
}

func newValidator(ctx context.Context, data map[string]any) *validator {
	return &validator{data: data, custom: map[string]string{}, ctx: ctx}
}

// messages 设置自定义文案（对齐 $request->validate($rules, $messages)）。
func (v *validator) messages(m map[string]string) *validator {
	for k, s := range m {
		v.custom[k] = s
	}
	return v
}

// existsChecker 注入 exists 规则的查库实现（Store 提供）。
func (v *validator) existsChecker(fn func(table, column string, value any) (bool, error)) *validator {
	v.exists = fn
	return v
}

func (v *validator) field(name string, rules ...string) *validator {
	v.fields = append(v.fields, ruleField{field: name, rules: rules})
	return v
}

// passes 执行校验；返回 false 时用 summary() 取合并文案。
func (v *validator) passes() bool {
	for _, f := range v.fields {
		v.applyField(f)
	}
	return len(v.errs) == 0
}

// summary 复刻 ValidationException::summarize：第一条 + " (and N more errors)"。
func (v *validator) summary() string {
	if len(v.errs) == 0 {
		return ""
	}
	first := v.errs[0].Msg
	if n := len(v.errs) - 1; n > 0 {
		plural := "error"
		if n > 1 {
			plural = "errors"
		}
		return first + " (and " + strconv.Itoa(n) + " more " + plural + ")"
	}
	return first
}

// storeFailure 返回校验过程中查库失败的错误（handler 据此回 500）。
func (v *validator) storeFailure() error { return v.storeErr }

// valueOf 取一个已通过校验的字段值。
func (v *validator) valueOf(name string) (any, bool) {
	val, ok := v.data[name]
	return val, ok
}

// applyField 按顺序应用规则，复刻 Validator::validateAttribute + isValidatable：
//   - required 是隐式规则：缺失 / null / 空白字符串时也执行；
//   - 其余规则只在「字段存在且值不为 null/空白」时执行；
//   - 字段一旦有隐式规则失败，后续规则停止（shouldStopValidating）。
func (v *validator) applyField(f ruleField) {
	if strings.Contains(f.field, "*") {
		v.applyWildcard(f)
		return
	}

	value, present := v.data[f.field]
	if !present {
		value = nil
	}
	blank := false
	if s, ok := value.(string); ok && phpTrimSpaceOnly(s) == "" {
		blank = true
	}

	for _, rule := range f.rules {
		name, params := splitRule(rule)

		if name == "sometimes" {
			if !present {
				return
			}
			continue
		}
		if name == "nullable" {
			continue
		}

		if name == "required" {
			if !requiredPasses(value) {
				v.addError(f.field, name, params, value)
				return // 隐式规则失败 → 后面的规则不再跑
			}
			continue
		}

		// 非隐式规则：字段缺失 / null / 空白字符串时跳过（「没有可校验的值」）。
		if !present || value == nil || blank {
			continue
		}
		if msg, failed := v.checkRule(name, params, f.field, value); failed {
			_ = msg
			v.addError(f.field, name, params, value)
		}
	}
}

// applyWildcard 处理 `ids.*`：逐个元素套用同一批规则。
//
// Laravel 把 `ids.*` 展开成 `ids.0`、`ids.1`……，文案里的字段名就是展开后的原始名
// （隐式属性不做下划线转空格），所以这里直接用 "ids.0" 作为展示名。
func (v *validator) applyWildcard(f ruleField) {
	base := strings.TrimSuffix(f.field, ".*")
	raw, ok := v.data[base]
	if !ok {
		// 父字段缺失时通配规则不产生错误（父字段自己由 `ids => required` 兜底）。
		return
	}
	items, isArr := raw.([]any)
	if !isArr {
		return
	}
	for i, item := range items {
		field := base + "." + strconv.Itoa(i)
		// 与 applyField 同一条 gating 规则：字段一旦有隐式规则（required）失败，
		// 后续规则停止 —— 否则 "x" 会同时产生 integer 与 min 两条错误。
		stopped := false
		for _, rule := range f.rules {
			name, params := splitRule(rule)
			switch name {
			case "sometimes", "nullable":
				continue
			case "required":
				if !requiredPasses(item) {
					v.addError(field, name, params, item)
					stopped = true
				}
				continue
			}
			if stopped {
				continue
			}
			if item == nil {
				continue
			}
			// 与 PHP 一致：integer 失败后不中断，min 仍会跑一遍（"x" 的 size 是 1，
			// 因此只留下 integer 一条错误；0 则是 integer 通过、min 失败）。
			if _, failed := v.checkRule(name, params, field, item); failed {
				v.addError(field, name, params, item)
			}
		}
	}
}

// checkRule 判定单条规则；返回 (文案, 是否失败)。
func (v *validator) checkRule(name string, params []string, field string, value any) (string, bool) {
	switch name {
	case "string":
		if _, ok := value.(string); !ok {
			return "", true
		}
	case "integer":
		if !phpIntegerPasses(value) {
			return "", true
		}
	case "numeric":
		if !phpNumericPasses(value) {
			return "", true
		}
	case "boolean":
		if !phpBooleanPasses(value) {
			return "", true
		}
	case "array":
		// PHP 的 is_array() 同时接受列表与关联数组（{"--accent":"#fff"} 也是 array）。
		if !phpIsArray(value) {
			return "", true
		}
	case "date":
		if !phpDatePasses(value) {
			return "", true
		}
	case "min":
		if compareSize(v.attributeType(field), value, paramsAt(params, 0)) < 0 {
			return "", true
		}
	case "max":
		if compareSize(v.attributeType(field), value, paramsAt(params, 0)) > 0 {
			return "", true
		}
	case "size":
		if compareSize(v.attributeType(field), value, paramsAt(params, 0)) != 0 {
			return "", true
		}
	case "gt":
		if phpNumberOf(value) <= parseFloatSafe(paramsAt(params, 0)) {
			return "", true
		}
	case "gte":
		if phpNumberOf(value) < parseFloatSafe(paramsAt(params, 0)) {
			return "", true
		}
	case "lt":
		if phpNumberOf(value) >= parseFloatSafe(paramsAt(params, 0)) {
			return "", true
		}
	case "lte":
		if phpNumberOf(value) > parseFloatSafe(paramsAt(params, 0)) {
			return "", true
		}
	case "in":
		if !phpInArray(value, params) {
			return "", true
		}
	case "regex":
		if !phpRegexPasses(value, paramsAt(params, 0)) {
			return "", true
		}
	case "exists":
		if !v.existsPasses(params, value) {
			return "", true
		}
	default:
		panic("admin: 未实现的校验规则 " + name)
	}
	return "", false
}

// addError 生成一条文案（含自定义文案与字段名展示）。
func (v *validator) addError(field, rule string, params []string, value any) {
	msg, ok := v.custom[field+"."+rule]
	if !ok {
		msg = v.defaultMessage(field, rule, params)
	}
	v.errs = append(v.errs, ruleError{Field: field, Msg: msg})
}

// defaultMessage 复刻 getMessage + makeReplacements 的本域子集。
func (v *validator) defaultMessage(field, rule string, params []string) string {
	display := displayAttribute(field)

	var tmpl string
	switch rule {
	case "required":
		tmpl = "The :attribute field is required."
	case "string":
		tmpl = "The :attribute field must be a string."
	case "integer":
		tmpl = "The :attribute field must be an integer."
	case "numeric":
		tmpl = "The :attribute field must be a number."
	case "boolean":
		tmpl = "The :attribute field must be true or false."
	case "array":
		tmpl = "The :attribute field must be an array."
	case "date":
		tmpl = "The :attribute field must be a valid date."
	case "exists":
		tmpl = "The selected :attribute is invalid."
	case "in":
		tmpl = "The selected :attribute is invalid."
	case "regex":
		tmpl = "The :attribute field format is invalid."
	case "min", "max", "size", "gt", "gte", "lt", "lte":
		tmpl = sizeMessage(v.attributeType(field), rule)
	default:
		panic("admin: 未实现文案的规则 " + rule)
	}

	out := strings.ReplaceAll(tmpl, ":attribute", display)
	switch rule {
	case "min":
		out = strings.ReplaceAll(out, ":min", paramsAt(params, 0))
	case "max":
		out = strings.ReplaceAll(out, ":max", paramsAt(params, 0))
	case "size":
		out = strings.ReplaceAll(out, ":size", paramsAt(params, 0))
	case "gt", "gte", "lt", "lte":
		out = strings.ReplaceAll(out, ":value", paramsAt(params, 0))
	}
	return out
}

// sizeMessage 取 validation.<rule>.<type>（numeric / array / string 三种）。
func sizeMessage(attrType, rule string) string {
	switch rule {
	case "min":
		switch attrType {
		case "numeric":
			return "The :attribute field must be at least :min."
		case "array":
			return "The :attribute field must have at least :min items."
		}
		return "The :attribute field must be at least :min characters."
	case "max":
		switch attrType {
		case "numeric":
			return "The :attribute field must not be greater than :max."
		case "array":
			return "The :attribute field must not have more than :max items."
		}
		return "The :attribute field must not be greater than :max characters."
	case "size":
		switch attrType {
		case "numeric":
			return "The :attribute field must be :size."
		case "array":
			return "The :attribute field must contain :size items."
		}
		return "The :attribute field must be :size characters."
	case "gt":
		if attrType == "numeric" {
			return "The :attribute field must be greater than :value."
		}
		return "The :attribute field must be greater than :value characters."
	case "gte":
		if attrType == "numeric" {
			return "The :attribute field must be greater than or equal to :value."
		}
		return "The :attribute field must be greater than or equal to :value characters."
	case "lt":
		if attrType == "numeric" {
			return "The :attribute field must be less than :value."
		}
		return "The :attribute field must be less than :value characters."
	case "lte":
		if attrType == "numeric" {
			return "The :attribute field must be less than or equal to :value."
		}
		return "The :attribute field must be less than or equal to :value characters."
	}
	panic("admin: 非法 size 规则 " + rule)
}

// attributeType 对齐 getAttributeType：有数值规则 → numeric，array → array，其余 string。
func (v *validator) attributeType(field string) string {
	for _, f := range v.fields {
		// 通配规则（ids.*）的类型由父规则决定：ids.0 也要按 integer 比较。
		if f.field != field && f.field != wildcardParent(field) {
			continue
		}
		for _, r := range f.rules {
			name, _ := splitRule(r)
			switch name {
			case "numeric", "integer", "decimal":
				return "numeric"
			case "array":
				return "array"
			}
		}
	}
	return "string"
}

// displayAttribute 对齐 getDisplayableAttribute：隐式属性（含 . 的通配展开）原样，
// 其余 str_replace('_', ' ', Str::snake($attribute))。
func displayAttribute(field string) string {
	if strings.Contains(field, ".") {
		return field
	}
	return strings.ReplaceAll(field, "_", " ")
}

// ---- 规则实现 ----

// requiredPasses 对齐 validateRequired：null / 空白字符串 / 空数组视为未填。
func requiredPasses(value any) bool {
	switch t := value.(type) {
	case nil:
		return false
	case string:
		return phpTrimSpaceOnly(t) != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// wildcardParent 把 "ids.0" 映射成 "ids.*"（非通配字段返回自身）。
func wildcardParent(field string) string {
	if i := strings.LastIndexByte(field, '.'); i >= 0 {
		return field[:i] + ".*"
	}
	return field
}

// phpIsArray 对齐 PHP 的 is_array()。
func phpIsArray(value any) bool {
	switch value.(type) {
	case []any, map[string]any:
		return true
	}
	return false
}

// compareSize 对齐 min/max/size 的比较口径：数值型比数值，数组比元素数，字符串比字符数。
func compareSize(attrType string, value any, want string) int {
	target := parseFloatSafe(want)
	switch attrType {
	case "numeric":
		// PHP 的 getSize：只有「值本身是数值」且字段有数值规则时按数值比，
		// 否则回落到 mb_strlen（"x" 的 size 是 1，不会因为 min:1 而失败）。
		if phpNumericPasses(value) {
			return cmpFloat(phpNumberOf(value), target)
		}
		return cmpFloat(float64(phpMbStrlen(phpScalarString(value))), target)
	case "array":
		switch t := value.(type) {
		case []any:
			return cmpFloat(float64(len(t)), target)
		case map[string]any:
			return cmpFloat(float64(len(t)), target)
		}
		return 1
	}
	return cmpFloat(float64(phpMbStrlen(phpScalarString(value))), target)
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// phpNumberOf 取数值（非数值按 0，调用前规则已保证是 numeric）。
func phpNumberOf(value any) float64 {
	switch t := value.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case jsonNumber:
		f, _ := strconv.ParseFloat(string(t), 64)
		return f
	case string:
		f, _ := strconv.ParseFloat(phpTrimSpaceOnly(t), 64)
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	}
	return 0
}

// phpScalarString 对齐 PHP 的 (string) 转换。
func phpScalarString(value any) string {
	switch t := value.(type) {
	case string:
		return t
	case jsonNumber:
		return string(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		if t {
			return "1"
		}
		return ""
	case nil:
		return ""
	}
	return fmt.Sprint(value)
}

// phpIntegerPasses 对齐 filter_var($v, FILTER_VALIDATE_INT) !== false。
//
// 实测口径（见 validate_test.go）：整数与整数值浮点通过；字符串必须是指数/小数之外的
// 规范整数（允许首尾空白与 +/-，拒绝 "01"、"1.0"、"1e3"）；布尔 true 通过、false 不通过。
func phpIntegerPasses(value any) bool {
	switch t := value.(type) {
	case jsonNumber:
		return phpIntegerStringPasses(string(t), true)
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return false
		}
		if t != math.Trunc(t) {
			return false
		}
		// FILTER_VALIDATE_INT 只接受 64 位范围内的整数。
		return t >= -9.2233720368547758e18 && t <= 9.2233720368547758e18
	case int:
		return true
	case bool:
		return t
	case string:
		return phpIntegerStringPasses(t, false)
	}
	return false
}

// phpIntegerStringPasses 判断字符串是否为合法整数。
// rawIsJSONNumber=true 时字符串来自 JSON 数字字面量（"1.0" 这种浮点写法不能算整数）。
func phpIntegerStringPasses(s string, rawIsJSONNumber bool) bool {
	trimmed := strings.Trim(s, " \t\n\r\x0B")
	if trimmed == "" {
		return false
	}
	body := trimmed
	if body[0] == '+' || body[0] == '-' {
		body = body[1:]
	}
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	// 前导零：PHP 的 FILTER_VALIDATE_INT 只接受 "0" 本身。
	if len(body) > 1 && body[0] == '0' {
		return false
	}
	if rawIsJSONNumber && len(body) > 18 {
		// 超出 64 位的 JSON 数字在 PHP 里是 float，filter_var 会因精度失败。
		return false
	}
	return true
}

// phpNumericPasses 对齐 is_numeric()（PHP 8 口径：十进制/科学计数，不含 0x 十六进制）。
func phpNumericPasses(value any) bool {
	switch t := value.(type) {
	case jsonNumber:
		return true
	case float64, int, int64:
		return true
	case string:
		return phpIsNumericString(t)
	}
	return false
}

// phpIsNumericString 判断字符串是否满足 PHP 的 is_numeric：可选前导空白、可选符号、
// 数字（可含小数点）、可选指数、可选尾随空白。
func phpIsNumericString(s string) bool {
	i := 0
	n := len(s)
	for i < n && isPHPSpace(s[i]) {
		i++
	}
	if i < n && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < n && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < n && s[i] == '.' {
		i++
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < n && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < n && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exp := 0
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
			exp++
		}
		if exp == 0 {
			return false
		}
	}
	for i < n && isPHPSpace(s[i]) {
		i++
	}
	return i == n
}

func isPHPSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// phpBooleanPasses 对齐 validateBoolean：严格接受 true/false/0/1/"0"/"1"。
func phpBooleanPasses(value any) bool {
	switch t := value.(type) {
	case bool:
		return true
	case jsonNumber:
		s := string(t)
		return s == "0" || s == "1"
	case float64:
		return t == 0 || t == 1
	case int:
		return t == 0 || t == 1
	case string:
		return t == "0" || t == "1"
	}
	return false
}

// dateRe 是 date 规则的可接受形态子集：Y-m-d[ T]H:i[:s][时区] 或 Y-m-d。
// （PHP 的 strtotime 还接受 "tomorrow" 等相对表达；管理端表单不会传，见交付风险。）
var dateRe = regexp.MustCompile(
	`^\d{4}-\d{1,2}-\d{1,2}([ T]\d{1,2}:\d{2}(:\d{2})?)?(Z|[+-]\d{2}:?\d{2})?$`)

// phpDatePasses 对齐 validateDate：非字符串/数值 → false；strtotime 解析失败 → false。
func phpDatePasses(value any) bool {
	switch t := value.(type) {
	case string:
		trimmed := phpTrimSpaceOnly(t)
		if !dateRe.MatchString(trimmed) {
			return false
		}
		return validYMD(trimmed)
	case jsonNumber, float64, int:
		// 纯数字在 PHP 里 strtotime('123') === false（不是时间戳，除非带 @ 前缀）。
		return false
	}
	return false
}

// validYMD 校验年月日的实际范围（2026-13-01 之类的要拒掉）。
func validYMD(s string) bool {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == ' ' || r == 'T' })
	if len(parts) < 3 {
		return false
	}
	y, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	d, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	if m < 1 || m > 12 || d < 1 || d > 31 || y < 1 {
		return false
	}
	// 每月天数
	days := [...]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	max := days[m-1]
	if m == 2 && (y%4 == 0 && (y%100 != 0 || y%400 == 0)) {
		max = 29
	}
	return d <= max
}

// phpRegexPasses 对齐 validateRegex：非字符串/数值 → false；preg_match > 0。
func phpRegexPasses(value any, pattern string) bool {
	switch value.(type) {
	case string, jsonNumber, float64, int:
	default:
		return false
	}
	re, err := compilePHPRegex(pattern)
	if err != nil {
		// PHP 里非法 pattern 会抛警告并让校验失败；这里按「不通过」处理并保持可用。
		return false
	}
	return re.MatchString(phpScalarString(value))
}

// compilePHPRegex 把 PCRE 写法（/.../i）转成 Go regexp。
//
// 只支持本域用到的形态：分隔符 '/'、可选修饰符 i。其它修饰符（m/s/u/x 等）在这里会 panic，
// 以便新增用法时立刻暴露而不是悄悄行为不同。
func compilePHPRegex(pattern string) (*regexp.Regexp, error) {
	if len(pattern) < 2 || pattern[0] != '/' {
		return nil, fmt.Errorf("仅支持 /.../ 形态的 pattern: %q", pattern)
	}
	end := strings.LastIndex(pattern, "/")
	if end <= 0 {
		return nil, fmt.Errorf("pattern 缺少结尾分隔符: %q", pattern)
	}
	body := pattern[1:end]
	flags := pattern[end+1:]
	for i := 0; i < len(flags); i++ {
		if flags[i] != 'i' {
			return nil, fmt.Errorf("不支持的 pattern 修饰符 %q（%q）", string(flags[i]), pattern)
		}
	}
	if strings.Contains(flags, "i") {
		body = "(?i)" + body
	}
	return regexp.Compile(body)
}

// existsPasses 查库判断取值是否存在（对应 exists:table,column）。
func (v *validator) existsPasses(params []string, value any) bool {
	if v.exists == nil || len(params) < 2 {
		return false
	}
	ok, err := v.exists(params[0], params[1], value)
	if err != nil {
		if v.storeErr == nil {
			v.storeErr = err
		}
		return false
	}
	return ok
}

// ---- 小工具 ----

// splitRule 拆「规则名 + 参数」。
//
// 对齐 Laravel 的 ValidationRuleParser::parseParameters：**regex 规则不按逗号切**
// （参数里可能含 {1,10} 这种量词），其余规则用逗号分隔。
func splitRule(rule string) (string, []string) {
	i := strings.IndexByte(rule, ':')
	if i < 0 {
		return rule, nil
	}
	name := rule[:i]
	raw := rule[i+1:]
	if strings.EqualFold(name, "regex") || strings.EqualFold(name, "not_regex") {
		return name, []string{raw}
	}
	params := strings.Split(raw, ",")
	for i := range params {
		params[i] = strings.TrimSpace(params[i])
	}
	return name, params
}

func paramsAt(params []string, i int) string {
	if i < len(params) {
		return params[i]
	}
	return ""
}

func parseFloatSafe(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// phpTrimSpaceOnly 对齐 PHP trim() 的默认字符集。
func phpTrimSpaceOnly(s string) string { return strings.Trim(s, " \t\n\r\x00\x0B") }

// phpMbStrlen 对齐 mb_strlen(UTF-8)。
func phpMbStrlen(s string) int { return utf8.RuneCountInString(s) }
