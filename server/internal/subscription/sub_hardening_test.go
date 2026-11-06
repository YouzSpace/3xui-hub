package subscription

// 订阅接口加固（sec-11）单测。
//
// 约束：**不引任何新依赖**。因此：
//   - HTTP 层用 net/http/httptest + gin（已在 go.mod）；
//   - SQL 层直接跑真实 MySQL（本机 3306），不可用时 skip —— 因为断言的是
//     「MySQL 的 *_ci collation 会让大小写命中」这个**数据库行为**，
//     用假 driver 打桩只会测到自己的字符串比较，等于没测。
//
// 变异测试靶子（去掉即必须变红）：
//   - store.go 的 `token = BINARY ?` → 大小写用例红；
//   - handler.renderCached 的缓存分支 → 缓存用例红。

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	_ "github.com/go-sql-driver/mysql"
)

// ───────────────────────── 测试替身 ─────────────────────────

// fakeRepo 是最小内存 Repository：只实现 handler 路径会用到的 SiteConfigs / UserByToken。
// 其余方法返回零值（测试不走那些路径）。
type fakeRepo struct {
	users   map[string]*User // 键为**原样** token；UserByToken 做逐字节比较，复刻 BINARY 语义
	configs map[string]string

	userCalls atomic.Int64
}

func (f *fakeRepo) SiteConfigs(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range f.configs {
		out[k] = v
	}
	return out, nil
}

