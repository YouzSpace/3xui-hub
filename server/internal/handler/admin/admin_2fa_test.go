package admin

// 管理端 2FA「开启」链路的契约测试。
//
// 背景：长期存在的一处前后端契约断裂 —— 前端 google2faEnable 只提交 {otp}，
// 后端却要求 {secret,otp}，导致「验证并启用」永远返回「请先生成密钥」，
// 2FA 根本开不起来（等于管理端只剩密码一道门）。同时 generate 只回 secret
// 不回 qr_url，前端拿空串去请求二维码服务，扫出来是一张扫不了的码。
//
// 这里把三件事钉死：
//  1. generate 必须同时给出 secret 和可直接渲染的 otpauth:// 链接；
//  2. enable 接受 {secret,otp}，校验通过时真的写库启用；
//  3. 只传 otp（历史 bug 的形态）必须被明确拒绝，不许含糊通过。

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// adminCtxKeyForTest 与 httpx 内部注入管理员用的 gin.Context 键保持一致
// （httpx.ctxAdminKey 未导出，这里按字面量对齐；一旦上游改名本测试会立刻失败，提醒同步）。
const adminCtxKeyForTest = "httpx.admin"

func withAdmin(identity *httpx.AdminIdentity, fn func(*gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(adminCtxKeyForTest, identity)
		fn(c)
	}
}

func newTestRouter(path string, h gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST(path, h)
	return r
}

func postJSON(t *testing.T, r *gin.Engine, path string, body any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (body=%s)", err, w.Body.String())
	}
	return out
}


func codeOf(out map[string]any) int {
	if f, ok := out["code"].(float64); ok {
		return int(f)
	}
	return -1
}


// ---- 用例 ----

func TestGoogle2faGenerateReturnsSecretAndOTPAuthURL(t *testing.T) {
	h := &AdminAuthRoutes{now: time.Now}
	r := newTestRouter("/admin-api/google2fa/generate",
		withAdmin(&httpx.AdminIdentity{ID: 1, Username: "zoe"},
			func(c *gin.Context) { h.Google2faGenerate(c) }))

	out := postJSON(t, r, "/admin-api/google2fa/generate", nil)
	if codeOf(out) != 0 {
		t.Fatalf("generate 应成功，实际 code=%v msg=%v", out["code"], out["msg"])
	}
	d := dataOf(t, out)

	secret, _ := d["secret"].(string)
	if secret == "" {
		t.Fatal("generate 必须返回 secret")
	}
	if len(secret) != 32 {
		t.Fatalf("secret 应为 32 字符（20 字节 base32 无填充），实际 %d：%q", len(secret), secret)
	}
	if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret); err != nil {
		t.Fatalf("secret 必须能被 NoPadding base32 解码（verifyTOTP 同口径）: %v", err)
	}

	qr, _ := d["qr_url"].(string)
	if qr == "" {
		t.Fatal("generate 必须返回 qr_url（否则前端只能拿空串生成空二维码）")
	}
	u, err := url.Parse(qr)
	if err != nil {
		t.Fatalf("qr_url 不是合法 URL: %v (%q)", err, qr)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("qr_url 必须是 otpauth://totp/…，实际 %q", qr)
	}
	q := u.Query()
	if q.Get("secret") != secret {
		t.Fatalf("qr_url 里的 secret 必须与返回的 secret 一致：url=%q secret=%q", q.Get("secret"), secret)
	}
	if q.Get("algorithm") != "SHA1" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Fatalf("TOTP 参数必须与 verifyTOTP 一致（SHA1/6/30），实际 %q", q.Encode())
	}
	if q.Get("issuer") != otpDefaultIssuer {
		t.Fatalf("未配置站点标题时 issuer 必须是项目名 %q（白标：不许硬编码品牌），实际 %q", otpDefaultIssuer, q.Get("issuer"))
	}
	// label 段保留字面冒号（otpauth 分段符），不能整体转义成 %3A
	if !strings.Contains(u.Path, ":") {
		t.Fatalf("label 应保留冒号分段符，实际 path=%q", u.Path)
	}
	if !strings.Contains(u.Path, "zoe") {
		t.Fatalf("label 应包含管理员用户名，实际 path=%q", u.Path)
	}
}

