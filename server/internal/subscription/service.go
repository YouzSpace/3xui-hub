package subscription

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Service 是订阅生成器，逐条对齐 backend/app/Services/SubscriptionService.php。
//
// 生成链路：
//
//	ensureUsable → getLinks（本地节点 + 第三方）→ 按格式渲染
//
// 三格式渲染都在本包内手写（不引 yaml / 第三方 json），以保证与 PHP 逐字节一致。
type Service struct {
	repo Repository
	// Now 是时钟（PHP 侧取 time() / now()）。测试可注入固定时刻。
	Now func() time.Time
	// Concurrency 对齐 config/panel.php 的 subscription_concurrency（<=0 视为 1）。
	Concurrency int
	// Logf 是可注入的日志器（nil 时用标准库 log）。
	Logf func(format string, args ...any)
}

// logf 输出一条诊断日志：订阅少节点这类问题全靠它留痕。
func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// NewService 构造订阅生成器。
func NewService(repo Repository) *Service {
	return &Service{repo: repo, Now: time.Now, Concurrency: 10}
}

// siteConfigReader 是 site_configs 的读取视图（每次请求只读一次库）。
//
// PHP 侧每个 getValue() 都是一次 SELECT；这里一次性读全表再取键，语义等价
// （同一请求内配置不会中途变化），但把 N 次查询压成 1 次。
type siteConfigReader struct {
	values map[string]string
}

// get 等价于 SiteConfig::getValue($key, $default)。
func (s *siteConfigReader) get(key, def string) string {
	if s == nil || s.values == nil {
		return def
	}
	if v, ok := s.values[key]; ok {
		return v
	}
	return def
}

// Generate 生成订阅内容。校验失败返回 *Error（对应 SubscriptionException）。
//
// 对应 SubscriptionService::generate()。
func (s *Service) Generate(ctx context.Context, user *User, format Format) (string, error) {
	cfg, err := s.loadSiteConfigs(ctx)
	if err != nil {
		return "", err
	}

	plan, err := s.loadPlan(ctx, user)
	if err != nil {
		return "", err
	}

	if err := ensureUsable(user, s.now()); err != nil {
		return "", err
	}

	links, err := s.getLinks(ctx, user, plan, cfg)
	if err != nil {
		return "", err
	}

	if len(links) == 0 {
		// 全空时不静默返回：xray 节点最常见的原因是「已启用、在线，但没填连接地址」——
		// 链接由 host 拼出，host 为空则一条都产不出。给出可诊断的原因（404 + 说明）。
		blankHost, err := s.repo.HasEnabledOnlineXrayNodeWithoutHost(ctx)
		if err != nil {
			return "", err
		}
		if blankHost {
			return "", &Error{
				Message: "暂无可用的节点配置：节点尚未设置连接地址，请联系管理员在面板中补充",
				Code:    404,
			}
		}
		return "", nil
	}

	switch format {
	case FormatClash:
		return s.generateClash(user, plan, links, cfg), nil
	case FormatSingbox:
		return s.generateSingbox(user, plan, links, cfg), nil
	default:
		return s.generateBase64(user, plan, links, cfg), nil
	}
}

// EnsureUsable 单独暴露，供 handler 在生成前做一次快速校验（与 PHP 同一口径）。
func (s *Service) EnsureUsable(user *User) error { return ensureUsable(user, s.now()) }

