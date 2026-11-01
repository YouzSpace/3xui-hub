package api

// 用户端 WebSocket 通道（预留）。
//
// 定位：**通用下行消息通道**，不是「订阅重置专用推送」。后期接入新功能（订单状态、
// 工单回复、公告变更、节点维护通知……）只加一种 msg 类型即可，无需再开通道。
//
// 协议约定（前后端都要遵守，改名等于破坏兼容）：
//
//	连接   wss://<站点>/api/ws?access_token=<access_token>
//	        或 携带 Authorization: Bearer <access_token>（浏览器 WS 不能自定义头，
//	        所以推荐用 query 参数；两者都支持，取先到的那个）
//	下行   {"type":"<消息类型>","data":{...},"ts":<毫秒时间戳>}
//	上行   {"type":"ping"}  →  下行 {"type":"pong","ts":...}
//
// 消息类型（type）：
//   - "subscription.reset" 订阅被重置，data = {"token":"<新token>","sub_path":"/api/sub/<新token>"}
//     前端收到后**立即**替换本地订阅链接并重新渲染，不要求用户刷新页面。
//   - "pong"               心跳回应
//   - 其它类型一律按「未知 type 忽略」处理 —— 这是兼容新旧前端的关键：
//     后端加新 type 不会打挂老前端。
//
// 心跳：服务端每 30s 发一次 ping 控制帧，客户端须在同一周期内回 pong；
// 连续两个周期无回应则关闭连接（nginx/云厂商常见 60s 空闲断连，靠这个保活）。

// WSMessage 是下行消息的通用封装（所有 type 共用）。
type WSMessage struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
	TS   int64  `json:"ts"`
}

// 消息类型常量。新增功能在这里加一条，并在 handleWS 的推送点补上对应触发。
const (
	// WSTypeSubscriptionReset 订阅被重置：data 含新 token 与订阅路径。
	WSTypeSubscriptionReset = "subscription.reset"
	// WSTypePong 心跳回应。
	WSTypePong = "pong"
)

// SubscriptionResetData 是 WSTypeSubscriptionReset 的 data 负载。
//
// 只放前端渲染必需的最小字段：token 用来拼链接，sub_path 是后端拼好的相对路径
// （前端按站点域名补全），uuid 不放 —— 用户端不需要知道节点凭据。
type SubscriptionResetData struct {
	Token   string `json:"token"`
	SubPath string `json:"sub_path"`
}
