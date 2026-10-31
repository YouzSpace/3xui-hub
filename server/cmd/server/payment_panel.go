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

// paymentPanelSyncer 把支付域的三个节点副作用接到已有的 collector / userPanelOps 上。
type paymentPanelSyncer struct {
	col *collector.Collector
	ops admin.UserPanelOps
	// users 用来把用户重读成「完整行」再下发。**不能**直接用支付域的 api.User：
	// 它没有 uuid 字段，而 xray 节点的 adu 指令要 uuid（`XrayDriver::createClient`），
	// 用残缺对象下发会给节点建出 uuid 为空的坏 client。
	users admin.AdminUserStore
}

// NewPaymentPanelSyncer 构造支付域的面板接缝（logger 为 nil 时用默认 log）。
func NewPaymentPanelSyncer(col *collector.Collector, users admin.AdminUserStore, logger *log.Logger) api.PanelSyncer {
	return &paymentPanelSyncer{col: col, ops: NewUserPanelOps(col, logger), users: users}
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

// ProvisionClient 在各 enabled 节点同步 client（对齐 UserAdminService::provisionClient）。
func (p *paymentPanelSyncer) ProvisionClient(ctx context.Context, user *api.User) error {
	return p.ops.ProvisionUser(ctx, p.fullRow(ctx, user))
}

// ResetClientTraffic 重置各节点上该用户的已用流量（对齐 resetClientOnNodes）。
func (p *paymentPanelSyncer) ResetClientTraffic(ctx context.Context, user *api.User) error {
	return p.ops.ResetClientTraffic(ctx, p.fullRow(ctx, user))
}

// ToggleClient 打开/关闭各节点上的 client（对齐 BanService::toggleClient）。
//
// 走 collector 而不是 userPanelOps：只有 collector 会在「全部节点确认」后写/清
// users.traffic_disabled_at，也才带 3x-ui 整条替换前的类型归一化。
func (p *paymentPanelSyncer) ToggleClient(ctx context.Context, user *api.User, enable bool) error {
	_, err := p.col.ToggleClient(ctx, int64(user.ID), enable)
	return err
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