// ensureUsable 对应 SubscriptionService::ensureUsable()。
func ensureUsable(user *User, now time.Time) error {
	if !user.Enabled {
		return &Error{Message: "账号已禁用", Code: 403}
	}
	if user.PlanID == nil || *user.PlanID == 0 {
		return &Error{Message: "无套餐，暂无流量", Code: 403}
	}
	if user.ExpiredAt != nil && user.ExpiredAt.Before(now) {
		return &Error{Message: "账号已过期", Code: 403}
	}
	if user.TrafficLimit > 0 && user.TrafficUsed >= user.TrafficLimit {
		return &Error{Message: "流量已耗尽", Code: 403}
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Service) loadSiteConfigs(ctx context.Context) (*siteConfigReader, error) {
	values, err := s.repo.SiteConfigs(ctx)
	if err != nil {
		return nil, err
	}
	return &siteConfigReader{values: values}, nil
}

func (s *Service) loadPlan(ctx context.Context, user *User) (*Plan, error) {
	if user.PlanID == nil || *user.PlanID == 0 {
		return nil, nil
	}
	return s.repo.PlanByID(ctx, *user.PlanID)
}

// getLinks 对应 SubscriptionService::getLinks()。
//
// 本地节点：套餐「不含本地」时跳过。xray 节点本地拼链接（零 HTTP）；
// 3x-ui 节点按「协议匹配入站」过滤端口后取链接（生产实现里 Bearer 节点整批并发）。
// 第三方链接只读本地缓存（后台定时拉取），失败不影响本地订阅。
func (s *Service) getLinks(ctx context.Context, user *User, plan *Plan, cfg *siteConfigReader) ([]string, error) {
	var links []string
	email := clientEmail(user.ID)

	if plan == nil || plan.IncludesLocal() {
		nodes, err := s.repo.SubscribableNodes(ctx, user.Protocol)
		if err != nil {
			return nil, err
		}

		var concurrent, sequential []nodeEntry

		for _, node := range nodes {
			if node.IsXray() {
				sequential = append(sequential, nodeEntry{node: node})
				continue
			}

			inboundIDs, err := s.repo.NodeInboundIDs(ctx, node.ID, user.Protocol)
			if err != nil {
				return nil, err
			}
			// 3x-ui 节点维持原逻辑：没有该协议入站就跳过
			if len(inboundIDs) == 0 {
				continue
			}
			entry := nodeEntry{node: node, inboundIDs: inboundIDs}
			if isBearerNode(node) {
				concurrent = append(concurrent, entry)
			} else {
				sequential = append(sequential, entry)
			}
		}

		// 并发组（Bearer 模式，无状态）：PHP 用 array_chunk(subscription_concurrency) 分批，
		// 每批内所有节点的两条请求先全部发出去再统一收结果。
		//
		// 这里用同一套「分批 + 批内并发 + 按批序收集」：并发只影响耗时，不影响顺序 ——
		// 结果按 batch 内原顺序落位，与 PHP 的 foreach ($batch as $entry) 一致。
		for _, batch := range chunkEntries(concurrent, s.concurrency()) {
			nodeLinks := s.fetchBatchConcurrently(ctx, batch, user, email, cfg)
			for _, perNode := range nodeLinks {
				links = appendUnique(links, perNode...)
			}
		}

		for _, entry := range sequential {
			nodeLinks, err := s.fetchNodeLinks(ctx, entry.node, entry.inboundIDs, user, email, cfg)
			if err != nil {
				return nil, err
			}
			links = appendUnique(links, nodeLinks...)
		}
	}

	// 第三方节点：只读本地缓存，不产生网络请求
	thirdParty, err := s.thirdPartyLinks(ctx, plan, cfg)
	if err != nil {
		// 第三方挂了不能毁掉本地订阅：PHP 侧 report() 后继续。
		thirdParty = nil
	}
	links = append(links, thirdParty...)

	return links, nil
}

// nodeEntry 是一个待取链的节点（xray 节点的 inboundIDs 为空）。
type nodeEntry struct {
	node       Node
	inboundIDs []int64
}

// concurrency 对应 concurrency()：config 缺省 10；<= 0 按 1 处理。
func (s *Service) concurrency() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return 1
}

// chunkEntries 对应 PHP 的 array_chunk($items, $size)。
func chunkEntries(items []nodeEntry, size int) [][]nodeEntry {
	if size <= 0 {
		size = 1
	}
	var out [][]nodeEntry
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		out = append(out, items[start:end])
	}
	return out
}

