package payment

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// submit.php 返回 HTML 跳转页（实测某 wwspay 系平台）时，应提取跳转地址作为 pay_url。
func TestEpayPayExtractsRedirectFromScript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<script>location.href='https://pay.example.com/index/zfb/zfbpay?order_sn=2026110121212527345914';</script>`))
	}))
	defer srv.Close()

	d := &EpayV1Driver{}
	res := d.Pay(context.Background(), PayRequest{
		Order:     Order{OrderNo: "O1", Amount: "1.00", ProductName: "x"},
		NotifyURL: "https://panel.example/api/payment/notify",
		ReturnURL: "https://panel.example/",
	}, Config{DriverConfig: map[string]string{
		"gateway":  srv.URL,
		"member_id": "1",
		"api_key":  "k",
	}})
	if !res.Success || res.PayURL != "https://pay.example.com/index/zfb/zfbpay?order_sn=2026110121212527345914" {
		t.Fatalf("应从 script location.href 提取支付链接: %+v", res)
	}
}

// meta refresh 变体同样要能提取。
func TestEpayPayExtractsRedirectFromMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><meta http-equiv="refresh" content="0;url=https://pay.example.com/m/qr?no=123"></head><body></body></html>`))
	}))
	defer srv.Close()

	d := &EpayV1Driver{}
	res := d.Pay(context.Background(), PayRequest{
		Order: Order{OrderNo: "O2", Amount: "1.00"},
	}, Config{DriverConfig: map[string]string{
		"gateway": srv.URL, "member_id": "1", "api_key": "k",
	}})
	if !res.Success || res.PayURL != "https://pay.example.com/m/qr?no=123" {
		t.Fatalf("应从 meta refresh 提取支付链接: %+v", res)
	}
}

// 标准 JSON 响应路径行为不变（url 字段优先）。
func TestEpayPayJSONStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1,"url":"https://pay.example.com/jsonlink"}`))
	}))
	defer srv.Close()

	d := &EpayV1Driver{}
	res := d.Pay(context.Background(), PayRequest{
		Order: Order{OrderNo: "O3", Amount: "1.00"},
	}, Config{DriverConfig: map[string]string{
		"gateway": srv.URL, "member_id": "1", "api_key": "k",
	}})
	if !res.Success || res.PayURL != "https://pay.example.com/jsonlink" {
		t.Fatalf("JSON 路径应不受影响: %+v", res)
	}
}

// 既非 JSON 也无可提取跳转的响应：仍按临时故障（Err）处理，不得给出成功。
func TestEpayPayGarbageStillErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`internal error`))
	}))
	defer srv.Close()

	d := &EpayV1Driver{}
	res := d.Pay(context.Background(), PayRequest{
		Order: Order{OrderNo: "O4", Amount: "1.00"},
	}, Config{DriverConfig: map[string]string{
		"gateway": srv.URL, "member_id": "1", "api_key": "k",
	}})
	if res.Success || res.Err == nil {
		t.Fatalf("无跳转的垃圾响应应保持 Err 语义: %+v", res)
	}
}