// UserByToken 复刻 SQLRepository 加 BINARY 之后的语义：完全按字节相等。
func (f *fakeRepo) UserByToken(_ context.Context, token string) (*User, error) {
	f.userCalls.Add(1)
	u, ok := f.users[token]
	if !ok {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}

func (f *fakeRepo) PlanByID(context.Context, int64) (*Plan, error) { return nil, nil }
func (f *fakeRepo) SubscribableNodes(context.Context, string) ([]Node, error) {
	return nil, nil
}
func (f *fakeRepo) NodeInboundIDs(context.Context, int64, string) ([]int64, error) {
	return nil, nil
}
func (f *fakeRepo) XrayInbounds(context.Context, int64) ([]XrayInbound, error) {
	return nil, nil
}
func (f *fakeRepo) ThirdPartySubs(context.Context, []int64) ([]ThirdPartySub, error) {
	return nil, nil
}
func (f *fakeRepo) HasEnabledOnlineXrayNodeWithoutHost(context.Context) (bool, error) {
	return false, nil
}
func (f *fakeRepo) PanelLinks(context.Context, Node, string) ([]string, error) {
	return nil, nil
}
func (f *fakeRepo) PanelInbounds(context.Context, Node) ([]PanelInbound, error) {
	return nil, nil
}

// newTestHandler 构造一个「限流/缓存显式指定、时钟可控」的 handler。
// 不走 NewHandler，避免受真实环境变量影响。
func newTestHandler(repo Repository, rateLimit, cacheSeconds int64, now func() time.Time) *Handler {
	h := &Handler{Service: NewService(repo), Repo: repo, now: now}
	h.limiter = newTokenLimiter(rateLimit, func() time.Time { return h.now() })
	h.cache = newRenderCache(cacheSeconds, func() time.Time { return h.now() })
	h.generator = func(context.Context, *User, Format) (string, error) { return "", nil }
	return h
}

// testUser 是一份「可用」的用户（enabled / 有套餐 / 未过期 / 未超量）。
func testUser(t *testing.T, token string) *User {
	t.Helper()
	plan := int64(1)
	return &User{
		ID:           7,
		Token:        token,
		UUID:         "11111111-2222-3333-4444-555555555555",
		Protocol:     "vless",
		PlanID:       &plan,
		TrafficLimit: 100 << 30,
		TrafficUsed:  1 << 30,
		Enabled:      true,
	}
}

// serveShow 发一次 GET /api/sub/:token（可选 query）并返回响应。
func serveShow(t *testing.T, h *Handler, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r)
	url := "/api/sub/" + token
	if query != "" {
		url += "?" + query
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w
}

// ───────────────────────── 1. 大小写敏感（store，实连 MySQL） ─────────────────────────

// openTestMySQL 连本机 MySQL（root/空密码，可用 SEC11_MYSQL_DSN 覆盖）。
// 连不上就 skip：这里要验证的是 MySQL collation 的真实行为，不能打桩。
func openTestMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SEC11_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?timeout=3s"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("打不开 MySQL 连接（%v），跳过 store 集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("连不上 MySQL（%v），跳过 store 集成测试", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mysqlDSN 由 SEC11_MYSQL_DSN（未设置时用本机默认值）派生一个带默认库的 DSN。
//
// 测试库名固定为 ch_sec11_test：DDL 用 <db>.users，连接默认库同名，
// 这样 store.go 里不带库名的 SQL 才能跑。
func mysqlDSN(schemaName string) string {
	dsn := os.Getenv("SEC11_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/"
	}
	slash := strings.LastIndex(dsn, "/")
	q := strings.Index(dsn, "?")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := ""
	if q > slash {
		tail = dsn[q:]
	}
	return head + schemaName + tail
}

// openTestMySQLDSN 用指定 DSN 建连；连不上就 skip。
func openTestMySQLDSN(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("打不开 MySQL 连接（%v），跳过 store 集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("连不上 MySQL（%v），跳过 store 集成测试", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestUserByTokenIsCaseSensitive 用真实的 utf8mb4_unicode_ci 表验证：
// 只有**逐字节相同**的 token 才能查到用户。
//
// 变异测试：把 store.go 的 `token = BINARY ?` 改回 `token = ?`，本用例必红。
func TestUserByTokenIsCaseSensitive(t *testing.T) {
	const schemaName = "ch_sec11_test"

	// 先建库，再以「默认库 = 该库」重新连接：SQLRepository 的 SQL 都是不带库名的，
	// 生产环境靠 DSN 的 dbname 定位，这里同样复刻。
	admin := openTestMySQL(t)
	mustExec(t, admin, "DROP DATABASE IF EXISTS "+schemaName)
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + schemaName) })
	mustExec(t, admin, "CREATE DATABASE "+schemaName+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")

	db := openTestMySQLDSN(t, mysqlDSN(schemaName))
	// 与生产 schema/schema.sql 的 users 表逐列对齐（含 users_token_unique + _ci collation）
	mustExec(t, db, "CREATE TABLE "+schemaName+".users ("+
		"id bigint unsigned NOT NULL AUTO_INCREMENT,"+
		"token varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,"+
		"uuid char(36) COLLATE utf8mb4_unicode_ci NOT NULL,"+
		"protocol varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'vless',"+
		"plan_id bigint unsigned DEFAULT NULL,"+
		"traffic_limit bigint NOT NULL DEFAULT '0',"+
		"traffic_used bigint NOT NULL DEFAULT '0',"+
		"monthly_traffic_used bigint NOT NULL DEFAULT '0',"+
		"monthly_traffic_limit bigint NOT NULL DEFAULT '0',"+
		"next_traffic_reset_at datetime DEFAULT NULL,"+
		"expired_at datetime DEFAULT NULL,"+
		"enabled tinyint(1) NOT NULL DEFAULT '1',"+
		"PRIMARY KEY (id), UNIQUE KEY users_token_unique (token)"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci")

	const realToken = "AbCdEf123456GhIj"
	mustExec(t, db, "INSERT INTO "+schemaName+".users (token, uuid) VALUES (?, ?)",
		realToken, "11111111-2222-3333-4444-555555555555")

	repo := NewSQLRepository(db, nil)
	repo.DB = db
	ctx := context.Background()

	// 原样 token 必须查得到（确认 BINARY 没有把正常路径打坏）
	u, err := repo.UserByToken(ctx, realToken)
	if err != nil {
		t.Fatalf("原样 token 查询出错: %v", err)
	}
	if u == nil {
		t.Fatalf("原样 token %q 应该能查到用户，实际查不到", realToken)
	}

	// 各种大小写变体必须查不到
	variants := map[string]string{
		"全小写":     strings.ToLower(realToken),
		"全大写":     strings.ToUpper(realToken),
		"仅前缀大写":   "ABCDEF123456GhIj",
		"仅首字母大写":  "abCdEf123456GhIj",
		"翻转一个字符":  "AbCdEf123456GhIJ",
	}
	for name, tok := range variants {
		tok := tok
		t.Run(name, func(t *testing.T) {
			if tok == realToken {
				t.Fatalf("用例写错了：变体与原值相同")
			}
			u, err := repo.UserByToken(ctx, tok)
			if err != nil {
				t.Fatalf("查询 %q 出错: %v", tok, err)
			}
			if u != nil {
				t.Fatalf("token %q 与 %q 仅大小写不同，必须查不到，实际查到 user id=%d (token=%q)",
					tok, realToken, u.ID, u.Token)
			}
		})
	}
}

// TestShowRejectsUppercaseTokenEndToEnd 走完整 HTTP 链路（gin → SQLRepository → MySQL）：
// 大写变体必须拿到 X-CH-Code=404，且不能进到生成阶段。
//
// 与 TestUserByTokenIsCaseSensitive 的区别：那条只测仓储层，这条证明**接口层**也堵住了。
func TestShowRejectsUppercaseTokenEndToEnd(t *testing.T) {
	const schemaName = "ch_sec11_e2e"

	admin := openTestMySQL(t)
	mustExec(t, admin, "DROP DATABASE IF EXISTS "+schemaName)
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + schemaName) })
	mustExec(t, admin, "CREATE DATABASE "+schemaName+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")

	db := openTestMySQLDSN(t, mysqlDSN(schemaName))
	mustExec(t, db, "CREATE TABLE users ("+
		"id bigint unsigned NOT NULL AUTO_INCREMENT,"+
		"token varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,"+
		"uuid char(36) COLLATE utf8mb4_unicode_ci NOT NULL,"+
		"protocol varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'vless',"+
		"plan_id bigint unsigned DEFAULT NULL,"+
		"traffic_limit bigint NOT NULL DEFAULT '0',"+
		"traffic_used bigint NOT NULL DEFAULT '0',"+
		"monthly_traffic_used bigint NOT NULL DEFAULT '0',"+
		"monthly_traffic_limit bigint NOT NULL DEFAULT '0',"+
		"next_traffic_reset_at datetime DEFAULT NULL,"+
		"expired_at datetime DEFAULT NULL,"+
		"enabled tinyint(1) NOT NULL DEFAULT '1',"+
		"PRIMARY KEY (id), UNIQUE KEY users_token_unique (token)"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci")
	// handler 每次请求都要读 site_configs（格式开关/标题），缺表会 500 而不是走到生成阶段。
	mustExec(t, db, "CREATE TABLE site_configs ("+
		"id bigint unsigned NOT NULL AUTO_INCREMENT,"+
		"`key` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,"+
		"`value` text COLLATE utf8mb4_unicode_ci,"+
		"created_at timestamp NULL DEFAULT NULL,"+
		"updated_at timestamp NULL DEFAULT NULL,"+
		"PRIMARY KEY (id), UNIQUE KEY site_configs_key_unique (`key`)"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci")

	const realToken = "MiXeDcAsE9876"
	mustExec(t, db, "INSERT INTO users (token, uuid) VALUES (?, ?)",
		realToken, "11111111-2222-3333-4444-555555555555")

	repo := NewSQLRepository(db, nil)
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	h := newTestHandler(repo, 0, 0, func() time.Time { return fixed })
	var genCalls atomic.Int64
	h.generator = func(context.Context, *User, Format) (string, error) {
		genCalls.Add(1)
		return "SHOULD-NOT-HAPPEN", nil
	}

	// 大写变体 → 404（HTTP 200 + X-CH-Code: 404，保持项目契约）
	for _, variant := range []string{
		strings.ToUpper(realToken),
		strings.ToLower(realToken),
	} {
		w := serveShow(t, h, variant, "")
		if w.Code != http.StatusOK {
			t.Fatalf("token=%q 应返回 HTTP 200（业务码在 X-CH-Code 里），实际 %d", variant, w.Code)
		}
		if got := w.Header().Get("X-CH-Code"); got != "404" {
			t.Fatalf("token=%q 大小写不符，必须 404，实际 X-CH-Code=%q 正文=%q",
				variant, got, w.Body.String())
		}
	}
	if got := genCalls.Load(); got != 0 {
		t.Fatalf("大小写不符的请求不得进入生成阶段，实际调了 %d 次 Generate", got)
	}

	// 原样 token → 进入生成阶段（证明链路本身是通的，上面的 404 不是环境问题）
	w := serveShow(t, h, realToken, "")
	if got := w.Header().Get("X-CH-Code"); got != "0" {
		t.Fatalf("原样 token 应成功（X-CH-Code=0），实际 %q 正文=%q", got, w.Body.String())
	}
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("原样 token 应生成 1 次，实际 %d 次", got)
	}
}

// TestUserByTokenSQLUsesBinary 是不依赖数据库的守卫：
// SQL 文本里必须带 BINARY，防止有人「顺手简化」掉。
//
// 该用例与 TestUserByTokenIsCaseSensitive 互补 —— 后者需要 MySQL，前者任何环境都能跑。
func TestUserByTokenSQLUsesBinary(t *testing.T) {
	db, err := sql.Open("mysql", "root:@tcp(127.0.0.1:1)/?timeout=1ms")
	if err != nil {
		t.Fatalf("构造 sql.DB 失败: %v", err)
	}
	defer db.Close()

	repo := NewSQLRepository(db, nil)
	// 用一个必然失败的连接也拿得到 SQL：QueryRowContext 会把错误延后到 Scan。
	_, _ = repo.UserByToken(context.Background(), "x")
	// 直接断言实现细节：源码里必须是 `token = BINARY ?`。
	// （这里读源码而不是发请求，正是为了在没有 MySQL 的 CI 上也能守住这行。）
	src, err := readStoreSource()
	if err != nil {
		t.Fatalf("读取 store.go 失败: %v", err)
	}
	if !strings.Contains(src, "token = BINARY ?") {
		t.Fatalf("store.go 的 UserByToken 必须使用 `token = BINARY ?`（大小写敏感），当前源码里没有")
	}
}

// readStoreSource 读取本包 store.go 源码（仅用于上面的守卫用例）。
func readStoreSource() (string, error) {
	b, err := os.ReadFile("store.go")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("执行 SQL 失败 (%.120s): %v", query, err)
	}
}

// ───────────────────────── 2. 限流 ─────────────────────────

// TestRateLimitReturns429AfterQuota 验证：配额内 200，越过配额 429 + Retry-After。
func TestRateLimitReturns429AfterQuota(t *testing.T) {
	const limit = 3
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, limit, 0, func() time.Time { return fixed })

	for i := 1; i <= limit; i++ {
		w := serveShow(t, h, "tok", "")
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求（配额内）应 200，实际 %d，正文=%q", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-CH-Code"); got != "0" {
			t.Fatalf("第 %d 次请求 X-CH-Code 应为 0，实际 %q", i, got)
		}
	}

	w := serveShow(t, h, "tok", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次请求应 429，实际 %d，正文=%q", limit+1, w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-CH-Code"); got != "429" {
		t.Fatalf("429 响应 X-CH-Code 应为 429，实际 %q", got)
	}
	if got := w.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Fatalf("429 响应必须带非零 Retry-After，实际 %q", got)
	}
	if !strings.Contains(w.Body.String(), "[429]") {
		t.Fatalf("429 正文应含 [429]，实际 %q", w.Body.String())
	}

	// 超限请求不应再打数据库（限流在最前面）
	if repo.userCalls.Load() != limit {
		t.Fatalf("超限请求不应继续查库：期望 UserByToken 被调 %d 次，实际 %d 次",
			limit, repo.userCalls.Load())
	}
}

