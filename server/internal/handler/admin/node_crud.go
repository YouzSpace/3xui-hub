package admin

// 节点 CRUD + test / upgrade-agent / probe-inbounds / 倍率。
// 逐条对齐 backend/app/Http/Controllers/Admin/NodeController.php。

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// siteConfig 键（与 PHP 侧同名字符串）。
const (
	siteKeyDefaultMultiplier = "default_node_multiplier"
	siteKeyShowMultiplier    = "show_node_multiplier"
	siteKeyXrayPullInterval  = "xray_pull_interval"
)

// nodeIndex 对齐 index()：默认 50/页，?all=1 才返回全量（禁止 per_page 绕过）。
func (d *NodeDomain) nodeIndex(c *gin.Context) {
	all := inputBool(c.Query("all"))
	page := queryInt(c, "page", 1)

	nodes, total, err := d.store.NodesPage(ctxOf(c), all, page, 50)
	if err != nil {
		d.failServer(c, "查询节点列表", err)
		return
	}

	// 倍率相关的站点配置只查一次（列表 50 条，逐条查会打出 50 次 SQL）。
	cfg, err := d.store.SiteConfigMany(ctxOf(c), []string{siteKeyDefaultMultiplier, siteKeyShowMultiplier, siteKeyXrayPullInterval})
	if err != nil {
		d.failServer(c, "读取节点配置", err)
		return
	}
	env := d.presentEnv(cfg)

	items := make([]any, 0, len(nodes))
	for i := range nodes {
		presented, ok := d.present(c, env, &nodes[i], false)
		if !ok {
			return
		}
		items = append(items, presented)
	}

	if all {
		d.success(c, jsArr(items))
		return
	}
	d.successPage(c, items, page, 50, total)
}

// nodeShow 对齐 show()：详情带 full 段（api_key / has_password 等）。
func (d *NodeDomain) nodeShow(c *gin.Context) {
	n, ok := d.loadNode(c)
	if !ok {
		return
	}
	env, ok := d.loadPresentEnv(c)
	if !ok {
		return
	}
	presented, ok := d.present(c, env, n, true)
	if !ok {
		return
	}
	d.success(c, presented)
}