// TestGoogle2faGenerateIssuerFollowsSiteTitle 钉死白标口径：
// issuer 必须来自站点标题（客户可改），任何情况下都不得出现硬编码的客户/品牌名。
func TestGoogle2faGenerateIssuerFollowsSiteTitle(t *testing.T) {
	h := &AdminAuthRoutes{
		now: time.Now,
		lookupSiteTitle: func(context.Context) (string, error) {
			return "  客户面板 A  ", nil
		},
	}
	r := newTestRouter("/admin-api/google2fa/generate",
		withAdmin(&httpx.AdminIdentity{ID: 1, Username: "zoe"},
			func(c *gin.Context) { h.Google2faGenerate(c) }))

	out := postJSON(t, r, "/admin-api/google2fa/generate", nil)
	if codeOf(out) != 0 {
		t.Fatalf("generate 应成功，实际 code=%v msg=%v", out["code"], out["msg"])
	}
	d := dataOf(t, out)
	qr, _ := d["qr_url"].(string)
	u, err := url.Parse(qr)
	if err != nil {
		t.Fatalf("qr_url 不是合法 URL: %v (%q)", err, qr)
	}
	if got := u.Query().Get("issuer"); got != "客户面板 A" {
		t.Fatalf("issuer 必须取站点标题（去首尾空格），实际 %q", got)
	}
	if iss, _ := d["issuer"].(string); iss != "客户面板 A" {
		t.Fatalf("data.issuer 必须与 qr_url 一致，实际 %q", iss)
	}
	if strings.Contains(qr, "无界云") {
		t.Fatalf("qr_url 里不允许出现硬编码品牌名：%q", qr)
	}
}

func TestGoogle2faEnable_RequiresSecret(t *testing.T) {
	// h.db 刻意留 nil：一旦代码走到写库会 panic，从而证明请求在写库前就被拒了
	h := &AdminAuthRoutes{now: time.Now}
	r := newTestRouter("/admin-api/google2fa/enable",
		withAdmin(&httpx.AdminIdentity{ID: 1, Username: "zoe"},
			func(c *gin.Context) { h.Google2faEnable(c) }))

	out := postJSON(t, r, "/admin-api/google2fa/enable", map[string]any{"otp": "123456"})
	if codeOf(out) == 0 {
		t.Fatal("只传 otp 不应成功启用 2FA")
	}
	if !strings.Contains(msgOf(out), "请先生成密钥") {
		t.Fatalf("只传 otp 应提示「请先生成密钥」，实际 %q", msgOf(out))
	}
}

func TestGoogle2faEnable_RejectsMalformedSecret(t *testing.T) {
	h := &AdminAuthRoutes{now: time.Now}
	r := newTestRouter("/admin-api/google2fa/enable",
		withAdmin(&httpx.AdminIdentity{ID: 1, Username: "zoe"},
			func(c *gin.Context) { h.Google2faEnable(c) }))

	// 0/1/8/9 不在 base32 字母表内
	out := postJSON(t, r, "/admin-api/google2fa/enable",
		map[string]any{"secret": "0189!!!!", "otp": "123456"})
	if codeOf(out) == 0 {
		t.Fatal("非法密钥不应通过")
	}
	if !strings.Contains(msgOf(out), "密钥格式") {
		t.Fatalf("非法密钥应提示格式问题，实际 %q", msgOf(out))
	}
}

