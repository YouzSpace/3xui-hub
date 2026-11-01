package payment

import "testing"

// 平台 2025-11 改版后的签名规则：参与签名的 7 字段按参数名升序、原始值拼接、&key= 密钥、MD5 大写。
// 测试向量由 tmp/genvectors.py 生成，与实测成功的下单请求同构（商品名/ip/type 不进签名）。
func TestPayindexSignParamsNewRules(t *testing.T) {
	params := map[string]string{
		"pay_memberid":    "260681124",
		"pay_orderid":     "20261101123718025428",
		"pay_applydate":   "2026-11-01 12:37:18",
		"pay_bankcode":    "1",
		"pay_notifyurl":   "https://wujiecloud.top/api/payment/notify",
		"pay_callbackurl": "https://wujiecloud.top/",
		"pay_amount":      "1.50",
		"pay_productname": "套餐购买-测试套餐", // 不参与签名
		"pay_ip":          "182.239.114.59",
		"pay_type":        "JSON", // 不参与签名
	}
	got := signSelected(params, paySignFields, "132xkk3pko3edbdmealepse95roagod0")
	want := "BFE742F5E4F45503CFDD423703670558"
	if got != want {
		t.Fatalf("SignParams 新规则 = %s, want %s", got, want)
	}
}

// 空值不参与、键序按字节升序（与大写字母排在小写字母之前的行为一致）。
func TestPayindexSignParamsSkipsEmpty(t *testing.T) {
	a := SignParams(map[string]string{"pay_orderid": "A", "pay_memberid": "B", "pay_applydate": ""}, "k")
	b := SignParams(map[string]string{"pay_orderid": "A", "pay_memberid": "B"}, "k")
	if a != b {
		t.Fatalf("空值字段应不参与签名: %s != %s", a, b)
	}
}

// 回调验签：新规则（原始值）必须通过；老规则（URL 编码）签名作为兼容兜底也必须通过。
func TestPayindexVerifyCallbackBothRules(t *testing.T) {
	d := &PayindexDriver{}
	data := map[string]string{
		"memberid":       "260681124",
		"orderid":        "20261101123718025428",
		"amount":         "1.50",
		"transaction_id": "QK20261101123718025428001",
		"datetime":       "2026-11-01 12:40:00",
		"returncode":     "00",
	}
	cfg := Config{DriverConfig: map[string]string{"api_key": "132xkk3pko3edbdmealepse95roagod0"}}
	signed := map[string]string{
		"memberid": data["memberid"], "orderid": data["orderid"], "amount": data["amount"],
		"transaction_id": data["transaction_id"], "datetime": data["datetime"],
		"returncode": data["returncode"], "sign": "6D3ACCA468BF3CE70CEDB36AB6905814",
	}
	if !d.VerifySignature(signed, cfg) {
		t.Fatalf("新规则签名应通过")
	}
	dataLegacy := map[string]string{
		"memberid": data["memberid"], "orderid": data["orderid"], "amount": data["amount"],
		"transaction_id": data["transaction_id"], "datetime": data["datetime"],
		"returncode": data["returncode"], "sign": "B297B4E3DB59CD049BF96A045500D504",
	}
	if !d.VerifySignature(dataLegacy, cfg) {
		t.Fatalf("老规则签名(兼容)应通过")
	}
	// 篡改金额：用新规则给篡改后的数据重签，签名合法但金额对不上原始单据 —— 验签本身只看签名串，
	// 这里验证的是「金额改了、签名串随之变化，原 sign 不再匹配」。
	forged := map[string]string{
		"memberid": data["memberid"], "orderid": data["orderid"], "amount": "9.99",
		"transaction_id": data["transaction_id"], "datetime": data["datetime"],
		"returncode": data["returncode"], "sign": "6D3ACCA468BF3CE70CEDB36AB6905814",
	}
	if d.VerifySignature(forged, cfg) {
		t.Fatalf("篡改金额的签名不应通过")
	}
}

// 回调业务判定不变：returncode=='00' 才成功、商户号必须一致。
func TestPayindexHandleCallback(t *testing.T) {
	d := &PayindexDriver{}
	cfg := Config{DriverConfig: map[string]string{"api_key": "k", "member_id": "260681124"}}
	base := map[string]string{
		"memberid": "260681124", "orderid": "O1", "amount": "1.50",
		"transaction_id": "T1", "datetime": "2026-11-01 12:40:00", "returncode": "00",
	}
	data := map[string]string{
		"memberid": base["memberid"], "orderid": base["orderid"], "amount": base["amount"],
		"transaction_id": base["transaction_id"], "datetime": base["datetime"],
		"returncode": base["returncode"], "sign": signSelected(base, callbackSignFields, "k"),
	}
	if r := d.HandleCallback(data, cfg); !r.Success || r.OrderNo != "O1" {
		t.Fatalf("正常回调应成功: %+v", r)
	}
	bad := map[string]string{}
	for k2, v := range data {
		bad[k2] = v
	}
	bad["memberid"] = "999"
	if r := d.HandleCallback(bad, cfg); r.Err == "" {
		t.Fatalf("商户号不匹配应报错")
	}
}
