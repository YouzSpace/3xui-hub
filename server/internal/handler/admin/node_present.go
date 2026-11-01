package admin

// 节点展示层：present() / presentMetrics() / presentInbounds() 与节点表单校验。
// 逐条对齐 NodeController 的同名私有方法（字段顺序即 PHP 数组顺序）。

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

// presentEnv 是一次列表/详情请求里共享的站点配置（避免逐行查库）。
type presentEnv struct {
	site        map[string]string
	originalMul phpFloat
	showMul     bool
	agent       *ManifestAgent
}

// loadPresentEnv 读 present 需要的站点配置与 manifest。
func (d *NodeDomain) loadPresentEnv(c *gin.Context) (presentEnv, bool) {
	cfg, err := d.store.SiteConfigMany(ctxOf(c), []string{siteKeyDefaultMultiplier, siteKeyShowMultiplier})
	if err != nil {
		d.failServer(c, "读取节点配置", err)
		return presentEnv{}, false
	}
	return d.presentEnv(cfg), true
}

// presentEnv 由已读出的 site_configs 构造展示环境。
func (d *NodeDomain) presentEnv(cfg map[string]string) presentEnv {
	env := presentEnv{site: cfg}

	// originalMultiplier：配置行不存在/空串时兜底 1.0（TrafficSyncService::originalMultiplier）。
	env.originalMul = phpFloat(1)
	if stored, ok := cfg[siteKeyDefaultMultiplier]; ok && stored != "" {
		env.originalMul = phpFloat(parseFloatSafe(stored))
	}
	// show_node_multiplier：只认 '1' / 'true' / 'on'。
	switch cfg[siteKeyShowMultiplier] {
	case "1", "true", "on":
		env.showMul = true
	}
	return env
}

// present 对齐 present()：列表（full=false）与详情（full=true）共用。
//
// 返回 (*jsObj, ok)；ok=false 表示已经写过错误响应。
func (d *NodeDomain) present(c *gin.Context, env presentEnv, n *NodeRecord, full bool) (any, bool) {
	data := jo(
		"id", n.ID,
		"name", n.Name,
		"host", n.Host,
		"port", n.Port,
		"scheme", n.Scheme,
		"web_base_path", n.WebBasePath,
		"username", n.Username,
		"enabled", n.Enabled,
		"verify_ssl", n.VerifySSL,
		"status", n.Status,
		"latency", n.Latency,
		"last_check_at", nullableISO(n.LastCheckAt),
		"region", nullableStr(nodeRegionCode(n.Name)),
		"metrics", d.presentMetrics(n),
		"driver_type", nodeDriverTypeOfRecord(n),
		"inbounds", presentNodeInbounds(n),
		// null = 该节点继承原始倍率（前端要靠这个区分「继承」和「手动设成 1.0」）
		"traffic_multiplier", nodeFloatPtr(n.TrafficMultiplier),
		"original_multiplier", env.originalMul,
		"show_multiplier", env.showMul,
	)

	if full {
		data.put("api_key", nullableStr(n.APIKey))
		data.put("has_api_key", n.APIKey != nil)
		data.put("has_password", n.Password != nil)
	}

	if n.IsXray() {
		data.put("reality_registered", presentRealityRegistered(n))
		if n.NodeSecret != nil {
			secret := []rune(*n.NodeSecret)
			prefix := string(secret)
			if len(secret) > 4 {
				prefix = string(secret[:4])
			}
			data.put("node_secret_prefix", prefix+"…")
		} else {
			data.put("node_secret_prefix", nil)
		}
		data.put("xray_version", nodeDriverConfigString(n, "xray_version"))
		// issue #88：IPv6 入口地址（编辑表单回显；null = 未配置）
		data.put("host_v6", nodeDriverConfigHostV6(n))
		data.put("config_version", n.ConfigVersion)
		data.put("lag_seconds", nodeDriverConfigInt(n, "lag_seconds"))
		data.put("agent", nodeDriverConfigAgent(n))
		// issue #94 R5：BBR 运行态（来自心跳，原样回传）。
		// 未上报 → null：不编造默认值，前端据此显示「未上报」。
		data.put("bbr", nodeDriverConfigBBR(n))
		data.put("ws_connected", nodeDriverConfigBool(n, "ws", "connected"))
		data.put("install_command", d.installCommand(n))

		agent := env.agent
		if agent == nil {
			m, err := d.ops.Manifest(ctxOf(c))
			if err != nil {
				d.failServer(c, "读取 agent manifest", err)
				return nil, false
			}
			agent = m
		}
		env.agent = agent
		data.put("agent_version", nullableStr(d.reportedAgentVersion(n)))
		data.put("agent_latest", nullableStr(manifestLatestVersion(agent)))
		// issue #94：端口跳跃要 agent ≥ 1.0.7，前端据此把按钮禁用并提示「需升级 agent」。
		// 与 node_hop 的入队门槛同一个判定，避免 UI 允许点、后端才拒。
		data.put("hop_supported", agentSupportsHop(n))
		data.put("hop_min_agent_version", minHopAgentVersion)
		inFlight, err := d.store.HasInFlightUpgrade(ctxOf(c), n.ID)
		if err != nil {
			d.failServer(c, "查询在途升级指令", err)
			return nil, false
		}
		data.put("agent_upgrade_pending", inFlight)
	}

	return data, true
}

