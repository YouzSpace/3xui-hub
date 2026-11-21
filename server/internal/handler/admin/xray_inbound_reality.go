package admin

// xray 节点入站的 REALITY 安全层：密钥对生成、目标扫描、serverNames 守卫与
// realitySettings 落库形状。从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"strconv"
	"strings"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

// realityKeypair 对齐 realityKeypair()：生成一组 Reality 密钥材料（不落库）。
func (d *NodeDomain) realityKeypair(c *gin.Context) {
	if _, ok := d.loadXrayNode(c); !ok {
		return
	}

	keypair, err := xray.GenerateKeypair()
	if err != nil {
		d.failServer(c, "生成 Reality 密钥", err)
		return
	}
	shortID, err := xray.ShortID(4)
	if err != nil {
		d.failServer(c, "生成 shortId", err)
		return
	}

	d.success(c, jo(
		"private_key", keypair[0],
		"public_key", keypair[1],
		"short_id", shortID,
		"dest", xray.DefaultDest,
		"server_name", xray.DefaultSNI,
	))
}

// realityScan 对齐 realityScan()：探测在节点侧跑（复用 node_api_commands 指令通道）。
func (d *NodeDomain) realityScan(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("targets", "nullable", "array", "max:20")
	v.field("targets.*", "nullable", "string", "max:255")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	rawTargets := []string{}
	if arr, ok := input["targets"].([]any); ok {
		for _, item := range arr {
			rawTargets = append(rawTargets, nodeScalarString(item))
		}
	}
	targets := xray.SanitizeTargets(rawTargets)
	if len(targets) == 0 {
		targets = xray.DefaultTargets()
	}

	payload := jo("targets", stringsToJSArr(targets))
	command, err := d.store.CreateCommand(ctxOf(c), node.ID, "reality_scan", payload)
	if err != nil {
		d.failServer(c, "入队 reality 扫描", err)
		return
	}

	wait := d.cfg.RealityScanWait
	deadline := d.now().Add(timeDurationSeconds(wait))
	for d.now().Before(deadline) {
		fresh, err := d.store.CommandByID(ctxOf(c), command.ID)
		if err != nil {
			d.failServer(c, "回读指令状态", err)
			return
		}
		switch fresh.Status {
		case "failed":
			output := ""
			if fresh.Result != nil {
				output = *fresh.Result
			}
			if strings.Contains(output, "unknown action") {
				output = "该节点的 agent 版本过旧，请先在「节点」页升级 agent 后重试"
			} else if output == "" {
				output = "节点执行失败"
			}
			d.success(c, jo("results", jsArr{}, "error", output))
			return
		case "success":
			raw := ""
			if fresh.Result != nil {
				raw = *fresh.Result
			}
			parsed, err := parseOrderedJS([]byte(raw))
			if err != nil {
				d.success(c, jo("results", jsArr{}, "error", "节点回执解析失败"))
				return
			}
			if arr, ok := parsed.(jsArr); ok {
				d.success(c, jo("results", arr))
				return
			}
			// 回执不是数组：PHP 的 is_array(json_decode(..., true)) 对对象也为真，
			// 此时原样塞进 results（前端按「不是数组」显示失败），这里保持同口径。
			if obj, ok := parsed.(*jsObj); ok {
				d.success(c, jo("results", obj))
				return
			}
			d.success(c, jo("results", jsArr{}, "error", "节点回执解析失败"))
			return
		}
		d.sleep(d.pollInterval)
	}

	d.success(c, jo(
		"pending", true,
		"message", "节点未在 "+strconv.Itoa(wait)+" 秒内回执（可能离线），稍后可重试",
	))
}

// ---- 内部 ----

// guardServerNames 对齐 guardServerNames()：REALITY serverNames 与 dest 的静态一致性拦截。
func (d *NodeDomain) guardServerNames(c *gin.Context, node *NodeRecord, input map[string]any, existing *XrayInboundRecord) bool {
	var prevStream any
	if existing != nil {
		prevStream, _ = parseOrderedJS(existing.StreamSettings)
	}
	security := nodeStringField(input, "security")
	if security == "" {
		security = jsonGetString(prevStream, "security")
	}
	if security != "reality" {
		return true
	}

	dest := nodeStringField(input, "dest")
	if dest == "" {
		dest = jsonGetString(prevStream, "realitySettings", "dest")
	}
	host := xray.HostOf(dest)
	if host == "" {
		return true
	}

	names := usableServerNames(stringListInput(input["server_names"], nil))
	suspect, evidence := xray.ServerNameSuspects(dest, names)

	if len(suspect) == 0 || inputBool(input["confirm_server_names"]) {
		return true
	}

	if evidence == xray.CertEvidenceSameSite {
		// 证书取不到 → 退回旧的注册域猜测，文案说明依据是「猜的」。
		d.abort422(c, "这些 serverNames 与 dest（"+host+"）不是同一站点的域名："+
			strings.Join(suspect, "、")+
			"。REALITY 会按客户端 SNI 向 dest 取证书，dest 给不出证书的域名会让被分到的用户全部连不上。"+
			"未能取到 dest 的真实证书，以上按注册域推测；若确认这些域名由 dest 的同一张证书覆盖（CDN 多 SAN），请确认后再保存。")
		return false
	}

	d.abort422(c, "这些 serverNames 不在 dest（"+host+"）出示的证书里："+
		strings.Join(suspect, "、")+
		"。REALITY 会按客户端 SNI 向 dest 取证书，dest 给不出证书的域名会让被分到的用户全部连不上。"+
		"若确认这些域名由 dest 的同一张证书覆盖（CDN 多 SAN），请确认后再保存。")
	return false
}

