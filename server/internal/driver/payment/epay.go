package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// httpDefaultClient 是驱动在没有注入 HTTP 客户端时的兜底（带与 PHP 一致的 10s 超时）。
var httpDefaultClient HTTPDoer = &http.Client{Timeout: DefaultTimeout}

// EpayV1Driver 是易支付标准版协议（backend/app/Drivers/Payments/EpayV1Driver.php）。
//
// 与 payindex 的三处关键差异（验签实现照抄会挂）：
//   - 签名串不经过 http_build_query：直接 `k=v&k=v` 原样拼接（**不做 URL 编码**）
//   - 密钥**直接接在末尾**，没有 `&key=` 前缀
//   - MD5 结果是小写
//
// 下单：POST {gateway}/submit.php（网关地址没带 submit.php 就自动补），
// 响应 {code:1,url:"..."} 取 url；code!=1 取 msg 作拒绝原因。
// 回调：pid/trade_no/out_trade_no/type/name/money/trade_status/sign/sign_type，
// 判定 = 验签 + pid==member_id + trade_status==TRADE_SUCCESS。
type EpayV1Driver struct {
	HTTP HTTPDoer
	Now  func() string
}

func (d *EpayV1Driver) Name() string { return DriverEpayV1 }

// SupportsQuery：多数 epay 站点没有查单接口。
func (d *EpayV1Driver) SupportsQuery() bool { return false }

func (d *EpayV1Driver) client() HTTPDoer {
	if d.HTTP != nil {
		return d.HTTP
	}
	return httpDefaultClient
}

// EpaySignParams 计算易支付签名：排除 sign/sign_type 与空值 → ASCII 升序 → `k=v&...` 直接接密钥 → 小写 MD5。
//
// 注意这里**不做 URL 编码**：签名串是给 MD5 用的，不是给 HTTP 用的。
// （PHP 侧用的就是 implode('&', $pairs) . $key。）
func EpaySignParams(params map[string]string, key string) string {
	filtered := make(map[string]string, len(params))
	for k, v := range params {
		if k == "sign" || k == "sign_type" {
			continue
		}
		filtered[k] = v
	}

	keys := sortedNonEmptyKeys(filtered)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(filtered[k])
	}
	b.WriteString(key)
	return md5Hex(b.String())
}

// sortedNonEmptyKeys 是「ASCII 升序 + 过滤空值」的键列表（易支付签名用，不做 URL 编码）。
func sortedNonEmptyKeys(params map[string]string) []string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// submitURL 对齐 `if (!str_ends_with($gateway,'/submit.php')) rtrim($gateway,'/').'/submit.php'`。
func submitURL(gateway string) string {
	if strings.HasSuffix(gateway, "/submit.php") {
		return gateway
	}
	return strings.TrimRight(gateway, "/") + "/submit.php"
}