// nodeStore 对齐 store()。
func (d *NodeDomain) nodeStore(c *gin.Context) {
	input := jsonInput(c)
	driverType := nodeDriverTypeOf(input)
	shouldEnable := true
	if v, ok := input["enabled"]; ok && v != nil {
		shouldEnable = inputBool(v)
	}

	if !d.validateNode(c, input, false) {
		return
	}

	inbounds := nodeInboundsOf(input)
	if driverType != "xray" && selectedInboundIDs(inbounds) == nil {
		d.fail(c, "请至少选择一个入站", 400)
		return
	}

	create := NodeCreateInput{
		Name:        nodeStringField(input, "name"),
		Host:        nodeStringField(input, "host"),
		Port:        443,
		Scheme:      "https",
		WebBasePath: "",
		Username:    "",
		VerifySSL:   inputBool(input["verify_ssl"]),
		DriverType:  driverType,
		Inbounds:    inbounds,
	}
	if v, ok := input["port"]; ok && v != nil {
		create.Port = inputInt(v)
	}
	if s, ok := inputString(input["scheme"]); ok && s != "" {
		create.Scheme = s
	}
	if s, ok := inputString(input["web_base_path"]); ok {
		create.WebBasePath = s
	}
	if s, ok := inputString(input["username"]); ok {
		create.Username = s
	}
	create.Password = ptrStringOrNil(input["password"])
	create.APIKey = ptrStringOrNil(input["api_key"])
	create.TrafficMultiplier = multiplierInput(input["traffic_multiplier"])

	if driverType == "xray" {
		create.NodeSecret = randomAlnum(32)
		// issue #88：host_v6 进 driver_config（nodes 表没这列），空串 = 未配置。
		create.HostV6 = nodeOptionalString(input, "host_v6")
		pull := int64(30)
		if raw, ok := d.store.SiteConfigMany(ctxOf(c), []string{siteKeyXrayPullInterval}); ok == nil {
			if v := raw[siteKeyXrayPullInterval]; v != "" {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					pull = n
				}
			}
		}
		create.PullInterval = &pull
	}

	node, err := d.store.CreateNode(ctxOf(c), create)
	if err != nil {
		d.failServer(c, "创建节点", err)
		return
	}

	env, ok := d.loadPresentEnv(c)
	if !ok {
		return
	}

	if node.IsXray() {
		// xray 节点：无 3x-ui 接入初始化流程；置 enabled 后等 agent 注册上线。
		enabled := shouldEnable
		if _, err := d.store.UpdateNode(ctxOf(c), node.ID, NodeUpdateInput{Enabled: &enabled}); err != nil {
			d.failServer(c, "启用节点", err)
			return
		}
		node, err = d.store.NodeByID(ctxOf(c), node.ID)
		if err != nil {
			d.failServer(c, "回读节点", err)
			return
		}
		presented, ok := d.present(c, env, node, true)
		if !ok {
			return
		}
		d.success(c, presented, "创建成功")
		return
	}

	// 3x-ui 节点：初始化异步化——按用户逐条派发，全部成功才由 maybeFinalize 启用节点。
	task, err := d.submitNodeInit(c, node, shouldEnable)
	if err != nil {
		d.failServer(c, "派发节点初始化任务", err)
		return
	}

	node, err = d.store.NodeByID(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	presented, ok := d.present(c, env, node, true)
	if !ok {
		return
	}
	message := "创建成功，接入初始化进行中（日志页可跟踪进度）"
	if obj, isObj := presented.(*jsObj); isObj {
		obj.put("init_task_id", task.ID)
		obj.put("init_status", task.Status)
		obj.put("message", message)
	}
	d.success(c, presented, message)
}

