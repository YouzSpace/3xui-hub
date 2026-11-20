package admin

// 管理端第三方订阅管理（/admin-api/nodes/third-party）。
// 逐条对齐 backend/app/Http/Controllers/Admin/ThirdPartyController.php。
//
// 第三方「不管流量、只管时间」：套餐勾选的订阅在账号有效期内自动注入用户订阅，
// 到期随账号失效。节点在用户端统一命名「地区+序号」，机场原名/品牌词不外露（保密）。

import (
	"github.com/gin-gonic/gin"
)

const thirdPartyEnabledKey = "third_party_enabled"

// thirdPartyIndex 对齐 index()：列表 + 总开关状态。
func (d *NodeDomain) thirdPartyIndex(c *gin.Context) {
	subs, err := d.store.ThirdPartySubs(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询第三方订阅", err)
		return
	}
	cfg, err := d.store.SiteConfigMany(ctxOf(c), []string{thirdPartyEnabledKey})
	if err != nil {
		d.failServer(c, "读取第三方订阅开关", err)
		return
	}

	items := make(jsArr, 0, len(subs))
	for i := range subs {
		items = append(items, presentThirdPartySub(&subs[i]))
	}
	d.success(c, jo(
		"items", items,
		"enabled", cfg[thirdPartyEnabledKey] == "1",
	))
}

// thirdPartyStore 对齐 store()。
func (d *NodeDomain) thirdPartyStore(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("name", "required", "string", "max:64")
	v.field("url", "required", "string", "max:2048", "url")
	v.field("enabled", "sometimes", "boolean")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	enabled := true
	if val, present := input["enabled"]; present {
		enabled = inputBool(val)
	}
	created, err := d.store.CreateThirdPartySub(ctxOf(c), ThirdPartySubInput{
		Name:    nodeStringField(input, "name"),
		URL:     nodeStringField(input, "url"),
		Enabled: enabled,
	})
	if err != nil {
		d.failServer(c, "创建第三方订阅", err)
		return
	}

	// 新建后立刻拉一次：失败保留 pending 状态 + 记原因，不阻塞创建。
	if !d.fetchThirdPartySub(c, created.ID) {
		return
	}
	fresh, err := d.store.ThirdPartySubByID(ctxOf(c), created.ID)
	if err != nil {
		d.failServer(c, "回读第三方订阅", err)
		return
	}
	d.success(c, presentThirdPartySub(fresh), "已创建")
}

// thirdPartyUpdate 对齐 update()。
func (d *NodeDomain) thirdPartyUpdate(c *gin.Context) {
	existing, ok := d.loadThirdPartySub(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("name", "sometimes", "string", "max:64")
	v.field("url", "sometimes", "string", "max:2048", "url")
	v.field("enabled", "sometimes", "boolean")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	update := ThirdPartySubUpdate{
		Name:    nodeOptionalString(input, "name"),
		URL:     nodeOptionalString(input, "url"),
		Enabled: nil,
	}
	if val, present := input["enabled"]; present {
		b := inputBool(val)
		update.Enabled = &b
	}
	if _, err := d.store.UpdateThirdPartySub(ctxOf(c), existing.ID, update); err != nil {
		d.failServer(c, "保存第三方订阅", err)
		return
	}

	// URL 变了立刻重拉，避免缓存还是旧源的数据。
	if _, present := input["url"]; present {
		if !d.fetchThirdPartySub(c, existing.ID) {
			return
		}
	}
	fresh, err := d.store.ThirdPartySubByID(ctxOf(c), existing.ID)
	if err != nil {
		d.failServer(c, "回读第三方订阅", err)
		return
	}
	d.success(c, presentThirdPartySub(fresh), "已更新")
}

// thirdPartyDestroy 对齐 destroy()：删订阅 + 清理套餐里的引用。
func (d *NodeDomain) thirdPartyDestroy(c *gin.Context) {
	existing, ok := d.loadThirdPartySub(c)
	if !ok {
		return
	}
	if err := d.store.DeleteThirdPartySub(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "删除第三方订阅", err)
		return
	}
	// 清理套餐里的引用（JSON 数组），避免套餐配了个已删除的订阅。
	if err := d.store.RemoveThirdPartyFromPlans(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "清理套餐中的订阅引用", err)
		return
	}
	d.success(c, nil, "已删除")
}