// TestRateLimitWindowResetsAfterHour 验证窗口按「首请求起算满 1 小时」重置。
func TestRateLimitWindowResetsAfterHour(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 2, 0, func() time.Time { return now })

	for i := 0; i < 2; i++ {
		if w := serveShow(t, h, "tok", ""); w.Code != http.StatusOK {
			t.Fatalf("配额内第 %d 次应 200，实际 %d", i+1, w.Code)
		}
	}
	if w := serveShow(t, h, "tok", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应 429，实际 %d", w.Code)
	}

	now = now.Add(time.Hour + time.Second) // 越过窗口
	if w := serveShow(t, h, "tok", ""); w.Code != http.StatusOK {
		t.Fatalf("窗口重置后应恢复 200，实际 %d", w.Code)
	}
}

// TestRateLimitIsPerToken 验证限流按 token 独立计数。
func TestRateLimitIsPerToken(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{
		"tokA": testUser(t, "tokA"),
		"tokB": testUser(t, "tokB"),
	}}
	h := newTestHandler(repo, 1, 0, func() time.Time { return fixed })

	if w := serveShow(t, h, "tokA", ""); w.Code != http.StatusOK {
		t.Fatalf("tokA 第 1 次应 200，实际 %d", w.Code)
	}
	if w := serveShow(t, h, "tokA", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("tokA 第 2 次应 429，实际 %d", w.Code)
	}
	if w := serveShow(t, h, "tokB", ""); w.Code != http.StatusOK {
		t.Fatalf("tokB 不应受 tokA 影响，应 200，实际 %d", w.Code)
	}
}

