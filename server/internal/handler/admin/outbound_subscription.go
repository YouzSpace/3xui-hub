package admin

// xray 节点「出站订阅」（服务器池）管理
// （/admin-api/nodes/{node}/xray-outbound-subscriptions）。
// 逐条对齐 backend/app/Http/Controllers/Admin/OutboundSubscriptionController.php。
//
// - CRUD + 立即刷新（同步拉取一次回显结果）
// - 拉取前 SSRF 校验（仅 http/https + 非内网），非法 URL 直接拒绝
// - 停用/删除/内容变化 → bump 该节点 config_version

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// outboundSubIndex 对齐 index()。
func (d *NodeDomain) outboundSubIndex(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	subs, err := d.store.OutboundSubsByNode(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询出站订阅", err)
		return
	}
	items := make(jsArr, 0, len(subs))
	for i := range subs {
		items = append(items, presentOutboundSub(&subs[i]))
	}
	d.success(c, jo("items", items, "config_version", node.ConfigVersion))
}

// outboundSubStore 对齐 store()。
func (d *NodeDomain) outboundSubStore(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateOutboundSubPayload(c, input) {
		return
	}

	url, err := d.ops.SanitizeSubURL(ctxOf(c), nodeStringField(input, "url"))
	if err != nil {
		d.fail(c, err.Error(), 500)
		return
	}

	priority, err := d.store.MaxOutboundSubPriority(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询出站订阅优先级", err)
		return
	}
	priority++

	in := outboundSubInputOf(node, input, url)
	in.Priority = &priority
	created, err := d.store.CreateOutboundSub(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建出站订阅", err)
		return
	}

	// 新建后立刻拉一次（失败保留 pending 状态 + 记原因，不阻塞创建）。
	if !d.refreshOutboundSub(c, created) {
		return
	}

	fresh, err := d.store.OutboundSubByID(ctxOf(c), node.ID, created.ID)
	if err != nil {
		d.failServer(c, "回读出站订阅", err)
		return
	}
	freshNode, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	d.success(c, jo(
		"subscription", presentOutboundSub(fresh),
		"config_version", freshNode.ConfigVersion,
	))
}

// outboundSubUpdate 对齐 update()。
func (d *NodeDomain) outboundSubUpdate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedOutboundSub(c, node)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateOutboundSubPayload(c, input) {
		return
	}

	url, err := d.ops.SanitizeSubURL(ctxOf(c), nodeStringField(input, "url"))
	if err != nil {
		d.fail(c, err.Error(), 500)
		return
	}
	urlChanged := url != existing.URL

	in := outboundSubInputOf(node, input, url)
	if _, err := d.store.UpdateOutboundSub(ctxOf(c), existing.ID, in); err != nil {
		d.failServer(c, "保存出站订阅", err)
		return
	}

	// URL 变了立刻重拉（否则缓存还是旧源的数据）。
	if urlChanged {
		refreshed, err := d.store.OutboundSubByID(ctxOf(c), node.ID, existing.ID)
		if err != nil {
			d.failServer(c, "回读出站订阅", err)
			return
		}
		if !d.refreshOutboundSub(c, refreshed) {
			return
		}
	}

	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	fresh, err := d.store.OutboundSubByID(ctxOf(c), node.ID, existing.ID)
	if err != nil {
		d.failServer(c, "回读出站订阅", err)
		return
	}
	d.success(c, jo(
		"subscription", presentOutboundSub(fresh),
		"config_version", version,
	))
}

// outboundSubDestroy 对齐 destroy()。
func (d *NodeDomain) outboundSubDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedOutboundSub(c, node)
	if !ok {
		return
	}
	if err := d.store.DeleteOutboundSub(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "删除出站订阅", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo("config_version", version))
}

