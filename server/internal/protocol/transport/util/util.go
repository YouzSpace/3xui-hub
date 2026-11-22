// Package util 是传输子包共用的字段读取小工具（叶子包，无内部依赖）。
package util

import (
	"strconv"
	"strings"
)

func Str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok { return strings.TrimSpace(v) }
	return ""
}
func Int(m map[string]any, k string) int64 {
	switch t := m[k].(type) {
	case int64: return t
	case int: return int64(t)
	case float64: return int64(t)
	case string: n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64); return n
	}
	return 0
}
func Bool(m map[string]any, k string) bool {
	switch t := m[k].(type) {
	case bool: return t
	case string: return t == "1" || strings.EqualFold(t, "true")
	case int64: return t != 0
	}
	return false
}
func OrSlash(v string) string { if v == "" { return "/" }; return v }