// thirdPartyRefresh 对齐 refresh()：立即拉取（同步等机场响应）。
func (d *NodeDomain) thirdPartyRefresh(c *gin.Context) {
	existing, ok := d.loadThirdPartySub(c)
	if !ok {
		return
	}
	if !d.fetchThirdPartySub(c, existing.ID) {
		return
	}
	fresh, err := d.store.ThirdPartySubByID(ctxOf(c), existing.ID)
	if err != nil {
		d.failServer(c, "回读第三方订阅", err)
		return
	}
	msg := "拉取失败（已保留上次成功缓存）"
	if fresh.LastStatus == "success" {
		msg = "拉取成功"
	}
	d.success(c, presentThirdPartySub(fresh), msg)
}

// thirdPartySwitch 对齐 switch()：总开关（一键从所有订阅里摘掉第三方）。
func (d *NodeDomain) thirdPartySwitch(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("enabled", "required", "boolean")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	enabled := inputBool(input["enabled"])
	value := "0"
	if enabled {
		value = "1"
	}
	if err := d.store.SiteConfigSet(ctxOf(c), thirdPartyEnabledKey, &value); err != nil {
		d.failServer(c, "保存第三方订阅开关", err)
		return
	}
	msg := "已关闭"
	if enabled {
		msg = "已开启"
	}
	d.success(c, jo("enabled", enabled), msg)
}

// ---- 内部 ----

// loadThirdPartySub 取 URL 里的订阅（不存在 → 404）。
func (d *NodeDomain) loadThirdPartySub(c *gin.Context) (*ThirdPartySubRecord, bool) {
	id, ok := parseUint(c.Param("sub"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	sub, err := d.store.ThirdPartySubByID(ctxOf(c), id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询第三方订阅", err)
		return nil, false
	}
	return sub, true
}

// fetchThirdPartySub 拉取并落库；失败只记原因（保留上次成功缓存）。
//
// 返回 false 表示已写错误响应（落库失败）。
func (d *NodeDomain) fetchThirdPartySub(c *gin.Context, id uint64) bool {
	ctx := ctxOf(c)
	sub, err := d.store.ThirdPartySubByID(ctx, id)
	if err != nil {
		d.failServer(c, "查询第三方订阅", err)
		return false
	}

	links, fetchErr := d.ops.ThirdPartyFetch(ctx, sub.URL)
	if fetchErr != nil || len(links) == 0 {
		msg := ""
		if fetchErr != nil {
			msg = fetchErr.Error()
		} else {
			msg = "拉取成功但未解析到任何节点链接"
		}
		if err := d.store.SaveThirdPartyFetch(ctx, id, nil, msg); err != nil {
			d.failServer(c, "记录第三方拉取失败", err)
			return false
		}
		return true
	}
	if err := d.store.SaveThirdPartyFetch(ctx, id, links, ""); err != nil {
		d.failServer(c, "保存第三方拉取结果", err)
		return false
	}
	return true
}

// presentThirdPartySub 对齐 present()：链接打码展示（含机场 token，敏感）。
func presentThirdPartySub(s *ThirdPartySubRecord) *jsObj {
	masked := s.URL
	runes := []rune(s.URL)
	if len(runes) > 20 {
		masked = string(runes[:12]) + "…" + string(runes[len(runes)-6:])
	}
	return jo(
		"id", s.ID,
		"name", s.Name,
		"url", s.URL, // 仅管理端接口返回完整链接
		"url_masked", masked,
		"enabled", s.Enabled,
		"node_count", s.NodeCount,
		"last_fetched_at", nullableISO(s.LastFetchedAt),
		"last_status", s.LastStatus,
		"last_error", nullableStr(s.LastError),
	)
}

