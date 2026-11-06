package admin

// 站点级配置：自定义首页 / 管理端配色 / 站点信息 / 订阅格式 / 邀请机制。
//
// 都存 site_configs（key-value），不建表。逐条对齐：
//   - Admin\HomeController
//   - Admin\PanelColorsController
//   - Admin\SiteSettingController
//   - Admin\SubscriptionSettingController
//   - Admin\DiscountCodeController::inviteSettings / updateInviteSettings

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ---- 自定义公开首页 ----

const (
	homeKeyHTML = "home_custom_html"
	homeKeyCSS  = "home_custom_css"

	// homeMaxHTML / homeMaxCSS 与控制器常量一致（HTML 1MB、CSS 256KB，分开限制）。
	homeMaxHTML = 1048576
	homeMaxCSS  = 262144
)

// showHomeCustom 对齐 GET /admin-api/home-custom。
func (h *Handler) showHomeCustom(c *gin.Context) {
	values, err := h.store.SiteConfigMany(ctxOf(c), []string{homeKeyHTML, homeKeyCSS})
	if err != nil {
		h.failServer(c, "读取自定义首页", err)
		return
	}
	html, css := values[homeKeyHTML], values[homeKeyCSS]
	h.success(c, jo(
		"html", html,
		"css", css,
		"max_html", homeMaxHTML,
		"max_css", homeMaxCSS,
		// 是否已启用自定义（空 HTML = 未启用）
		"enabled", phpTrimSpaceOnly(html) != "",
	))
}

// saveHomeCustom 对齐 PUT /admin-api/home-custom：只做存储，不做消毒（消毒在前端渲染前）。
func (h *Handler) saveHomeCustom(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("html", "nullable", "string", "max:"+strconv.Itoa(homeMaxHTML))
	v.field("css", "nullable", "string", "max:"+strconv.Itoa(homeMaxCSS))
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	// 空值也要写：管理员可能想清空自定义回到默认首页。
	htmlVal := stringOrEmpty(input["html"])
	cssVal := stringOrEmpty(input["css"])
	if err := h.store.SiteConfigSetMany(ctxOf(c), []KV{
		{Key: homeKeyHTML, Value: &htmlVal},
		{Key: homeKeyCSS, Value: &cssVal},
	}); err != nil {
		h.failServer(c, "保存自定义首页", err)
		return
	}
	h.success(c, nil, "首页已保存，公开首页立即生效")
}

// resetHomeCustom 对齐 POST /admin-api/home-custom/reset。
func (h *Handler) resetHomeCustom(c *gin.Context) {
	empty := ""
	if err := h.store.SiteConfigSetMany(ctxOf(c), []KV{
		{Key: homeKeyHTML, Value: &empty},
		{Key: homeKeyCSS, Value: &empty},
	}); err != nil {
		h.failServer(c, "重置自定义首页", err)
		return
	}
	h.success(c, nil, "已恢复默认首页")
}

// ---- 管理端配色 ----

const (
	panelColorsLight = "panel_colors_light"
	panelColorsDark  = "panel_colors_dark"
)

// panelColorVars 是允许覆盖的变量白名单（与前端「配色」页一一对应，顺序即输出顺序）。
var panelColorVars = []string{
	"--accent", "--bg-base", "--bg-surface",
	"--text-primary", "--text-secondary", "--border-subtle",
}

// panelColorRe 只接受 #RRGGBB（这些值会拼进 <style> 注入前端，白名单外一律丢弃）。
var panelColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// showPanelColors 对齐 GET /admin-api/panel-colors。
func (h *Handler) showPanelColors(c *gin.Context) {
	values, err := h.store.SiteConfigMany(ctxOf(c), []string{panelColorsLight, panelColorsDark})
	if err != nil {
		h.failServer(c, "读取管理端配色", err)
		return
	}
	h.success(c, jo(
		"light", decodePanelColors(values[panelColorsLight]),
		"dark", decodePanelColors(values[panelColorsDark]),
	))
}