// fetchBatchConcurrently 对应 fetchNodeLinksConcurrently()：批内并发取链，按入参顺序返回。
//
// 单节点失败只丢该节点（PHP 侧 report 后 continue），不影响同批其它节点。
func (s *Service) fetchBatchConcurrently(ctx context.Context, batch []nodeEntry, user *User, email string, cfg *siteConfigReader) [][]string {
	results := make([][]string, len(batch))
	var wg sync.WaitGroup

	for i, entry := range batch {
		i, entry := i, entry
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodeLinks, err := s.fetchNodeLinks(ctx, entry.node, entry.inboundIDs, user, email, cfg)
			if err != nil {
				results[i] = nil // 与单节点失败同口径：该节点不产出链接
				return
			}
			results[i] = nodeLinks
		}()
	}
	wg.Wait()

	return results
}

// fetchNodeLinks 对应 fetchNodeLinks()（单节点）：单节点失败只记日志，不影响其它节点。
func (s *Service) fetchNodeLinks(ctx context.Context, node Node, inboundIDs []int64, user *User, email string, cfg *siteConfigReader) ([]string, error) {
	var raw []string

	if node.IsXray() {
		raw = xraySubscriptionURIs(node, user, ctx, s.repo)
	} else {
		panelLinks, err := s.repo.PanelLinks(ctx, node, email)
		if err != nil {
			// 取链接失败不是「该节点没链接」，必须留痕：否则用户看到订阅少一个节点却无从追查。
			s.logf("[sub] 节点 %s 取链接失败 user=%d email=%s: %v", node.Name, user.ID, email, err)
			return nil, nil
		}
		raw = filterNodeLinks(panelLinks)
		if len(raw) == 0 {
			// 节点在线、入站也配了，但该用户在 3x-ui 侧一个 client 都没有 —— 建号没跟上，
			// 这是「订阅里少节点」最常见的原因，留一条便于定位。
			s.logf("[sub] 节点 %s 无任何链接 user=%d email=%s（3x-ui 上可能缺 client）", node.Name, user.ID, email)
		}
	}

	return s.withMultiplierSuffix(node, raw, cfg), nil
}

// xraySubscriptionURIs 对应 XrayConfigService::subscriptionUris()（同步取入站，故不返回 error：
// 读库失败等同于该节点没有入站，PHP 侧异常同样被 catch 掉）。
func xraySubscriptionURIs(node Node, user *User, ctx context.Context, repo Repository) []string {
	host := phpTrim(node.Host)
	hostV6 := node.HostV6()
	// issue #88：只配了 host_v6、没配 host 的节点仍然可以有 v6 入口的链接，
	// 所以这里不再以 host 为空直接放弃，改为逐入站选地址。
	if user.UUID == "" {
		return nil
	}

	inbounds, err := repo.XrayInbounds(ctx, node.ID)
	if err != nil {
		return nil
	}

	uris := make([]string, 0, len(inbounds))
	for _, inbound := range inbounds {
		// 用户全协议通用：节点上启用的每个入站都出一条链接，不按 users.protocol 过滤。
		inboundHost, ok := subscriptionHostFor(host, hostV6, inbound.Listen)
		if !ok {
			// 两个地址都空 / v6 入站但只有 v4 地址：跳过，绝不产出 "host:port" 残缺链接。
			continue
		}
		uris = append(uris, xrayInboundURI(node, inbound, user, inboundHost))
	}
	// PHP 侧 XrayDriver::getClientLinks 返回后还要过一遍 sanitizeLinks（过滤空串、去重）。
	return sanitizeLinks(uris)
}

// isBearerNode 对应 canFetchConcurrently()：3x-ui 节点 + api_key 非空（Bearer 无状态）。
func isBearerNode(node Node) bool { return !node.IsXray() && node.APIKey != "" }

// clientEmail 对应 User::clientEmail()。
func clientEmail(userID int64) string { return "ch_user_" + itoa(userID) }

// appendUnique 按 PHP 的 in_array($x, $arr, true) 去重后追加。
func appendUnique(dst []string, items ...string) []string {
	for _, item := range items {
		dup := false
		for _, existing := range dst {
			if existing == item {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, item)
		}
	}
	return dst
}

// sanitizeLinks 对应 sanitizeLinks()：过滤空串、去重。
func sanitizeLinks(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, link := range raw {
		if link != "" {
			out = appendUnique(out, link)
		}
	}
	return out
}

