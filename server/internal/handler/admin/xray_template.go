package admin

// 管理端配置模板域（/admin-api/xray-templates…）—— 地区功能-19。
//
//	GET    /admin-api/xray-templates             三类模板一次返回
//	POST   /admin-api/xray-templates/apply       套用到一批节点 {node_ids, overwrite_custom}
//	POST   /admin-api/xray-templates/:kind        新增一条（kind=inbound/outbound/routing）
//	PUT    /admin-api/xray-templates/:kind/:id    修改（整行替换）
//	DELETE /admin-api/xray-templates/:kind/:id    删除
//
// 只对原生 xray 节点生效：3x-ui 节点的入站/出站/路由在面板侧，没有对应表，
// 套用时整台跳过并在结果里说明原因，不改它的任何数据。
//
// 静态段 /apply 必须排在 /:kind 之前（gin 的路由树同位置静态段优先，
// 与 /nodes/third-party 排在 /nodes/:node 之前是同一原因）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// 模板字段长度上限（与建表列宽对齐，超长直接 422，不靠 MySQL 截断）。
const (
	xrayTemplateNameMaxRunes     = 64
	xrayTemplateTagMaxRunes      = 64
	xrayTemplateProtocolMaxRunes = 32
	xrayTemplateRemarkMaxRunes   = 128
	xrayTemplateListenMaxRunes   = 64
	xrayTemplateNodeMaxPerApply  = 200
)

// XrayTemplateDeps 是配置模板域的依赖清单。
type XrayTemplateDeps struct {
	DB *sql.DB
	// AppKey 用于加密套用时新生成的 reality 私钥，必须与节点域同一个。
	AppKey []byte
	Logger loggerLike
	Now    func() time.Time
}

// XrayTemplateDomain 是管理端配置模板域的 HTTP 入口。
type XrayTemplateDomain struct {
	store  *xrayTemplateStore
	logger loggerLike
}