// epayRedirectRe 从 submit.php 返回的 HTML 跳转页里提取支付地址。
//
// 不少易支付实现下单成功后不回 JSON，而是 HTTP 200 + 一段前端跳转脚本
// （实测：`<script>location.href='https://…/zfbpay?order_sn=…';</script>`，或 meta refresh 变体）。
// 按优先级依次尝试：JS location 赋值 → meta refresh → 页面里第一个 https?:// 链接。
var epayRedirectRe = []*regexp.Regexp{
	regexp.MustCompile(`location(?:\.href)?\s*=\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`location\.replace\(\s*['"]([^'"]+)['"]\s*\)`),
	regexp.MustCompile(`(?i)<meta[^>]+http-equiv=['"]?refresh['"]?[^>]+url=([^'">]+)`),
	regexp.MustCompile(`['"](https?://[^'"]{16,})['"]`),
}

// extractRedirectURL 从 HTML 响应体提取跳转地址；没有返回空串。
func extractRedirectURL(body string) string {
	for _, re := range epayRedirectRe {
		if m := re.FindStringSubmatch(body); len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// Pay 下单。
func (d *EpayV1Driver) Pay(ctx context.Context, req PayRequest, cfg Config) PayResult {
	order := req.Order
	productName := order.ProductName
	if productName == "" {
		productName = "套餐购买-"
	}

	params := map[string]string{
		"pid":          cfg.Cfg("member_id", ""),
		"type":         cfg.Cfg("bank_code", ""),
		"out_trade_no": order.OrderNo,
		"notify_url":   req.NotifyURL,
		"return_url":   req.ReturnURL,
		"name":         productName,
		"money":        NormalizeMoney(order.Amount),
	}
	// 键序与 PHP 的 $params 一致（签名内部自己排序，请求体顺序保持稳定便于对拍）。
	pairs := [][2]string{
		{"pid", params["pid"]},
		{"type", params["type"]},
		{"out_trade_no", params["out_trade_no"]},
		{"notify_url", params["notify_url"]},
		{"return_url", params["return_url"]},
		{"name", params["name"]},
		{"money", params["money"]},
		{"sign", EpaySignParams(params, cfg.Cfg("api_key", ""))},
		{"sign_type", "MD5"},
	}

	body, err := postForm(ctx, d.client(), submitURL(cfg.Cfg("gateway", "")), pairs)
	if err != nil {
		return PayResult{Err: err}
	}

	var data map[string]any
	if jerr := json.Unmarshal(body, &data); jerr != nil {
		// 非 JSON 响应：不少易支付实现下单成功后回 HTML 跳转页（平台侧已建单成功）。
		// 提取跳转地址作为 pay_url；提取不到仍按解析异常（临时故障，不得作废订单）。
		if u := extractRedirectURL(string(body)); u != "" {
			return PayResult{Success: true, PayURL: u}
		}
		return PayResult{Err: jerr}
	}

	if codeIsOne(data["code"]) {
		if u, ok := stringValue(data["url"]); ok && u != "" {
			return PayResult{Success: true, PayURL: u}
		}
	}

	msg, _ := stringValue(data["msg"])
	if msg == "" {
		msg = "unknown"
	}
	return PayResult{FailMsg: msg}
}

// HandleCallback 验签 + 判状态。
func (d *EpayV1Driver) HandleCallback(data map[string]string, cfg Config) CallbackResult {
	orderNo := data["out_trade_no"]

	if cfg.Cfg("member_id", "") != data["pid"] {
		return CallbackResult{Err: "商户号不匹配"}
	}
	if !d.VerifySignature(data, cfg) {
		return CallbackResult{Err: "签名验证失败"}
	}
	if data["trade_status"] != "TRADE_SUCCESS" {
		return CallbackResult{Err: "状态异常"}
	}

	return CallbackResult{
		Success: true,
		OrderNo: orderNo,
		TradeNo: data["trade_no"],
		Amount:  data["money"],
	}
}

// VerifySignature 验签：sign 为空直接失败；不区分大小写。
func (d *EpayV1Driver) VerifySignature(data map[string]string, cfg Config) bool {
	sign := data["sign"]
	if sign == "" {
		return false
	}
	return strings.EqualFold(sign, EpaySignParams(data, cfg.Cfg("api_key", "")))
}

// Query：该协议不支持查单。
func (d *EpayV1Driver) Query(ctx context.Context, order Order, cfg Config) *QueryResult {
	return nil
}

// NotifyResponse：易支付要求应答 success / fail。
func (d *EpayV1Driver) NotifyResponse(success bool) string {
	if success {
		return "success"
	}
	return "fail"
}

// Definition 对应 EpayV1Driver::definition()。
func (d *EpayV1Driver) Definition() Definition {
	return Definition{
		Key:           DriverEpayV1,
		Label:         "易支付（标准版）",
		Description:   "标准易支付：MD5 签名（排除 sign/sign_type，末尾直接追加密钥，小写）。该协议不支持查单。",
		SupportsQuery: false,
		Fields: []FieldDef{
			{Name: "gateway", Label: "支付网关地址", Placeholder: "https://xxx.com/（结尾可不带 submit.php，自动补）", Required: true},
			{Name: "member_id", Label: "商户ID（pid）", Placeholder: "输入商户ID", Required: true},
			{Name: "api_key", Label: "通信密钥（key）", Placeholder: "输入通信密钥", Required: true, Secret: true},
			{Name: "bank_code", Label: "支付方式（type）", Placeholder: "如：alipay / wxpay / qqpay", Required: true},
		},
	}
}

// codeIsOne 对齐 PHP 的 (int)($data['code'] ?? 0) === 1。
// 注意是 (int) 强转：JSON 里 "1" 与 1.9 都会被转成 1。
func codeIsOne(v any) bool {
	switch t := v.(type) {
	case float64:
		return int64(t) == 1
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return err == nil && n == 1
	case bool:
		return t
	}
	return false
}

// formatFloatPHP 对齐 PHP 的 (string)$float：最短往返表示（serialize_precision=-1）。
func formatFloatPHP(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// nowFormatter 是默认时钟：pay_applydate 用 `Y-m-d H:i:s`（PHP app.timezone=UTC）。
func nowFormatter() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }
