package admin

// 本文件把 admin 包对「PHP json_encode 口径」的依赖转接到 internal/phpjson 共享包
// （2026-11-22：为把协议逻辑拆进 internal/protocol/* 独立包做准备 —— 协议包也要这套
// 有序 JSON 编码器，不能再是 admin 私有实现）。
//
// 用「类型别名 + 转发」保持 1147 处调用点零改动：
//   jsObj/jsArr → phpjson.Object/Array 的别名（故 jsObj{...} 字面量、*jsObj 都照旧）；
//   jo/phpEncode → 转发到 phpjson.New/Encode。
// 行为与抽取前逐字节一致（admin 既有的 golden/特征测试守护）。
import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"controlhub-server/internal/phpjson"
)

type jsObj = phpjson.Object
type jsArr = phpjson.Array
type phpFloat = phpjson.Float
type eloqTime = phpjson.EloqTime
type isoTime = phpjson.ISOTime
type dateTimeStr = phpjson.DateTimeStr

func jo(pairs ...any) *jsObj { return phpjson.New(pairs...) }
func phpEncode(v any) []byte { return phpjson.Encode(v) }

// ---- 可空值的输出助手（PHP 里 null 原样输出，非 null 走对应口径）----

// nullableStr 对齐 PHP 里 `$row->col` 为 null 时输出 null、否则输出字符串。
func nullableStr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableDateTime 对齐 `$m->col?->toDateTimeString()`。
func nullableDateTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return dateTimeStr(*t)
}

// nullableISO 对齐 `$m->col?->toIso8601String()`。
func nullableISO(t *time.Time) any {
	if t == nil {
		return nil
	}
	return isoTime(*t)
}

// nullableEloq 对齐 Eloquent 模型 toJSON 的可空时间字段。
func nullableEloq(t *time.Time) any {
	if t == nil {
		return nil
	}
	return eloqTime(*t)
}

// newJSONDecoder 构造保留数字原文的解码器（上游响应里的 id 可能是大整数）。
func newJSONDecoder(raw string) *json.Decoder {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	return dec
}

// sortedKeys 取 map 的键并按字典序排序（Go map 无序，输出必须稳定）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