// savePanelColors 对齐 PUT /admin-api/panel-colors（整体覆盖；空对象 = 该模式回到默认）。
func (h *Handler) savePanelColors(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("light", "nullable", "array")
	v.field("dark", "nullable", "array")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	light := sanitizePanelColors(input["light"])
	dark := sanitizePanelColors(input["dark"])

	lightEnc := ""
	if len(light) > 0 {
		lightEnc = encodePanelColors(light)
	}
	darkEnc := ""
	if len(dark) > 0 {
		darkEnc = encodePanelColors(dark)
	}

	if err := h.store.SiteConfigSetMany(ctxOf(c), []KV{
		{Key: panelColorsLight, Value: &lightEnc},
		{Key: panelColorsDark, Value: &darkEnc},
	}); err != nil {
		h.failServer(c, "保存管理端配色", err)
		return
	}
	h.success(c, nil, "配色已保存，立即生效")
}

// resetPanelColors 对齐 POST /admin-api/panel-colors/reset。
func (h *Handler) resetPanelColors(c *gin.Context) {
	empty := ""
	if err := h.store.SiteConfigSetMany(ctxOf(c), []KV{
		{Key: panelColorsLight, Value: &empty},
		{Key: panelColorsDark, Value: &empty},
	}); err != nil {
		h.failServer(c, "重置管理端配色", err)
		return
	}
	h.success(c, nil, "已恢复默认配色")
}

// sanitizePanelColors 只保留白名单变量 + 合法 #RRGGBB，值统一转小写（非法值静默丢弃）。
func sanitizePanelColors(raw any) []panelColor {
	obj, _ := raw.(map[string]any)
	out := make([]panelColor, 0, len(panelColorVars))
	for _, name := range panelColorVars {
		val, ok := obj[name]
		if !ok {
			continue
		}
		s, isStr := val.(string)
		if !isStr || !panelColorRe.MatchString(s) {
			continue
		}
		out = append(out, panelColor{Name: name, Value: strings.ToLower(s)})
	}
	return out
}

// panelColor 是一对变量名与色值（保持白名单顺序）。
type panelColor struct {
	Name  string
	Value string
}

// panelColorsJSON 输出配色对象。
//
// 空的 PHP 数组 json_encode 得到 `[]`（不是 `{}`），所以空配置必须输出数组，
// 非空时才是对象 —— 金样本里未配置时就是 "light":[]。
func panelColorsJSON(list []panelColor) any {
	if len(list) == 0 {
		return jsArr{}
	}
	o := jo()
	for _, c := range list {
		o.put(c.Name, c.Value)
	}
	return o
}

// decodePanelColors 读存储的 JSON；坏数据（非法 JSON / 含非法色值）一律当未配置。
func decodePanelColors(raw string) any {
	if phpTrimSpaceOnly(raw) == "" {
		return jsArr{}
	}
	var decoded any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return jsArr{}
	}
	return panelColorsJSON(sanitizePanelColors(decoded))
}

// encodePanelColors 把已净化的对象编码成存储串（与 PHP json_encode 默认选项一致）。
func encodePanelColors(list []panelColor) string {
	if len(list) == 0 {
		return ""
	}
	return string(phpEncode(panelColorsJSON(list)))
}

// ---- 站点信息 / 订阅格式 ----

// siteSettingKeys 是 SiteSettingController::FIELDS（顺序即输出顺序）。
// feedback_links 是 Go 侧新增（2026-11-06）：用户端「反馈与联系」二级页的数据，
// JSON 数组 [{title,type,url}]（type=link 跳转 / qr 二维码图片），复用 key-value 存储。
var siteSettingKeys = []string{
	"site_title", "site_subtitle", "site_description", "site_keywords",
	"site_logo", "site_favicon", "announcement", "feedback_link",
	"feedback_links",
}

// subscriptionSettingKeys 是 SubscriptionSettingController::FIELDS。
var subscriptionSettingKeys = []string{
	"sub_clash_enabled", "sub_singbox_enabled", "sub_show_traffic", "sub_show_expire",
	"sub_show_flag", "sub_show_userid", "sub_rename_enabled", "sub_rename_regex",
	"sub_rename_replacement", "sub_custom_info_enabled", "sub_custom_info_text",
	"sub_title",
}

// showSiteSettings 对齐 GET /admin-api/site-settings（缺失键补空串）。
func (h *Handler) showSiteSettings(c *gin.Context) {
	h.showKeyValues(c, siteSettingKeys, "读取站点信息")
}

// updateSiteSettings 对齐 PUT /admin-api/site-settings：只认白名单字段。
func (h *Handler) updateSiteSettings(c *gin.Context) {
	h.saveKeyValues(c, siteSettingKeys, "保存站点信息")
}