// TestRateLimitDisabledWhenZero 验证 SUB_RATE_LIMIT_PER_HOUR=0 时关闭限流。
func TestRateLimitDisabledWhenZero(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 0, 0, func() time.Time { return fixed })
	if h.limiter != nil {
		t.Fatalf("limit=0 时 limiter 应为 nil（关闭）")
	}
	for i := 0; i < 10; i++ {
		if w := serveShow(t, h, "tok", ""); w.Code != http.StatusOK {
			t.Fatalf("关闭限流后第 %d 次应 200，实际 %d", i+1, w.Code)
		}
	}
}

// TestEnvInt64 覆盖环境变量解析口径。
func TestEnvInt64(t *testing.T) {
	cases := []struct {
		raw  string
		def  int64
		want int64
	}{
		{"", 60, 60},       // 未设置
		{"0", 60, 0},       // 0 是合法值（关闭）
		{"90", 60, 90},     // 正常值
		{" 120 ", 60, 120}, // 两侧空白
		{"abc", 60, 60},    // 非法
		{"-5", 60, 60},     // 负数按非法处理
	}
	for _, c := range cases {
		const key = "SEC11_TEST_ENV_INT64"
		t.Setenv(key, c.raw)
		if got := envInt64(key, c.def); got != c.want {
			t.Fatalf("envInt64(%q=%q, %d) = %d，期望 %d", key, c.raw, c.def, got, c.want)
		}
	}
}

