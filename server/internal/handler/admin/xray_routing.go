package admin

// xray 节点路由管理（/admin-api/nodes/{node}/xray-routing）。
// 逐条对齐 backend/app/Http/Controllers/Admin/XrayRoutingController.php。
//
// - 规则 CRUD（域名/IP/端口/网络/协议/入站 → 目标出站 / balancer）
// - 负载均衡器（balancer）CRUD：selector 前缀 → 多个出站间分摊
// - 基础设置（routing.domainStrategy / domainMatcher）存取（driver_config.xray_routing）
// - 路由测试器：输入域名或 IP，按渲染口径逐条模拟匹配

import (
	"net"
	"regexp"
	"strings"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

// routing.domainStrategy 合法值（xray routing 枚举）。
var domainStrategies = []string{"AsIs", "IPIfNonMatch", "IPOnDemand"}

// routing.domainMatcher 合法值（空 = 内核默认）。
var domainMatchers = []string{"", "mph", "hybrid"}

// balancer 策略（与 XrayBalancer::STRATEGIES 同序）。
var balancerStrategies = []string{"random", "roundRobin", "leastPing", "leastLoad"}

// routingIndex 对齐 index()。
func (d *NodeDomain) routingIndex(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	rules, err := d.store.RoutingRulesByNode(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询路由规则", err)
		return
	}
	balancers, err := d.store.BalancersByNode(ctxOf(c), node.ID, false)
	if err != nil {
		d.failServer(c, "查询负载均衡器", err)
		return
	}

	rulesArr := make(jsArr, 0, len(rules))
	for i := range rules {
		rulesArr = append(rulesArr, presentRoutingRule(&rules[i]))
	}
	balancersArr := make(jsArr, 0, len(balancers))
	for i := range balancers {
		balancersArr = append(balancersArr, presentBalancer(&balancers[i]))
	}

	candidates, err := d.targetTags(c, node)
	if err != nil {
		return
	}
	onlyTags, err := d.outboundTags(c, node)
	if err != nil {
		return
	}
	inbounds, err := d.inboundCandidates(c, node)
	if err != nil {
		return
	}
	renderPreview, err := d.renderRoutingPreview(c, node)
	if err != nil {
		return
	}

	d.success(c, jo(
		"rules", rulesArr,
		"balancers", balancersArr,
		"outbound_tags", candidates,
		"outbound_only_tags", onlyTags,
		"inbounds", inbounds,
		"settings", routingSettings(node),
		"strategies", stringsToJSArr(balancerStrategies),
		"domain_strategies", stringsToJSArr(domainStrategies),
		"domain_matchers", stringsToJSArr(domainMatchers),
		"config_version", node.ConfigVersion,
		"render_preview", jo(
			"rules", renderPreview,
			"generated_at", isoTime(d.now()),
		),
	))
}

// routingStore 对齐 store()。
func (d *NodeDomain) routingStore(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateRoutingPayload(c, node, input) {
		return
	}
	in := routingRuleInputOf(node, input)
	created, err := d.store.CreateRoutingRule(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建路由规则", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	warning, err := d.udpGuardWarning(c, node, created)
	if err != nil {
		return
	}
	d.success(c, jo(
		"rule", presentRoutingRule(created),
		"config_version", version,
		"warning", warning,
	))
}

// routingUpdate 对齐 update()。
func (d *NodeDomain) routingUpdate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedRule(c, node)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateRoutingPayload(c, node, input) {
		return
	}
	in := routingRuleInputOf(node, input)
	updated, err := d.store.UpdateRoutingRule(ctxOf(c), existing.ID, in)
	if err != nil {
		d.failServer(c, "保存路由规则", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	warning, err := d.udpGuardWarning(c, node, updated)
	if err != nil {
		return
	}
	d.success(c, jo(
		"rule", presentRoutingRule(updated),
		"config_version", version,
		"warning", warning,
	))
}

// routingDestroy 对齐 destroy()：回收该规则引用的自动中转出站。
func (d *NodeDomain) routingDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedRule(c, node)
	if !ok {
		return
	}
	if err := d.store.DeleteRoutingRule(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "删除路由规则", err)
		return
	}

	// 回收：该规则引用的自动中转出站若无其它规则再引用 → 删出站 + 清对方承接凭据。
	if !d.cleanupOutboundForRule(c, node, existing) {
		return
	}

	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo("config_version", version))
}