// presentMetrics 对齐 presentMetrics()：两种驱动统一成一个形状，一个数据都没有时返回 null。
func (d *NodeDomain) presentMetrics(n *NodeRecord) any {
	if n.IsXray() {
		agentVal := nodeDriverConfig(n, "agent")
		if _, isObj := agentVal.(*jsObj); !isObj {
			return nil
		}
		agent := jsonObject(agentVal)
		num := func(key string) any { return phpNumericOrNil(agent.get(key)) }
		intOrNull := func(key string) any { return phpIntOrNil(agent.get(key)) }
		at, atOK := agent.get("at")
		return jo(
			"cpu", num("cpu"),
			"mem", num("mem"),
			"conns", intOrNull("conns"),
			"active_users", intOrNull("active_users"),
			"up_speed", num("up_speed"),
			"down_speed", num("down_speed"),
			"at", nonNilValue(at, atOK),
		)
	}

	cpu := nullableNumericString(n.CPU)
	mem := nullableNumericString(n.Mem)
	if cpu == nil && mem == nil {
		return nil
	}
	return jo(
		"cpu", cpu,
		"mem", mem,
		"conns", nil,
		"active_users", nil,
		"up_speed", nil,
		"down_speed", nil,
		"at", nullableISO(n.LastCheckAt),
	)
}

// presentRealityRegistered 对齐 present 的 reality_registered 判定：
// 存在启用中的 vless 入站且 security=reality。
func presentRealityRegistered(n *NodeRecord) bool {
	for i := range n.XrayInbounds {
		ib := &n.XrayInbounds[i]
		if ib.Protocol != "vless" || !ib.Enabled {
			continue
		}
		stream, err := parseOrderedJS(ib.StreamSettings)
		if err != nil {
			continue
		}
		if jsonGetString(stream, "security") == "reality" {
			return true
		}
	}
	return false
}

// presentNodeInbounds 对齐 presentInbounds()：统一成 {协议: [id...]}，只算启用中的入站。
//
// 顺序：PHP 的 groupBy 保留「首次出现」的协议顺序（不是固定协议清单），
// 这里同样按入站出现顺序建键，避免多协议节点输出顺序与 PHP 不一致。
func presentNodeInbounds(n *NodeRecord) *jsObj {
	grouped := jo()
	if n.IsXray() {
		for _, ib := range n.XrayInbounds {
			if !ib.Enabled {
				continue
			}
			nodeAppendInboundID(grouped, ib.Protocol, ib.ID)
		}
		return grouped
	}
	for _, ib := range n.Inbounds {
		nodeAppendInboundID(grouped, ib.Protocol, ib.InboundID)
	}
	return grouped
}

// nodeAppendInboundID 往「协议 → id 数组」里追加（首见时建键）。
func nodeAppendInboundID(grouped *jsObj, protocol string, id any) {
	existing, ok := grouped.get(protocol)
	if !ok {
		grouped.put(protocol, jsArr{id})
		return
	}
	grouped.put(protocol, append(existing.(jsArr), id))
}

// ---- 展示用小工具 ----

func nodeDriverTypeOfRecord(n *NodeRecord) string {
	if n.DriverType == "" {
		return "3x-ui"
	}
	return n.DriverType
}