// outboundSubRefresh 对齐 refresh()。
func (d *NodeDomain) outboundSubRefresh(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedOutboundSub(c, node)
	if !ok {
		return
	}
	if !d.refreshOutboundSub(c, existing) {
		return
	}
	fresh, err := d.store.OutboundSubByID(ctxOf(c), node.ID, existing.ID)
	if err != nil {
		d.failServer(c, "回读出站订阅", err)
		return
	}

	msg := "拉取失败（已保留上次成功缓存）"
	if fresh.LastStatus == "success" {
		msg = "拉取成功"
	}
	d.success(c, presentOutboundSub(fresh), msg)
}

// ---- 内部 ----

// loadOwnedOutboundSub 取 URL 里的订阅并校验归属。
func (d *NodeDomain) loadOwnedOutboundSub(c *gin.Context, node *NodeRecord) (*OutboundSubRecord, bool) {
	id, ok := parseUint(c.Param("subscription"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	sub, err := d.store.OutboundSubByID(ctxOf(c), node.ID, id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询出站订阅", err)
		return nil, false
	}
	return sub, true
}

// validateOutboundSubPayload 对齐 validatePayload()。
func (d *NodeDomain) validateOutboundSubPayload(c *gin.Context, input map[string]any) bool {
	v := newNodeValidator(ctxOf(c), input)
	v.field("remark", "nullable", "string", "max:128")
	v.field("url", "required", "string", "max:2048")
	v.field("tag_prefix", "nullable", "string", "max:64", "regex:/^[A-Za-z0-9_.-]*$/")
	v.field("update_interval", "nullable", "integer", "min:60", "max:86400")
	v.field("prepend", "nullable", "boolean")
	v.field("priority", "nullable", "integer", "min:0", "max:65535")
	v.field("enabled", "nullable", "boolean")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return false
	}
	return true
}

// outboundSubInputOf 对齐 fillFromPayload()。
func outboundSubInputOf(node *NodeRecord, input map[string]any, url string) OutboundSubInput {
	in := OutboundSubInput{NodeID: node.ID, URL: url}
	if remark := nodeStringField(input, "remark"); remark != "" {
		in.Remark = &remark
	}
	if prefix := nodeStringField(input, "tag_prefix"); prefix != "" {
		in.TagPrefix = &prefix
	}
	interval := int64(600)
	if v, present := input["update_interval"]; present && v != nil {
		// PHP: `(int) ($data['update_interval'] ?? 600) ?: 600`
		if n := inputInt(v); n != 0 {
			interval = n
		}
	}
	in.UpdateInterval = &interval
	if v, present := input["prepend"]; present {
		b := inputBool(v)
		in.Prepend = &b
	}
	if v, present := input["priority"]; present && v != nil {
		n := inputInt(v)
		in.Priority = &n
	}
	if v, present := input["enabled"]; present {
		b := inputBool(v)
		in.Enabled = &b
	}
	return in
}

// refreshOutboundSub 拉取 → 解析 → 标签稳定 → 落库（失败只记原因）。
//
// 返回 false 表示已写错误响应（落库失败）；拉取失败不算错误（PHP 同样保留 pending）。
func (d *NodeDomain) refreshOutboundSub(c *gin.Context, sub *OutboundSubRecord) bool {
	ctx := ctxOf(c)

	url, err := d.ops.SanitizeSubURL(ctx, sub.URL)
	if err != nil {
		if err := d.store.RecordOutboundSubError(ctx, sub.ID, err.Error(), d.now().UTC()); err != nil {
			d.failServer(c, "记录出站订阅拉取失败", err)
			return false
		}
		return true
	}

	outbounds, identities, fetchErr := d.ops.OutboundSubFetch(ctx, url)
	if fetchErr != nil || len(outbounds) == 0 {
		msg := ""
		if fetchErr != nil {
			msg = fetchErr.Error()
		} else {
			msg = "拉取成功但未解析到任何可用出站"
		}
		if err := d.store.RecordOutboundSubError(ctx, sub.ID, msg, d.now().UTC()); err != nil {
			d.failServer(c, "记录出站订阅拉取失败", err)
			return false
		}
		return true
	}

	// 上一次的 identity→tag 映射 + 位置→tag 兜底。
	prev := parseIdentityMap(sub.LinkIdentities)
	prevTagByIndex := map[int]string{}
	for i, tag := range outboundTagsFromFetched(sub.Fetched) {
		if tag != "" {
			prevTagByIndex[i] = tag
		}
	}

	assigned := assignStableOutboundTags(
		outbounds, identities, prev, prevTagByIndex,
		int64(sub.ID), derefOr(sub.TagPrefix, ""),
	)

	for i := range outbounds {
		if obj, isObj := outbounds[i].(*jsObj); isObj {
			obj.Put("tag", assigned[i])
		}
	}
	newIdent := map[string]string{}
	for i := range outbounds {
		if i < len(identities) && identities[i] != "" {
			newIdent[identities[i]] = assigned[i]
		}
	}

	fetchedJSON := string(phpEncode(jsArr(outbounds)))
	hash := sha1HexString(fetchedJSON)

	result := OutboundSubFetchResult{
		Fetched:        fetchedJSON,
		LinkIdentities: identityMapJSON(newIdent),
		ContentHash:    hash,
		NodeCount:      int64(len(outbounds)),
		At:             d.now().UTC(),
	}
	if err := d.store.SaveOutboundSubFetch(ctx, sub.ID, result); err != nil {
		d.failServer(c, "保存出站订阅拉取结果", err)
		return false
	}

	// 内容变化才 bump 版本 → 节点重拉 config。
	if hash != derefOr(sub.ContentHash, "") {
		if _, err := d.store.BumpConfigVersion(ctx, sub.NodeID); err != nil {
			d.failServer(c, "递增配置版本", err)
			return false
		}
	}
	return true
}

// presentOutboundSub 对齐 present()。
func presentOutboundSub(s *OutboundSubRecord) *jsObj {
	url := s.URL
	masked := url
	runes := []rune(url)
	if len(runes) > 24 {
		masked = string(runes[:12]) + "…" + string(runes[len(runes)-8:])
	}

	outboundCount := int64(0)
	if s.Fetched != nil && strings.TrimSpace(*s.Fetched) != "" {
		if parsed, err := parseOrderedJS([]byte(*s.Fetched)); err == nil {
			if arr, ok := parsed.(jsArr); ok {
				for _, item := range arr {
					if _, isObj := item.(*jsObj); isObj {
						outboundCount++
					}
				}
			}
		}
	}

	return jo(
		"id", s.ID,
		"remark", nullableStr(s.Remark),
		"url", url,
		"url_masked", masked,
		"tag_prefix", nullableStr(s.TagPrefix),
		"update_interval", s.UpdateInterval,
		"prepend", s.Prepend,
		"priority", s.Priority,
		"enabled", s.Enabled,
		"node_count", s.NodeCount,
		"outbound_count", outboundCount,
		"last_fetched_at", nullableISO(s.LastFetchedAt),
		"last_status", s.LastStatus,
		"last_error", nullableStr(s.LastError),
	)
}

// ---- 标签稳定（复刻 OutboundSubscriptionService::assignStableTags）----

// assignStableOutboundTags 对齐 assignStableTags()：
//  1. identity 见过 → 复用旧 tag
//  2. 否则位置兜底复用（未被其它 identity 预留时）
//  3. 否则 prefix + slug(remark) 新分配
//
// 批内用 -N 去重。
func assignStableOutboundTags(outbounds []any, identities []string, prev map[string]string, prevTagByIndex map[int]string, subID int64, prefix string) []string {
	reserved := map[string]bool{}
	for i := range outbounds {
		id := identityAt(identities, i)
		if id == "" {
			continue
		}
		if tag := prev[id]; tag != "" {
			reserved[tag] = true
		}
	}

	used := map[string]bool{}
	assigned := make([]string, len(outbounds))
	for i, ob := range outbounds {
		id := identityAt(identities, i)
		candidate := ""
		identityTag := ""

		if id != "" {
			if tag := prev[id]; tag != "" {
				candidate = tag
				identityTag = tag
			}
		}
		if candidate == "" {
			if tag := prevTagByIndex[i]; tag != "" && !reserved[tag] {
				candidate = tag
			}
		}
		if candidate == "" {
			p := prefix
			if p == "" {
				p = "sub" + i64String(subID) + "-"
			}
			candidate = suggestOutboundTag(p, outboundTagOf(ob), i)
		}

		final := candidate
		for k := 1; used[final] || (reserved[final] && final != identityTag); k++ {
			final = candidate + "-" + i64String(int64(k))
		}
		used[final] = true
		assigned[i] = final
	}
	return assigned
}

// suggestOutboundTag 对齐 suggestTag()：由 prefix + remark 生成 tag（remark 空则用序号）。
func suggestOutboundTag(prefix, remark string, idx int) string {
	base := slugRemark(remark)
	if base == "" {
		base = i64String(int64(idx))
	}
	p := strings.TrimRight(prefix, "-")
	if p != "" {
		return p + "-" + base
	}
	return base
}

// slugRemark 对齐 slug()：保留字母数字与 CJK，其余折叠成 '-'；截 48 字符。
func slugRemark(remark string) string {
	s := strings.ToLower(strings.TrimSpace(remark))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if isLetterOrDigitRune(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	return truncateRunes(out, 48)
}

// isLetterOrDigitRune 近似 PHP 的 `\p{L}\p{N}`（Go 的 unicode 判定）。
func isLetterOrDigitRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK 统一表意文字
		return true
	case r >= 0x3040 && r <= 0x30FF: // 日文假名
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // 韩文
		return true
	}
	return false
}

