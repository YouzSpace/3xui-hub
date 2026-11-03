package admin

// 续签回写入站的纯函数测试（issue：TLS 入站也要能自动续签、写了 PEM 也要自动更新）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApplyCertificateToStreamOverwritesInlinePEM(t *testing.T) {
	in := []byte(`{
		"network":"ws",
		"security":"tls",
		"tlsSettings":{
			"serverName":"old.example.com",
			"certificates":[{"certificate":["OLD-CERT"],"key":["OLD-KEY"]}]
		},
		"wsSettings":{"path":"/ws"}
	}`)
	out, err := applyCertificateToStream(in, 42, "NEW-CERT", "NEW-KEY")
	if err != nil {
		t.Fatalf("applyCertificateToStream: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	tls := doc["tlsSettings"].(map[string]any)
	if got := tls["tlsCertId"]; got != float64(42) {
		t.Fatalf("tlsCertId = %v, want 42", got)
	}
	certs := tls["certificates"].([]any)
	first := certs[0].(map[string]any)
	if got := first["certificate"].([]any)[0]; got != "NEW-CERT" {
		t.Fatalf("certificate = %v, want NEW-CERT", got)
	}
	if got := first["key"].([]any)[0]; got != "NEW-KEY" {
		t.Fatalf("key = %v, want NEW-KEY", got)
	}
	// 其它字段必须原样保留（只换证书，不动传输层配置）
	if got := tls["serverName"]; got != "old.example.com" {
		t.Fatalf("serverName = %v, want old.example.com", got)
	}
	ws := doc["wsSettings"].(map[string]any)
	if got := ws["path"]; got != "/ws" {
		t.Fatalf("wsSettings.path = %v, want /ws", got)
	}
}

func TestApplyCertificateToStreamCreatesTLSSettings(t *testing.T) {
	out, err := applyCertificateToStream([]byte(`{"security":"tls"}`), 7, "C", "K")
	if err != nil {
		t.Fatalf("applyCertificateToStream: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if _, ok := doc["tlsSettings"].(map[string]any); !ok {
		t.Fatalf("tlsSettings 应被补上: %s", out)
	}
}

func TestApplyCertificateToStreamRejectsBrokenJSON(t *testing.T) {
	if _, err := applyCertificateToStream([]byte("{not json"), 1, "C", "K"); err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
}

func TestInlineCertOfStream(t *testing.T) {
	stream := []byte(`{"tlsSettings":{"certificates":[{"certificate":["LEAF","INTERMEDIATE"]}]}}`)
	if got := inlineCertOfStream(stream); got != "LEAF" {
		t.Fatalf("inlineCertOfStream = %q, want LEAF", got)
	}
	if got := inlineCertOfStream([]byte(`{"tlsSettings":{}}`)); got != "" {
		t.Fatalf("没有内联证书应返回空串，got %q", got)
	}
}

func TestNormalizePEMTextIgnoresWhitespace(t *testing.T) {
	a := "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n"
	b := "-----BEGIN CERTIFICATE----- AAA -----END CERTIFICATE-----"
	if normalizePEMText(a) != normalizePEMText(b) {
		t.Fatal("换行/空白差异不应影响 PEM 内容比对")
	}
	if !strings.Contains(normalizePEMText(a), "BEGINCERTIFICATE") {
		t.Fatal("规范化结果应去掉空白")
	}
}
