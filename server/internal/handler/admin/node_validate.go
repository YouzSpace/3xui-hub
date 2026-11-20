package admin

// 节点域的校验器。
//
// 为什么不复用 validate.go 的 validator：那是 P8d 的共享文件（任务边界禁止修改），
// 而节点域还需要 between / url 两条它刻意未实现的规则（默认分支 panic）。
// 这里复制同一套「隐式规则 / gating / 文案合并」语义，并补上节点域用到的规则，
// 输出口径（HTTP 200 + code=500 + "<第一条> (and N more errors)"）与 PHP 完全一致。
//
// 已实现：required / nullable / sometimes / string / integer / numeric / boolean /
// array / in / min / max / between / regex / url，以及 `x.*` 通配展开。

import (
	"context"
	"strconv"
	"strings"
)

// nodeRuleError 是一条校验失败。
type nodeRuleError struct {
	Field string
	Msg   string
}

// nodeValidator 是节点域的 Laravel 校验器等价物。
type nodeValidator struct {
	data   map[string]any
	fields []ruleField
	errs   []nodeRuleError
	ctx    context.Context
}

func newNodeValidator(ctx context.Context, data map[string]any) *nodeValidator {
	return &nodeValidator{data: data, ctx: ctx}
}

func (v *nodeValidator) field(name string, rules ...string) *nodeValidator {
	v.fields = append(v.fields, ruleField{field: name, rules: rules})
	return v
}

func (v *nodeValidator) passes() bool {
	for _, f := range v.fields {
		v.applyField(f)
	}
	return len(v.errs) == 0
}

// summary 复刻 ValidationException::summarize。
func (v *nodeValidator) summary() string {
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

// storeFailure 对齐共享 validator 的同名方法：节点域校验器不查库，恒为 nil
// （exists 规则在节点域没有使用 —— 节点入站/出站引用都在 handler 里显式查）。
func (v *nodeValidator) storeFailure() error { return nil }

func (v *nodeValidator) applyField(f ruleField) {
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
		switch name {
		case "sometimes":
			if !present {
				return
			}
			continue
		case "nullable":
			continue
		case "required":
			if !requiredPasses(value) {
				v.addError(f.field, name, params, value)
				return
			}
			continue
		}
		if !present || value == nil || blank {
			continue
		}
		if v.ruleFails(name, params, f.field, value) {
			v.addError(f.field, name, params, value)
		}
	}
}

func (v *nodeValidator) applyWildcard(f ruleField) {
	base := strings.TrimSuffix(f.field, ".*")
	raw, ok := v.data[base]
	if !ok {
		return
	}
	items, isArr := raw.([]any)
	if !isArr {
		return
	}
	for i, item := range items {
		field := base + "." + strconv.Itoa(i)
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
			if stopped || item == nil {
				continue
			}
			if v.ruleFails(name, params, field, item) {
				v.addError(field, name, params, item)
			}
		}
	}
}

// ruleFails 判定单条规则（true = 失败）。
func (v *nodeValidator) ruleFails(name string, params []string, field string, value any) bool {
	switch name {
	case "string":
		_, ok := value.(string)
		return !ok
	case "integer":
		return !phpIntegerPasses(value)
	case "numeric":
		return !phpNumericPasses(value)
	case "boolean":
		return !phpBooleanPasses(value)
	case "array":
		return !phpIsArray(value)
	case "min", "max", "size":
		attr := v.attributeType(field)
		cmp := compareSize(attr, value, paramsAt(params, 0))
		switch name {
		case "min":
			return cmp < 0
		case "max":
			return cmp > 0
		}
		return cmp != 0
	case "between":
		attr := v.attributeType(field)
		size := sizeOf(attr, value)
		return size < parseFloatSafe(paramsAt(params, 0)) || size > parseFloatSafe(paramsAt(params, 1))
	case "in":
		return !phpInArray(value, params)
	case "regex":
		return !phpRegexPasses(value, paramsAt(params, 0))
	case "url":
		return !phpURLPasses(value)
	}
	panic("admin: 节点域未实现的校验规则 " + name)
}

// sizeOf 返回 min/max/between 比较用的「大小」（对齐 Laravel getSize）。
func sizeOf(attrType string, value any) float64 {
	switch attrType {
	case "numeric":
		if phpNumericPasses(value) {
			return phpNumberOf(value)
		}
		return float64(phpMbStrlen(phpScalarString(value)))
	case "array":
		switch t := value.(type) {
		case []any:
			return float64(len(t))
		case map[string]any:
			return float64(len(t))
		}
		return 1
	}
	return float64(phpMbStrlen(phpScalarString(value)))
}

// attributeType 对齐 getAttributeType（numeric / array / string）。
func (v *nodeValidator) attributeType(field string) string {
	for _, f := range v.fields {
		if f.field != field && f.field != wildcardParent(field) {
			continue
		}
		for _, r := range f.rules {
			name, _ := splitRule(r)
			switch name {
			case "numeric", "integer", "decimal", "between":
				return "numeric"
			case "array":
				return "array"
			}
		}
	}
	return "string"
}

// addError 生成一条文案（字段名按 Laravel 的 getDisplayableAttribute 规则展示）。
func (v *nodeValidator) addError(field, rule string, params []string, value any) {
	display := displayAttribute(field)
	attrType := v.attributeType(field)

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
	case "in":
		tmpl = "The selected :attribute is invalid."
	case "regex":
		tmpl = "The :attribute field format is invalid."
	case "url":
		tmpl = "The :attribute field must be a valid URL."
	case "between":
		switch attrType {
		case "numeric":
			tmpl = "The :attribute field must be between :min and :max."
		case "array":
			tmpl = "The :attribute field must have between :min and :max items."
		default:
			tmpl = "The :attribute field must be between :min and :max characters."
		}
	case "min", "max", "size":
		tmpl = sizeMessage(attrType, rule)
	default:
		panic("admin: 节点域未实现文案的规则 " + rule)
	}

	out := strings.ReplaceAll(tmpl, ":attribute", display)
	switch rule {
	case "min":
		out = strings.ReplaceAll(out, ":min", paramsAt(params, 0))
	case "max":
		out = strings.ReplaceAll(out, ":max", paramsAt(params, 0))
	case "size":
		out = strings.ReplaceAll(out, ":size", paramsAt(params, 0))
	case "between":
		out = strings.ReplaceAll(out, ":min", paramsAt(params, 0))
		out = strings.ReplaceAll(out, ":max", paramsAt(params, 1))
	}
	v.errs = append(v.errs, nodeRuleError{Field: field, Msg: out})
}

// phpURLPasses 近似 Laravel 的 url 规则（filter_var($v, FILTER_VALIDATE_URL)）。
func phpURLPasses(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	s = phpTrimSpaceOnly(s)
	if s == "" || strings.ContainsAny(s, " \t\n\r") {
		return false
	}
	i := strings.Index(s, "://")
	if i <= 0 {
		return false
	}
	scheme := strings.ToLower(s[:i])
	for _, c := range scheme {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	rest := s[i+3:]
	hostEnd := len(rest)
	for idx := 0; idx < len(rest); idx++ {
		if c := rest[idx]; c == '/' || c == '?' || c == '#' {
			hostEnd = idx
			break
		}
	}
	return rest[:hostEnd] != ""
}