// filterNodeLinks 过滤空串并去重。
//
// 曾经按「入站端口」二次过滤（对应 PHP 的 filterNodeLinks），但 3x-ui 的
// /panel/api/clients/links/<email> 返回的链接本来就只属于该 client 的入站，
// 而 hy2/hysteria2 链接的端口位置与 vless 不同，按端口匹配会把它们整条丢掉
// （线上实测：hy2 入站已有 105 个 client，订阅里却一条 hy2 链接都没有）。
func filterNodeLinks(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, link := range raw {
		if link == "" {
			continue
		}
		out = appendUnique(out, link)
	}
	return out
}

// PanelInbound 是 3x-ui listInbounds() 的最小投影（id + port）。
type PanelInbound struct {
	ID   int64
	Port int64
}

// linkMatchesAnyPort 对应 linkMatchesAnyPort()：取 @host:port 的端口比较。
func linkMatchesAnyPort(link string, ports []int64) bool {
	m := linkPortPattern.FindStringSubmatch(link)
	if m == nil {
		return false
	}
	port := parseIntPrefix(m[1])
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// multiplierSuffix 对应 SubscriptionService::multiplierSuffix()：`-1.25x` / `-2x`。
//
// 两位小数：nodes.traffic_multiplier 是 decimal(6,2)，计费值最多两位 ——
// 用 1 位会把 1.25 显示成 1.3x、0.85 显示成 0.9x，展示与计费分叉。
func multiplierSuffix(multiplier float64) string {
	s := numberFormat2(multiplier)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return "-" + s + "x"
}

// withMultiplierSuffix 对应 withMultiplierSuffix()：开关开着时在链接 #fragment 末尾追加倍率。
func (s *Service) withMultiplierSuffix(node Node, links []string, cfg *siteConfigReader) []string {
	flag := cfg.get("show_node_multiplier", "")
	if flag != "1" && flag != "true" && flag != "on" {
		return links
	}

	suffix := multiplierSuffix(s.effectiveMultiplier(node, cfg))

	out := make([]string, 0, len(links))
	for _, link := range links {
		if link == "" {
			out = append(out, link)
			continue
		}
		out = append(out, appendMultiplierToName(link, suffix))
	}
	return out
}

// effectiveMultiplier 对应 TrafficSyncService::multiplierFor()：
// 节点 traffic_multiplier 为 NULL → 继承 site_configs.default_node_multiplier（缺省 1.0）。
func (s *Service) effectiveMultiplier(node Node, cfg *siteConfigReader) float64 {
	if node.TrafficMultiplier != nil {
		return *node.TrafficMultiplier
	}
	stored := cfg.get("default_node_multiplier", "")
	if stored == "" {
		return 1.0
	}
	return mustParseFloat(stored)
}

// appendMultiplierToName 对应 appendMultiplierToName()：把后缀挂到 #fragment 末尾。
//
// 后缀追加在名称处理链的最末端：先摘掉旧后缀，名字（用户 ID 清理 / 正则改名 /
// 旗帜重建）全部处理完后再放回 —— 不管用户 ID 显不显示、旗帜开不开，倍率永远排最后。
func appendMultiplierToName(link, suffix string) string {
	hashPos := strings.LastIndex(link, "#")
	if hashPos < 0 {
		return link
	}

	name := urldecode(link[hashPos+1:])
	// 防重复追加：上次订阅已带后缀的名称先摘掉
	name = multiplierSuffixPattern.ReplaceAllString(name, "")

	if phpTrim(name) == "" {
		return link
	}

	return link[:hashPos] + "#" + urlencode(name+suffix)
}

// processNodeName 统一节点名称处理：用户 ID 清理 → 正则重命名 → 旗帜。
// 倍率后缀（-2.0x）先摘下来，全部处理完后再放回名称末尾。
func (s *Service) processNodeName(name string, regionCounters map[string]int, cfg *siteConfigReader) string {
	multSuffix := ""
	if loc := multiplierSuffixPattern.FindStringSubmatchIndex(name); loc != nil {
		multSuffix = name[loc[0]:loc[1]]
		name = name[:loc[0]] + name[loc[1]:]
	}

	if cfg.get("sub_show_userid", "0") != "1" {
		name = stripUserIDFromName(name)
	}

	name = s.renameNode(name, cfg)

	if cfg.get("sub_show_flag", "0") == "1" {
		name = addFlagToName(name, regionCounters)
	}

	return name + multSuffix
}

// stripUserIDFromName 对应 stripUserIdFromName()：清理 ch_user_N 与残留分隔符。
func stripUserIDFromName(name string) string {
	name = userIDPattern.ReplaceAllString(name, "")
	name = emptyBracketPattern.ReplaceAllString(name, "")
	name = multiSpacePattern.ReplaceAllString(name, " ")
	name = multiDashPattern.ReplaceAllString(name, "-")
	name = edgeSeparatorPattern.ReplaceAllString(name, "")
	return name
}

// renameNode 对应 renameNode()：高级开关（sub_rename_enabled）开着时才走正则改名。
func (s *Service) renameNode(name string, cfg *siteConfigReader) string {
	if cfg.get("sub_rename_enabled", "0") != "1" {
		return name
	}

	regex := cfg.get("sub_rename_regex", "")
	replacement := cfg.get("sub_rename_replacement", "")
	if regex == "" || replacement == "" {
		return name
	}

	pat, err := compilePHPRegex(regex)
	if err != nil {
		// 正则表达式错误，返回原名称
		return name
	}
	return phpPregReplaceAll(pat, replacement, name)
}

// addFlagToName 对应 addFlagToName()：识别地区 → 「旗帜 + 地区 + 两位编号」。
func addFlagToName(name string, regionCounters map[string]int) string {
	// 检查名称是否已经有旗帜，如果有则移除后再处理
	name = stripFlagEmoji(name)

	code := regionCode(name)
	if code == "" {
		return name
	}

	regionCounters[code]++
	number := fmt.Sprintf("%02d", regionCounters[code])

	return regionFlag(code) + " " + code + number
}

// applyNameToLink 对应 applyNameToLink()：只改 #fragment，链接其余部分原样保留。
func (s *Service) applyNameToLink(link string, regionCounters map[string]int, cfg *siteConfigReader) string {
	hashPos := strings.LastIndex(link, "#")
	if hashPos < 0 {
		return link
	}
	rawName := urldecode(link[hashPos+1:])
	newName := s.processNodeName(rawName, regionCounters, cfg)
	return link[:hashPos] + "#" + urlencode(newName)
}

// generateBase64 对应 generateBase64()：信息条目在前，节点链接在后，\n 连接后整体 base64。
func (s *Service) generateBase64(user *User, plan *Plan, links []string, cfg *siteConfigReader) string {
	regionCounters := map[string]int{}
	outLinks := make([]string, 0, len(links)+3)

	outLinks = append(outLinks, s.createInfoLinks(user, plan, cfg)...)

	for _, link := range links {
		outLinks = append(outLinks, s.applyNameToLink(link, regionCounters, cfg))
	}

	return b64encode([]byte(strings.Join(outLinks, "\n")))
}

// customInfoLines 对应 customInfoLines()：自定义显示文本行（空行忽略）。
func customInfoLines(cfg *siteConfigReader) []string {
	if cfg.get("sub_custom_info_enabled", "0") != "1" {
		return nil
	}
	text := cfg.get("sub_custom_info_text", "")
	lines := lineSplitPattern.Split(text, -1)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = phpTrim(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// infoState 是三个信息条目开关的公共判定（三种格式共用同一口径）。
type infoState struct {
	showExpire  bool
	showTraffic bool
	showReset   bool
	customLines []string
}

func newInfoState(user *User, plan *Plan, cfg *siteConfigReader) infoState {
	return infoState{
		showExpire:  cfg.get("sub_show_expire", "0") == "1",
		showTraffic: cfg.get("sub_show_traffic", "0") == "1",
		// 只有周期套餐且月数 > 1 才显示重置时间
		showReset:   plan.IsPeriod() && plan.Months > 1,
		customLines: customInfoLines(cfg),
	}
}

// expireText 返回「到期」条目里的日期文本：无到期时间显示「永久」。
func expireText(user *User) string {
	if user.ExpiredAt == nil {
		return "永久"
	}
	ts := user.ExpiredAt.Unix()
	if ts <= 0 {
		return "永久"
	}
	return phpDate(ts)
}

// resetDaysText 返回「重置: N天后」里的 N。
func resetDaysText(user *User, now time.Time) string {
	if user.NextTrafficResetAt == nil {
		return "0"
	}
	ts := user.NextTrafficResetAt.Unix()
	if ts <= 0 {
		return "0"
	}
	return ceilDays(ts, now.Unix())
}

// createInfoLinks 对应 createInfoLinks()：Base64 格式的信息条目（ss:// URI）。
func (s *Service) createInfoLinks(user *User, plan *Plan, cfg *siteConfigReader) []string {
	st := newInfoState(user, plan, cfg)
	userInfo := b64encode([]byte("aes-256-gcm:info"))

	links := make([]string, 0, 3+len(st.customLines))
	appendLink := func(name string) { links = append(links, "ss://"+userInfo+"@127.0.0.1:1#"+urlencode(name)) }

	if st.showExpire {
		appendLink("到期: " + expireText(user))
	}
	if st.showTraffic && plan.IsPeriod() {
		appendLink("当月: " + formatBytes(user.MonthlyTrafficUsed) + " / " + formatBytes(user.MonthlyTrafficLim))
	}
	if st.showReset {
		appendLink("重置: " + resetDaysText(user, s.now()) + "天后")
	}
	for _, line := range st.customLines {
		appendLink(line)
	}
	return links
}

// createInfoProxies 对应 createInfoProxies()：Clash 格式的信息节点（ss 代理，udp=false）。
func (s *Service) createInfoProxies(user *User, plan *Plan, cfg *siteConfigReader) []phpMap {
	st := newInfoState(user, plan, cfg)

	proxies := make([]phpMap, 0, 3+len(st.customLines))
	appendProxy := func(name string) {
		proxies = append(proxies, obj(
			"name", name,
			"type", "ss",
			"server", "127.0.0.1",
			"port", jsonNumber("1"),
			"cipher", "aes-256-gcm",
			"password", "info",
			"udp", false,
		))
	}

	if st.showExpire {
		appendProxy("到期: " + expireText(user))
	}
	if st.showTraffic && plan.IsPeriod() {
		appendProxy("当月: " + formatBytes(user.MonthlyTrafficUsed) + " / " + formatBytes(user.MonthlyTrafficLim))
	}
	if st.showReset {
		appendProxy("重置: " + resetDaysText(user, s.now()) + "天后")
	}
	for _, line := range st.customLines {
		appendProxy(line)
	}
	return proxies
}

// createInfoOutbounds 对应 createInfoOutbounds()：Sing-box 格式的信息出站。
func (s *Service) createInfoOutbounds(user *User, plan *Plan, cfg *siteConfigReader) []phpMap {
	st := newInfoState(user, plan, cfg)

	outbounds := make([]phpMap, 0, 3+len(st.customLines))
	appendOutbound := func(tag string) {
		outbounds = append(outbounds, obj(
			"type", "shadowsocks",
			"tag", tag,
			"server", "127.0.0.1",
			"server_port", jsonNumber("1"),
			"method", "aes-256-gcm",
			"password", "info",
		))
	}

	if st.showExpire {
		appendOutbound("到期: " + expireText(user))
	}
	if st.showTraffic && plan.IsPeriod() {
		appendOutbound("当月: " + formatBytes(user.MonthlyTrafficUsed) + " / " + formatBytes(user.MonthlyTrafficLim))
	}
	if st.showReset {
		appendOutbound("重置: " + resetDaysText(user, s.now()) + "天后")
	}
	for _, line := range st.customLines {
		appendOutbound(line)
	}
	return outbounds
}

// cleanSni 对应 cleanSni()：剥掉 `host:443` 形式的端口（纯 IPv6 多冒号不动）。
func cleanSni(sni string) string {
	last := strings.LastIndex(sni, ":")
	if last < 0 {
		return sni
	}
	if strings.Index(sni, ":") != last || last == 0 {
		return sni
	}
	if !isAllDigitsASCII(sni[last+1:]) {
		return sni
	}
	return sni[:last]
}
