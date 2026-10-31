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

// RebuildUserCredentials 在凭据（uuid）换新后重建该用户在各节点的 client。
//
// 用户定稿：**先删旧的、再加新的**，不做原地 update —— 这样节点上不会残留任何旧凭据，
// 订阅链接泄露后导入出去的旧配置当场失效。
//
//   - 3x-ui：DeleteClient(email, keepTraffic=false) → AddClient(新 uuid) 挂回各入站。
//     刻意 keepTraffic=false：旧 client 的流量基线属于旧凭据，重置订阅是一次身份更换，
//     不该把旧用量带进新身份。
//   - xray（裸内核）：DeleteClient 投 rmu，随后 ProvisionUser 走 adu 按**新 uuid**重建；
//     agent 重新拉配置时 users.uuid 已是新值，所以这里只需保证调用顺序。
//
// 单节点失败只记日志，不影响接口成功（与 PHP 对面板错误 report() 后继续的口径一致）。
func (o *userPanelOps) RebuildUserCredentials(ctx context.Context, user *admin.UserRow) error {
	drivers, err := o.col.NodeDrivers(ctx)
	if err != nil {
		return err
	}
	email := user.ClientEmail()
	for _, nd := range drivers {
		switch {
		case nd.ThreeX != nil:
			if err := nd.ThreeX.DeleteClient(ctx, email, false); err != nil {
				// 旧 client 可能本就不存在（历史遗留），删失败不阻断后续新建。
				o.logger.Printf("[userops] rebuild 删除旧 client 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
			if err := o.provisionThreeX(ctx, nd.ThreeX, nd.NodeID, user, email); err != nil {
				o.logger.Printf("[userops] rebuild 新建 client 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		case nd.Xray != nil:
			if err := nd.Xray.DeleteClient(ctx, email, false); err != nil {
				o.logger.Printf("[userops] rebuild 删除旧 xray client 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
			if err := o.provisionXray(ctx, nd.Xray, user, email); err != nil {
				o.logger.Printf("[userops] rebuild 新建 xray client 失败 user=%d node=%d: %v", user.ID, nd.NodeID, err)
			}
		}
	}
	return nil
}

// SwitchProtocol 遍历节点切协议，对齐 ProtocolSwitchService::switch 的顺序与保护：
//
//  1. **该节点没有新协议入站 → 整个跳过**（什么都不动）。旧实现在这里仍然 detach 旧入站，
//     结果是「用户切到一个这台机器不支持的协议 → 这台节点上彻底没有挂载 → 直接断连」。
//  2. **先 attach 新协议入站，成功后才 detach 旧协议**（排除与新协议重合的 id）。
//     反过做的人会有一个「新旧都不在」的窗口；attach 失败时旧挂载也已经没了。
//  3. attach 任一节点失败 → **回滚已改的节点**（detach 新的、重新 attach 旧的）并返回 error，
//     让调用方不要落库 users.protocol —— 否则面板上的协议与节点实际挂载不一致。
//  4. 旧入站 detach 失败不致命：用户已经能用新协议，回滚反而更糟，只记日志。
//
// xray 节点跳过：协议在 users 表，agent 渲染时按协议取客户端，没有 attach/detach 概念。
func (o *userPanelOps) SwitchProtocol(ctx context.Context, user *admin.UserRow, newProtocol string) error {
	oldProtocol := user.Protocol
	if oldProtocol == newProtocol {
		return nil
	}
	if newProtocol != "vless" && newProtocol != "trojan" && newProtocol != "hysteria" {
		return fmt.Errorf("不支持的协议: %s", newProtocol)
	}
	drivers, err := o.col.NodeDrivers(ctx)
	if err != nil {
		return err
	}
	email := user.ClientEmail()

	type attachRecord struct {
		nodeID int64
		client *threexui.Client
		newIDs []int64
		oldIDs []int64
	}
	var done []attachRecord

	rollback := func() {
		for _, rec := range done {
			if err := rec.client.DetachClient(ctx, email, rec.newIDs); err != nil {
				o.logger.Printf("[userops] 协议切换回滚 detach 失败 node=%d user=%d: %v", rec.nodeID, user.ID, err)
			}
			if len(rec.oldIDs) > 0 {
				if err := rec.client.AttachClient(ctx, email, rec.oldIDs); err != nil {
					o.logger.Printf("[userops] 协议切换回滚 attach 旧入站失败 node=%d user=%d: %v", rec.nodeID, user.ID, err)
				}
			}
		}
	}

	for _, nd := range drivers {
		if nd.ThreeX == nil {
			continue
		}
		newIDs, err := o.col.InboundIDs(ctx, nd.NodeID, newProtocol)
		if err != nil {
			rollback()
			return fmt.Errorf("节点 #%d 读新协议入站失败: %w", nd.NodeID, err)
		}
		if len(newIDs) == 0 {
			continue // 该节点没有新协议入站：保留旧挂载，不动
		}
		oldIDs, err := o.col.InboundIDs(ctx, nd.NodeID, oldProtocol)
		if err != nil {
			rollback()
			return fmt.Errorf("节点 #%d 读旧协议入站失败: %w", nd.NodeID, err)
		}

		if err := nd.ThreeX.AttachClient(ctx, email, newIDs); err != nil {
			rollback()
			return fmt.Errorf("节点 #%d 挂载新协议入站失败: %w", nd.NodeID, err)
		}
		done = append(done, attachRecord{nodeID: nd.NodeID, client: nd.ThreeX, newIDs: newIDs, oldIDs: oldIDs})

		if toDetach := idsExcept(oldIDs, newIDs); len(toDetach) > 0 {
			if err := nd.ThreeX.DetachClient(ctx, email, toDetach); err != nil {
				o.logger.Printf("[userops] 旧协议 detach 失败（新协议已可用，不回滚）user=%d node=%d ids=%v: %v",
					user.ID, nd.NodeID, toDetach, err)
			}
		}
	}
	return nil
}

// idsExcept 返回 all 里不在 exclude 中的 id（保序）。
func idsExcept(all, exclude []int64) []int64 {
	if len(all) == 0 {
		return nil
	}
	drop := make(map[int64]bool, len(exclude))
	for _, id := range exclude {
		drop[id] = true
	}
	out := make([]int64, 0, len(all))
	for _, id := range all {
		if !drop[id] {
			out = append(out, id)
		}
	}
	return out
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
	// 3x-ui 的 update 只改 client 定义，**不会把 client 挂到它还没挂的入站上**：
	// 节点后加了入站、或上次入站同步失败时，光更新会让用户一直在新入站上连不上
	// （续费/重置看起来成功、实际只有一个入站可用）。attach 幂等（同一批 id
	// 重复挂结果不变），这里顺手补齐。
	if err := client.AttachClient(ctx, email, inboundIDs); err != nil {
		return fmt.Errorf("AttachClient 补齐入站: %w", err)
	}
	return nil
}

// provisionXray 给 xray 节点下发 adu 指令（对齐 XrayDriver::createClient）。
func (o *userPanelOps) provisionXray(ctx context.Context, d *xray.Driver, user *admin.UserRow, email string) error {
	// xray CreateClient 只需 email + uuid；Source 会从 users 表补齐其余字段
	_, err := d.CreateClient(ctx, map[string]any{"email": email, "uuid": user.UUID}, nil)
	return err
}