// nodeDriverConfig 解析 driver_config 并取嵌套字段（坏 JSON → nil）。
func nodeDriverConfig(n *NodeRecord, path ...string) any {
	if len(n.DriverConfigRaw) == 0 {
		return nil
	}
	cfg, err := parseOrderedJS(n.DriverConfigRaw)
	if err != nil {
		return nil
	}
	return jsonGet(cfg, path...)
}

func nodeDriverConfigString(n *NodeRecord, path ...string) any {
	v := nodeDriverConfig(n, path...)
	if v == nil {
		return nil
	}
	return nodeScalarString(v)
}

func nodeDriverConfigInt(n *NodeRecord, path ...string) any {
	v := nodeDriverConfig(n, path...)
	if v == nil {
		return 0
	}
	return jsonInt(v)
}

func nodeDriverConfigBool(n *NodeRecord, path ...string) bool {
	return nodeTruthy(nodeDriverConfig(n, path...))
}

// nodeDriverConfigHostV6 取 driver_config.host_v6：空/缺失都输出 null（前端占位符即「未配置」）。
func nodeDriverConfigHostV6(n *NodeRecord) any {
	v := nodeDriverConfig(n, "host_v6")
	if v == nil {
		return nil
	}
	s := phpTrimSpaceOnly(nodeScalarString(v))
	if s == "" {
		return nil
	}
	return s
}

func nodeDriverConfigAgent(n *NodeRecord) any {
	v := nodeDriverConfig(n, "agent")
	if _, isObj := v.(*jsObj); isObj {
		return v
	}
	return nil
}

// nodeDriverConfigBBR 取 driver_config.bbr 原样回传（issue #94 R5）。
//
// 缺键 / 不是对象 → null（前端显示「未上报」）。不补默认值：把缺失的
// tcp_congestion_control 补成 "cubic"、bbr_module 补成 false 都是在编造节点状态。
func nodeDriverConfigBBR(n *NodeRecord) any {
	v := nodeDriverConfig(n, "bbr")
	if _, isObj := v.(*jsObj); isObj {
		return v
	}
	return nil
}

// nonNilValue 把 (any, bool) 的第二个返回值抹平成 PHP 的 `?? null`。
func nonNilValue(v any, ok bool) any {
	if !ok {
		return nil
	}
	return v
}

// nullableFloat 输出 *float64（nil → null）。
func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return phpFloat(*v)
}

// phpNumericOrNil 对应 NodeController::numOrNull（is_numeric → (float)，否则 null）。
func phpNumericOrNil(v any, ok bool) any {
	if !ok || !phpNumericPasses(v) {
		return nil
	}
	return phpFloat(phpNumberOf(v))
}

// nullableNumericString 对应 numOrNull 的可空字符串入参（decimal 列）。
func nullableNumericString(v *string) any {
	if v == nil {
		return nil
	}
	return phpNumericOrNil(*v, true)
}

// phpIntOrNil 对应 NodeController::intOrNull。
func phpIntOrNil(v any, ok bool) any {
	if !ok || !phpNumericPasses(v) {
		return nil
	}
	return int64(phpNumberOf(v))
}

// reportedAgentVersion 对齐 AgentUpgradeService::reportedVersion。
func (d *NodeDomain) reportedAgentVersion(n *NodeRecord) *string {
	v := nodeDriverConfigString(n, "agent_version")
	if v == nil {
		return nil
	}
	s := nodeScalarString(v)
	if s == "" {
		return nil
	}
	return &s
}

// manifestLatestVersion 对齐 AgentUpgradeService::latestVersion。
func manifestLatestVersion(m *ManifestAgent) *string {
	if m == nil || m.Version == "" {
		return nil
	}
	v := m.Version
	return &v
}

// upgradeTarget 是「该节点要升级到哪份资产」。
type upgradeTarget struct {
	Arch    string
	File    string
	SHA256  string
	Version string
}

// nodeUpgradeTarget 对齐 AgentUpgradeService::targetFor（DEFAULT_ARCH = linux-64）。
func nodeUpgradeTarget(n *NodeRecord, m *ManifestAgent) *upgradeTarget {
	if m == nil || len(m.Assets) == 0 {
		return nil
	}
	arch := "linux-64"
	if v := nodeScalarString(nodeDriverConfig(n, "kernel_arch")); v != "" {
		arch = v
	}
	for _, a := range m.Assets {
		if a.Arch != arch || a.File == "" || a.SHA256 == "" {
			continue
		}
		return &upgradeTarget{Arch: arch, File: a.File, SHA256: a.SHA256, Version: m.Version}
	}
	return nil
}