// showSubscriptionSettings 对齐 GET /admin-api/subscription-settings。
func (h *Handler) showSubscriptionSettings(c *gin.Context) {
	h.showKeyValues(c, subscriptionSettingKeys, "读取订阅格式配置")
}

// updateSubscriptionSettings 对齐 PUT /admin-api/subscription-settings。
func (h *Handler) updateSubscriptionSettings(c *gin.Context) {
	h.saveKeyValues(c, subscriptionSettingKeys, "保存订阅格式配置")
}

// ---- App 版本分发（系统设置 → App 版本）----

// appVersionSettingKeys 是「App 版本」页的 4 个字段（顺序即输出顺序）。
// 与用户端 GET /api/app-version 读的是同一批键。
var appVersionSettingKeys = []string{
	"app_latest_version_name", "app_update_notes",
	"app_download_url", "app_force_update",
}

// showAppVersionSettings 对齐 GET /admin-api/app-version-settings（缺失键补空串）。
func (h *Handler) showAppVersionSettings(c *gin.Context) {
	h.showKeyValues(c, appVersionSettingKeys, "读取 App 版本配置")
}

// updateAppVersionSettings 对齐 PUT /admin-api/app-version-settings：只认白名单字段，
// app_force_update 是开关统一存 '1'/'0'，其余按字符串存。
func (h *Handler) updateAppVersionSettings(c *gin.Context) {
	input := jsonInput(c)

	kvs := make([]KV, 0, len(appVersionSettingKeys))
	for _, k := range appVersionSettingKeys {
		val, ok := input[k]
		if !ok {
			continue
		}
		s := phpScalarString(val)
		if k == "app_force_update" {
			if inputBool(val) {
				s = "1"
			} else {
				s = "0"
			}
		}
		kvs = append(kvs, KV{Key: k, Value: &s})
	}
	if len(kvs) > 0 {
		if err := h.store.SiteConfigSetMany(ctxOf(c), kvs); err != nil {
			h.failServer(c, "保存 App 版本配置", err)
			return
		}
	}
	h.success(c, nil, "保存成功")
}

// showKeyValues 输出指定键的当前值（键序即声明顺序，缺失补空串）。
func (h *Handler) showKeyValues(c *gin.Context, keys []string, what string) {
	values, err := h.store.SiteConfigMany(ctxOf(c), keys)
	if err != nil {
		h.failServer(c, what, err)
		return
	}
	o := jo()
	for _, k := range keys {
		o.put(k, values[k])
	}
	h.success(c, o)
}

// saveKeyValues 只写入请求里出现过的白名单键（对齐 $request->only(self::FIELDS)）。
func (h *Handler) saveKeyValues(c *gin.Context, keys []string, what string) {
	input := jsonInput(c)
	allowed := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		allowed[k] = struct{}{}
	}

	kvs := make([]KV, 0, len(keys))
	for _, k := range keys {
		val, ok := input[k]
		if !ok {
			continue
		}
		s := phpScalarString(val)
		kvs = append(kvs, KV{Key: k, Value: &s})
	}
	if len(kvs) > 0 {
		if err := h.store.SiteConfigSetMany(ctxOf(c), kvs); err != nil {
			h.failServer(c, what, err)
			return
		}
	}
	h.success(c, nil, "保存成功")
}

// ---- 邀请机制 + 优惠码校验限流 ----

// inviteKeys 是 DiscountCodeController::INVITE_KEYS（顺序即输出顺序）。
//
// affiliate_rebate_rate 是 Go 侧新增的「邀请返佣比例」（0~1，默认 0.10，0 = 暂停返佣）：
// 不放进白名单的话，后台保存这个键会被 saveKeyValues 静默丢弃。
var inviteKeys = []string{
	"invite_discount", "invite_refresh_days", "invite_enabled", "redeem_discount",
	"affiliate_rebate_rate",
}

// discountRateKeys 是优惠码校验限流的 5 项（顺序即输出顺序）。
var discountRateKeys = []string{
	"rate_discount_per_minute", "rate_discount_fail_per_day",
	"rate_discount_ip_fail_hourly", "rate_discount_max_attempts",
	"rate_discount_lock_seconds",
}

