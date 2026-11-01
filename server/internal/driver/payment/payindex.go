package payment

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// PayindexDriver 是 wwspay 网关协议（backend/app/Drivers/Payments/PayindexDriver.php）。
//
// wwspay 是网关品牌，payindex 是协议名 —— 线上 driver_type 列默认值写的就是 'wwspay'，
// 两者是同一套协议，这里只保留 payindex 一个实现（回退规则见 Registry.For）。
//
// 协议要点（支付配置/开发文档.md）：
//   - 下单：POST 网关，返回 JSON；status==1 且 h5_url 非空即成功，否则 msg 是拒绝原因
//   - 签名：过滤空值 → 参与签名的字段按参数名 ASCII 升序 → `k=v&k=v` → 末尾拼 `&key={api_key}` → MD5 大写
//     平台 2025-11 改版后默认只有 7 个字段参与签名、且签名串用**原始值**（不 URL 编码）；
//     商品名/ip/type 只进请求体。字段集合与编码方式都走配置（sign_fields / sign_encode），
//     平台再改版只改配置，不动代码。
//   - 回调：memberid/orderid/amount/transaction_id/datetime/returncode 参与验签，returncode=='00' 才算成功
//   - 应答：成功回 "OK"，失败回 "FAIL"（网关只认这两个字母，否则会重推三次）
//
// 签名串**不含** pay_md5sign / sign 本身，所以调用方传进来的 map 里带上它们也无妨 ——
// 这里显式挑出参与签名的字段，不依赖「调用方恰好只传了该传的」。
type PayindexDriver struct {
	// HTTP 为空时用 http.DefaultClient（测试注入 httptest 的 client）。
	HTTP HTTPDoer
	// Now 供测试固定 pay_applydate 等时间字段。
	Now func() string
}

func (d *PayindexDriver) Name() string { return DriverPayindex }

func (d *PayindexDriver) SupportsQuery() bool { return true }

func (d *PayindexDriver) client() HTTPDoer {
	if d.HTTP != nil {
		return d.HTTP
	}
	return httpDefaultClient
}

func (d *PayindexDriver) now() string {
	if d.Now != nil {
		return d.Now()
	}
	return nowFormatter()
}

// payBodyFields 是下单请求体的全部字段（顺序即网关要求的请求体顺序）。
// 注意 pay_productname / pay_ip / pay_type 在新规则下**只进请求体，不进签名**。
var payBodyFields = []string{
	"pay_memberid",
	"pay_orderid",
	"pay_applydate",
	"pay_bankcode",
	"pay_notifyurl",
	"pay_callbackurl",
	"pay_amount",
	"pay_productname",
	"pay_ip",
	"pay_type",
}

// defaultPaySignFields 是默认参与签名的字段（平台 2025-11 改版后的规则：只有这 7 个）。
// 配置项 sign_fields 留空时用它；老平台可用 sign_fields 配回 10 字段 + sign_encode=urlencode 还原。
var defaultPaySignFields = []string{
	"pay_memberid",
	"pay_orderid",
	"pay_applydate",
	"pay_bankcode",
	"pay_notifyurl",
	"pay_callbackurl",
	"pay_amount",
}

// callbackSignFields 是回调参与验签的字段（与 PHP verifySignature 的 $verifyData 一致）。
var callbackSignFields = []string{
	"memberid", "orderid", "amount", "transaction_id", "datetime", "returncode",
}

// SignEncode 是签名串的取值编码方式（配置项 sign_encode）。
type SignEncode string

const (
	// SignEncodeRaw 用原始值拼接（平台 2025-11 新规则，默认）。
	SignEncodeRaw SignEncode = "raw"
	// SignEncodeURLEncode 用 PHP urlencode（http_build_query）编码后拼接（老规则）。
	SignEncodeURLEncode SignEncode = "urlencode"
)

// SignParams 计算 payindex 签名：过滤空值 + ASCII 升序 + `k=v&k=v` + `&key=` + MD5 大写。
// 默认按新规则（原始值）拼接；需要老规则（URL 编码）走 signParamsEncoded。
//
// 导出是为了让上层（订单对账、离线补单）与测试都能用同一份实现。
func SignParams(params map[string]string, apiKey string) string {
	return signParamsEncoded(params, apiKey, SignEncodeRaw)
}

// signParamsEncoded 按 encode 指定的编码方式算签名（raw / urlencode）。
func signParamsEncoded(params map[string]string, apiKey string, encode SignEncode) string {
	if encode == SignEncodeURLEncode {
		return strings.ToUpper(md5Hex(sortedNonEmpty(params).String() + "&key=" + apiKey))
	}
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" {
			continue // 空值不参与签名（"0" 保留）
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.ToUpper(md5Hex(strings.Join(parts, "&") + "&key=" + apiKey))
}

