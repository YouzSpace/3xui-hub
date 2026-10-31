package api

import (
	"context"
	"log"
)

// UserNotifier 实现 admin.UserNotifier：把用户侧变更推给该用户的在线前端。
//
// 放在 api 包是因为通道（UserHub）在这里；admin 只依赖窄接口，不反向依赖本包。
// 后期接入订单状态、工单回复等通知时，在这里加方法 + 在 ws_message.go 加消息类型即可。
type UserNotifier struct {
	Hub    *UserHub
	Logger *log.Logger
}

// NewUserNotifier 构造通知器（Hub 为 nil 时全部推送静默跳过）。
func NewUserNotifier(hub *UserHub, logger *log.Logger) *UserNotifier {
	if logger == nil {
		logger = log.Default()
	}
	return &UserNotifier{Hub: hub, Logger: logger}
}

// NotifySubscriptionReset 推送「订阅已重置」。用户不在线不算失败。
func (n *UserNotifier) NotifySubscriptionReset(ctx context.Context, userID uint64, token, subPath string) error {
	if n == nil || n.Hub == nil {
		return nil
	}
	sent := n.Hub.Push(userID, WSMessage{
		Type: WSTypeSubscriptionReset,
		Data: SubscriptionResetData{Token: token, SubPath: subPath},
	})
	if sent == 0 {
		n.Logger.Printf("[ws] 订阅重置通知未送达（用户 %d 不在线），前端下次轮询可见", userID)
	}
	return nil
}
