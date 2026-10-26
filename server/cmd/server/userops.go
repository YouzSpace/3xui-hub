package main

// userPanelOps 实现 admin.UserPanelOps：管理端用户操作（建号/重置/开关/删号/切协议）
// 遍历启用节点下发 —— 3x-ui 直连 HTTP、xray 落 node_api_commands 指令（agent 消费）。
//
// 对齐 PHP UserAdminService 的同步 provision/reset/toggle：数据库侧行为与 PHP 一致，
// 面板侧单节点失败只记日志不影响接口成功（PHP 对面板错误同样 report() 后继续）。
//
// 依赖 collector 的 NodeDrivers（驱动构造 + 解密凭据 + xray Source/Enqueuer）与
// InboundIDs（3x-ui 入站），不自己碰 driver 构造细节。

import (
	"context"
	"fmt"
	"log"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/driver/threexui"
	"controlhub-server/internal/driver/xray"
	"controlhub-server/internal/handler/admin"
)

type userPanelOps struct {
	col    *collector.Collector
	logger *log.Logger
}

// NewUserPanelOps 构造用户域的面板下发实现。
func NewUserPanelOps(col *collector.Collector, logger *log.Logger) admin.UserPanelOps {
	if logger == nil {
		logger = log.Default()
	}
	return &userPanelOps{col: col, logger: logger}
}

