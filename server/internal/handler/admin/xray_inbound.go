package admin

// xray 节点入站管理（/admin-api/nodes/{node}/xray-inbounds）。
// 逐条对齐 backend/app/Http/Controllers/Admin/XrayInboundController.php。
//
// 入站协议面板自建：vless / vmess / trojan / shadowsocks / hysteria
// （含 tcp/ws/grpc/xhttp/httpupgrade 传输、none/tls/reality 安全层；reality 仅 vless），
// 另有传统代理与自有 UDP 栈的 socks / http / tunnel / wireguard。
// 任何变更后 bump 该节点 config_version → 节点秒级拉取。

import (
	"github.com/gin-gonic/gin"
)

// xrayInboundIndex 对齐 index()。
func (d *NodeDomain) xrayInboundIndex(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	inbounds, err := d.store.XrayInboundsByNode(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询入站", err)
		return
	}
	counts, err := d.store.EligibleProtocolCounts(ctxOf(c), int64(node.ID))
	if err != nil {
		d.failServer(c, "统计可用用户", err)
		return
	}
	regionNames := d.regionNames(c)

	items := make(jsArr, 0, len(inbounds))
	for i := range inbounds {
		items = append(items, presentXrayInboundWithRegion(&inbounds[i], counts, regionNames))
	}
	d.success(c, jo(
		"inbounds", items,
		"config_version", node.ConfigVersion,
		// issue #94：端口跳跃要在节点上配防火墙，只有 agent ≥ 1.0.7 才有那几个 action。
		// 前端据此禁用「端口跳跃」区块并提示「需升级 agent」（与入队门槛同一判定）。
		"hop_supported", agentSupportsHop(node),
		"hop_min_agent_version", minHopAgentVersion,
	))
}

// xrayInboundStore 对齐 store()。
func (d *NodeDomain) xrayInboundStore(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateInboundPayload(c, input) {
		return
	}
	if !d.guardServerNames(c, node, input, nil) {
		return
	}

	// HY2 自动端口跳跃：用户没填区间时后端先探测一段空闲区间写进 payload。
	// 失败只降级成「无跳跃」，不挡建入站（原因随回执的 hop 段回给前端）。
	hopAuto, hopReason := d.autoAssignHysteriaHop(c, node, input, nil)

	in, ok := d.buildInboundInput(c, node, input, nil)
	if !ok {
		return
	}
	created, err := d.store.CreateXrayInbound(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建入站", err)
		return
	}
	d.markNodeCustomConfig(ctxOf(c), node.ID)
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}

	counts, err := d.store.EligibleProtocolCounts(ctxOf(c), int64(node.ID))
	if err != nil {
		d.failServer(c, "统计可用用户", err)
		return
	}
	resp := jo(
		"inbound", presentXrayInboundWithRegion(created, counts, d.regionNames(c)),
		"config_version", version,
	)
	// 入站已落库，防火墙这一步只报状态：下发失败不能让创建算失败。
	if hop := d.applyHysteriaHopAfterSave(c, node, created, hopAuto, hopReason); hop != nil {
		resp.put("hop", hop)
	}
	d.success(c, resp)
}

// xrayInboundUpdate 对齐 update()。
func (d *NodeDomain) xrayInboundUpdate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedInbound(c, node)
	if !ok {
		return
	}

	input := jsonInput(c)
	if !d.validateInboundPayload(c, input) {
		return
	}
	if !d.guardServerNames(c, node, input, existing) {
		return
	}

	// HY2 自动端口跳跃（编辑场景）：原配置已有区间就沿用，没有才重新探测。
	hopAuto, hopReason := d.autoAssignHysteriaHop(c, node, input, existing)

	in, ok := d.buildInboundInput(c, node, input, existing)
	if !ok {
		return
	}
	updated, err := d.store.UpdateXrayInbound(ctxOf(c), existing.ID, in)
	if err != nil {
		d.failServer(c, "保存入站", err)
		return
	}
	d.markNodeCustomConfig(ctxOf(c), node.ID)
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}

	counts, err := d.store.EligibleProtocolCounts(ctxOf(c), int64(node.ID))
	if err != nil {
		d.failServer(c, "统计可用用户", err)
		return
	}
	resp := jo(
		"inbound", presentXrayInboundWithRegion(updated, counts, d.regionNames(c)),
		"config_version", version,
	)
	// 入站已落库，防火墙这一步只报状态：下发失败不能让保存算失败。
	if hop := d.applyHysteriaHopAfterSave(c, node, updated, hopAuto, hopReason); hop != nil {
		resp.put("hop", hop)
	}
	d.success(c, resp)
}