// nodeUpdate 对齐 update()。
func (d *NodeDomain) nodeUpdate(c *gin.Context) {
	node, ok := d.loadNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	if !d.validateNode(c, input, true) {
		return
	}

	driverType := node.DriverType
	if s, ok := inputString(input["driver_type"]); ok && s != "" {
		driverType = s
	}

	inbounds, inboundsPresent := nodeInboundsOfOK(input)
	if !node.IsXray() && inboundsPresent && selectedInboundIDs(inbounds) == nil {
		d.fail(c, "请至少选择一个入站", 400)
		return
	}

	in := NodeUpdateInput{
		DriverType:  ptrStringOf(driverType),
		Inbounds:    inbounds,
		InboundsSet: inboundsPresent,
	}
	in.Name = nodeOptionalString(input, "name")
	in.Host = nodeOptionalString(input, "host")
	in.Scheme = nodeOptionalString(input, "scheme")
	in.WebBasePath = nodeOptionalString(input, "web_base_path")
	in.Username = nodeOptionalString(input, "username")
	if v, present := input["password"]; present {
		in.PasswordSet = true
		in.Password = ptrStringOrNil(v)
	}
	if v, present := input["api_key"]; present {
		in.APIKeySet = true
		in.APIKey = ptrStringOrNil(v)
	}
	if v, present := input["port"]; present && v != nil {
		p := inputInt(v)
		in.Port = &p
	}
	if v, present := input["enabled"]; present {
		b := inputBool(v)
		in.Enabled = &b
	}
	if v, present := input["verify_ssl"]; present {
		b := inputBool(v)
		in.VerifySSL = &b
	}
	if v, present := input["traffic_multiplier"]; present {
		in.MultiplierSet = true
		in.TrafficMultiplier = multiplierInput(v)
	}
	// issue #88：host_v6 只对 xray 节点有意义（3x-ui 节点面板主动连节点，不涉及 v6 入口）。
	// 传空串/null = 清空该配置。
	if v, present := input["host_v6"]; present && driverType == "xray" {
		in.HostV6Set = true
		in.HostV6 = ptrStringOrNil(v)
	}

	// 初始化进行中：改连接信息/入站直接拒绝；只改 enabled 时转交任务（不落 nodes）。
	activeTask, err := d.store.ActiveNodeInitTask(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询节点初始化任务", err)
		return
	}
	if activeTask != nil {
		if d.changesInitConfiguration(node, input, inboundsPresent, inbounds) {
			d.fail(c, "节点初始化进行中，暂不能修改连接信息或入站", 400)
			return
		}
		if in.Enabled != nil {
			if err := d.store.UpdateTaskDesiredEnabled(ctxOf(c), activeTask.ID, node.ID, *in.Enabled); err != nil {
				d.failServer(c, "更新初始化任务目标状态", err)
				return
			}
			in.Enabled = nil
		}
	}

	updated, err := d.store.UpdateNode(ctxOf(c), node.ID, in)
	if err != nil {
		d.failServer(c, "更新节点", err)
		return
	}

	// xray：确保 node_secret 存在（首次转 xray 时补生成）。
	if updated.IsXray() && updated.NodeSecret == nil {
		secret := randomAlnum(32)
		if err := d.store.SetNodeSecret(ctxOf(c), updated.ID, secret); err != nil {
			d.failServer(c, "生成节点密钥", err)
			return
		}
	}

	// issue #88：host_v6 存 driver_config（行锁 + 键级合并，不整块回写）。
	// 空串/null 一并写入：mergeDriverConfig 里空串是有效值，读取侧按「没配」处理。
	if in.HostV6Set && updated.IsXray() {
		patch := map[string]any{"host_v6": ""}
		if in.HostV6 != nil {
			patch["host_v6"] = phpTrimSpaceOnly(*in.HostV6)
		}
		if err := d.store.MergeDriverConfig(ctxOf(c), updated.ID, patch); err != nil {
			d.failServer(c, "保存节点 IPv6 地址", err)
			return
		}
	}

	var syncTask *NodeTaskRecord
	if !updated.IsXray() && inboundsPresent {
		changed, err := d.store.ReplaceNodeInbounds(ctxOf(c), updated.ID, inbounds)
		if err != nil {
			d.failServer(c, "保存节点入站", err)
			return
		}
		if len(changed) > 0 {
			syncTask, err = d.submitInboundSync(c, updated, changed)
			if err != nil {
				d.failServer(c, "派发入站同步任务", err)
				return
			}
		}
	}

	env, ok := d.loadPresentEnv(c)
	if !ok {
		return
	}
	fresh, err := d.store.NodeByID(ctxOf(c), updated.ID)
	if err != nil {
		d.failServer(c, "回读节点", err)
		return
	}
	presented, ok := d.present(c, env, fresh, true)
	if !ok {
		return
	}

	if syncTask != nil {
		message := "已更新，用户正在后台同步到新入站（日志页可看进度）"
		if obj, isObj := presented.(*jsObj); isObj {
			obj.put("sync_task_id", syncTask.ID)
			obj.put("message", message)
		}
		d.success(c, presented, message)
		return
	}
	d.success(c, presented, "更新成功")
}