// inviteSettings 对齐 GET /admin-api/invite-settings。
//
// 限流 5 项刻意回「生效值」而不是原始 SiteConfig 值：没配过时回 config/panel.php
// 的默认值，页面输入框直接显示当前真正在用的阈值。
func (h *Handler) inviteSettings(c *gin.Context) {
	values, err := h.store.SiteConfigMany(ctxOf(c), inviteKeys)
	if err != nil {
		h.failServer(c, "读取邀请配置", err)
		return
	}
	// 未配置时的默认值（SiteConfig::getValue($key, $default) 口径）。
	defaults := map[string]string{
		"invite_discount":     "0.90",
		"invite_refresh_days": "30",
		"invite_enabled":      "1",
		"redeem_discount":     "0.90",
		// Go 侧新增：邀请返佣比例（0~1，0 = 暂停返佣）。没配过时 GET 显示这个生效值。
		"affiliate_rebate_rate": "0.10",
	}
	o := jo()
	for _, k := range inviteKeys {
		val := values[k]
		if val == "" {
			if def, ok := defaults[k]; ok {
				val = def
			}
		}
		o.put(k, val)
	}

	rateValues, err := h.store.SiteConfigMany(ctxOf(c), discountRateKeys)
	if err != nil {
		h.failServer(c, "读取优惠码限流配置", err)
		return
	}
	rateDefaults := map[string]int64{
		"rate_discount_per_minute":     h.effectiveRateDefault("per_minute"),
		"rate_discount_fail_per_day":   h.effectiveRateDefault("fail_per_day"),
		"rate_discount_ip_fail_hourly": h.effectiveRateDefault("ip_fail_hourly"),
		"rate_discount_max_attempts":   h.effectiveRateDefault("max_attempts"),
		"rate_discount_lock_seconds":   h.effectiveRateDefault("lock_seconds"),
	}
	for _, k := range discountRateKeys {
		stored := rateValues[k]
		if stored == "" {
			// 空值 = 用 config/panel.php 的默认值（0 = 该项不限）。
			o.put(k, strconv.FormatInt(rateDefaults[k], 10))
			continue
		}
		o.put(k, stored)
	}

	h.success(c, o)
}

// effectiveRateDefault 取 config/panel.php 里对应项的默认值。
func (h *Handler) effectiveRateDefault(name string) int64 {
	d := h.cfg.DiscountRateDefaults
	switch name {
	case "per_minute":
		return d.PerMinute
	case "fail_per_day":
		return d.FailPerDay
	case "ip_fail_hourly":
		return d.IPFailHourly
	case "max_attempts":
		return d.MaxAttempts
	case "lock_seconds":
		return d.LockSeconds
	}
	return 0
}

// updateInviteSettings 对齐 PUT /admin-api/invite-settings。
func (h *Handler) updateInviteSettings(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("invite_discount", "sometimes", "numeric", "gt:0", "lte:1")
	v.field("invite_refresh_days", "sometimes", "integer", "min:1")
	v.field("invite_enabled", "sometimes", "boolean")
	v.field("redeem_discount", "sometimes", "numeric", "gt:0", "lte:1")
	// 邀请返佣比例：0 合法（= 暂停返佣）；>1 会被返佣服务当配错回默认 0.10。
	v.field("affiliate_rebate_rate", "sometimes", "numeric", "gte:0", "lte:1")
	for _, k := range discountRateKeys {
		v.field(k, "sometimes", "nullable", "integer", "min:0")
	}
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	kvs := make([]KV, 0, len(inviteKeys)+len(discountRateKeys))
	for _, k := range inviteKeys {
		val, ok := input[k]
		if !ok {
			continue
		}
		// 开关统一存 '1'/'0'，其它值按字符串存。
		s := phpScalarString(val)
		if k == "invite_enabled" {
			if inputBool(val) {
				s = "1"
			} else {
				s = "0"
			}
		}
		kvs = append(kvs, KV{Key: k, Value: &s})
	}
	for _, k := range discountRateKeys {
		val, ok := input[k]
		if !ok {
			continue
		}
		// 限流项传 null 时 (string) 后是空串，正好等于「没配过、用 panel 默认值」。
		s := phpScalarString(val)
		kvs = append(kvs, KV{Key: k, Value: &s})
	}

	if len(kvs) > 0 {
		if err := h.store.SiteConfigSetMany(ctxOf(c), kvs); err != nil {
			h.failServer(c, "保存邀请配置", err)
			return
		}
	}
	h.success(c, nil, "保存成功")
}

// stringOrEmpty 取输入里的字符串（null → 空串）。
func stringOrEmpty(v any) string {
	s, ok := inputString(v)
	if !ok {
		return ""
	}
	return s
}