// installCommand 对齐 Node::installCommand()：拼面板自己的地址（GitHub 零依赖）。
func (d *NodeDomain) installCommand(n *NodeRecord) string {
	base := strings.TrimRight(d.cfg.PanelURL, "/")
	name := phpRawURLEncode(n.Name)
	secret := ""
	if n.NodeSecret != nil {
		secret = *n.NodeSecret
	}
	return "curl -fsSL " + base + "/node-install.sh -o /tmp/controlhub-node-install.sh && " +
		"bash /tmp/controlhub-node-install.sh --hub " + base +
		" --node " + name + " --secret " + secret + d.installHubV6Flag()
}

// installHubV6Flag 返回安装命令里的 --hub-v6 段（issue #88）。
//
// 只在面板配了 APP_URL_V6 时才拼出来：没配就不出现这个参数，老部署拿到的命令逐字节不变；
// 配了才有 agent 的 v6 兜底通道（hub_url 优先、hub_url_v6 兜底）。
func (d *NodeDomain) installHubV6Flag() string {
	v6 := strings.TrimRight(strings.TrimSpace(d.cfg.PanelURLV6), "/")
	if v6 == "" {
		return ""
	}
	return " --hub-v6 " + v6
}

// ---- 节点表单校验 ----
// validateNode 对齐 validateNode()：xray 节点的 host/username/api_key 非必填。
func (d *NodeDomain) validateNode(c *gin.Context, input map[string]any, forUpdate bool) bool {
	isXray := nodeDriverTypeOf(input) == "xray"

	hostRule := "sometimes"
	if !isXray && !forUpdate {
		hostRule = "required"
	}
	nameRule := "required"
	if forUpdate {
		nameRule = "sometimes"
	}

	v := newNodeValidator(ctxOf(c), input)
	v.field("driver_type", "sometimes", "in:3x-ui,xray")
	v.field("name", nameRule, "string", "max:120")
	v.field("host", hostRule, "string", "max:200")
	// issue #88：IPv6 入口地址（可选）。裸地址或域名，长度上限与 host 一致。
	v.field("host_v6", "sometimes", "nullable", "string", "max:200")
	v.field("port", "sometimes", "integer", "between:1,65535")
	v.field("scheme", "sometimes", "in:http,https")
	v.field("web_base_path", "sometimes", "nullable", "string", "max:200")
	v.field("username", "sometimes", "nullable", "string")
	v.field("password", "sometimes", "nullable", "string")
	v.field("api_key", "sometimes", "nullable", "string")
	v.field("enabled", "sometimes", "boolean")
	v.field("verify_ssl", "sometimes", "boolean")
	v.field("inbounds", "sometimes", "array")
	// 下限取 1：倍率 < 1 时 floor(delta*m) 在小增量下恒为 0，用户流量永远不涨。
	v.field("traffic_multiplier", "sometimes", "nullable", "numeric", "min:1", "max:100")
	v.field("inbounds.vless", "sometimes", "nullable", "array")
	v.field("inbounds.vless.*", "integer")
	v.field("inbounds.trojan", "sometimes", "nullable", "array")
	v.field("inbounds.trojan.*", "integer")

	if !v.passes() {
		d.failNodeValidation(c, v)
		return false
	}
	// issue #88：host_v6 只能是 IPv6 字面量或域名。填 IPv4 会被当成「v6 入口地址」，
	// 于是绑 v6 的入站产出的订阅链接指向 v4 —— 客户端按 v6 入口却连不上，且很难看出来。
	if val, present := input["host_v6"]; present && val != nil {
		s := strings.Trim(strings.TrimSpace(nodeScalarString(val)), "[]")
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			d.fail(c, "host_v6 要填节点的 IPv6 地址或只解析 AAAA 的域名，不能填 IPv4", 500)
			return false
		}
	}

	return true
}

