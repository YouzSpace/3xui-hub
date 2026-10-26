package admin

// xray 节点 WARP 管理（/admin-api/nodes/{node}/warp）。
// 逐条对齐 backend/app/Http/Controllers/Admin/NodeWarpController.php。
//
// 生命周期：register（注册设备）→ apply（生成 wireguard 出站下发节点）
// → rotate（一键换 IP）/ license（WARP+）/ auto_rotate_hours（定时换 IP）。

import (
	"github.com/gin-gonic/gin"
)

// warpShow 对齐 show()。
func (d *NodeDomain) warpShow(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	presented, ok := d.presentWarp(c, node)
	if !ok {
		return
	}
	d.success(c, presented)
}

// warpRegister 对齐 register()：注册并应用（一步到位；apply 失败不阻断注册）。
func (d *NodeDomain) warpRegister(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}

	existing, err := d.store.WarpAccountByNode(ctxOf(c), node.ID)
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		d.failServer(c, "查询 WARP 账户", err)
		return
	}
	account, err := d.ops.WarpRegister(ctxOf(c), node.ID, node.Name, existing)
	if err != nil {
		d.fail(c, "WARP 注册失败："+err.Error(), 400)
		return
	}
	if err := d.store.SaveWarpAccount(ctxOf(c), *account); err != nil {
		d.failServer(c, "保存 WARP 账户", err)
		return
	}

	// 注册成功即自动应用。apply 失败不阻断注册 —— 账户已就绪，管理员可点「重新应用」。
	applyError := ""
	if _, err := d.applyWarp(c, node, account); err != nil {
		applyError = err.Error()
	}

	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	presented, ok := d.presentWarp(c, fresh)
	if !ok {
		return
	}
	if obj, isObj := presented.(*jsObj); isObj {
		if applyError != "" {
			obj.put("apply_error", applyError)
		} else {
			obj.put("apply_error", nil)
		}
	}
	d.success(c, presented)
}

// warpApply 对齐 apply()。
func (d *NodeDomain) warpApply(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	account, err := d.store.WarpAccountByNode(ctxOf(c), node.ID)
	if err != nil {
		if d.noRowsToNotFoundSilent(err) {
			d.fail(c, "尚未注册 WARP，请先注册", 400)
			return
		}
		d.failServer(c, "查询 WARP 账户", err)
		return
	}
	if _, err := d.applyWarp(c, node, account); err != nil {
		d.fail(c, err.Error(), 400)
		return
	}

	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	presented, ok := d.presentWarp(c, fresh)
	if !ok {
		return
	}
	if obj, isObj := presented.(*jsObj); isObj {
		obj.put("config_version", fresh.ConfigVersion)
	}
	d.success(c, presented)
}

// warpRotate 对齐 rotate()：一键换 IP = 重新注册（保留 license）+ 已应用则重组出站。
func (d *NodeDomain) warpRotate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}

	existing, err := d.store.WarpAccountByNode(ctxOf(c), node.ID)
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		d.failServer(c, "查询 WARP 账户", err)
		return
	}
	wasEnabled := existing != nil && existing.Enabled

	account, err := d.ops.WarpRegister(ctxOf(c), node.ID, node.Name, existing)
	if err != nil {
		d.fail(c, "换 IP 失败："+err.Error(), 400)
		return
	}
	if err := d.store.SaveWarpAccount(ctxOf(c), *account); err != nil {
		d.failServer(c, "保存 WARP 账户", err)
		return
	}
	if wasEnabled {
		if _, err := d.applyWarp(c, node, account); err != nil {
			d.fail(c, "换 IP 失败："+err.Error(), 400)
			return
		}
	}
	now := d.now().UTC()
	if err := d.store.PatchWarpAccount(ctxOf(c), node.ID, nil, nil, &now, nil); err != nil {
		d.failServer(c, "更新换 IP 时间", err)
		return
	}

	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	presented, ok := d.presentWarp(c, fresh)
	if !ok {
		return
	}
	if obj, isObj := presented.(*jsObj); isObj {
		obj.put("config_version", fresh.ConfigVersion)
	}
	d.success(c, presented)
}