// NewXrayTemplateDomain 组装配置模板域。
//
// 不校验 DB 非空：路由装配（newRouter）在部分用例里不连库（只测中间件/静态资源），
// 那时传进来的 DB 是 nil —— 与地区/余额域一致，只有真正查库时才报错。
func NewXrayTemplateDomain(d XrayTemplateDeps) *XrayTemplateDomain {
	store := NewXrayTemplateSQLStore(d.DB, d.AppKey)
	if d.Now != nil {
		store.now = d.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &XrayTemplateDomain{store: store, logger: logger}
}

// Register 把配置模板域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
func (d *XrayTemplateDomain) Register(r gin.IRouter) {
	r.GET("/xray-templates", d.templateIndex)
	// 静态段必须排在 :kind 之前（apply 不能被当成模板种类）。
	r.POST("/xray-templates/apply", d.templateApply)
	r.POST("/xray-templates/:kind", d.templateStore)
	r.PUT("/xray-templates/:kind/:id", d.templateUpdate)
	r.DELETE("/xray-templates/:kind/:id", d.templateDestroy)
}

// ---- 响应封装（与地区/余额域同口径：HTTP 200 + PHP json_encode 字节）----

func (d *XrayTemplateDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *XrayTemplateDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *XrayTemplateDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

func (d *XrayTemplateDomain) notFound(c *gin.Context) { d.fail(c, "接口不存在", 404) }

// ---- 列表 ----

// templateIndex 对齐 GET /admin-api/xray-templates：三类模板一次给全。
func (d *XrayTemplateDomain) templateIndex(c *gin.Context) {
	set, err := d.store.TemplateSet(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询配置模板", err)
		return
	}
	d.success(c, presentTemplateSet(set))
}

// ---- 新增 / 修改 / 删除 ----

// templateStore 对齐 POST /admin-api/xray-templates/:kind。
func (d *XrayTemplateDomain) templateStore(c *gin.Context) {
	kind, ok := templateKindOf(c.Param("kind"))
	if !ok {
		d.notFound(c)
		return
	}
	input := jsonInput(c)
	switch kind {
	case xrayTemplateKindInbound:
		in, ok := d.inboundInput(c, input)
		if !ok {
			return
		}
		rec, err := d.store.CreateTemplateInbound(ctxOf(c), in)
		if err != nil {
			d.failServer(c, "创建入站模板", err)
			return
		}
		d.success(c, presentTemplateInbound(rec), "创建成功")
	case xrayTemplateKindOutbound:
		in, ok := d.outboundInput(c, input)
		if !ok {
			return
		}
		rec, err := d.store.CreateTemplateOutbound(ctxOf(c), in)
		if err != nil {
			d.failServer(c, "创建出站模板", err)
			return
		}
		d.success(c, presentTemplateOutbound(rec), "创建成功")
	default:
		in, ok := d.routingInput(c, input)
		if !ok {
			return
		}
		rec, err := d.store.CreateTemplateRouting(ctxOf(c), in)
		if err != nil {
			d.failServer(c, "创建路由模板", err)
			return
		}
		d.success(c, presentTemplateRouting(rec), "创建成功")
	}
}

// templateUpdate 对齐 PUT /admin-api/xray-templates/:kind/:id（整行替换）。
func (d *XrayTemplateDomain) templateUpdate(c *gin.Context) {
	kind, ok := templateKindOf(c.Param("kind"))
	if !ok {
		d.notFound(c)
		return
	}
	id, ok := parseUint(c.Param("id"))
	if !ok {
		d.notFound(c)
		return
	}
	input := jsonInput(c)
	switch kind {
	case xrayTemplateKindInbound:
		in, ok := d.inboundInput(c, input)
		if !ok {
			return
		}
		rec, err := d.store.UpdateTemplateInbound(ctxOf(c), id, in)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				d.notFound(c)
				return
			}
			d.failServer(c, "修改入站模板", err)
			return
		}
		d.success(c, presentTemplateInbound(rec), "保存成功")
	case xrayTemplateKindOutbound:
		in, ok := d.outboundInput(c, input)
		if !ok {
			return
		}
		rec, err := d.store.UpdateTemplateOutbound(ctxOf(c), id, in)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				d.notFound(c)
				return
			}
			d.failServer(c, "修改出站模板", err)
			return
		}
		d.success(c, presentTemplateOutbound(rec), "保存成功")
	default:
		in, ok := d.routingInput(c, input)
		if !ok {
			return
		}
		rec, err := d.store.UpdateTemplateRouting(ctxOf(c), id, in)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				d.notFound(c)
				return
			}
			d.failServer(c, "修改路由模板", err)
			return
		}
		d.success(c, presentTemplateRouting(rec), "保存成功")
	}
}

// templateDestroy 对齐 DELETE /admin-api/xray-templates/:kind/:id。
// 删除模板不影响已经套用过的节点（套用是拷贝，不是引用）。
func (d *XrayTemplateDomain) templateDestroy(c *gin.Context) {
	kind, ok := templateKindOf(c.Param("kind"))
	if !ok {
		d.notFound(c)
		return
	}
	id, ok := parseUint(c.Param("id"))
	if !ok {
		d.notFound(c)
		return
	}
	var err error
	switch kind {
	case xrayTemplateKindInbound:
		err = d.store.DeleteTemplateInbound(ctxOf(c), id)
	case xrayTemplateKindOutbound:
		err = d.store.DeleteTemplateOutbound(ctxOf(c), id)
	default:
		err = d.store.DeleteTemplateRouting(ctxOf(c), id)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			d.notFound(c)
			return
		}
		d.failServer(c, "删除模板", err)
		return
	}
	d.success(c, nil, "删除成功")
}

// ---- 套用 ----

