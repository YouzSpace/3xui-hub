// Package vless 是 VLESS 协议专属的「出站 settings 构造」。
//
// 「改一个协议只动一个文件夹」：VLESS 的协议特有逻辑集中在本目录。
package vless

import (
	"controlhub-server/internal/phpjson"
	"controlhub-server/internal/protocol/fields"
)

// BuildOutboundSettings 构造 VLESS 出站 settings（vnext[0].users[0]）。
// 返回 (nil,false) 表示必填校验失败（调用方负责写错误响应）。
func BuildOutboundSettings(f fields.Fields, address string, port int64, require func(string) (string, bool)) (*phpjson.Object, bool) {
	id, ok := require(f.Str("user_id"))
	if !ok {
		return nil, false
	}
	user := phpjson.New("id", id, "encryption", "none")
	if flow := f.Str("flow"); flow != "" {
		user.Put("flow", flow)
	}
	return phpjson.New("vnext", phpjson.Array{phpjson.New(
		"address", address,
		"port",    port,
		"users",   phpjson.Array{user},
	)}), true
}