// xrayInboundDestroy 对齐 destroy()。
func (d *NodeDomain) xrayInboundDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedInbound(c, node)
	if !ok {
		return
	}
	if err := d.store.DeleteXrayInbound(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "删除入站", err)
		return
	}
	d.markNodeCustomConfig(ctxOf(c), node.ID)
	// 入站删了，节点上属于它的 HY2 跳跃链也要跟着删（只入队，不影响删除结果）。
	d.removeHysteriaHopAfterDelete(ctxOf(c), node, existing)
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo("config_version", version))
}

// xrayInboundBatchDestroy 对齐 batchDestroy()：逐条处理、按 id 回报，只 bump 一次版本。
func (d *NodeDomain) xrayInboundBatchDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("ids", "required", "array", "min:1", "max:100")
	v.field("ids.*", "integer", "min:1")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	rawIDs, _ := input["ids"].([]any)
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id := inputInt(raw)
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	deleted := jsArr{}
	failed := jsArr{}
	anyDeleted := false
	for _, id := range ids {
		if id <= 0 {
			failed = append(failed, jo("id", id, "reason", "not_found"))
			continue
		}
		target, err := d.store.XrayInboundByID(ctxOf(c), node.ID, uint64(id))
		if err != nil {
			if d.noRowsToNotFoundQuiet(err) {
				failed = append(failed, jo("id", id, "reason", "not_found"))
				continue
			}
			failed = append(failed, jo("id", id, "reason", err.Error()))
			continue
		}
		if err := d.store.DeleteXrayInbound(ctxOf(c), uint64(id)); err != nil {
			failed = append(failed, jo("id", id, "reason", err.Error()))
			continue
		}
		// 与单条删除同一口径：删掉入站就清掉它自己的 HY2 跳跃链。
		d.removeHysteriaHopAfterDelete(ctxOf(c), node, target)
		deleted = append(deleted, id)
		anyDeleted = true
	}

	if anyDeleted {
		d.markNodeCustomConfig(ctxOf(c), node.ID)
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
	d.success(c, jo(
		"deleted", deleted,
		"failed", failed,
		"config_version", fresh.ConfigVersion,
	))
}

// noRowsToNotFoundQuiet 判断「查无此行」且不写响应（batch 里要按 id 回报）。
func (d *NodeDomain) noRowsToNotFoundQuiet(err error) bool {
	return err != nil && d.noRowsToNotFoundSilent(err)
}

// regionNames 取「地区 id → 名称」用于入站响应回显（#入站级分组）。
//
// regions 未注入（老构造方式 / 单测）时返回空表，presentXrayInboundWithRegion 就会把
// region_name 输出成 null —— 入站本身照常读写，不因为地区查询失败而 500（读不到名称
// 只是少一个展示字段，不该挡住整个列表）。
func (d *NodeDomain) regionNames(c *gin.Context) map[uint64]string {
	if d.regions == nil {
		return nil
	}
	names, err := d.regions.RegionNames(ctxOf(c))
	if err != nil {
		d.logger.Printf("admin 读取地区名称失败（入站 region_name 将为空）: %v", err)
		return nil
	}
	return names
}

// loadOwnedInbound 取 URL 里的入站并校验归属（PHP ensureOwned → abort(404)）。
func (d *NodeDomain) loadOwnedInbound(c *gin.Context, node *NodeRecord) (*XrayInboundRecord, bool) {
	id, ok := parseUint(c.Param("inbound"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	ib, err := d.store.XrayInboundByID(ctxOf(c), node.ID, id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询入站", err)
		return nil, false
	}
	return ib, true
}