// templateApply 对齐 POST /admin-api/xray-templates/apply。
//
// 每台节点一个事务、彼此独立：一台失败（或跳过）不影响其它节点。
// 返回 results（逐节点结果）+ 三个计数，前端直接展示。
func (d *XrayTemplateDomain) templateApply(c *gin.Context) {
	input := jsonInput(c)

	rawIDs, _ := input["node_ids"].([]any)
	if len(rawIDs) == 0 {
		d.fail(c, "请选择要套用的节点", 422)
		return
	}
	if len(rawIDs) > xrayTemplateNodeMaxPerApply {
		d.fail(c, "一次最多套用 "+strconv.Itoa(xrayTemplateNodeMaxPerApply)+" 台节点", 422)
		return
	}
	nodeIDs := make([]uint64, 0, len(rawIDs))
	seen := map[uint64]bool{}
	for _, raw := range rawIDs {
		id := inputInt(raw)
		if id <= 0 || seen[uint64(id)] {
			continue
		}
		seen[uint64(id)] = true
		nodeIDs = append(nodeIDs, uint64(id))
	}
	if len(nodeIDs) == 0 {
		d.fail(c, "节点 id 不合法", 422)
		return
	}
	overwrite := inputBool(input["overwrite_custom"])

	set, err := d.store.TemplateSet(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询配置模板", err)
		return
	}
	// 空模板套用等于把节点配置清空，必须拦住（不是「没什么可做」那么轻）。
	if set.IsEmpty() {
		d.fail(c, "还没有任何模板内容，未做套用", 422)
		return
	}

	results := jsArr{}
	var applied, skipped, failed int
	for _, id := range nodeIDs {
		res, err := d.store.ApplyToNode(ctxOf(c), id, set, overwrite)
		if err != nil {
			d.logger.Printf("admin 套用配置模板到节点 %d 失败: %v", id, err)
			c.Set(httpx.OpLogErrorKey, "套用配置模板: "+err.Error())
			res.Status = xrayTemplateApplyFailed
			res.Message = "套用失败: " + err.Error()
		}
		switch res.Status {
		case xrayTemplateApplyApplied:
			applied++
		case xrayTemplateApplyFailed:
			failed++
		default:
			skipped++
		}
		results = append(results, presentTemplateApplyResult(res))
	}
	d.success(c, jo(
		"results", results,
		"applied", applied,
		"skipped", skipped,
		"failed", failed,
	))
}

// ---- 输入整理 / 校验 ----

// templateKindOf 把 URL 段翻成模板种类（只认三个单数形式）。
func templateKindOf(raw string) (string, bool) {
	switch raw {
	case xrayTemplateKindInbound, xrayTemplateKindOutbound, xrayTemplateKindRouting:
		return raw, true
	}
	return "", false
}

// inboundInput 整理并校验一条入站模板的入参。
func (d *XrayTemplateDomain) inboundInput(c *gin.Context, input map[string]any) (XrayTemplateInboundInput, bool) {
	var in XrayTemplateInboundInput

	name, ok := templateTextInput(input["name"], xrayTemplateNameMaxRunes)
	if !ok {
		d.fail(c, "模板名称必填，且不超过 64 个字符", 422)
		return in, false
	}
	tag, ok := templateTextInput(input["tag"], xrayTemplateTagMaxRunes)
	if !ok {
		d.fail(c, "入站 tag 必填，且不超过 64 个字符", 422)
		return in, false
	}
	protocol, ok := templateTextInput(input["protocol"], xrayTemplateProtocolMaxRunes)
	if !ok {
		d.fail(c, "协议必填", 422)
		return in, false
	}
	port := inputInt(input["port"])
	if port < 1 || port > 65535 {
		d.fail(c, "端口必须在 1-65535 之间", 422)
		return in, false
	}
	listen := "0.0.0.0"
	if v, exists := input["listen"]; exists && v != nil {
		listen, ok = templateTextInput(v, xrayTemplateListenMaxRunes)
		if !ok {
			d.fail(c, "监听地址不能超过 64 个字符", 422)
			return in, false
		}
	}
	remark, ok := templateOptionalText(input["remark"], xrayTemplateRemarkMaxRunes)
	if !ok {
		d.fail(c, "备注不能超过 128 个字符", 422)
		return in, false
	}
	settings, ok := templateJSONInput(input["settings"])
	if !ok {
		d.fail(c, "settings 必须是合法的 JSON", 422)
		return in, false
	}
	stream, ok := templateJSONInput(input["stream_settings"])
	if !ok {
		d.fail(c, "stream_settings 必须是合法的 JSON", 422)
		return in, false
	}
	sniffing, ok := templateJSONInput(input["sniffing"])
	if !ok {
		d.fail(c, "sniffing 必须是合法的 JSON", 422)
		return in, false
	}

	in = XrayTemplateInboundInput{
		Name:           name,
		Sort:           inputInt(input["sort"]),
		Tag:            tag,
		Remark:         remark,
		Protocol:       protocol,
		Port:           port,
		Listen:         listen,
		Settings:       settings,
		StreamSettings: stream,
		Sniffing:       sniffing,
	}
	if v, exists := input["enabled"]; exists {
		enabled := inputBool(v)
		in.Enabled = &enabled
	}
	return in, true
}