// ---- 负载均衡器 ----

// balancerStore 对齐 storeBalancer()。
func (d *NodeDomain) balancerStore(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	in, ok := d.validateBalancerPayload(c, node, input, nil)
	if !ok {
		return
	}
	created, err := d.store.CreateBalancer(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建负载均衡器", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo(
		"balancer", presentBalancer(created),
		"config_version", version,
	))
}

// balancerUpdate 对齐 updateBalancer()。
func (d *NodeDomain) balancerUpdate(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedBalancer(c, node)
	if !ok {
		return
	}
	input := jsonInput(c)
	in, ok := d.validateBalancerPayload(c, node, input, existing)
	if !ok {
		return
	}
	updated, err := d.store.UpdateBalancer(ctxOf(c), existing.ID, in)
	if err != nil {
		d.failServer(c, "保存负载均衡器", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo(
		"balancer", presentBalancer(updated),
		"config_version", version,
	))
}

// balancerDestroy 对齐 destroyBalancer()。
func (d *NodeDomain) balancerDestroy(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	existing, ok := d.loadOwnedBalancer(c, node)
	if !ok {
		return
	}
	if err := d.store.DeleteBalancer(ctxOf(c), existing.ID); err != nil {
		d.failServer(c, "删除负载均衡器", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}
	d.success(c, jo("config_version", version))
}

// routingUpdateSettings 对齐 updateSettings()。
func (d *NodeDomain) routingUpdateSettings(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("domain_strategy", "nullable", "string", "in:"+strings.Join(domainStrategies, ","))
	v.field("domain_matcher", "nullable", "string", "in:"+strings.Join(nonEmpty(domainMatchers), ","))
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	strategy := nodeStringField(input, "domain_strategy")
	if strategy == "" {
		strategy = "AsIs"
	}
	matcher := nodeStringField(input, "domain_matcher")

	// 只 merge 自家键（行锁 + 键级合并，避免覆盖并发 bump 的 config_version）。
	patch := map[string]any{
		"xray_routing": map[string]any{
			"domainStrategy": strategy,
			"domainMatcher":  matcher,
		},
	}
	if err := d.store.MergeDriverConfig(ctxOf(c), node.ID, patch); err != nil {
		d.failServer(c, "保存路由设置", err)
		return
	}
	version, err := d.store.BumpConfigVersion(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "递增配置版本", err)
		return
	}

	fresh, err := d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	d.success(c, jo(
		"settings", routingSettings(fresh),
		"config_version", version,
	))
}

// routingTest 对齐 test()：按渲染口径逐条模拟匹配。
func (d *NodeDomain) routingTest(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("domain", "nullable", "string", "max:255")
	v.field("ip", "nullable", "string", "max:64")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	domain := strings.TrimSpace(nodeStringField(input, "domain"))
	ip := strings.TrimSpace(nodeStringField(input, "ip"))
	if domain == "" && ip == "" {
		d.fail(c, "请输入域名或 IP", 400)
		return
	}

	rules, err := d.renderRoutingRules(c, node)
	if err != nil {
		return
	}

	var matched any
	for idx, rule := range rules {
		hit := false
		notes := jsArr{}

		if domain != "" {
			for _, pattern := range stringValues(jsonGet(rule, "domain")) {
				result := matchDomainPattern(pattern, domain)
				switch result {
				case matchHit:
					hit = true
					notes = append(notes, "域名命中 "+pattern)
				case matchUnknown:
					notes = append(notes, "规则含 "+pattern+"（需内核 geo 数据，无法在面板精确判定）")
				}
				if hit {
					break
				}
			}
		}

		if !hit && ip != "" {
			for _, pattern := range stringValues(jsonGet(rule, "ip")) {
				result := matchIPPattern(pattern, ip)
				switch result {
				case matchHit:
					hit = true
					notes = append(notes, "IP 命中 "+pattern)
				case matchUnknown:
					notes = append(notes, "规则含 "+pattern+"（需内核 geo 数据，无法在面板精确判定）")
				}
				if hit {
					break
				}
			}
		}

		if hit {
			balancerTag, hasBalancer := jsonGetOK(rule, "balancerTag")
			outbound := jsonGet(rule, "outboundTag")
			if hasBalancer {
				outbound = balancerTag
			}
			matched = jo(
				"index", int64(idx),
				"rule", rule,
				"outbound", outbound,
				"is_balancer", hasBalancer,
				"notes", notes,
			)
			break
		}
	}

	defaultOutbound := "direct"
	var finalOutbound any = defaultOutbound
	if obj, isObj := matched.(*jsObj); isObj {
		if v, ok := obj.get("outbound"); ok {
			finalOutbound = v
		}
	}
	d.success(c, jo(
		"matched", matched,
		"outbound", finalOutbound,
		"default_outbound", defaultOutbound,
		"rules_checked", int64(len(rules)),
	))
}

// ---- 内部 ----

// loadOwnedRule 取 URL 里的规则并校验归属。
func (d *NodeDomain) loadOwnedRule(c *gin.Context, node *NodeRecord) (*RoutingRuleRecord, bool) {
	id, ok := parseUint(c.Param("rule"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	rule, err := d.store.RoutingRuleByID(ctxOf(c), node.ID, id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询路由规则", err)
		return nil, false
	}
	return rule, true
}

// loadOwnedBalancer 取 URL 里的 balancer 并校验归属。
func (d *NodeDomain) loadOwnedBalancer(c *gin.Context, node *NodeRecord) (*BalancerRecord, bool) {
	id, ok := parseUint(c.Param("balancer"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	b, err := d.store.BalancerByID(ctxOf(c), node.ID, id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询负载均衡器", err)
		return nil, false
	}
	return b, true
}

// validateRoutingPayload 对齐 validatePayload()。
func (d *NodeDomain) validateRoutingPayload(c *gin.Context, node *NodeRecord, input map[string]any) bool {
	v := newNodeValidator(ctxOf(c), input)
	v.field("domains", "nullable", "array", "max:200")
	v.field("domains.*", "string", "max:512")
	v.field("ips", "nullable", "array", "max:200")
	v.field("ips.*", "string", "max:512")
	v.field("port", "nullable", "string", "max:64")
	v.field("network", "nullable", "in:tcp,udp")
	v.field("protocol", "nullable", "in:http,tls,bittorrent")
	v.field("inbound_tag", "nullable", "string", "max:64")
	v.field("outbound_tag", "required", "string", "max:64")
	v.field("remark", "nullable", "string", "max:128")
	v.field("enabled", "nullable", "boolean")
	v.field("sort", "nullable", "integer", "min:0", "max:65535")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return false
	}

	// 至少一个匹配条件（否则会命中全部流量）。
	emptyCond := true
	for _, item := range phpArrayCast(input["domains"]) {
		if strings.TrimSpace(nodeScalarString(item)) != "" {
			emptyCond = false
			break
		}
	}
	for _, item := range phpArrayCast(input["ips"]) {
		if strings.TrimSpace(nodeScalarString(item)) != "" {
			emptyCond = false
			break
		}
	}
	for _, key := range []string{"port", "network", "protocol", "inbound_tag"} {
		if nodeStringField(input, key) != "" {
			emptyCond = false
			break
		}
	}
	if emptyCond {
		d.fail(c, "至少需要一个匹配条件（域名/IP/端口/网络/协议/入站）", 500)
		return false
	}

	// 目标 tag 必须存在（内置出站 / 自定义出站（含中转出站）/ 已启用 balancer）。
	target := nodeStringField(input, "outbound_tag")
	candidates, err := d.targetTags(c, node)
	if err != nil {
		return false
	}
	for _, item := range candidates {
		obj, _ := item.(*jsObj)
		if jsonGetString(obj, "tag") == target {
			return true
		}
	}
	d.fail(c, "目标出站不存在", 500)
	return false
}

// validateBalancerPayload 对齐 validateBalancer()。
func (d *NodeDomain) validateBalancerPayload(c *gin.Context, node *NodeRecord, input map[string]any, self *BalancerRecord) (BalancerInput, bool) {
	v := newNodeValidator(ctxOf(c), input)
	v.field("tag", "required", "string", "max:64", "regex:/^[A-Za-z0-9._-]+$/")
	v.field("selectors", "required", "array", "min:1", "max:50")
	v.field("selectors.*", "string", "max:64")
	v.field("strategy", "required", "string", "in:"+strings.Join(balancerStrategies, ","))
	v.field("fallback_tag", "nullable", "string", "max:64")
	v.field("remark", "nullable", "string", "max:128")
	v.field("enabled", "nullable", "boolean")
	v.field("sort", "nullable", "integer", "min:0", "max:65535")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return BalancerInput{}, false
	}

	tag := strings.TrimSpace(nodeStringField(input, "tag"))
	var excludeID *uint64
	if self != nil {
		excludeID = ptrUintOf(self.ID)
	}
	dup, err := d.store.BalancerTagTaken(ctxOf(c), node.ID, tag, excludeID)
	if err != nil {
		d.failServer(c, "检查负载均衡 tag", err)
		return BalancerInput{}, false
	}
	if dup {
		d.fail(c, "该负载均衡 tag 已存在", 500)
		return BalancerInput{}, false
	}

	outboundTags, err := d.outboundTagStrings(c, node)
	if err != nil {
		return BalancerInput{}, false
	}
	for _, existing := range outboundTags {
		if existing == tag {
			d.fail(c, "该 tag 与已有出站 tag 冲突", 500)
			return BalancerInput{}, false
		}
	}

	selectors := []string{}
	for _, item := range phpArrayCast(input["selectors"]) {
		s := strings.TrimSpace(nodeScalarString(item))
		if s != "" {
			selectors = append(selectors, s)
		}
	}
	if len(selectors) == 0 {
		d.fail(c, "至少需要一个 selector", 500)
		return BalancerInput{}, false
	}
	for _, selector := range selectors {
		matched := false
		for _, t := range outboundTags {
			if strings.HasPrefix(t, selector) {
				matched = true
				break
			}
		}
		if !matched {
			d.fail(c, "selector「"+selector+"」没有匹配到任何出站", 500)
			return BalancerInput{}, false
		}
	}

	fallback := strings.TrimSpace(nodeStringField(input, "fallback_tag"))
	if fallback != "" && !phpInArray(fallback, outboundTags) {
		d.fail(c, "回退出站不存在", 500)
		return BalancerInput{}, false
	}

	selectorsArr := jsArr{}
	for _, s := range selectors {
		selectorsArr = append(selectorsArr, s)
	}
	in := BalancerInput{
		NodeID:    node.ID,
		Tag:       tag,
		Selectors: phpEncode(selectorsArr),
		Strategy:  nodeStringField(input, "strategy"),
	}
	if fallback != "" {
		in.FallbackTag = &fallback
	}
	if remark := nodeStringField(input, "remark"); remark != "" {
		in.Remark = &remark
	}
	if val, present := input["enabled"]; present {
		b := inputBool(val)
		in.Enabled = &b
	}
	if val, present := input["sort"]; present && val != nil {
		n := inputInt(val)
		in.Sort = &n
	}
	return in, true
}

// routingRuleInputOf 对齐 fillFromPayload()。
func routingRuleInputOf(node *NodeRecord, input map[string]any) RoutingRuleInput {
	in := RoutingRuleInput{
		NodeID:      node.ID,
		OutboundTag: nodeStringField(input, "outbound_tag"),
	}
	if domains := trimmedStringList(input["domains"]); len(domains) > 0 {
		in.Domains = phpEncode(domains)
	}
	if ips := trimmedStringList(input["ips"]); len(ips) > 0 {
		in.IPs = phpEncode(ips)
	}
	if v, present := input["port"]; present && v != nil {
		s := nodeScalarString(v)
		in.Port = &s
	}
	if v, present := input["network"]; present && v != nil {
		s := nodeScalarString(v)
		in.Network = &s
	}
	if v, present := input["protocol"]; present && v != nil {
		s := nodeScalarString(v)
		in.Protocol = &s
	}
	if v, present := input["inbound_tag"]; present && v != nil {
		s := nodeScalarString(v)
		in.InboundTag = &s
	}
	if remark := nodeStringField(input, "remark"); remark != "" {
		in.Remark = &remark
	}
	if v, present := input["enabled"]; present {
		b := inputBool(v)
		in.Enabled = &b
	}
	if v, present := input["sort"]; present && v != nil {
		n := inputInt(v)
		in.Sort = &n
	}
	return in
}

// trimmedStringList 复刻 `array_values(array_filter(array_map('trim', $arr), fn($v) => $v !== ''))`。
func trimmedStringList(raw any) []string {
	out := []string{}
	for _, item := range phpArrayCast(raw) {
		s := strings.TrimSpace(nodeScalarString(item))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---- 展示 ----

func presentRoutingRule(r *RoutingRuleRecord) *jsObj {
	return jo(
		"id", r.ID,
		"domains", arrayValues(parseJSONOrEmpty(r.Domains)),
		"ips", arrayValues(parseJSONOrEmpty(r.IPs)),
		"port", nullableStr(r.Port),
		"network", nullableStr(r.Network),
		"protocol", nullableStr(r.Protocol),
		"inbound_tag", nullableStr(r.InboundTag),
		"outbound_tag", r.OutboundTag,
		"remark", nullableStr(r.Remark),
		"enabled", r.Enabled,
		"sort", r.Sort,
	)
}

func presentBalancer(b *BalancerRecord) *jsObj {
	return jo(
		"id", b.ID,
		"tag", b.Tag,
		"selectors", arrayValues(parseJSONOrEmpty(b.Selectors)),
		"strategy", b.Strategy,
		"fallback_tag", nullableStr(b.FallbackTag),
		"remark", nullableStr(b.Remark),
		"enabled", b.Enabled,
		"sort", b.Sort,
	)
}

// parseJSONOrEmpty 解析 JSON 列（坏数据 / nil → nil，对应 PHP 的 json_decode 返回 null）。
func parseJSONOrEmpty(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	v, err := parseOrderedJS(raw)
	if err != nil {
		return nil
	}
	return v
}

// routingSettings 对齐 Node::xrayRoutingSettings()：默认 AsIs / 空。
func routingSettings(n *NodeRecord) *jsObj {
	raw := nodeDriverConfig(n, "xray_routing")
	strategy := "AsIs"
	matcher := ""
	if v := jsonGetString(raw, "domainStrategy"); v != "" {
		strategy = v
	}
	matcher = jsonGetString(raw, "domainMatcher")
	return jo("domainStrategy", strategy, "domainMatcher", matcher)
}

// ---- 目标候选 ----

// outboundTags 对齐 outboundTags()：内置 + 自定义 + warp + 订阅。
func (d *NodeDomain) outboundTags(c *gin.Context, node *NodeRecord) (jsArr, error) {
	tags := jsArr{
		jo("tag", "direct", "label", "直连（内置）"),
		jo("tag", "blocked", "label", "拦截（内置）"),
	}
	seen := map[string]bool{"direct": true, "blocked": true, "api": true}

	outbounds, err := d.store.XrayOutboundsByNode(ctxOf(c), node.ID, true)
	if err != nil {
		d.failServer(c, "查询出站", err)
		return nil, err
	}
	for _, ob := range outbounds {
		if seen[ob.Tag] {
			continue
		}
		seen[ob.Tag] = true
		label := ob.Protocol
		if ob.Tag == "warp" {
			label = "WARP"
		} else if ob.Remark != nil && *ob.Remark != "" {
			label = *ob.Remark
		}
		tags = append(tags, jo(
			"tag", ob.Tag,
			"label", label,
			"protocol", ob.Protocol,
			"relay", ob.RelayTargetNodeID != nil,
		))
	}

	// 订阅出站（服务器池）也是合法路由目标。
	subs, err := d.store.OutboundSubsByNode(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询出站订阅", err)
		return nil, err
	}
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		for _, tag := range activeSubscriptionTags(&sub) {
			if seen[tag] {
				continue
			}
			seen[tag] = true
			tags = append(tags, jo("tag", tag, "label", "订阅"))
		}
	}
	return tags, nil
}

// activeSubscriptionTags 对齐 XrayOutboundSubscription::activeTags()（从 fetched 里取 tag）。
func activeSubscriptionTags(sub *OutboundSubRecord) []string {
	if sub.Fetched == nil || strings.TrimSpace(*sub.Fetched) == "" {
		return nil
	}
	parsed, err := parseOrderedJS([]byte(*sub.Fetched))
	if err != nil {
		return nil
	}
	arr, ok := parsed.(jsArr)
	if !ok {
		return nil
	}
	out := []string{}
	for _, item := range arr {
		obj, isObj := item.(*jsObj)
		if !isObj {
			continue
		}
		tag := jsonGetString(obj, "tag")
		if tag == "" || tag == "direct" || tag == "blocked" || tag == "api" {
			continue
		}
		out = append(out, tag)
	}
	return out
}

// outboundTagStrings 对齐 outboundTagStrings()。
func (d *NodeDomain) outboundTagStrings(c *gin.Context, node *NodeRecord) ([]string, error) {
	tags, err := d.outboundTags(c, node)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(tags))
	for _, item := range tags {
		obj, _ := item.(*jsObj)
		out = append(out, jsonGetString(obj, "tag"))
	}
	return out, nil
}

// targetTags 对齐 targetTags()：出站 + 已启用 balancer。
func (d *NodeDomain) targetTags(c *gin.Context, node *NodeRecord) (jsArr, error) {
	tags, err := d.outboundTags(c, node)
	if err != nil {
		return nil, err
	}
	balancers, err := d.store.BalancersByNode(ctxOf(c), node.ID, true)
	if err != nil {
		d.failServer(c, "查询负载均衡器", err)
		return nil, err
	}
	seen := map[string]bool{}
	for _, item := range tags {
		obj, _ := item.(*jsObj)
		seen[jsonGetString(obj, "tag")] = true
	}
	for _, b := range balancers {
		if seen[b.Tag] {
			continue
		}
		seen[b.Tag] = true
		label := b.Strategy
		if b.Remark != nil && *b.Remark != "" {
			label = *b.Remark
		}
		tags = append(tags, jo("tag", b.Tag, "label", "负载均衡："+label))
	}
	return tags, nil
}

// inboundCandidates 对齐 inboundCandidates()：本节点启用入站的 {value,tag,port,protocol,label}。
//
// label = 「端口-tag」，入站有备注时再追加「-备注」（前端下拉直接展示该字段，
// 备注为空/未填则保持原样）。
func (d *NodeDomain) inboundCandidates(c *gin.Context, node *NodeRecord) (jsArr, error) {
	inbounds, err := d.store.XrayInboundsByNode(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询入站", err)
		return nil, err
	}
	out := jsArr{}
	for _, in := range inbounds {
		if !in.Enabled {
			continue
		}
		label := uInt64String(uint64(in.Port)) + "-" + in.Tag
		if in.Remark != nil {
			if remark := strings.TrimSpace(*in.Remark); remark != "" {
				label += "-" + remark
			}
		}
		out = append(out, jo(
			"value", in.Tag,
			"tag", in.Tag,
			"port", in.Port,
			"protocol", in.Protocol,
			"label", label,
		))
	}
	return out, nil
}

// ---- 渲染预览 ----

// renderRoutingRules 复刻 XrayConfigService::renderRouting() 的 rules 段（含内置规则与 UDP 护栏）。
//
// 为什么不自己重写一遍匹配逻辑：UDP 护栏、balancerTag 判定、空规则跳过等细节
// 在 P5 的 xray 包里已经有逐字节对拍过的实现，重写必然漂移。这里只负责把数据装配成
// 驱动层要的 Data，再取渲染结果的 routing.rules。
func (d *NodeDomain) renderRoutingRules(c *gin.Context, node *NodeRecord) (jsArr, error) {
	data, err := d.renderData(c, node)
	if err != nil {
		d.failServer(c, "装配渲染数据", err)
		return nil, err
	}
	// 走 RenderJSON → 再解析：驱动层的保序结构是 internal 包，本包无法直接引用；
	// JSON 往返语义无损（数字类型由 parseOrderedJS 按 PHP json_decode 口径还原）。
	body, err := xray.RenderJSON(data, d.cfg.AppKey)
	if err != nil {
		d.failServer(c, "渲染路由配置", err)
		return nil, err
	}
	rendered, err := parseOrderedJS(body)
	if err != nil {
		d.failServer(c, "解析渲染结果", err)
		return nil, err
	}
	rulesVal := jsonGet(rendered, "routing", "rules")
	if arr, ok := rulesVal.(jsArr); ok {
		return arr, nil
	}
	return jsArr{}, nil
}

// renderRoutingPreview 取渲染后的 rules（供「下发预览」展示）。
func (d *NodeDomain) renderRoutingPreview(c *gin.Context, node *NodeRecord) (jsArr, error) {
	return d.renderRoutingRules(c, node)
}

// cleanupOutboundForRule 对齐 cleanupOutboundForRule()：无其它规则引用时回收中转出站。
func (d *NodeDomain) cleanupOutboundForRule(c *gin.Context, node *NodeRecord, rule *RoutingRuleRecord) bool {
	ob, err := d.store.XrayOutboundByTag(ctxOf(c), node.ID, rule.OutboundTag)
	if err != nil {
		if d.noRowsToNotFoundSilent(err) {
			return true
		}
		d.failServer(c, "查询路由目标出站", err)
		return false
	}
	if ob == nil || ob.RelayTargetNodeID == nil {
		return true
	}

	// 还有别的规则引用它 → 保留出站与凭据。
	rules, err := d.store.RoutingRulesByNode(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询路由规则", err)
		return false
	}
	for _, other := range rules {
		if other.OutboundTag == ob.Tag {
			return true
		}
	}
	return d.deleteRelayOutbound(c, node, ob)
}

// udpGuardWarning 对齐 udpGuardWarning()：socks/http 出站命中 UDP 时提示护栏已兜底。
func (d *NodeDomain) udpGuardWarning(c *gin.Context, node *NodeRecord, rule *RoutingRuleRecord) (any, error) {
	if !rule.Enabled {
		return nil, nil
	}
	if rule.Network != nil && *rule.Network != "" && *rule.Network != "udp" {
		return nil, nil
	}
	outbounds, err := d.store.XrayOutboundsByNode(ctxOf(c), node.ID, true)
	if err != nil {
		d.failServer(c, "查询出站", err)
		return nil, err
	}
	for _, ob := range outbounds {
		if ob.Tag != rule.OutboundTag {
			continue
		}
		if ob.Protocol == "socks" || ob.Protocol == "http" {
			return "「" + rule.OutboundTag + "」是 socks/http 出站（xray 不支持 UDP）：该规则命中的 UDP 流量将自动回落直连，不会走此出站。", nil
		}
		return nil, nil
	}
	return nil, nil
}

// activeOutboundsSplitFrom sub.Fetched → prepend/append 出站（渲染数据装配用）。

// 枚举模板：target 候选不是字符串列表时返回 nil（对应 PHP 的 empty()）。
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// stringValues 取 JSON 数组里的字符串（非数组 → 空）。
func stringValues(v any) []string {
	arr, ok := v.(jsArr)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, isStr := item.(string); isStr {
			out = append(out, s)
		}
	}
	return out
}

// ---- 路由匹配模拟 ----

// matchResult 是 matchDomain/matchIP 的三态返回（PHP 的 true/false/null）。
type matchResult int

const (
	matchMiss matchResult = iota
	matchHit
	matchUnknown
)

// matchDomainPattern 对齐 matchDomain()。
func matchDomainPattern(pattern, domain string) matchResult {
	domain = strings.ToLower(domain)
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return matchMiss
	}

	switch {
	case strings.HasPrefix(pattern, "geosite:"):
		return matchUnknown // 需要内核 geosite 数据
	case strings.HasPrefix(pattern, "full:"):
		return boolResult(domain == strings.ToLower(pattern[5:]))
	case strings.HasPrefix(pattern, "domain:"):
		d := strings.ToLower(pattern[7:])
		return boolResult(domain == d || strings.HasSuffix(domain, "."+d))
	case strings.HasPrefix(pattern, "keyword:"):
		return boolResult(strings.Contains(domain, strings.ToLower(pattern[8:])))
	case strings.HasPrefix(pattern, "regexp:"):
		re, err := compilePHPLikeRegex(pattern[7:])
		if err != nil {
			return matchMiss
		}
		return boolResult(re.MatchString(domain))
	case strings.HasPrefix(pattern, "ext:"):
		return boolResult(strings.HasSuffix(domain, "."+strings.ToLower(pattern[4:])))
	}
	// 裸域名等价 domain:
	d := strings.ToLower(pattern)
	return boolResult(domain == d || strings.HasSuffix(domain, "."+d))
}

// compilePHPLikeRegex 复刻 `@preg_match('/' . str_replace('/', '\/', $p) . '/i', $domain)`。
func compilePHPLikeRegex(body string) (*regexp.Regexp, error) {
	escaped := strings.ReplaceAll(body, "/", `\/`)
	return regexp.Compile("(?i)" + escaped)
}

// matchIPPattern 对齐 matchIp()。
func matchIPPattern(pattern, ip string) matchResult {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return matchMiss
	}
	if strings.HasPrefix(pattern, "geoip:") {
		return matchUnknown // 需要内核 geoip 数据
	}
	if !strings.Contains(pattern, "/") {
		return boolResult(ip == pattern)
	}

	_, network, err := net.ParseCIDR(pattern)
	if err != nil {
		return matchMiss
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return matchMiss
	}
	return boolResult(network.Contains(parsed))
}

func boolResult(v bool) matchResult {
	if v {
		return matchHit
	}
	return matchMiss
}
