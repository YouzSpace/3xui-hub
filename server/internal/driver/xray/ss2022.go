package xray

// SS-2022 多用户口径（见 internal/ss2022 包注释）。
//
// 入站的 method 决定凭据落点：
//   - 2022-blake3-aes-128-gcm / -aes-256-gcm：每用户一把 PBK，走用户级指令通道；
//   - 其余（aes-256-gcm / chacha20-poly1305 / xchacha20-poly1305 / chacha20-poly1305 的 2022 变体）：
//     入站级单凭据，所有用户共用一套 method+password，不走用户级通道。

import (
	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
	"controlhub-server/internal/ss2022"
)

// SSMethodOf 读入站 settings.method（读不到返回空串，由调用方决定默认值）。
func SSMethodOf(ib Inbound) string {
	settingsVal, err := phpjson.ParseOrdered(ib.Settings)
	if err != nil {
		return ""
	}
	return phpval.String(mustGet(phpval.ToObject(settingsVal), "method"))
}

// SSIsMultiUser 判断 ss 入站是否按用户发凭据（true 才注入 clients / 走 adu-rmu）。
func SSIsMultiUser(ib Inbound) bool {
	return ib.Protocol == "shadowsocks" && ss2022.IsMultiUser(SSMethodOf(ib))
}