// outboundInput 整理并校验一条出站模板的入参。
func (d *XrayTemplateDomain) outboundInput(c *gin.Context, input map[string]any) (XrayTemplateOutboundInput, bool) {
	var in XrayTemplateOutboundInput

	name, ok := templateTextInput(input["name"], xrayTemplateNameMaxRunes)
	if !ok {
		d.fail(c, "模板名称必填，且不超过 64 个字符", 422)
		return in, false
	}
	tag, ok := templateTextInput(input["tag"], xrayTemplateTagMaxRunes)
	if !ok {
		d.fail(c, "出站 tag 必填，且不超过 64 个字符", 422)
		return in, false
	}
	protocol, ok := templateTextInput(input["protocol"], xrayTemplateProtocolMaxRunes)
	if !ok {
		d.fail(c, "协议必填", 422)
		return in, false
	}
	settings, ok := templateJSONInput(input["settings"])
	if !ok {
		d.fail(c, "settings 必须是合法的 JSON", 422)
		return in, false
	}
	stream, ok := templateJSONInput(input["stream_settings"])
	if !ok {
		d.fail(c, "stream_settings 必须是合法的 JSON", 422)
		return in, false
	}

	in = XrayTemplateOutboundInput{
		Name:           name,
		Sort:           inputInt(input["sort"]),
		Tag:            tag,
		Protocol:       protocol,
		Settings:       settings,
		StreamSettings: stream,
	}
	if v, exists := input["enabled"]; exists {
		enabled := inputBool(v)
		in.Enabled = &enabled
	}
	return in, true
}

// routingInput 整理并校验一条路由模板的入参。
func (d *XrayTemplateDomain) routingInput(c *gin.Context, input map[string]any) (XrayTemplateRoutingInput, bool) {
	var in XrayTemplateRoutingInput

	name, ok := templateTextInput(input["name"], xrayTemplateNameMaxRunes)
	if !ok {
		d.fail(c, "模板名称必填，且不超过 64 个字符", 422)
		return in, false
	}
	outboundTag, ok := templateTextInput(input["outbound_tag"], xrayTemplateTagMaxRunes)
	if !ok {
		d.fail(c, "出站 tag 必填，且不超过 64 个字符", 422)
		return in, false
	}
	domains, ok := templateJSONInput(input["domains"])
	if !ok {
		d.fail(c, "domains 必须是合法的 JSON", 422)
		return in, false
	}
	ips, ok := templateJSONInput(input["ips"])
	if !ok {
		d.fail(c, "ips 必须是合法的 JSON", 422)
		return in, false
	}
	remark, ok := templateOptionalText(input["remark"], xrayTemplateRemarkMaxRunes)
	if !ok {
		d.fail(c, "备注不能超过 128 个字符", 422)
		return in, false
	}

	in = XrayTemplateRoutingInput{
		Name:        name,
		Sort:        inputInt(input["sort"]),
		Domains:     domains,
		IPs:         ips,
		Port:        templateOptionalString(input, "port", xrayTemplateTagMaxRunes),
		Network:     templateOptionalString(input, "network", 16),
		Protocol:    templateOptionalString(input, "protocol", 16),
		InboundTag:  templateOptionalString(input, "inbound_tag", xrayTemplateTagMaxRunes),
		OutboundTag: outboundTag,
		Remark:      remark,
	}
	if v, exists := input["enabled"]; exists {
		enabled := inputBool(v)
		in.Enabled = &enabled
	}
	return in, true
}