// ───────────────────────── 3. 缓存 ─────────────────────────

// TestCacheHitsGenerateOnlyOnce 验证：同一 token 连续请求，Generate 只被调 1 次。
//
// 变异测试：把 handler.renderCached 的缓存分支去掉（直接调 generator），本用例必红。
func TestCacheHitsGenerateOnlyOnce(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 0, 300, func() time.Time { return fixed })

	var genCalls atomic.Int64
	h.generator = func(context.Context, *User, Format) (string, error) {
		genCalls.Add(1)
		return "SUBSCRIBE-BODY", nil
	}

	const n = 10
	for i := 0; i < n; i++ {
		w := serveShow(t, h, "tok", "")
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应 200，实际 %d，正文=%q", i+1, w.Code, w.Body.String())
		}
		if got := w.Body.String(); got != "SUBSCRIBE-BODY" {
			t.Fatalf("第 %d 次正文应为缓存内容，实际 %q", i+1, got)
		}
	}
	if got := genCalls.Load(); got != 1 {
		t.Fatalf("缓存开启时 Generate 应只被调 1 次，实际 %d 次", got)
	}
}

// TestCacheKeyIncludesFormat 验证缓存键含 format：
// 同 token 的 clash / base64 不能互相串（三种格式输出不同）。
func TestCacheKeyIncludesFormat(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{
		users:   map[string]*User{"tok": testUser(t, "tok")},
		configs: map[string]string{"sub_clash_enabled": "1", "sub_singbox_enabled": "1"},
	}
	h := newTestHandler(repo, 0, 300, func() time.Time { return fixed })

	var mu sync.Mutex
	seen := map[Format]int{}
	h.generator = func(_ context.Context, _ *User, f Format) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		seen[f]++
		return "BODY-" + string(f), nil
	}

	// 先 base64，再 clash —— 顺序刻意反过来，专门抓「只按 token 做键」
	if w := serveShow(t, h, "tok", ""); w.Body.String() != "BODY-base64" {
		t.Fatalf("base64 正文不对：%q", w.Body.String())
	}
	if w := serveShow(t, h, "tok", "clash=1"); w.Body.String() != "BODY-clash" {
		t.Fatalf("clash 不应命中 base64 的缓存：%q", w.Body.String())
	}
	if w := serveShow(t, h, "tok", "singbox=1"); w.Body.String() != "BODY-singbox" {
		t.Fatalf("singbox 不应命中前两者缓存：%q", w.Body.String())
	}
	// 再各拉一次，应全部命中，不再新增生成
	_ = serveShow(t, h, "tok", "")
	_ = serveShow(t, h, "tok", "clash=1")
	_ = serveShow(t, h, "tok", "singbox=1")

	mu.Lock()
	defer mu.Unlock()
	for _, f := range []Format{FormatBase64, FormatClash, FormatSingbox} {
		if seen[f] != 1 {
			t.Fatalf("format %q 应只生成 1 次，实际 %d 次", f, seen[f])
		}
	}
}

