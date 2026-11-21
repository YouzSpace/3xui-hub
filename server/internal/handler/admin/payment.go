package admin

// 管理端支付配置（/admin-api/payments…）：CRUD + protocols。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/PaymentController.php。
// 协议清单来自 driver/payment.Registry 的 Definition()；新增协议自动出现在清单里。

import (
	"strconv"
	"strings"

	"controlhub-server/internal/driver/payment"
	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// legacyPaymentColumns 是可回落到旧列的协议字段（与 payment_configs 列名一致，保老数据/老接口语义）。
var legacyPaymentColumns = []string{"gateway", "query_gateway", "member_id", "api_key", "bank_code"}

// Register 把支付域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
//
// 顺序逐条对齐 backend/routes/web.php：静态段（/payments/protocols、/payments/settings）
// 必须先于参数段（/payments/:payment），否则 "settings" 会被当成 id 落到详情路由上。
func (d *PaymentDomain) Register(r gin.IRouter) {
	r.GET("/payments", d.paymentIndex)
	r.POST("/payments", d.paymentStore)
	r.GET("/payments/protocols", d.paymentProtocols)
	r.GET("/payments/settings", d.paymentSettingsGet)
	r.PUT("/payments/settings", d.paymentSettingsUpdate)
	r.GET("/payments/:payment", d.paymentShow)
	r.PUT("/payments/:payment", d.paymentUpdate)
	r.DELETE("/payments/:payment", d.paymentDestroy)
}

// ---- 响应封装（PHP ApiResponse 口径）----

func (d *PaymentDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *PaymentDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// failValidation 渲染 Laravel 校验失败：HTTP 200 + code 500 + 合并文案。
func (d *PaymentDomain) failValidation(c *gin.Context, summary string) {
	d.fail(c, summary, 500)
}

func (d *PaymentDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

func (d *PaymentDomain) notFound(c *gin.Context) { d.fail(c, "接口不存在", 404) }

func (d *PaymentDomain) noRowsToNotFound(c *gin.Context, err error) bool {
	if NoRows(err) {
		d.notFound(c)
		return true
	}
	return false
}

// loadPayment 按路由参数取支付配置；非数字 / 不存在 → 404（对齐隐式模型绑定）。
func (d *PaymentDomain) loadPayment(c *gin.Context) (*PaymentRow, bool) {
	id, ok := parseUint(c.Param("payment"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	p, err := d.store.PaymentByID(ctxOf(c), id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询支付配置", err)
		return nil, false
	}
	return p, true
}

// ---- 协议定义 ----

// definitionFor 按协议 key 找定义；未注册返回 nil。
func (d *PaymentDomain) definitionFor(driverType string) *payment.Definition {
	for _, def := range d.registry.Definitions() {
		if def.Key == driverType {
			def := def
			return &def
		}
	}
	return nil
}

// normalizeDriverType 对齐 PaymentController::normalizeDriverType：
// 为空、或不在 registry 已注册支付驱动里 → 一律回退 'payindex'。
func (d *PaymentDomain) normalizeDriverType(t string) string {
	t = strings.TrimSpace(t)
	if t != "" && d.definitionFor(t) != nil {
		return t
	}
	return payment.DriverPayindex
}

// ---- handlers ----

// paymentIndex 对齐 GET /admin-api/payments（orderByDesc id）。
func (d *PaymentDomain) paymentIndex(c *gin.Context) {
	rows, err := d.store.PaymentsAll(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询支付配置", err)
		return
	}
	out := make(jsArr, 0, len(rows))
	for i := range rows {
		out = append(out, d.present(&rows[i], false))
	}
	d.success(c, out)
}

// paymentProtocols 对齐 GET /admin-api/payments/protocols。
func (d *PaymentDomain) paymentProtocols(c *gin.Context) {
	defs := d.registry.Definitions()
	out := make(jsArr, 0, len(defs))
	for _, def := range defs {
		fields := make(jsArr, 0, len(def.Fields))
		for _, f := range def.Fields {
			fields = append(fields, jo(
				"name", f.Name,
				"label", f.Label,
				"placeholder", f.Placeholder,
				"required", f.Required,
				"isSecret", f.Secret,
			))
		}
		out = append(out, jo(
			"key", def.Key,
			"label", def.Label,
			"description", def.Description,
			"supports_query", def.SupportsQuery,
			"fields", fields,
		))
	}
	d.success(c, out)
}

// paymentShow 对齐 GET /admin-api/payments/{payment}（full=true 回显 api_key）。
func (d *PaymentDomain) paymentShow(c *gin.Context) {
	row, ok := d.loadPayment(c)
	if !ok {
		return
	}
	d.success(c, d.present(row, true))
}

// ---- 全局支付设置（订单支付有效期 + 允许继续支付）----

// paymentSettingsGet 读全局支付设置。未配置 → 回缺省值（页面拿到的永远是可直接编辑的值）。
//
// 只有两项：订单支付有效期 + 允许继续支付。
// （兜底作废任务每小时自动跑，只做低风险扫地，不需要开关暴露给页面。）
func (d *PaymentDomain) paymentSettingsGet(c *gin.Context) {
	minutes := payment.DefaultTTLMinutes
	continuePayEnabled := true // 缺省开（键不存在=开启，老行为不变）
	if d.settings != nil {
		keys := []string{payment.TTLMinutesKey, payment.ContinuePayEnabledKey}
		values, err := d.settings.SiteConfigMany(ctxOf(c), keys)
		if err != nil {
			d.failServer(c, "读取支付设置", err)
			return
		}
		if v, ok := payment.ParseTTLMinutes(values[payment.TTLMinutesKey]); ok {
			minutes = v
		}
		continuePayEnabled = !isOffValue(values[payment.ContinuePayEnabledKey])
	}
	d.success(c, jo(
		"pay_ttl_minutes", minutes,
		"continue_pay_enabled", continuePayEnabled,
	))
}

// paymentSettingsUpdate 保存全局支付设置（各字段都是可选的，传哪个改哪个）。
//
// 支付有效期校验复用 payment.ParseTTLMinutes —— 与运行时读取同一套判据（存得进就读得出）。
func (d *PaymentDomain) paymentSettingsUpdate(c *gin.Context) {
	if d.settings == nil {
		d.fail(c, "当前实例未接入站点配置存储", 500)
		return
	}
	data := jsonInput(c)

	kv := make([]KV, 0, 2)
	if raw, ok := data["pay_ttl_minutes"]; ok && raw != nil {
		text, _ := inputString(raw)
		minutes, valid := payment.ParseTTLMinutes(text)
		if !valid {
			d.fail(c, "支付有效期需为 1~120 之间的整数（分钟）", 400)
			return
		}
		value := strconv.Itoa(minutes)
		kv = append(kv, KV{Key: payment.TTLMinutesKey, Value: &value})
	}
	if raw, ok := data["continue_pay_enabled"]; ok && raw != nil {
		value := boolConfigValue(inputBool(raw))
		kv = append(kv, KV{Key: payment.ContinuePayEnabledKey, Value: &value})
	}
	if len(kv) == 0 {
		d.fail(c, "没有需要保存的设置", 400)
		return
	}
	if err := d.settings.SiteConfigSetMany(ctxOf(c), kv); err != nil {
		d.failServer(c, "保存支付设置", err)
		return
	}
	// 「允许继续支付」被关掉时，整套逻辑关闭：立刻清空所有已缓存的旧支付链接。
	if raw, ok := data["continue_pay_enabled"]; ok && raw != nil && !inputBool(raw) {
		if clearer, ok := d.settings.(payURLClearer); ok {
			if n, cerr := clearer.ClearOrderPayURLs(ctxOf(c)); cerr != nil {
				d.logger.Printf("admin 清空已缓存支付链接失败: %v", cerr)
			} else {
				d.logger.Printf("admin 已关闭继续支付，清空缓存支付链接 %d 条", n)
			}
		}
	}
	// 回读一次，返回保存后的完整状态（前端据此同步开关与输入框，不自己猜）
	d.paymentSettingsGet(c)
}

// isOffValue 判定 site_configs 的「关」值：'0'/'false'/'off'/'no'（忽略大小写），
// 其余（含键不存在、空串）都算开 —— 与调度器侧 task 包的判据逐字一致。
func isOffValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// boolConfigValue 把布尔写成 site_configs 的字面值（'1'/'0'）。
func boolConfigValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// paymentStore 对齐 POST /admin-api/payments。
func (d *PaymentDomain) paymentStore(c *gin.Context) {
	in, ok := d.validateConfig(c, false, nil)
	if !ok {
		return
	}
	row, err := d.store.CreatePayment(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建支付配置", err)
		return
	}
	d.success(c, d.present(row, false), "创建成功")
}

// paymentUpdate 对齐 PUT /admin-api/payments/{payment}。
func (d *PaymentDomain) paymentUpdate(c *gin.Context) {
	existing, ok := d.loadPayment(c)
	if !ok {
		return
	}
	in, ok := d.validateConfig(c, true, existing)
	if !ok {
		return
	}
	row, err := d.store.UpdatePayment(ctxOf(c), existing.ID, in)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "更新支付配置", err)
		return
	}
	d.success(c, d.present(row, false), "更新成功")
}

// paymentDestroy 对齐 DELETE /admin-api/payments/{payment}。
func (d *PaymentDomain) paymentDestroy(c *gin.Context) {
	row, ok := d.loadPayment(c)
	if !ok {
		return
	}
	if err := d.store.DeletePayment(ctxOf(c), row.ID); err != nil {
		d.failServer(c, "删除支付配置", err)
		return
	}
	d.success(c, nil, "已删除")
}

// ---- 校验与归一化 ----

// validateConfig 对齐 PaymentController::validateConfig：
//   - 协议特有字段全部收进 driver_config（JSON），同时把与旧列同名的字段写回旧列
//   - 密文字段「留空不修改」：空值从 driver_config 剔除后与旧值合并，旧列同步不动
//   - 老客户端仍可直接传 gateway/member_id/api_key 等顶层字段，会并入 driver_config
func (d *PaymentDomain) validateConfig(c *gin.Context, isUpdate bool, existing *PaymentRow) (PaymentWrite, bool) {
	var zero PaymentWrite
	data := jsonInput(c)

	v := newValidator(ctxOf(c), data)
	v.field("name", "required", "string", "max:64")
	v.field("driver_type", "sometimes", "nullable", "string", "max:32")
	v.field("driver_config", "sometimes", "nullable", "array")
	v.field("notify_url", "sometimes", "nullable", "string", "max:255")
	v.field("enabled", "sometimes", "boolean")
	v.field("gateway", "sometimes", "nullable", "string", "max:255")
	v.field("query_gateway", "sometimes", "nullable", "string", "max:255")
	v.field("member_id", "sometimes", "nullable", "string", "max:64")
	v.field("api_key", "sometimes", "nullable", "string", "max:255")
	v.field("bank_code", "sometimes", "nullable", "string", "max:64")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验支付配置", err)
			return zero, false
		}
		d.failValidation(c, v.summary())
		return zero, false
	}

	driverTypeRaw, _ := inputString(data["driver_type"])
	driverType := d.normalizeDriverType(driverTypeRaw)

	def := d.definitionFor(driverType)
	if def == nil {
		// 兜底：normalizeDriverType 已按 registry 判定过，正常走不到这里
		d.failValidation(c, "未知支付协议")
		return zero, false
	}

	// 字段定义按 name 建索引
	fieldByName := map[string]payment.FieldDef{}
	for _, f := range def.Fields {
		fieldByName[f.Name] = f
	}

	// 老客户端顶层字段并入 driver_config（driver_config 优先）
	incoming := map[string]string{}
	if raw, ok := data["driver_config"].(map[string]any); ok {
		for k, val := range raw {
			incoming[k] = paymentConfigStringValue(val)
		}
	}
	for _, col := range legacyPaymentColumns {
		if _, exists := incoming[col]; exists {
			continue
		}
		if val, ok := data[col]; ok {
			incoming[col] = paymentConfigStringValue(val)
		}
	}

	// 只保留该协议定义过的字段（防脏写）
	config := map[string]string{}
	for name, value := range incoming {
		if _, known := fieldByName[name]; !known {
			continue
		}
		if value == "" && fieldByName[name].Secret && isUpdate {
			// 密文字段留空 = 不修改
			continue
		}
		config[name] = value
	}

	// 必填校验：更新时密文字段已有旧值可豁免（留空不修改）
	for _, f := range def.Fields {
		if !f.Required {
			continue
		}
		if config[f.Name] != "" {
			continue
		}
		hasOldSecret := false
		if f.Secret && isUpdate && existing != nil {
			if existing.DriverConfig[f.Name] != "" || (f.Name == "api_key" && existing.APIKey != "") {
				hasOldSecret = true
			}
		}
		if hasOldSecret {
			continue
		}
		d.failValidation(c, f.Label+"为必填项")
		return zero, false
	}

	// 更新：密文空值已被剔除，这里与旧 driver_config 合并，保证「留空不修改」
	if isUpdate && existing != nil {
		merged := map[string]string{}
		for k, val := range existing.DriverConfig {
			merged[k] = val
		}
		for k, val := range config {
			merged[k] = val
		}
		config = merged
	}

	name, _ := inputString(data["name"])
	out := PaymentWrite{
		Name:         name,
		DriverType:   driverType,
		DriverConfig: config,
		Legacy:       map[string]*string{},
		Now:          d.now().UTC(),
	}
	// notify_url / enabled：更新时不传就整个键不进 $out，forceFill 自然不碰该列。
	if val, ok := data["notify_url"]; ok {
		if val != nil {
			s, _ := inputString(val)
			out.NotifyURL = &s
		} else {
			out.NotifyURL = nil
		}
		out.NotifyURLSet = true
	} else if !isUpdate {
		out.NotifyURL = nil
		out.NotifyURLSet = true
	}
	if val, ok := data["enabled"]; ok {
		out.Enabled = inputBool(val)
		out.EnabledSet = true
	} else if !isUpdate {
		out.Enabled = true
		out.EnabledSet = true
	}

	// 与旧列同名的字段写回旧列（methods 列表、老代码读列不受影响）；
	// 新建时 NOT NULL 列补空串、query_gateway 置 NULL；更新时密文空值路径不在 config 里 → 列不动
	for _, col := range legacyPaymentColumns {
		if val, ok := config[col]; ok {
			v := val
			out.Legacy[col] = &v
		} else if !isUpdate {
			if col == "query_gateway" {
				out.Legacy[col] = nil
			} else {
				empty := ""
				out.Legacy[col] = &empty
			}
		}
	}

	return out, true
}

// paymentConfigStringValue 对齐 PHP `is_scalar($v) ? trim((string)$v) : ”`。
func paymentConfigStringValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := inputString(v); ok {
		return phpTrimSpaceOnly(s)
	}
	return ""
}