// nodeDestroy 对齐 destroy()：本地行立刻删，远端 client 清理交给后台任务。
func (d *NodeDomain) nodeDestroy(c *gin.Context) {
	node, ok := d.loadNode(c)
	if !ok {
		return
	}

	snapshot := jo(
		"id", node.ID,
		"name", node.Name,
		"host", node.Host,
		"port", node.Port,
		"scheme", node.Scheme,
		"web_base_path", node.WebBasePath,
		"username", node.Username,
		"password", nullableStr(node.PasswordEnc),
		"api_key", nullableStr(node.APIKeyEnc),
		"verify_ssl", node.VerifySSL,
		"driver_type", node.DriverType,
	)
	meta := jo("action", "cleanup_clients", "node_name", node.Name, "node_snapshot", snapshot)
	task, err := d.store.CreateTask(ctxOf(c), TaskCreateInput{
		Type:        "node_action",
		SubjectType: "node",
		SubjectID:   node.ID,
		Meta:        phpEncode(meta),
		MaxAttempts: 3,
		ItemKeys:    []string{"node:" + strconv.FormatUint(node.ID, 10)},
	})
	if err != nil {
		d.failServer(c, "派发节点清理任务", err)
		return
	}

	if err := d.store.DeleteNode(ctxOf(c), node.ID); err != nil {
		d.failServer(c, "删除节点", err)
		return
	}

	// 节点行已删，远端清理靠 meta 里的连接快照 —— 必须派发，否则任务永远 pending、
	// 节点上的 ch_user_* 会一直留在面板里。
	d.dispatchNodeTask(ctxOf(c), task.ID)

	message := "已删除，正在后台清理远端客户端（日志页可看进度）"
	d.success(c, jo("cleanup_task_id", task.ID, "message", message), message)
}

// nodeTest 对齐 test()：healthCheck 并更新节点状态/延迟。
func (d *NodeDomain) nodeTest(c *gin.Context) {
	node, ok := d.loadNode(c)
	if !ok {
		return
	}

	health, err := d.ops.HealthCheck(ctxOf(c), node)
	if err != nil {
		if uerr := d.store.UpdateNodeStatus(ctxOf(c), node.ID, "offline", 0, true); uerr != nil {
			d.logger.Printf("admin 更新节点状态失败: %v", uerr)
		}
		d.fail(c, "连接失败："+err.Error(), 500)
		return
	}

	status := "offline"
	if health.OK {
		status = "online"
	}
	// xray 节点的心跳时间由 agent 维护（/node-api/alive），手动测试不覆盖。
	touch := !node.IsXray()
	if err := d.store.UpdateNodeStatus(ctxOf(c), node.ID, status, health.LatencyMs, touch); err != nil {
		d.failServer(c, "更新节点状态", err)
		return
	}

	if !health.OK {
		msg := health.Error
		if msg == "" {
			msg = "unknown"
		}
		d.fail(c, "节点离线："+msg, 500)
		return
	}

	d.success(c, jo(
		"ok", true,
		"latency_ms", health.LatencyMs,
		"cpu", health.CPU,
		"mem", nullableFloat(health.Mem),
		"xray_state", nullableStr(health.XrayState),
	))
}

// upgradeAgent 对齐 upgradeAgent()：下发 agent 自升级指令（强制 force）。
func (d *NodeDomain) upgradeAgent(c *gin.Context) {
	node, ok := d.loadNode(c)
	if !ok {
		return
	}
	if !node.IsXray() {
		d.fail(c, "该节点不是 xray 节点（没有 agent）", 400)
		return
	}

	manifest, err := d.ops.Manifest(ctxOf(c))
	if err != nil {
		d.failServer(c, "读取 agent manifest", err)
		return
	}
	target := nodeUpgradeTarget(node, manifest)
	if target == nil {
		d.fail(c, "面板 manifest 里没有该架构的 agent 资产", 400)
		return
	}

	inFlight, err := d.store.HasInFlightUpgrade(ctxOf(c), node.ID)
	if err != nil {
		d.failServer(c, "查询在途升级指令", err)
		return
	}
	if inFlight {
		d.fail(c, "已有待执行的升级指令", 400)
		return
	}

	payload := jo(
		"file", target.File,
		"sha256", target.SHA256,
		"version", target.Version,
		"url", "/node-bin/agent/"+target.Arch,
	)
	if _, err := d.store.CreateCommand(ctxOf(c), node.ID, "upgrade", payload); err != nil {
		d.failServer(c, "入队升级指令", err)
		return
	}

	d.success(c, jo(
		"queued", true,
		"version", target.Version,
		"reported_version", nullableStr(d.reportedAgentVersion(node)),
	), "已下发升级指令（节点取走后自动下载校验替换并重启）")
}