// templateTextInput 取必填文本（去空白后非空、不超长）。
func templateTextInput(v any, maxRunes int) (string, bool) {
	raw, _ := inputString(v)
	s := strings.TrimSpace(raw)
	if s == "" || utf8.RuneCountInString(s) > maxRunes {
		return "", false
	}
	return s, true
}

// templateOptionalText 取可选文本（空 → nil，超长 → 校验失败）。
func templateOptionalText(v any, maxRunes int) (*string, bool) {
	if v == nil {
		return nil, true
	}
	raw, _ := inputString(v)
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, true
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return nil, false
	}
	return &s, true
}

// templateOptionalString 取可选文本列（空 → SQL NULL），超长按截断处理：
// 这些是 xray 的匹配条件，超长的值本身也不会有匹配结果，不值得为它拒掉整条规则。
func templateOptionalString(input map[string]any, key string, maxRunes int) *string {
	v, exists := input[key]
	if !exists || v == nil {
		return nil
	}
	raw, _ := inputString(v)
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return &s
}

// templateJSONInput 把入参里的 JSON 列整理成可入库的字节：
//   - 字符串：必须是合法 JSON（前端「高级模式」直接贴 JSON 文本）；
//   - 对象 / 数组：按 PHP json_encode 口径重新编码（数字保留原文）；
//   - 空值：nil（落库为 NULL）。
func templateJSONInput(v any) ([]byte, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil, true
		}
		parsed, err := parseOrderedJS([]byte(s))
		if err != nil {
			return nil, false
		}
		if parsed == nil {
			return nil, true
		}
		return phpEncode(parsed), true
	case map[string]any, []any:
		return phpEncode(templatePlainJSON(t)), true
	default:
		// 标量（数字 / 布尔）当 JSON 列没有意义，直接判非法。
		var probe any
		b, err := json.Marshal(t)
		if err != nil || json.Unmarshal(b, &probe) != nil {
			return nil, false
		}
		return nil, false
	}
}

// templatePlainJSON 把 jsonInput 的嵌套结构转成可交给 phpEncode 的形态
// （map → 键序固定的 jsObj，jsonNumber → int64/float64）。
func templatePlainJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		obj := jo()
		for _, k := range sortedKeys(t) {
			obj.Put(k, templatePlainJSON(t[k]))
		}
		return obj
	case []any:
		arr := make(jsArr, 0, len(t))
		for _, item := range t {
			arr = append(arr, templatePlainJSON(item))
		}
		return arr
	case jsonNumber:
		return t.phpValue()
	default:
		return v
	}
}

// ---- 展示层 ----

// presentTemplateSet 输出三类模板（字段顺序固定，前端契约）。
func presentTemplateSet(set *XrayTemplateSet) *jsObj {
	inbounds := make(jsArr, 0, len(set.Inbounds))
	for i := range set.Inbounds {
		inbounds = append(inbounds, presentTemplateInbound(&set.Inbounds[i]))
	}
	outbounds := make(jsArr, 0, len(set.Outbounds))
	for i := range set.Outbounds {
		outbounds = append(outbounds, presentTemplateOutbound(&set.Outbounds[i]))
	}
	routing := make(jsArr, 0, len(set.Routing))
	for i := range set.Routing {
		routing = append(routing, presentTemplateRouting(&set.Routing[i]))
	}
	return jo("inbounds", inbounds, "outbounds", outbounds, "routing", routing)
}

