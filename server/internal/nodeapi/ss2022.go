package nodeapi

// SS-2022 多用户口径 —— 与 driver/xray 是同一套判定与派生（都走 internal/ss2022）。
//
// 为什么要两份：nodeapi 这份才是真正下发给节点 agent 的渲染器（driver/xray/config.go
// 那份只用于管理端预览与 adu 载荷），两边刻意互不依赖，但**口径必须一字不差** ——
// 一旦 key 派生分叉，节点上的 client 与订阅里的链接就对不上，用户连不上且没有报错。

import (
	"controlhub-server/internal/ss2022"
)

// ssMethodOf 读入站 settings.method（读不到返回空串）。
func ssMethodOf(ib RenderInbound) string {
	settingsVal, err := ParseJSON(ib.Settings)
	if err != nil {
		return ""
	}
	return stringOf(mustGet(toObject(settingsVal), "method"))
}

// ssIsMultiUser 判断 ss 入站是否按用户发凭据。
func ssIsMultiUser(ib RenderInbound) bool {
	return ib.Protocol == "shadowsocks" && ss2022.IsMultiUser(ssMethodOf(ib))
}