// ---- present ----

// present 对齐 PaymentController::present。
//
// 列表（full=false）给 has_api_key；详情（full=true）给 api_key 明文。
// 字段顺序与 PHP 的 $data 数组字面量一致。
func (d *PaymentDomain) present(row *PaymentRow, full bool) *jsObj {
	driverType := d.normalizeDriverType(row.DriverType)
	out := jo(
		"id", row.ID,
		"name", row.Name,
		"driver_type", driverType,
		"gateway", row.Gateway,
		"query_gateway", nullableStr(row.QueryGateway),
		"member_id", row.MemberID,
		"notify_url", nullableStr(row.NotifyURL),
		"bank_code", row.BankCode,
		"enabled", row.Enabled,
		"created_at", nullableISO(row.CreatedAt),
	)
	if full {
		out.Put("api_key", row.APIKey)
	} else {
		has := row.APIKey != "" || row.DriverConfig["api_key"] != ""
		out.Put("has_api_key", has)
	}
	out.Put("driver_config", d.presentDriverConfig(row, driverType))
	return out
}

// presentDriverConfig 对齐 PaymentController::presentDriverConfig：
// 密文字段值不回显（置空）；老数据 driver_config 为空 → 用旧列拼一份视图。
//
// 键序：空视图按 LEGACY_COLUMNS 顺序；非空视图按定义字段顺序优先、其余按字典序（稳定输出）。
func (d *PaymentDomain) presentDriverConfig(row *PaymentRow, driverType string) *jsObj {
	config := map[string]string{}
	fromLegacy := len(row.DriverConfig) == 0
	if fromLegacy {
		if row.Gateway != "" {
			config["gateway"] = row.Gateway
		}
		if row.QueryGateway != nil && *row.QueryGateway != "" {
			config["query_gateway"] = *row.QueryGateway
		}
		if row.MemberID != "" {
			config["member_id"] = row.MemberID
		}
		if row.APIKey != "" {
			config["api_key"] = row.APIKey
		}
		if row.BankCode != "" {
			config["bank_code"] = row.BankCode
		}
	} else {
		for k, v := range row.DriverConfig {
			config[k] = v
		}
	}

	out := &jsObj{}
	def := d.definitionFor(driverType)

	put := func(k string, secret bool) {
		val, ok := config[k]
		if !ok {
			return
		}
		if secret {
			val = ""
		}
		out.Put(k, val)
	}

	if fromLegacy {
		// array_filter 保序：按 LEGACY_COLUMNS 声明顺序
		secretOf := map[string]bool{"api_key": true}
		if def != nil {
			secretOf = map[string]bool{}
			for _, f := range def.Fields {
				secretOf[f.Name] = f.Secret
			}
		}
		for _, k := range legacyPaymentColumns {
			put(k, secretOf[k])
		}
		return out
	}

	seen := map[string]bool{}
	if def != nil {
		for _, f := range def.Fields {
			if _, ok := config[f.Name]; ok {
				put(f.Name, f.Secret)
				seen[f.Name] = true
			}
		}
	}
	rest := make([]string, 0, len(config))
	for k := range config {
		if seen[k] {
			continue
		}
		rest = append(rest, k)
	}
	for i := 0; i < len(rest); i++ {
		for j := i + 1; j < len(rest); j++ {
			if rest[j] < rest[i] {
				rest[i], rest[j] = rest[j], rest[i]
			}
		}
	}
	for _, k := range rest {
		// 未知协议也绝不回显 api_key
		if def == nil && k == "api_key" {
			continue
		}
		put(k, false)
	}
	return out
}
