package main

// 支付域 → 节点副作用的接线（api.PanelSyncer）。
//
// 为什么要有这个文件：routes.go 里 NewPaymentService 的 Options 过去只填了
// ProxyOrders / TGSender，**Panel 一直是 nil**，于是 payment_service.go 完成订单时
// 那三段 `if s.panel != nil { … }` 全部静默跳过：
//   - ProvisionClient（把新套餐的 enable/totalGB/expiry 推到各节点）
//   - ToggleClient(user, true)（BanService::unban：打开 client，并在全部节点确认后
//     清掉 users.traffic_disabled_at）
//
// 后果就是线上这一单：13:15 因流量被关闭 → 14:33 付款买新套餐（限额已重算、enabled=1）
// → 解封动作压根没执行 → 标记一直挂着 → 4 台 3x-ui 节点上 client 仍是 enable=false，
// 2 台 xray 节点的配置渲染（eligibleUserRows: traffic_disabled_at IS NULL）也把他排除，
// 用户「买了套餐但所有节点都连不上」。
//
// 语义对齐 PHP：完成订单 = applyPlan → provisionClient → unban(enabled=true +
// toggleClient(true))；失败只记日志、不阻断收款（与 PHP 的 report() 后继续一致）。

import (
	"context"
	"log"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/handler/api"
)

// paymentPanelSyncer 把支付域的节点下发接到后台任务（不可用时退回同步下发）。
type paymentPanelSyncer struct {
	ops admin.UserPanelOps
	// users 用来把用户重读成「完整行」再下发。**不能**直接用支付域的 api.User：
	// 它没有 uuid 字段，而 xray 节点的 adu 指令要 uuid（`XrayDriver::createClient`），
	// 用残缺对象下发会给节点建出 uuid 为空的坏 client。
	users admin.AdminUserStore
	// delivery 是「用户下发」异步任务派发口（可空：为空则退回同步下发）。
	delivery admin.UserDelivery
	logger   *log.Logger
}

// NewPaymentPanelSyncer 构造支付域的面板接缝（logger 为 nil 时用默认 log）。
func NewPaymentPanelSyncer(col *collector.Collector, users admin.AdminUserStore, delivery admin.UserDelivery, logger *log.Logger) api.PanelSyncer {
	if logger == nil {
		logger = log.Default()
	}
	return &paymentPanelSyncer{ops: NewUserPanelOps(col, logger), users: users, delivery: delivery, logger: logger}
}

// fullRow 取「完整用户行」：优先按 ID 重读（带 uuid/protocol/plan/限额/到期），
// 重读失败才退回支付域对象（至少不会 panic）。
func (p *paymentPanelSyncer) fullRow(ctx context.Context, user *api.User) *admin.UserRow {
	if p.users != nil {
		if fresh, err := p.users.UserByID(ctx, user.ID); err == nil && fresh != nil {
			return fresh
		}
	}
	return paymentUserToRow(user)
}

// DeliverUser 把「清流量 + 下发 + 开 client」交给后台任务（对齐 resetClientOnNodes +
// provisionClient + BanService::toggleClient 的组合效果）。
//
// 为什么异步：这两条路径都跑在支付回调里，同步做要 2–3 秒（线上 7 台节点、每台 2–3 次
// 往返）；平台回调有自己的超时时限，被掐断就会重试。改成入队后回调立即返回，
// 下发失败会写进 async_tasks，管理员在「异步任务」页看得到。
//
// 队列不可用（未注入 / 入队失败）时退回同步下发，宁可慢也不能不生效。
func (p *paymentPanelSyncer) DeliverUser(ctx context.Context, user *api.User) error {
	if p.delivery != nil {
		if err := p.delivery.Dispatch(ctx, user.ID); err == nil {
			return nil
		} else {
			p.logger.Printf("支付完成：下发入队失败，回退同步 user=%d: %v", user.ID, err)
		}
	}

	row := p.fullRow(ctx, user)
	if err := p.ops.ResetClientTraffic(ctx, row); err != nil {
		p.logger.Printf("支付完成（同步兜底）：清节点流量失败 user=%d: %v", user.ID, err)
	}
	if err := p.ops.ProvisionUser(ctx, row); err != nil {
		p.logger.Printf("支付完成（同步兜底）：provision 失败 user=%d: %v", user.ID, err)
	}
	if row.Enabled && row.PlanID != nil {
		if err := p.ops.SetClientEnabled(ctx, row, true); err != nil {
			p.logger.Printf("支付完成（同步兜底）：开 client 失败 user=%d: %v", user.ID, err)
		}
	}
	return nil
}

// paymentUserToRow 把支付域的 api.User 映射成用户域行（只带节点下发用到的字段）。
func paymentUserToRow(u *api.User) *admin.UserRow {
	row := &admin.UserRow{
		ID: u.ID,
		// ⚠️ 兜底行拿不到 users.uuid（api.User 不含该列），所以 UUID 为空：
		// 建 client 时只能退化回「由面板自动生成 uuid」，hysteria 的 client 会因缺 auth
		// 建不出来。正常路径是上面 fullRow 里的 UserByID 重读完整行（带 uuid），
		// 这里只在「重读失败」时才走到，属于最后兜底。
		Protocol:            u.Protocol,
		TrafficLimit:        u.TrafficLimit,
		TrafficUsed:         u.TrafficUsed,
		MonthlyTrafficUsed:  u.MonthlyTrafficUsed,
		MonthlyTrafficLimit: u.MonthlyTrafficLimit,
		Enabled:             u.Enabled,
	}
	if u.Email.Valid {
		e := u.Email.String
		row.Email = &e
	}
	if u.PlanID.Valid {
		pid := uint64(u.PlanID.Int64)
		row.PlanID = &pid
	}
	if u.ExpiredAt.Valid {
		t := u.ExpiredAt.Time
		row.ExpiredAt = &t
	}
	if u.NextTrafficResetAt.Valid {
		t := u.NextTrafficResetAt.Time
		row.NextTrafficResetAt = &t
	}
	return row
}

var _ api.PanelSyncer = (*paymentPanelSyncer)(nil)