// signSelected 只挑出参与签名的字段再按 encode 算签名。
func signSelected(data map[string]string, fields []string, apiKey string, encode SignEncode) string {
	sub := make(map[string]string, len(fields))
	for _, f := range fields {
		if v, ok := data[f]; ok {
			sub[f] = v
		}
	}
	return signParamsEncoded(sub, apiKey, encode)
}

// resolvePaySignFields 读 sign_fields 配置：逗号分隔，留空 / 全部无效时用默认 7 字段。
// 只接受 payBodyFields 里的字段名（写错的名字直接忽略，避免拼出网关不认的签名串）。
func resolvePaySignFields(cfg Config) []string {
	raw := strings.TrimSpace(cfg.Cfg("sign_fields", ""))
	if raw == "" {
		return defaultPaySignFields
	}
	known := make(map[string]bool, len(payBodyFields))
	for _, f := range payBodyFields {
		known[f] = true
	}
	out := make([]string, 0, len(payBodyFields))
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f == "" || !known[f] {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return defaultPaySignFields
	}
	return out
}

// resolveSignEncode 读 sign_encode 配置：留空 / 无法识别时用 raw（新规则）。
func resolveSignEncode(cfg Config) SignEncode {
	if strings.EqualFold(strings.TrimSpace(cfg.Cfg("sign_encode", "")), string(SignEncodeURLEncode)) {
		return SignEncodeURLEncode
	}
	return SignEncodeRaw
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Pay 下单，返回 h5_url。
func (d *PayindexDriver) Pay(ctx context.Context, req PayRequest, cfg Config) PayResult {
	order := req.Order
	// 商品名：PHP 是 '套餐购买-' . $order->plan->name，拼装由上层完成（驱动不查库）。
	productName := order.ProductName
	if productName == "" {
		productName = "套餐购买-"
	}

	params := map[string]string{
		"pay_memberid":    cfg.Cfg("member_id", ""),
		"pay_orderid":     order.OrderNo,
		"pay_applydate":   d.now(),
		"pay_bankcode":    cfg.Cfg("bank_code", ""),
		"pay_notifyurl":   req.NotifyURL,
		"pay_callbackurl": req.ReturnURL,
		"pay_amount":      NormalizeMoney(order.Amount),
		"pay_productname": productName,
		"pay_ip":          order.PayIP,
		"pay_type":        "JSON",
	}
	if params["pay_ip"] == "" {
		params["pay_ip"] = "127.0.0.1"
	}

	apiKey := cfg.Cfg("api_key", "")

	// 请求体按 payBodyFields 顺序发全部 10 个字段，签名只覆盖 sign_fields 指定的字段
	// （默认 7 个，商品名/IP/type 默认不参与）。
	signFields := resolvePaySignFields(cfg)
	signEncode := resolveSignEncode(cfg)

	pairs := make([][2]string, 0, len(payBodyFields)+1)
	for _, f := range payBodyFields {
		pairs = append(pairs, [2]string{f, params[f]})
	}
	pairs = append(pairs, [2]string{"pay_md5sign", signSelected(params, signFields, apiKey, signEncode)})

	body, err := postForm(ctx, d.client(), cfg.Cfg("gateway", ""), pairs)
	if err != nil {
		// 网络异常：error 非空、FailMsg 为空 —— 调用方不得作废订单（用户可能已在支付）
		return PayResult{Err: err}
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return PayResult{Err: err}
	}

	if statusIsOne(data["status"]) {
		if h5, ok := stringValue(data["h5_url"]); ok && h5 != "" {
			return PayResult{Success: true, PayURL: h5}
		}
	}

	msg, _ := stringValue(data["msg"])
	if msg == "" {
		msg = "unknown"
	}
	return PayResult{FailMsg: msg}
}

// handleCallback 验签 + 判状态。金额比对交给上层。
func (d *PayindexDriver) HandleCallback(data map[string]string, cfg Config) CallbackResult {
	orderNo := data["orderid"]

	// 商户号必须与本条配置一致（PHP 用松散比较，这里两侧都是字符串，等价）。
	if cfg.Cfg("member_id", "") != data["memberid"] {
		return CallbackResult{Err: "商户号不匹配"}
	}
	if !d.VerifySignature(data, cfg) {
		return CallbackResult{Err: "签名验证失败"}
	}
	if data["returncode"] != "00" {
		return CallbackResult{Err: "状态异常"}
	}

	return CallbackResult{
		Success: true,
		OrderNo: orderNo,
		TradeNo: data["transaction_id"],
		Amount:  data["amount"],
	}
}

// VerifySignature 验签（不区分大小写，与 PHP strcasecmp 一致）。
//
// 平台 2025-11 改版后签名串用**原始值**，老平台用 URL 编码值，两者在 datetime 这类
// 含空格/冒号的字段上结果不同。这里**不读 sign_encode 配置**：先按新规则验，
// 不中再退老规则兜底 —— 下单的编码配置填错了也不能把真实支付回调当伪造拒掉。
func (d *PayindexDriver) VerifySignature(data map[string]string, cfg Config) bool {
	sign := data["sign"]
	if sign == "" {
		return false
	}
	key := cfg.Cfg("api_key", "")
	if strings.EqualFold(sign, signSelected(data, callbackSignFields, key, SignEncodeRaw)) {
		return true
	}
	return strings.EqualFold(sign, signSelected(data, callbackSignFields, key, SignEncodeURLEncode))
}

// Query 查单：returncode=='00' 且 trade_state=='SUCCESS' 视为已支付。
func (d *PayindexDriver) Query(ctx context.Context, order Order, cfg Config) *QueryResult {
	queryGateway := cfg.Cfg("query_gateway", "")
	if queryGateway == "" {
		return nil
	}

	params := map[string]string{
		"pay_memberid": cfg.Cfg("member_id", ""),
		"pay_orderid":  order.OrderNo,
	}
	// 查单的签名同样覆盖全部参数（这两个字段都是数字/字母，raw 与 urlencode 结果相同；
	// 仍按 sign_encode 走，保证把驱动配置回老规则时行为一致）。
	body, err := postForm(ctx, d.client(), queryGateway, [][2]string{
		{"pay_memberid", params["pay_memberid"]},
		{"pay_orderid", params["pay_orderid"]},
		{"pay_md5sign", signParamsEncoded(params, cfg.Cfg("api_key", ""), resolveSignEncode(cfg))},
	})
	if err != nil {
		return nil
	}

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	returnCode, _ := stringValue(resp["returncode"])
	if returnCode != "00" {
		return nil
	}

	tradeState, _ := stringValue(resp["trade_state"])
	tradeNo, _ := stringValue(resp["transaction_id"])

	status := "pending"
	if tradeState == "SUCCESS" {
		status = "paid"
	}
	return &QueryResult{Status: status, TradeState: tradeState, TradeNo: tradeNo}
}

// NotifyResponse：payindex/vvspay 网关只认 OK / FAIL。
func (d *PayindexDriver) NotifyResponse(success bool) string {
	if success {
		return "OK"
	}
	return "FAIL"
}

// Definition 对应 PayindexDriver::definition()。
func (d *PayindexDriver) Definition() Definition {
	return Definition{
		Key:           DriverPayindex,
		Label:         "PayIndex（现网关协议）",
		Description:   "MD5 签名：参与签名的字段按参数名升序、拼 `k=v&k=v` + &key= 密钥取大写。默认 2025-11 新规则（7 字段 + 原始值），可通过 sign_fields / sign_encode 还原老规则。支持查单。",
		SupportsQuery: true,
		Fields: []FieldDef{
			{Name: "gateway", Label: "支付网关地址", Placeholder: "https://xxx.com/Pay_Index.html", Required: true},
			{Name: "query_gateway", Label: "查询网关地址", Placeholder: "https://xxx.com/Pay_Trade_query.html"},
			{Name: "member_id", Label: "商户号", Placeholder: "输入商户号", Required: true},
			{Name: "api_key", Label: "API密钥", Placeholder: "输入API密钥", Required: true, Secret: true},
			{Name: "bank_code", Label: "银行编码（支付平台后台查看）", Placeholder: "如：1=支付宝, 2=微信", Required: true},
			{Name: "sign_fields", Label: "参与签名的字段", Placeholder: "留空=pay_memberid,pay_orderid,pay_applydate,pay_bankcode,pay_notifyurl,pay_callbackurl,pay_amount（平台改版时按新文档填）"},
			{Name: "sign_encode", Label: "签名取值编码", Placeholder: "留空=raw（原始值）；老平台填 urlencode"},
		},
	}
}

// ---- JSON 取值小工具（PHP 的松散判定）----

// statusIsOne 对齐 PHP 的 ($data['status'] ?? 0) == 1（松散比较）：
// JSON 里的 1 / 1.0 / "1" / true 都算成功。
func statusIsOne(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 1
	case string:
		return t == "1"
	case bool:
		return t
	}
	return false
}

// stringValue 对齐 PHP 的 (string)$v（数字按最短表示转）。
func stringValue(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return formatFloatPHP(t), true
	case bool:
		if t {
			return "1", true
		}
		return "", true
	}
	return "", false
}