// realitySettings 对齐 realitySettings()：核心四字段 + 3x-ui 高级字段（显式提交才落库）。
func (d *NodeDomain) realitySettings(input map[string]any, publicKey string) *jsObj {
	dest := nodeStringField(input, "dest")
	if dest == "" {
		dest = xray.DefaultDest
	}
	serverNames := usableServerNames(stringListInput(input["server_names"], nil))
	shortIDs := stringListInput(input["short_ids"], nil)
	if len(shortIDs) == 0 {
		if sid, err := xray.ShortID(4); err == nil {
			shortIDs = []string{sid}
		}
	}

	settings := jo(
		"dest", dest,
		"serverNames", stringsToJSArr(serverNames),
		"shortIds", stringsToJSArr(shortIDs),
		"publicKey", publicKey,
	)

	if v, present := input["xver"]; present && inputInt(v) != 0 {
		settings.Put("xver", inputInt(v))
	}
	if v, present := input["show"]; present && inputBool(v) {
		settings.Put("show", true)
	}
	if v := strings.TrimSpace(nodeStringField(input, "min_client_ver")); v != "" {
		settings.Put("minClientVer", v)
	}
	if v := strings.TrimSpace(nodeStringField(input, "max_client_ver")); v != "" {
		settings.Put("maxClientVer", v)
	}
	if v, present := input["max_timediff"]; present && inputInt(v) != 0 {
		settings.Put("maxTimediff", inputInt(v))
	}
	if v := strings.TrimSpace(nodeStringField(input, "mldsa65_seed")); v != "" {
		settings.Put("mldsa65Seed", v)
	}
	if v := strings.TrimSpace(nodeStringField(input, "master_key_log")); v != "" {
		settings.Put("masterKeyLog", v)
	}
	if v, ok := limitFallback(input["limit_fallback_upload"]); ok {
		settings.Put("limitFallbackUpload", v)
	}
	if v, ok := limitFallback(input["limit_fallback_download"]); ok {
		settings.Put("limitFallbackDownload", v)
	}

	// 客户端段（订阅链接：fp / spx / pqv）
	client := jo()
	if fp := strings.TrimSpace(nodeStringField(input, "fingerprint")); fp != "" {
		client.Put("fingerprint", fp)
	}
	if spx, present := input["spider_x"]; present && nodeScalarString(spx) != "" {
		client.Put("spiderX", nodeScalarString(spx))
	}
	if pqv := strings.TrimSpace(nodeStringField(input, "mldsa65_verify")); pqv != "" {
		client.Put("mldsa65Verify", pqv)
	}
	if len(client.Keys()) > 0 {
		settings.Put("settings", client)
	}
	return settings
}

// limitFallback 对齐 limitFallback()：三字段全 0（或未提交）→ 不落库。
func limitFallback(raw any) (*jsObj, bool) {
	block, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	after := inputInt(block["after_bytes"])
	rate := inputInt(block["bytes_per_sec"])
	burst := inputInt(block["burst_bytes_per_sec"])
	if after == 0 && rate == 0 && burst == 0 {
		return nil, false
	}
	return jo("afterBytes", after, "bytesPerSec", rate, "burstBytesPerSec", burst), true
}

// presentLimitFallback 对齐 presentLimitFallback()：无则三键全 0（供表单绑定）。
func presentLimitFallback(raw any) *jsObj {
	block := jsonObject(raw)
	return jo(
		"after_bytes", jsonInt(valueOrNil(block, "afterBytes")),
		"bytes_per_sec", jsonInt(valueOrNil(block, "bytesPerSec")),
		"burst_bytes_per_sec", jsonInt(valueOrNil(block, "burstBytesPerSec")),
	)
}