// probeInbounds 对齐 probeInbounds()：用表单参数临时连 3x-ui 拉入站清单。
func (d *NodeDomain) probeInbounds(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("scheme", "sometimes", "in:http,https")
	v.field("host", "required", "string")
	v.field("port", "sometimes", "integer", "between:1,65535")
	v.field("web_base_path", "sometimes", "nullable", "string")
	v.field("api_key", "sometimes", "nullable", "string")
	v.field("username", "sometimes", "nullable", "string")
	v.field("password", "sometimes", "nullable", "string")
	v.field("verify_ssl", "sometimes", "boolean")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	conn := ProbeConn{
		Scheme:      "https",
		Host:        nodeStringField(input, "host"),
		Port:        443,
		WebBasePath: nodeStringField(input, "web_base_path"),
		APIKey:      ptrStringOrNil(input["api_key"]),
		Username:    nodeStringField(input, "username"),
		Password:    nodeStringField(input, "password"),
		VerifySSL:   inputBool(input["verify_ssl"]),
	}
	if s, ok := inputString(input["scheme"]); ok && s != "" {
		conn.Scheme = s
	}
	if val, ok := input["port"]; ok && val != nil {
		conn.Port = inputInt(val)
	}

	inbounds, err := d.ops.ProbeInbounds(ctxOf(c), conn)
	if err != nil {
		d.fail(c, "拉取入站失败："+err.Error(), 500)
		return
	}

	grouped := jo("vless", jsArr{}, "trojan", jsArr{})
	for _, in := range inbounds {
		proto := nodeScalarString(in["protocol"])
		if _, exists := grouped.get(proto); !exists {
			grouped.put(proto, jsArr{})
		}
		remark := nodeScalarString(in["remark"])
		if remark == "" {
			remark = nodeScalarString(in["tag"])
		}
		if remark == "" {
			remark = "inbound-" + nodeScalarString(in["id"])
		}
		port := nodeScalarString(in["port"])
		if port == "" {
			port = "?"
		}
		arr, _ := grouped.get(proto)
		grouped.put(proto, append(arr.(jsArr), jo(
			"id", jsonInt(in["id"]),
			"remark", remark,
			"label", remark+" ("+proto+" :"+port+")",
		)))
	}

	d.success(c, grouped)
}

// updateOriginalMultiplier 对齐 updateOriginalMultiplier()。
//
// 只改 site_configs 一个值，不回写 nodes.traffic_multiplier ——
// 否则「继承」会被固化成手动指定。
func (d *NodeDomain) updateOriginalMultiplier(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("multiplier", "required", "numeric", "min:1", "max:100")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	mult := phpNumberOf(input["multiplier"])
	value := strconv.FormatFloat(mult, 'f', -1, 64)
	if err := d.store.SiteConfigSet(ctxOf(c), siteKeyDefaultMultiplier, &value); err != nil {
		d.failServer(c, "保存原始倍率", err)
		return
	}

	d.success(c, jo("multiplier", phpFloat(mult)), "原始倍率已保存（仅影响继承中的节点）")
}

// updateMultiplierDisplay 对齐 updateMultiplierDisplay()。
//
// 关掉时删除该行而不是存 '0'：用户端靠「有没有这行」判断（默认关）。
func (d *NodeDomain) updateMultiplierDisplay(c *gin.Context) {
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("show_multiplier", "required", "boolean")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	on := inputBool(input["show_multiplier"])
	var err error
	if on {
		one := "1"
		err = d.store.SiteConfigSet(ctxOf(c), siteKeyShowMultiplier, &one)
	} else {
		err = d.store.SiteConfigSet(ctxOf(c), siteKeyShowMultiplier, nil)
	}
	if err != nil {
		d.failServer(c, "保存倍率展示开关", err)
		return
	}

	msg := "已关闭"
	if on {
		msg = "已开启"
	}
	d.success(c, jo("show_multiplier", on), msg)
}