// TestCacheExpiresAfterTTL 验证缓存按 TTL 过期后重新生成。
func TestCacheExpiresAfterTTL(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 0, 300, func() time.Time { return now })

	var genCalls atomic.Int64
	h.generator = func(context.Context, *User, Format) (string, error) {
		return fmt.Sprintf("gen-%d", genCalls.Add(1)), nil
	}

	if got := serveShow(t, h, "tok", "").Body.String(); got != "gen-1" {
		t.Fatalf("首次应生成 gen-1，实际 %q", got)
	}
	now = now.Add(299 * time.Second)
	if got := serveShow(t, h, "tok", "").Body.String(); got != "gen-1" {
		t.Fatalf("TTL 内应命中缓存 gen-1，实际 %q", got)
	}
	now = now.Add(2 * time.Second) // 越过 300s
	if got := serveShow(t, h, "tok", "").Body.String(); got != "gen-2" {
		t.Fatalf("TTL 过期后应重新生成 gen-2，实际 %q", got)
	}
}

// TestCacheDisabledWhenZero 验证 SUB_CACHE_SECONDS=0 时关闭缓存（每次都生成）。
func TestCacheDisabledWhenZero(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 0, 0, func() time.Time { return fixed })
	if h.cache != nil {
		t.Fatalf("ttl=0 时 cache 应为 nil（关闭）")
	}
	var genCalls atomic.Int64
	h.generator = func(context.Context, *User, Format) (string, error) {
		genCalls.Add(1)
		return "x", nil
	}
	for i := 0; i < 3; i++ {
		_ = serveShow(t, h, "tok", "")
	}
	if got := genCalls.Load(); got != 3 {
		t.Fatalf("关闭缓存后每次都应生成：期望 3 次，实际 %d 次", got)
	}
}

// TestCacheDoesNotCacheErrors 验证业务错误不进缓存：
// 一次瞬时错误不能把用户钉在错误页上整个 TTL。
func TestCacheDoesNotCacheErrors(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 0, 300, func() time.Time { return fixed })

	var calls atomic.Int64
	h.generator = func(context.Context, *User, Format) (string, error) {
		if calls.Add(1) == 1 {
			return "", &Error{Message: "流量已耗尽", Code: 403}
		}
		return "OK-BODY", nil
	}

	w := serveShow(t, h, "tok", "")
	if !strings.Contains(w.Body.String(), "[403]") {
		t.Fatalf("首次应为 403 业务错误，实际 %q", w.Body.String())
	}
	// 第二次必须重新生成并成功（错误没被缓存）
	w = serveShow(t, h, "tok", "")
	if w.Body.String() != "OK-BODY" {
		t.Fatalf("错误不应被缓存，第二次应重新生成成功，实际 %q", w.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("错误后应重试生成：期望 2 次，实际 %d 次", got)
	}
}