func presentTemplateInbound(t *XrayTemplateInbound) *jsObj {
	return jo(
		"id", t.ID,
		"name", t.Name,
		"sort", t.Sort,
		"tag", t.Tag,
		"remark", nullableStr(t.Remark),
		"protocol", t.Protocol,
		"port", t.Port,
		"listen", t.Listen,
		"settings", parseJSONOrEmpty(t.Settings),
		"stream_settings", parseJSONOrEmpty(t.StreamSettings),
		"sniffing", parseJSONOrEmpty(t.Sniffing),
		"enabled", t.Enabled,
		"created_at", nullableISO(t.CreatedAt),
		"updated_at", nullableISO(t.UpdatedAt),
	)
}

func presentTemplateOutbound(t *XrayTemplateOutbound) *jsObj {
	return jo(
		"id", t.ID,
		"name", t.Name,
		"sort", t.Sort,
		"tag", t.Tag,
		"protocol", t.Protocol,
		"settings", parseJSONOrEmpty(t.Settings),
		"stream_settings", parseJSONOrEmpty(t.StreamSettings),
		"enabled", t.Enabled,
		"created_at", nullableISO(t.CreatedAt),
		"updated_at", nullableISO(t.UpdatedAt),
	)
}

func presentTemplateRouting(t *XrayTemplateRouting) *jsObj {
	return jo(
		"id", t.ID,
		"name", t.Name,
		"sort", t.Sort,
		"domains", arrayValues(parseJSONOrEmpty(t.Domains)),
		"ips", arrayValues(parseJSONOrEmpty(t.IPs)),
		"port", nullableStr(t.Port),
		"network", nullableStr(t.Network),
		"protocol", nullableStr(t.Protocol),
		"inbound_tag", nullableStr(t.InboundTag),
		"outbound_tag", t.OutboundTag,
		"remark", nullableStr(t.Remark),
		"enabled", t.Enabled,
		"created_at", nullableISO(t.CreatedAt),
		"updated_at", nullableISO(t.UpdatedAt),
	)
}

// presentTemplateApplyResult 输出单个节点的套用结果。
func presentTemplateApplyResult(r XrayTemplateApplyResult) *jsObj {
	return jo(
		"node_id", r.NodeID,
		"node_name", r.NodeName,
		"status", r.Status,
		"message", r.Message,
		"inbound_count", r.Inbounds,
		"outbound_count", r.Outbounds,
		"routing_count", r.Rules,
	)
}

// ---- 节点「单独配置过」标记 ----

// nodeCustomConfigMarker 是「把节点标成已单独配置」的可选能力。
//
// 用可选接口而不是往 NodeStore 里加方法：仓库里有几个嵌入式 fake store
// （NodeStore 字段为 nil），给接口加方法会让它们悄悄实现成「零值调用即 panic」，
// 而写接口的单测（如删除入站的跳跃链清理）根本不该关心模板标记。
type nodeCustomConfigMarker interface {
	MarkNodeCustomConfig(ctx context.Context, nodeID uint64) error
}

// markNodeCustomConfig 在入站写接口成功后调用：把节点标成「配置已单独改过」，
// 之后套用模板时默认跳过它（除非勾了「覆盖单独配置」）。
//
// 失败只记日志不打回请求：入站本身已经改成功了，模板标记是附带信息。
func (d *NodeDomain) markNodeCustomConfig(ctx context.Context, nodeID uint64) {
	marker, ok := d.store.(nodeCustomConfigMarker)
	if !ok {
		return
	}
	if err := marker.MarkNodeCustomConfig(ctx, nodeID); err != nil {
		d.logger.Printf("admin 标记节点 %d 已单独配置失败: %v", nodeID, err)
	}
}