// warpUpdate 对齐 update()：{license?, auto_rotate_hours?}。
func (d *NodeDomain) warpUpdate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("license", "nullable", "string", "max:128")
	v.field("auto_rotate_hours", "nullable", "integer", "min:0", "max:720")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	account, err := d.store.WarpAccountByNode(ctxOf(c), node.ID)
	if err != nil {
		if d.noRowsToNotFoundSilent(err) {
			d.fail(c, "尚未注册 WARP，请先注册", 400)
			return
		}
		d.failServer(c, "查询 WARP 账户", err)
		return
	}

	if license := nodeStringField(input, "license"); license != "" {
		if err := d.ops.WarpSetLicense(ctxOf(c), account, license); err != nil {
			d.fail(c, err.Error(), 400)
			return
		}
		if err := d.store.PatchWarpAccount(ctxOf(c), node.ID, &license, nil, nil, nil); err != nil {
			d.failServer(c, "保存 WARP license", err)
			return
		}
	}
	if val, present := input["auto_rotate_hours"]; present && val != nil {
		hours := inputInt(val)
		if err := d.store.PatchWarpAccount(ctxOf(c), node.ID, nil, &hours, nil, nil); err != nil {
			d.failServer(c, "保存定时换 IP 间隔", err)
			return
		}
	}

	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	presented, ok := d.presentWarp(c, fresh)
	if !ok {
		return
	}
	d.success(c, presented)
}

// warpDestroy 对齐 destroy()：清账户 + 移除 warp 出站 + bump。
func (d *NodeDomain) warpDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}

	if err := d.store.DeleteWarpAccount(ctxOf(c), node.ID); err != nil {
		d.failServer(c, "删除 WARP 账户", err)
		return
	}
	ob, err := d.store.XrayOutboundByTag(ctxOf(c), node.ID, "warp")
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		d.failServer(c, "查询 WARP 出站", err)
		return
	}
	if ob != nil {
		if err := d.store.DeleteXrayOutbound(ctxOf(c), ob.ID); err != nil {
			d.failServer(c, "删除 WARP 出站", err)
			return
		}
		if _, err := d.store.BumpConfigVersion(ctxOf(c), node.ID); err != nil {
			d.failServer(c, "递增配置版本", err)
			return
		}
	}

	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	d.success(c, jo("config_version", fresh.ConfigVersion))
}

// presentWarp 对齐 present()。
func (d *NodeDomain) presentWarp(c *gin.Context, node *NodeRecord) (any, bool) {
	account, err := d.store.WarpAccountByNode(ctxOf(c), node.ID)
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		d.failServer(c, "查询 WARP 账户", err)
		return nil, false
	}
	ob, err := d.store.XrayOutboundByTag(ctxOf(c), node.ID, "warp")
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		d.failServer(c, "查询 WARP 出站", err)
		return nil, false
	}

	presented := jo(
		"registered", account != nil,
		"applied", ob != nil && ob.Enabled,
		"device_id", nil,
		"peer_endpoint", nil,
		"addresses", jsArr{},
		"reserved", jsArr{},
		"license_set", false,
		"auto_rotate_hours", int64(0),
		"last_rotate_at", nil,
		"updated_at", nil,
	)
	if account == nil {
		return presented, true
	}

	presented.put("device_id", nullableOrEmpty(account.DeviceID))
	presented.put("peer_endpoint", nullableOrEmpty(account.PeerEndpoint))
	presented.put("addresses", stringsToJSArr(account.Addresses))
	reserved := make(jsArr, len(account.Reserved))
	for i, v := range account.Reserved {
		reserved[i] = v
	}
	presented.put("reserved", reserved)
	presented.put("license_set", len(account.LicenseKey) >= 26)
	presented.put("auto_rotate_hours", account.AutoRotateHours)
	presented.put("last_rotate_at", nullableISO(account.LastRotateAt))
	presented.put("updated_at", nullableISO(account.UpdatedAt))
	return presented, true
}

// nullableOrEmpty 对齐 `$account?->field`：可空字符串列，空值输出 null。
func nullableOrEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