// TestCacheSingleFlight 验证并发未命中同一个键时只生成一次（防击穿）。
func TestCacheSingleFlight(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 0, 300, func() time.Time { return fixed })

	var genCalls atomic.Int64
	release := make(chan struct{})
	h.generator = func(context.Context, *User, Format) (string, error) {
		genCalls.Add(1)
		<-release // 卡住第一个，让其余请求都堆在 single-flight 上
		return "ONCE", nil
	}

	const n = 32
	var wg sync.WaitGroup
	bodies := make([]string, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := serveShow(t, h, "tok", "")
			bodies[i], codes[i] = w.Body.String(), w.Code
		}()
	}

	time.Sleep(100 * time.Millisecond) // 让请求都进入等待
	close(release)
	wg.Wait()

	if got := genCalls.Load(); got != 1 {
		t.Fatalf("并发未命中同一键应只生成 1 次，实际 %d 次", got)
	}
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusOK || bodies[i] != "ONCE" {
			t.Fatalf("并发请求 #%d 结果不一致：code=%d body=%q", i, codes[i], bodies[i])
		}
	}
}

// TestCacheKeyDistinguishesToken 验证不同 token 不共用缓存。
func TestCacheKeyDistinguishesToken(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{
		"tokA": testUser(t, "tokA"),
		"tokB": testUser(t, "tokB"),
	}}
	h := newTestHandler(repo, 0, 300, func() time.Time { return fixed })

	h.generator = func(_ context.Context, u *User, _ Format) (string, error) {
		return "BODY-" + u.Token, nil
	}

	if got := serveShow(t, h, "tokA", "").Body.String(); got != "BODY-tokA" {
		t.Fatalf("tokA 正文不对：%q", got)
	}
	if got := serveShow(t, h, "tokB", "").Body.String(); got != "BODY-tokB" {
		t.Fatalf("tokB 不应命中 tokA 的缓存：%q", got)
	}
}

// TestCacheKeyHasSeparator 直接钉住键的构成（token + NUL + format）。
func TestCacheKeyHasSeparator(t *testing.T) {
	if got, want := cacheKey("abc", FormatClash), "abc\x00clash"; got != want {
		t.Fatalf("cacheKey = %q，期望 %q", got, want)
	}
	if cacheKey("abc", FormatClash) == cacheKey("abc", FormatBase64) {
		t.Fatalf("不同 format 的键必须不同")
	}
	if cacheKey("abc", FormatClash) == cacheKey("abc\x00clash", FormatBase64) {
		// 这条只是记录：用 NUL 分隔后，token 里含 \x00clash 也仍会与 (abc, clash) 撞键，
		// 但 token 来自 URL 路径段，不会出现 NUL，故不构成实际风险。
		t.Log("注意：token 含 NUL 时理论上可构造键碰撞（URL 路径段不会出现 NUL）")
	}
}

// ───────────────────────── 4. 其它 ─────────────────────────

// TestShowStreamsBodyWhenCacheDisabled 顺带确认关闭缓存/限流时行为与改造前一致。
func TestShowStreamsBodyWhenCacheDisabled(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{users: map[string]*User{"tok": testUser(t, "tok")}}
	h := newTestHandler(repo, 0, 0, func() time.Time { return fixed })
	h.generator = func(context.Context, *User, Format) (string, error) { return "PLAIN", nil }

	w := serveShow(t, h, "tok", "")
	if w.Code != http.StatusOK || w.Body.String() != "PLAIN" {
		t.Fatalf("基础路径应 200 + PLAIN，实际 code=%d body=%q", w.Code, w.Body.String())
	}
	body, _ := io.ReadAll(w.Result().Body)
	if string(body) != "PLAIN" {
		t.Fatalf("响应体应为 PLAIN，实际 %q", string(body))
	}
}