// changesInitConfiguration 对齐 changesInitConfiguration()：连接信息或入站是否变化。
func (d *NodeDomain) changesInitConfiguration(n *NodeRecord, input map[string]any, inboundsPresent bool, inbounds map[string][]int64) bool {
	for _, field := range []string{"host", "port", "scheme", "web_base_path", "username", "password", "api_key", "verify_ssl"} {
		val, present := input[field]
		if !present {
			continue
		}
		var current any
		switch field {
		case "host":
			current = n.Host
		case "port":
			current = n.Port
		case "scheme":
			current = n.Scheme
		case "web_base_path":
			current = n.WebBasePath
		case "username":
			current = n.Username
		case "password":
			current = nullableStr(n.Password)
		case "api_key":
			current = nullableStr(n.APIKey)
		case "verify_ssl":
			current = n.VerifySSL
		}
		if !phpLooseEqual(val, current) {
			return true
		}
	}

	if !inboundsPresent {
		return false
	}
	// 遍历「节点上实际有的协议 ∪ 表单里提交的协议」，不能写死 vless/trojan：
	// 否则只改 hy2 等新协议入站时判不出配置变化，保存被当成「无改动」。
	protos := map[string]bool{}
	for _, ni := range n.Inbounds {
		protos[ni.Protocol] = true
	}
	for proto := range inbounds {
		protos[proto] = true
	}
	for proto := range protos {
		if !int64SetsEqual(nodeInboundIDs(n, proto), inbounds[proto]) {
			return true
		}
	}
	return false
}

// phpLooseEqual 复刻 PHP 的 `!=` 松散比较（用于 changesInitConfiguration）。
func phpLooseEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	aNumeric := phpNumericPasses(a)
	bNumeric := phpNumericPasses(b)
	if aNumeric && bNumeric {
		return phpNumberOf(a) == phpNumberOf(b)
	}
	if aNumeric != bNumeric {
		// 一边数值一边非数值：PHP 8 里 "abc" != 1 → true（不等）
		// 但 "" == 0、null == false 之类的空值比较仍相等。
		if isEmptyPHPValue(a) || isEmptyPHPValue(b) {
			return phpNumberOf(valueOrZero(a)) == phpNumberOf(valueOrZero(b))
		}
		return false
	}
	if aBool, ok := a.(bool); ok {
		if bBool, ok := b.(bool); ok {
			return aBool == bBool
		}
		return aBool == phpTruthyValue(b)
	}
	if bBool, ok := b.(bool); ok {
		return phpTruthyValue(a) == bBool
	}
	return nodeScalarString(a) == nodeScalarString(b)
}

func valueOrZero(v any) any {
	if v == nil {
		return int64(0)
	}
	return v
}

func isEmptyPHPValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == "" || t == "0"
	case bool:
		return !t
	}
	return false
}

func phpTruthyValue(v any) bool {
	return nodeTruthy(v) || phpNumericPasses(v) && phpNumberOf(v) != 0
}

// nodeInboundIDs 取节点某协议已挂的入站（升序）。
func nodeInboundIDs(n *NodeRecord, proto string) []int64 {
	out := []int64{}
	for _, ib := range n.Inbounds {
		if ib.Protocol == proto {
			out = append(out, ib.InboundID)
		}
	}
	sortInt64s(out)
	return out
}