// ---- 小工具 ----

func identityAt(identities []string, i int) string {
	if i < len(identities) {
		return identities[i]
	}
	return ""
}

func outboundTagOf(v any) string {
	obj, ok := v.(*jsObj)
	if !ok {
		return ""
	}
	return jsonGetString(obj, "tag")
}

// outboundTagsFromFetched 取上次拉取结果里的 tag 序列（位置兜底用）。
func outboundTagsFromFetched(fetched *string) []string {
	if fetched == nil || strings.TrimSpace(*fetched) == "" {
		return nil
	}
	parsed, err := parseOrderedJS([]byte(*fetched))
	if err != nil {
		return nil
	}
	arr, ok := parsed.(jsArr)
	if !ok {
		return nil
	}
	out := make([]string, len(arr))
	for i, item := range arr {
		out[i] = outboundTagOf(item)
	}
	return out
}

// parseIdentityMap 解析 link_identities JSON（坏数据 → 空 map）。
func parseIdentityMap(raw *string) map[string]string {
	out := map[string]string{}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return out
	}
	parsed, err := parseOrderedJS([]byte(*raw))
	if err != nil {
		return out
	}
	obj, ok := parsed.(*jsObj)
	if !ok {
		return out
	}
	keys := obj.Keys()
	vals := obj.Vals()
	for i, k := range keys {
		out[k] = nodeScalarString(vals[i])
	}
	return out
}

// identityMapJSON 把 identity→tag 映射编码成 JSON（PHP 的 json_encode 保序）。
//
// 输入是 Go map（无序）：PHP 侧写入顺序也来自 foreach，但语义上只是查表，
// 这里按键排序保证同一输入产出稳定字节（content_hash 只对 fetched 算，不受影响）。
func identityMapJSON(m map[string]string) string {
	obj := jo()
	for _, k := range sortedStringKeys(m) {
		obj.Put(k, m[k])
	}
	return string(phpEncode(obj))
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 插入排序（键数量是订阅里的节点数，几千级也足够快，且避免额外依赖）。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func derefOr(v *string, def string) string {
	if v == nil {
		return def
	}
	return *v
}
