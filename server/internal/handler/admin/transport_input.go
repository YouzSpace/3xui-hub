package admin

// transportFriendlyInput 把面板 input 适配成 internal/protocol/transport 能读的形态。
//
// 背景：jsonInput() 解码时用 UseNumber 并 convertNumbers，数字在 input 里是 admin 的
// jsonNumber（底层 string、但类型不是 string）；而 protocol/transport/util 只做
// `v.(string)` / `v.(bool)` 这类精确类型断言。不经这一步转换，凡是「以 JSON 数字提交」
// 的传输层字段（xhttp 的 scMaxBufferedPosts/sessionIDLength、ws 的 path 用数字等）
// 都会被静默丢弃 —— 改动前 admin 自己的 nodeStringField/inputInt/inputBool 都认
// jsonNumber，所以这是重构引入的回归（PR #149 审核发现）。
//
// 转成字面量字符串即可同时喂饱 util 的三个函数：
//   - Str  : 直接返回（TrimSpace 后再判断非空）
//   - Int  : strconv.ParseInt("100") 成功
//   - Bool : "1"/"true" 为真，"0" 为假
//
// 只处理顶层：传输层字段都是扁平的（没有嵌套数字）。
func transportFriendlyInput(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		if n, ok := v.(jsonNumber); ok {
			out[k] = string(n)
			continue
		}
		out[k] = v
	}
	return out
}