// TestGoogle2faEnable_AcceptsGeneratedSecretAndOTP 走完整闭环：
// generate 拿 secret → 用同一算法算出当前验证码 → enable 必须成功并写库。
//
// 这是最关键的回归：同时证明「前端传 secret」与「后端校验 secret」同口径，
// 也证明 buildOTPAuthURL 里的 SHA1/6/30 与 verifyTOTP 真的一致。
func TestGoogle2faEnable_AcceptsGeneratedSecretAndOTP(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	h := &AdminAuthRoutes{now: func() time.Time { return now }}

	gen := newTestRouter("/gen", withAdmin(&httpx.AdminIdentity{ID: 1, Username: "zoe"},
		func(c *gin.Context) { h.Google2faGenerate(c) }))
	out := postJSON(t, gen, "/gen", nil)
	secret, _ := dataOf(t, out)["secret"].(string)
	if secret == "" {
		t.Fatal("generate 未返回 secret")
	}

	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("secret 解码失败: %v", err)
	}
	code := totpCode(key, now.Unix()/30)

	rec := &execRecorder{}
	h.db = openFakeDB(t, rec)

	en := newTestRouter("/en", withAdmin(&httpx.AdminIdentity{ID: 1, Username: "zoe"},
		func(c *gin.Context) { h.Google2faEnable(c) }))
	out = postJSON(t, en, "/en", map[string]any{"secret": secret, "otp": code})
	if codeOf(out) != 0 {
		t.Fatalf("带正确 secret+otp 应启用成功，实际 code=%v msg=%v", out["code"], out["msg"])
	}
	if len(rec.calls) != 1 {
		t.Fatalf("应恰好一次写库，实际 %d 次", len(rec.calls))
	}
	call := rec.calls[0]
	if !strings.Contains(call.query, "UPDATE admins SET google2fa_enabled = 1, google2fa_secret = ?") {
		t.Fatalf("enable 的更新语句不符合预期: %q", call.query)
	}
	if len(call.args) == 0 || fmt.Sprint(call.args[0]) != secret {
		t.Fatalf("落库 secret 不对：%v（期望 %q）", call.args, secret)
	}
}

// ---- 假驱动：只用于精确断言「谁写了什么」 ----

type execCall struct {
	query string
	args  []any
}

type execRecorder struct {
	mu    sync.Mutex
	calls []execCall
}

var (
	fakeDBMu     sync.Mutex
	fakeDBID     int
	fakeDBRecs   = map[string]*execRecorder{}
	admin2faOnce sync.Once
)

func init() {
	admin2faOnce.Do(func() { sql.Register("admin2fatest", fake2faDriver{}) })
}

func openFakeDB(t *testing.T, rec *execRecorder) *sql.DB {
	t.Helper()
	fakeDBMu.Lock()
	fakeDBID++
	dsn := fmt.Sprint(fakeDBID)
	fakeDBRecs[dsn] = rec
	fakeDBMu.Unlock()

	db, err := sql.Open("admin2fatest", dsn)
	if err != nil {
		t.Fatalf("打开假库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fake2faDriver struct{}

func (fake2faDriver) Open(dsn string) (driver.Conn, error) {
	fakeDBMu.Lock()
	rec := fakeDBRecs[dsn]
	fakeDBMu.Unlock()
	if rec == nil {
		return nil, fmt.Errorf("未知假库 dsn=%s", dsn)
	}
	return &fake2faConn{rec: rec}, nil
}

type fake2faConn struct{ rec *execRecorder }

func (c *fake2faConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("不支持 Prepare") }
func (c *fake2faConn) Close() error                       { return nil }
func (c *fake2faConn) Begin() (driver.Tx, error)          { return nil, fmt.Errorf("不支持事务") }

func (c *fake2faConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals := make([]any, 0, len(args))
	for _, a := range args {
		vals = append(vals, a.Value)
	}
	c.rec.mu.Lock()
	c.rec.calls = append(c.rec.calls, execCall{query: query, args: vals})
	c.rec.mu.Unlock()
	return driver.RowsAffected(1), nil
}

func (c *fake2faConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	return nil, fmt.Errorf("本测试不应发起查询: %s", query)
}