// ProvisionUser 遍历启用节点给用户建 client（3x-ui AddClient/UpdateClient，xray adu 指令）。
func (o *userPanelOps) ProvisionUser(ctx context.Context, user *admin.UserRow) error {
	drivers, err := o.col.NodeDrivers(ctx)
	if err != nil {
		return err
	}
	email := user.ClientEmail()
	for _, nd := range drivers {
		switch {
		case nd.ThreeX != nil:
			if err := o.provisionThreeX(ctx, nd.ThreeX, nd.NodeID, user, email); err != nil {
				o.logger.Printf("[userops] provision 3x-ui 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		case nd.Xray != nil:
			if err := o.provisionXray(ctx, nd.Xray, user, email); err != nil {
				o.logger.Printf("[userops] provision xray 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		}
	}
	return nil
}

// ResetClientTraffic 遍历节点清零 client 已用流量（xray 流量由 agent 推送，跳过）。
func (o *userPanelOps) ResetClientTraffic(ctx context.Context, user *admin.UserRow) error {
	drivers, err := o.col.NodeDrivers(ctx)
	if err != nil {
		return err
	}
	email := user.ClientEmail()
	for _, nd := range drivers {
		if nd.ThreeX == nil {
			continue // xray 无面板侧流量重置（agent 推送口径）
		}
		if err := nd.ThreeX.ResetClientTraffic(ctx, email); err != nil {
			o.logger.Printf("[userops] reset 3x-ui 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
		}
	}
	return nil
}

// SetClientEnabled 是**账号级**开关（后台 users.enabled，封号/解封用）。
//
// 账号级与流量级是两件事，别混（用户定稿）：
//   - 封禁（enable=false）= 全部关闭、连套餐一起停 → 只把各节点 client 关到位，
//     **不碰** users.traffic_disabled_at（那是「流量用尽/到期」的记账，账号开关无权替
//     一个超量用户施加或解除它）；
//   - 解封（enable=true）→ 交给 collector.EnableAccount：**流量能否用由套餐判据定论**
//     （到期 / 周期额度用尽 / 总量用尽 → 仍按流量级关闭并写标记；无套餐 → 只关不记；
//     判据干净 → 打开并在全部节点确认后清标记）。
//
// 两条分支都复用 collector 的节点下发实现，所以 3x-ui 整条替换前的 NormalizeForUpdate、
// 以及 5.6 的判定矩阵（没端口/没 client 等同关闭、未知不算确认）都在。
func (o *userPanelOps) SetClientEnabled(ctx context.Context, user *admin.UserRow, enable bool) error {
	if enable {
		_, err := o.col.EnableAccount(ctx, int64(user.ID))
		return err
	}
	_, err := o.col.SetNodesEnabled(ctx, int64(user.ID), false)
	return err
}

// DeleteUserClients 遍历节点删除用户的 client（3x-ui DeleteClient，xray rmu 指令）。
func (o *userPanelOps) DeleteUserClients(ctx context.Context, user *admin.UserRow) error {
	drivers, err := o.col.NodeDrivers(ctx)
	if err != nil {
		return err
	}
	email := user.ClientEmail()
	for _, nd := range drivers {
		switch {
		case nd.ThreeX != nil:
			if err := nd.ThreeX.DeleteClient(ctx, email, false); err != nil {
				o.logger.Printf("[userops] delete 3x-ui 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		case nd.Xray != nil:
			if err := nd.Xray.DeleteClient(ctx, email, false); err != nil {
				o.logger.Printf("[userops] delete xray 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		}
	}
	return nil
}

// SwitchProtocol 遍历节点切协议（3x-ui 新协议 attach + 旧协议 detach；xray 协议在
// users 表由 agent 渲染时读，无 attach/detach 概念，跳过）。
func (o *userPanelOps) SwitchProtocol(ctx context.Context, user *admin.UserRow, newProtocol string) error {
	oldProtocol := user.Protocol
	if oldProtocol == newProtocol {
		return nil
	}
	drivers, err := o.col.NodeDrivers(ctx)
	if err != nil {
		return err
	}
	email := user.ClientEmail()
	for _, nd := range drivers {
		if nd.ThreeX == nil {
			continue
		}
		oldIDs, err := o.col.InboundIDs(ctx, nd.NodeID, oldProtocol)
		if err != nil {
			o.logger.Printf("[userops] switch 读旧入站失败 node=%d: %v", nd.NodeID, err)
			continue
		}
		newIDs, err := o.col.InboundIDs(ctx, nd.NodeID, newProtocol)
		if err != nil {
			o.logger.Printf("[userops] switch 读新入站失败 node=%d: %v", nd.NodeID, err)
			continue
		}
		if len(oldIDs) > 0 {
			if err := nd.ThreeX.DetachClient(ctx, email, oldIDs); err != nil {
				o.logger.Printf("[userops] detach 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		}
		if len(newIDs) > 0 {
			if err := nd.ThreeX.AttachClient(ctx, email, newIDs); err != nil {
				o.logger.Printf("[userops] attach 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		}
	}
	return nil
}

// provisionThreeX 给 3x-ui 节点建 client（对齐 PHP prepareClientData + 存在则更新）。
//
// ⚠️ 更新分支必须「先取回节点上的完整 client，归一化后只覆盖我们要改的字段」：
// 3x-ui 的 clients/update 是整条替换语义，只发 email/enable/totalGB/expiryTime/limitIp
// 会把没带上的一串字段（uuid、flow、limitIp、subId…）按零值写掉。
func (o *userPanelOps) provisionThreeX(ctx context.Context, client *threexui.Client, nodeID int64, user *admin.UserRow, email string) error {
	inboundIDs, err := o.col.InboundIDs(ctx, nodeID, user.Protocol)
	if err != nil {
		return err
	}
	if len(inboundIDs) == 0 {
		return nil // 没有该协议入站，跳过（对齐 PHP）
	}

	enable := user.PlanID != nil && user.Enabled
	totalGB := user.TrafficLimit
	var expiryMs int64
	if user.ExpiredAt != nil {
		expiryMs = user.ExpiredAt.UnixMilli()
	}

	existing, getErr := client.GetClient(ctx, email)
	if getErr != nil || existing == nil {
		clientData := map[string]any{
			"email":   email,
			"enable":  enable,
			"totalGB": totalGB,
			"limitIp": 0,
		}
		if expiryMs > 0 {
			clientData["expiryTime"] = expiryMs
		}
		if _, err := client.AddClient(ctx, clientData, inboundIDs); err != nil {
			return fmt.Errorf("AddClient: %w", err)
		}
		return nil
	}

	payload := collector.NormalizeForUpdate(existing)
	payload["email"] = email
	payload["enable"] = enable
	payload["totalGB"] = totalGB
	payload["expiryTime"] = expiryMs
	payload["limitIp"] = 0
	if err := client.UpdateClient(ctx, email, payload, nil); err != nil {
		return fmt.Errorf("UpdateClient: %w", err)
	}
	return nil
}

// provisionXray 给 xray 节点下发 adu 指令（对齐 XrayDriver::createClient）。
func (o *userPanelOps) provisionXray(ctx context.Context, d *xray.Driver, user *admin.UserRow, email string) error {
	// xray CreateClient 只需 email + uuid；Source 会从 users 表补齐其余字段
	_, err := d.CreateClient(ctx, map[string]any{"email": email, "uuid": user.UUID}, nil)
	return err
}
