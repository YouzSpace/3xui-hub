package xray

import (
	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
	"controlhub-server/internal/driver/internal/secrets"
)

// 值转换与载荷编码小工具（与 config.go / driver.go 共用，避免各写一套）。

// jsonStringField 读 driver_config JSON 里的顶层字符串字段（缺省/非字符串 → 空串）。
//
// driver_config 是 JSON 列，缓存了 xray_version 等元数据；这里不引入完整解析
// （字段极少，且坏 JSON 不应让 Version() 报错）。
func jsonStringField(raw []byte, field string) string {
	val, err := phpjson.ParseOrdered(raw)
	if err != nil {
		return ""
	}
	obj, ok := val.(phpjson.Object)
	if !ok {
		return ""
	}
	v, _ := obj.Get(field)
	return phpval.String(v)
}

// stringOfAny 复刻 PHP 的「取值为 null 时用空串」再转字符串的口径。
func stringOfAny(v any) string { return phpval.String(v) }

// truthyAny 复刻 PHP 的布尔判定（enable 字段可能给 true/false、1/0、"1"/"0"）。
func truthyAny(v any) bool { return phpval.Bool(v) }

// marshalPayload 把指令载荷编码成入库形态。
//
// node_api_commands.payload 是 JSON 列 + Eloquent `'payload' => 'array'` cast，
// Laravel 落库用 **json_encode 默认参数**（斜杠转义、非 ASCII 转 \uXXXX）。
// 节点 agent 按语义解析，但只有与 PHP 写入的历史行格式一致才能逐字节对拍。
func marshalPayload(v any) ([]byte, error) {
	return phpjson.EncodeDefault(v)
}

// toRenderUser 把 Source 的行转成渲染用的用户视图。
func toRenderUser(u *UserRecord) User {
	return User{
		ID:           u.ID,
		UUID:         u.UUID,
		Protocol:     u.Protocol,
		TrafficLimit: u.TrafficLimit,
		ExpiredAt:    u.ExpiredAt,
	}
}

// NodeSecret 解密 driver_config.node_secret（对应 Node::nodeSecret()）。
//
// driver_config 是整块 JSON 列，node_secret 是其中唯一加密的字符串值；
// 这里用保序解析取值，再用**宽松**口径解密（与 NodeWarpAccount::decryptField 一致：
// 手工写库的明文原样返回）。解密失败且确实是密文形态时返回错误，
// 由调用方决定是「拒绝该节点」还是「按无密钥处理」。
func NodeSecret(cfgRaw []byte, key []byte) (*string, error) {
	val, err := phpjson.ParseOrdered(cfgRaw)
	if err != nil {
		return nil, nil // 坏 JSON 视作没有该字段（PHP 的 $arr['k'] ?? null 同口径）
	}
	obj, ok := val.(phpjson.Object)
	if !ok {
		return nil, nil
	}
	raw, ok := obj.Get("node_secret")
	if !ok || raw == nil {
		return nil, nil
	}
	secret, isStr := raw.(string)
	if !isStr || secret == "" {
		return nil, nil
	}
	return secrets.DecryptPtrOrNil(&secret, key), nil
}
