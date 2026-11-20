package api

// 注册邮箱域名白名单 / 一次性邮箱拦截的单测。
//
// 背景（真实事故）：`register_email_verify=1` 确实在验 6 位验证码，但
// `euploading.com` / `mailinator.com` / `smtp.is` 这类一次性收信服务能直接收到码，
// 验证码形同虚设。实测 226 个用户里唯一的非主流域名就是 euploading.com，
// 时间线是「先发码 → 再注册成功」—— 所以必须在**域名维度**拦，而不是加验证码。
//
// 覆盖三条硬口径：
//  1. 未配置白名单 → 完全放行（升级不能把存量企业邮箱挡在门外）；
//  2. 配置白名单 → 子域自动匹配、名单外一律拒；
//  3. 黑名单优先于白名单（防「误加白名单」放行一次性邮箱）。
//  4. 两个入口（注册 / 发码）口径一致：一次性邮箱连码都不发。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// ---- 1. 纯函数：domainOf / splitDomainList / domainMatches ----

func TestDomainOf(t *testing.T) {
	cases := map[string]string{
		"a@qq.com":            "qq.com",
		"a@VIP.QQ.COM":        "vip.qq.com",
		"a@Mail.Example.COM.": "mail.example.com", // 去尾部点
		"no-at-sign":          "",
		"@leading.com":        "",
		"trailing@":           "",
		"multi@at@qq.com":     "qq.com", // LastIndexByte，取最后一段
	}
	for in, want := range cases {
		if got := domainOf(in); got != want {
			t.Errorf("domainOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitDomainList_NormalizesGarbage(t *testing.T) {
	raw := " qq.com , @163.com;*.gmail.com,  ,example.com. "
	got := splitDomainList(raw)
	want := []string{"qq.com", "163.com", "gmail.com", "example.com"}
	if len(got) != len(want) {
		t.Fatalf("splitDomainList 数量不对: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitDomainList[%d] = %q, want %q（全部: %v）", i, got[i], want[i], got)
		}
	}
}

func TestDomainMatches_Subdomain(t *testing.T) {
	yes := [][2]string{
		{"qq.com", "qq.com"},
		{"vip.qq.com", "qq.com"},
		{"a.b.c.qq.com", "qq.com"},
	}
	no := [][2]string{
		{"notqq.com", "qq.com"},   // 不能是后缀匹配
		{"qq.com.evil.com", "qq.com"}, // 反向包含要拒
		{"", "qq.com"},
	}
	for _, c := range yes {
		if !domainMatches(c[0], c[1]) {
			t.Errorf("domainMatches(%q,%q) 应为 true", c[0], c[1])
		}
	}
	for _, c := range no {
		if domainMatches(c[0], c[1]) {
			t.Errorf("domainMatches(%q,%q) 应为 false", c[0], c[1])
		}
	}
}

// ---- 2. emailDomainAllowed 的策略矩阵 ----

func TestEmailDomainAllowed_PolicyMatrix(t *testing.T) {
	tests := []struct {
		name   string
		cfg    map[string]string
		email  string
		expect bool
	}{
		{
			name:   "两个都空=不限制（升级兼容）",
			cfg:    map[string]string{},
			email:  "anything@euploading.com",
			expect: true,
		},
		{
			name:   "白名单内放行",
			cfg:    map[string]string{cfgRegisterEmailAllowedDomains: "qq.com,163.com"},
			email:  "a@163.com",
			expect: true,
		},
		{
			name:   "白名单子域自动匹配（vip.qq.com 靠 qq.com 放行）",
			cfg:    map[string]string{cfgRegisterEmailAllowedDomains: "qq.com"},
			email:  "a@vip.qq.com",
			expect: true,
		},
		{
			name:   "白名单外拒绝（一次性邮箱被挡）",
			cfg:    map[string]string{cfgRegisterEmailAllowedDomains: "qq.com,163.com"},
			email:  "x@euploading.com",
			expect: false,
		},
		{
			name: "黑名单优先于白名单",
			cfg: map[string]string{
				cfgRegisterEmailAllowedDomains: "qq.com,euploading.com",
				cfgRegisterEmailBlockedDomains: "euploading.com",
			},
			email:  "x@euploading.com",
			expect: false,
		},
		{
			name:   "只有黑名单也能拦（无需配白名单）",
			cfg:    map[string]string{cfgRegisterEmailBlockedDomains: "mailinator.com"},
			email:  "x@mailinator.com",
			expect: false,
		},
		{
			name:   "黑名单不影响其它域名",
			cfg:    map[string]string{cfgRegisterEmailBlockedDomains: "mailinator.com"},
			email:  "x@qq.com",
			expect: true,
		},
		{
			name:   "邮箱格式非法时放行（交给格式校验报更准的文案）",
			cfg:    map[string]string{cfgRegisterEmailAllowedDomains: "qq.com"},
			email:  "not-an-email",
			expect: true,
		},
		{
			name:   "大小写不敏感",
			cfg:    map[string]string{cfgRegisterEmailAllowedDomains: "qq.com"},
			email:  "A@QQ.COM",
			expect: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := emailDomainAllowed(tc.cfg, tc.email); got != tc.expect {
				t.Fatalf("emailDomainAllowed(%v, %q) = %v, want %v", tc.cfg, tc.email, got, tc.expect)
			}
		})
	}
}

func TestDefaultAllowedEmailDomainsCSV_HasCoreProviders(t *testing.T) {
	csv := DefaultAllowedEmailDomainsCSV()
	for _, must := range []string{"qq.com", "163.com", "gmail.com", "outlook.com", "icloud.com"} {
		if !strings.Contains(csv, must) {
			t.Fatalf("默认白名单应包含 %q，实际: %s", must, csv)
		}
	}
}

// ---- 3. 两个真实入口：一次性邮箱必须被拦 ----

// domainTestHandler 造一个只够跑「域名闸门」的 Handler：
// 配置里白名单只放 qq.com，黑名单放 euploading.com。
func domainTestHandler(t *testing.T) *Handler {
	t.Helper()
	f := newRateFixtureWith(t, map[string]string{
		"register_email_verify":          "1",
		"rate_code_interval":             "0",
		"rate_code_per_day":              "100000",
		"rate_code_ip_hourly":            "100000",
		"rate_register_ip_hourly":        "100000",
		cfgRegisterEmailAllowedDomains: "qq.com",
		cfgRegisterEmailBlockedDomains: "euploading.com",
	})
	return f.h
}

func postJSONTo(t *testing.T, h gin.HandlerFunc, path, body, ip string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.POST(path, h)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestRegister_RejectsDisposableEmailDomain(t *testing.T) {
	h := domainTestHandler(t)
	// 带完整验证码字段也没用：域名闸门在验证码之前就拒
	_, out := postJSONTo(t, h.handleRegister, "/api/register",
		`{"email":"zxgnuydvp4f2@euploading.com","password":"pw123456","email_code":"123456"}`,
		"198.51.100.20")
	msg, _ := out["msg"].(string)
	if msg != msgEmailDomainDenied {
		t.Fatalf("注册应返回域名拒绝文案，实际 msg=%q（body=%v）", msg, out)
	}
}

func TestRegister_AllowsWhitelistedDomain(t *testing.T) {
	h := domainTestHandler(t)
	// qq.com 在白名单 → 越过域名闸门，随后因 SMTP/验证码等后续原因失败，
	// 但绝不能是「域名被拒」这条文案。
	_, out := postJSONTo(t, h.handleRegister, "/api/register",
		`{"email":"someone@qq.com","password":"pw123456"}`,
		"198.51.100.21")
	if msg, _ := out["msg"].(string); msg == msgEmailDomainDenied {
		t.Fatalf("qq.com 属于白名单，不应被判域名拒绝")
	}
}

func TestEmailVerifySend_RejectsDisposableBeforeSending(t *testing.T) {
	h := domainTestHandler(t)
	// 一次性邮箱：连码都不该发（避免「收到码→填码→注册才被拒」）
	_, out := postJSONTo(t, h.handleEmailVerifySend, "/api/email-verify/send",
		`{"email":"x@euploading.com"}`, "198.51.100.22")
	if msg, _ := out["msg"].(string); msg != msgEmailDomainDenied {
		t.Fatalf("发码接口应返回域名拒绝文案，实际 msg=%q（body=%v）", msg, out)
	}
}
