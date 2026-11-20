package nodeapi

// SS-2022 多用户口径 —— 与 driver/xray 是同一套判定与派生（都走 internal/ss2022）。
//
// 为什么要两份：nodeapi 这份才是真正下发给节点 agent 的渲染器（driver/xray/config.go
// 那份只用于管理端预览与 adu 载荷），两边刻意互不依赖，但**口径必须一字不差** ——
// 一旦 key 派生分叉，节点上的 client 与订阅里的链接就对不上，用户连不上且没有报错。

// ssMethodOf 读入站 settings.method（读不到返回空串）。
func ssMethodOf(ib RenderInbound) string {
	settingsVal, err := ParseJSON(ib.Settings)
	if err != nil {
		return ""
	}
	return stringOf(mustGet(toObject(settingsVal), "method"))
}

// ss 是否按用户发凭据的判定在 protocol_clients.go 的 prepareShadowsocksClients
// （那里顺手把 method 解析结果缓存给 Build 复用，避免每个用户都重新解析 settings）。