// sortInt64s 升序排序（PHP sort() 对整数数组）。
func sortInt64s(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// int64SetsEqual 比较两个已排序序列是否相同（PHP 的 `$a !== $b`）。
func int64SetsEqual(a, b []int64) bool {
	aa := append([]int64{}, a...)
	bb := append([]int64{}, b...)
	sortInt64s(aa)
	sortInt64s(bb)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// ---- 表单取值 ----

// nodeDriverTypeOf 取表单里的 driver_type（缺省 '3x-ui'）。
func nodeDriverTypeOf(input map[string]any) string {
	if s, ok := inputString(input["driver_type"]); ok && s != "" {
		return s
	}
	return "3x-ui"
}

// nodeStringField 取字符串字段（缺失 → 空串）。
func nodeStringField(input map[string]any, key string) string {
	if s, ok := inputString(input[key]); ok {
		return s
	}
	return ""
}

// nodeOptionalString 取「请求里出现过才更新」的字符串字段。
func nodeOptionalString(input map[string]any, key string) *string {
	val, present := input[key]
	if !present {
		return nil
	}
	return ptrStringOrNil(val)
}

// multiplierInput 把 traffic_multiplier 规范成 decimal(6,2) 字符串；null → nil（继承）。
func multiplierInput(v any) *string {
	if v == nil {
		return nil
	}
	s, ok := decimalString(v, 2)
	if !ok {
		return nil
	}
	// PHP 写的是 (float)$data['traffic_multiplier']：(float)"1" → 1.0 → MySQL 存 1.00。
	// decimalString 已给出两位小数，与 MySQL decimal(6,2) 的落库结果一致。
	return &s
}

// nodeInboundsOf 取表单里的入站（不区分是否出现过）。
func nodeInboundsOf(input map[string]any) map[string][]int64 {
	m, _ := nodeInboundsOfOK(input)
	if m == nil {
		m = map[string][]int64{}
	}
	return m
}

// nodeInboundsOfOK 取表单入站并返回「是否出现过 inbounds 键」。
func nodeInboundsOfOK(input map[string]any) (map[string][]int64, bool) {
	raw, present := input["inbounds"]
	if !present || raw == nil {
		return map[string][]int64{}, false
	}
	obj, isObj := raw.(map[string]any)
	if !isObj {
		return map[string][]int64{}, true
	}
	out := map[string][]int64{}
	// 前端节点表单的入站分组已经是三列（vless / trojan / hysteria），3x-ui 还可能有
	// vmess / shadowsocks。这里以前只认 vless、trojan，等于把用户勾上的 hysteria 入站
	// 直接丢掉 —— 表现是「入站选好了，保存后再看用户数还是 0」，因为 node_inbounds
	// 里没有这条记录，建号时按 users.protocol 查不到入站就整台跳过。
	for proto, val := range obj {
		if !isKnownInboundProtocol(proto) || val == nil {
			continue
		}
		out[proto] = inboundIDList(val)
	}
	return out, true
}

// isKnownInboundProtocol 判断入站分组名是不是我们认识的协议（防止把表单里的杂键落库）。
func isKnownInboundProtocol(proto string) bool {
	switch proto {
	case "vless", "vmess", "trojan", "shadowsocks", "hysteria", "socks", "http", "wireguard", "tunnel":
		return true
	}
	return false
}

// inboundIDList 复刻 syncInbounds 的兼容口径：单值也接受（旧数据/前端过渡）。
func inboundIDList(v any) []int64 {
	switch t := v.(type) {
	case []any:
		out := make([]int64, 0, len(t))
		for _, item := range t {
			out = append(out, inputInt(item))
		}
		return out
	case nil:
		return []int64{}
	default:
		return []int64{inputInt(t)}
	}
}

// selectedInboundIDs 对齐 selectedInboundIds()：两协议合并；全空返回 nil（= PHP 的 []）。
func selectedInboundIDs(inbounds map[string][]int64) []int64 {
	out := []int64{}
	out = append(out, inbounds["vless"]...)
	out = append(out, inbounds["trojan"]...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- 任务派发 ----

// dispatchNodeTask 把刚建好的 async_task 派进队列（对齐 PHP 的 dispatchAfterCommit）。
//
// 不派发的后果是任务永远停在 pending：item 不会执行、日志页看着在「处理中」、节点也永远
// 不会被 FinalizeInitTask 启用。派发失败只记日志并保留任务行 —— 管理员可在日志页点重试
// （重试接口走 task.Service.Retry，会重新派发）。
//
// 为什么在后台 goroutine 里派发：Service.Dispatch 会把 pending item 逐批 Enqueue，
// 而队列的 channel 只有 256 缓冲 —— node_init 的 item 是「全部用户」（批 2），
// 用户上千时同步派发会把 HTTP 请求线程卡在 Enqueue 上直到 worker 腾出空位，
// 前端等到超时。任务行已经落库，派发只是通知 worker，异步化不影响正确性
// （真正没派出去的，日志页重试能补）。
func (d *NodeDomain) dispatchNodeTask(ctx context.Context, taskID uint64) {
	if taskID == 0 || d.tasks == nil {
		return
	}
	tasks := d.tasks
	logger := d.logger
	// 不能挂在请求 ctx 上：请求返回即取消，派发会半路断掉。
	bg := context.WithoutCancel(ctx)
	go func() {
		if err := tasks.Dispatch(bg, taskID); err != nil {
			logger.Printf("admin 派发节点任务失败 task=%d: %v", taskID, err)
		}
	}()
}

// submitNodeInit 对齐 NodeInitService::submit：按用户逐条建 item。
func (d *NodeDomain) submitNodeInit(c *gin.Context, n *NodeRecord, shouldEnable bool) (*NodeTaskRecord, error) {
	userIDs, err := d.store.AllUserIDs(ctxOf(c))
	if err != nil {
		return nil, err
	}
	itemKeys := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		itemKeys = append(itemKeys, "user:"+strconv.FormatUint(id, 10))
	}
	meta := phpEncode(jo("should_enable", shouldEnable, "node_name", n.Name))

	task, err := d.store.CreateTask(ctxOf(c), TaskCreateInput{
		Type:        "node_init",
		SubjectType: "node",
		SubjectID:   n.ID,
		Meta:        meta,
		MaxAttempts: 3,
		ItemKeys:    itemKeys,
	})
	if err != nil {
		return nil, err
	}

	if len(userIDs) == 0 {
		// 无用户：直接判终态（succeeded），enable 交给 FinalizeInitTask。
		task, err = d.store.CompleteEmptyTask(ctxOf(c), task.ID)
		if err != nil {
			return nil, err
		}
		if err := d.store.FinalizeInitTask(ctxOf(c), n.ID, task.ID); err != nil {
			return nil, err
		}
		return task, nil
	}

	// 有用户：必须派发执行体（NodeInitUserJob），否则任务永远停在 pending、节点永远不会启用。
	d.dispatchNodeTask(ctxOf(c), task.ID)
	return task, nil
}

// submitInboundSync 对齐 NodeInboundSyncService::submit：只取「变化协议下、有套餐」的用户。
func (d *NodeDomain) submitInboundSync(c *gin.Context, n *NodeRecord, changed map[string][]int64) (*NodeTaskRecord, error) {
	protocols := make([]string, 0, len(changed))
	for proto := range changed {
		protocols = append(protocols, proto)
	}
	userIDs, err := d.store.UserIDsWithPlanByProtocols(ctxOf(c), protocols)
	if err != nil {
		return nil, err
	}
	if len(userIDs) == 0 {
		return nil, nil
	}

	changedJSON := jo()
	// 遍历 changed 的全部协议键（不能写死 vless/trojan：hy2 等新入站会被丢掉，
	// 保存节点后同步任务里没有它，那条入站就永远 0 客户端）。
	protos := make([]string, 0, len(changed))
	for proto := range changed {
		protos = append(protos, proto)
	}
	sort.Strings(protos)
	for _, proto := range protos {
		arr := jsArr{}
		for _, id := range changed[proto] {
			arr = append(arr, id)
		}
		changedJSON.put(proto, arr)
	}
	meta := phpEncode(jo(
		"action", "sync_inbounds",
		"node_name", n.Name,
		"inbounds", changedJSON,
	))
	itemKeys := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		itemKeys = append(itemKeys, "user:"+strconv.FormatUint(id, 10))
	}

	task, err := d.store.CreateTask(ctxOf(c), TaskCreateInput{
		Type:        "node_action",
		SubjectType: "node",
		SubjectID:   n.ID,
		Meta:        meta,
		MaxAttempts: 3,
		ItemKeys:    itemKeys,
	})
	if err != nil {
		return nil, err
	}
	// 只派扫描（不是逐 item 派发）：请求内零面板请求，扫描时一次清单拿全量。
	d.dispatchInboundSync(ctxOf(c), task.ID, n.ID)
	return task, nil
}

// dispatchInboundSync 派发「入站同步扫描」：先拉一次清单做 diff，只有缺挂载的才 attach。
// 不派发 = 用户永远不会出现在新入站上（保存节点时只落了一条 pending 任务）。
func (d *NodeDomain) dispatchInboundSync(ctx context.Context, taskID, nodeID uint64) {
	if taskID == 0 || d.tasks == nil {
		return
	}
	if err := d.tasks.DispatchInboundScan(ctx, taskID, nodeID); err != nil {
		d.logger.Printf("admin 派发入站同步扫描失败 task=%d node=%d: %v", taskID, nodeID, err)
	}
}

// 编译期断言：本地引用的 xray 常量与包内实现保持一致（防止将来包重构时静默漂移）。
var _ = xray.HeartbeatFreshSeconds
var _ = context.Background
