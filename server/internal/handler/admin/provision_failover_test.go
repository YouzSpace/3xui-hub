package admin

// webshare 多账号切换（地区功能-16）的集成测试。
//
// 为什么连真库：这轮改的是「开号失败 → 换账号 → 落库」整条链路，其中
// proxy_providers.last_status/last_error 是 SQL 写入、proxy_accounts 是 SQL 插入，
// 用 fake store 只能证明传参，证明不了状态和账号真的落了库（项目里地区/迁移测试同口径）。
// 上游用 fake UpstreamClient：不发外网请求，还能数清 ListSubusers 被调了几次。
//
// 变异测试锚点：把 pickProviders 里的 count 排序去掉（改回按 store 返回顺序），
// TestProvision16PickBySubuserCount 必须变红。

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// provision16AppKey 是 32 字节 AES key（store 加密 api_token/password 用）。
const provision16AppKey = "0123456789abcdef0123456789abcdef"

// provision16Now 是固定时钟：让 60 秒缓存不会在测试里自然过期。
var provision16Now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// provision16DSN 连本机 MySQL（root/空密码），可用 REGION16_MYSQL_DSN 覆盖。
// parseTime=true 必须带上：store 要扫 last_checked_at / expires_at 这些 timestamp。
func provision16DSN() string {
	if dsn := os.Getenv("REGION16_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

// provision16WithSchema 把 DSN 的默认库换成 schema（DSN 形如 root:@tcp(host:port)/?opt）。
func provision16WithSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	return head + schema + tail
}

// provision16DDL 只建本流程真正读写的 2 张表（列与 schema/schema.sql 一致，省掉无关列和外键）。
var provision16DDL = []string{
	`CREATE TABLE proxy_providers (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) NOT NULL,
		driver varchar(32) NOT NULL DEFAULT 'webshare',
		api_token text NOT NULL,
		enabled tinyint(1) NOT NULL DEFAULT '1',
		last_status varchar(16) NOT NULL DEFAULT 'pending',
		last_error varchar(255) DEFAULT NULL,
		last_checked_at timestamp NULL DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_accounts (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		provider_id bigint unsigned NOT NULL,
		upstream_id varchar(64) NOT NULL,
		user_id bigint unsigned DEFAULT NULL,
		sku_id bigint unsigned DEFAULT NULL,
		label varchar(64) NOT NULL,
		username varchar(64) NOT NULL,
		password text,
		proxy_limit decimal(8,2) NOT NULL DEFAULT '0.00',
		traffic_limit_bytes bigint unsigned DEFAULT NULL,
		expires_at timestamp NULL DEFAULT NULL,
		order_id bigint unsigned DEFAULT NULL,
		max_thread_count int unsigned NOT NULL DEFAULT '0',
		proxy_countries json DEFAULT NULL,
		used_bytes bigint unsigned NOT NULL DEFAULT '0',
		status varchar(16) NOT NULL DEFAULT 'active',
		last_synced_at timestamp NULL DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY proxy_accounts_provider_id_upstream_id_unique (provider_id, upstream_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newProvision16DB 建一个干净的测试库 + 两张表，跑完自动 DROP。
func newProvision16DB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", provision16DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过 webshare 多账号集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过 webshare 多账号集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", provision16DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", provision16WithSchema(provision16DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range provision16DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// fakeWebshare 是 UpstreamClient 的假实现：按 token 决定列表条数 / 建号成败，
// 并记录每个方法被调了几次（缓存用例靠它断言）。
type fakeWebshare struct {
	mu          sync.Mutex
	listCalls   int
	createCalls int
	listByToken map[string]int
	createErr   map[string]error
	createCount map[string]int
	seq         int
}

func newFakeWebshare() *fakeWebshare {
	return &fakeWebshare{
		listByToken: map[string]int{},
		createErr:   map[string]error{},
		createCount: map[string]int{},
	}
}

// setSubusers 设某账号下已有多少子用户。
func (f *fakeWebshare) setSubusers(token string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listByToken[token] = n
}

// failCreate 让某账号开号固定失败。
func (f *fakeWebshare) failCreate(token string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErr[token] = err
}

func (f *fakeWebshare) calls() (list, create int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.createCalls
}

func (f *fakeWebshare) callsFor(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount[token]
}

func (f *fakeWebshare) ListPlans(ctx context.Context, token string) ([]map[string]any, error) {
	return nil, nil
}

func (f *fakeWebshare) ListSubusers(ctx context.Context, token string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	n := f.listByToken[token]
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{"id": fmt.Sprintf("%s-%d", token, i), "label": "existing"})
	}
	return out, nil
}

func (f *fakeWebshare) ListProxies(ctx context.Context, token string, query map[string]string) (map[string]any, error) {
	return nil, nil
}

func (f *fakeWebshare) GetSubuser(ctx context.Context, token, upstreamID string) (map[string]any, error) {
	return nil, nil
}

func (f *fakeWebshare) CreateSubuser(ctx context.Context, token string, attrs map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.createCount[token]++
	if err, ok := f.createErr[token]; ok {
		return nil, err
	}
	f.seq++
	return map[string]any{
		"id":               fmt.Sprintf("sub-%s-%d", token, f.seq),
		"label":            attrs["label"],
		"username":         "user-" + token,
		"password":         "pass-" + token,
		"proxy_limit":      attrs["proxy_limit"],
		"max_thread_count": attrs["max_thread_count"],
		"proxy_countries":  map[string]any{"US": 10},
	}, nil
}

func (f *fakeWebshare) UpdateSubuser(ctx context.Context, token, upstreamID string, attrs map[string]any) (map[string]any, error) {
	return nil, nil
}

func (f *fakeWebshare) DeleteSubuser(ctx context.Context, token, upstreamID string) error {
	return nil
}

// resetProvision16Cache 清掉进程内子账号数缓存，避免用例之间按 provider id 串味。
func resetProvision16Cache() {
	subuserCountMu.Lock()
	defer subuserCountMu.Unlock()
	subuserCountCache = map[uint64]subuserCountEntry{}
}

// newProvision16Env 建一套「真库 + 假上游」的 Handler。
func newProvision16Env(t *testing.T) (*Handler, *fakeWebshare) {
	t.Helper()
	resetProvision16Cache()

	schema := "ch_prov16_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	db := newProvision16DB(t, schema)
	fake := newFakeWebshare()
	h := New(Deps{
		Store:    NewSQLStore(db, []byte(provision16AppKey)),
		Upstream: fake,
		Now:      func() time.Time { return provision16Now },
		Logger:   log.New(io.Discard, "", 0),
	})
	return h, fake
}

// provision16CreateProvider 落一个 enabled 账号。
func provision16CreateProvider(t *testing.T, h *Handler, name, token string) *Provider {
	t.Helper()
	p, err := h.store.CreateProvider(context.Background(), name, token, true)
	if err != nil {
		t.Fatalf("建账号失败 name=%s: %v", name, err)
	}
	return p
}

func provision16SKU() *SKU {
	return &SKU{ID: 11, ProductID: 1, Name: "100G30天", TrafficGB: 100, DurationDays: 30, MaxThreadCount: 5}
}

func provision16Order() *Order {
	return &Order{ID: 21, OrderNo: "PO16-0001", UserID: 7}
}

// TestProvision16FailoverToNextProvider：第 1 个账号开号失败 → 换第 2 个成功，
// 并把第 1 个标 failed、第 2 个标 success。
func TestProvision16FailoverToNextProvider(t *testing.T) {
	h, fake := newProvision16Env(t)
	ctx := context.Background()

	p1 := provision16CreateProvider(t, h, "acc-1", "tok16-a")
	p2 := provision16CreateProvider(t, h, "acc-2", "tok16-b")
	p3 := provision16CreateProvider(t, h, "acc-3", "tok16-c")
	fake.failCreate("tok16-a", &WebshareError{Msg: "子用户额度已用尽（4xx）", Status: 400})

	acc, err := h.openWebshareAccount(ctx, provision16Order(), provision16SKU(), provision16Now)
	if err != nil {
		t.Fatalf("期望第 2 个账号开号成功，实际报错: %v", err)
	}
	if acc.ProviderID != p2.ID {
		t.Fatalf("账号应落在第 2 个 provider(%d)，实际 provider=%d", p2.ID, acc.ProviderID)
	}
	if !strings.HasPrefix(acc.UpstreamID, "sub-tok16-b-") {
		t.Fatalf("upstream_id 应来自 tok16-b，实际 %q", acc.UpstreamID)
	}
	if acc.Label != "u7-100G30天" {
		t.Fatalf("label 不对: %q", acc.Label)
	}

	got1, err := h.store.ProviderByID(ctx, p1.ID)
	if err != nil {
		t.Fatalf("读 provider1 失败: %v", err)
	}
	if got1.LastStatus != "failed" {
		t.Fatalf("第 1 个账号 last_status 应为 failed，实际 %q", got1.LastStatus)
	}
	if got1.LastError == nil || !strings.Contains(*got1.LastError, "额度") {
		t.Fatalf("第 1 个账号 last_error 应记下失败原因，实际 %v", got1.LastError)
	}
	if got1.LastCheckedAt == nil {
		t.Fatal("第 1 个账号 last_checked_at 应被写入")
	}
	got2, err := h.store.ProviderByID(ctx, p2.ID)
	if err != nil {
		t.Fatalf("读 provider2 失败: %v", err)
	}
	if got2.LastStatus != "success" {
		t.Fatalf("第 2 个账号 last_status 应为 success，实际 %q", got2.LastStatus)
	}
	got3, err := h.store.ProviderByID(ctx, p3.ID)
	if err != nil {
		t.Fatalf("读 provider3 失败: %v", err)
	}
	if got3.LastStatus != "pending" {
		t.Fatalf("没被碰到的第 3 个账号状态应保持 pending，实际 %q", got3.LastStatus)
	}

	// 只落了一条账号，且挂在第 2 个 provider 下。
	accounts, err := h.store.ProviderAccounts(ctx, p2.ID)
	if err != nil {
		t.Fatalf("读账号列表失败: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("第 2 个 provider 下应有 1 个账号，实际 %d", len(accounts))
	}
}

// TestProvision16AllProvidersFail：3 个账号全失败 → 返回错误，文案含「试了 3 个」。
func TestProvision16AllProvidersFail(t *testing.T) {
	h, fake := newProvision16Env(t)
	ctx := context.Background()

	p1 := provision16CreateProvider(t, h, "acc-1", "tok16-a")
	p2 := provision16CreateProvider(t, h, "acc-2", "tok16-b")
	p3 := provision16CreateProvider(t, h, "acc-3", "tok16-c")
	fake.failCreate("tok16-a", &WebshareError{Msg: "额度满", Status: 400})
	fake.failCreate("tok16-b", &WebshareError{Msg: "401 Unauthorized", Status: 401})
	fake.failCreate("tok16-c", &WebshareError{Msg: "上游 500", Status: 500})

	acc, err := h.openWebshareAccount(ctx, provision16Order(), provision16SKU(), provision16Now)
	if err == nil {
		t.Fatalf("三个账号全失败应报错，实际返回账号 id=%d", acc.ID)
	}
	if !strings.Contains(err.Error(), "试了 3 个") {
		t.Fatalf("错误文案应含「试了 3 个」，实际: %v", err)
	}
	if _, create := fake.calls(); create != 3 {
		t.Fatalf("三个账号都应被试一遍，实际建号请求 %d 次", create)
	}
	for _, p := range []*Provider{p1, p2, p3} {
		got, err := h.store.ProviderByID(ctx, p.ID)
		if err != nil {
			t.Fatalf("读 provider %d 失败: %v", p.ID, err)
		}
		if got.LastStatus != "failed" {
			t.Fatalf("provider %d 应标 failed，实际 %q", p.ID, got.LastStatus)
		}
	}
}

// TestProvision16ExplicitSourceNoFailover：sku 指定了账号 → 只试那一个，失败不切换。
func TestProvision16ExplicitSourceNoFailover(t *testing.T) {
	h, fake := newProvision16Env(t)
	ctx := context.Background()

	p1 := provision16CreateProvider(t, h, "acc-1", "tok16-a")
	p2 := provision16CreateProvider(t, h, "acc-2", "tok16-b")
	p3 := provision16CreateProvider(t, h, "acc-3", "tok16-c")
	fake.failCreate("tok16-b", &WebshareError{Msg: "额度满", Status: 400})

	sku := provision16SKU()
	sku.SourceID = &p2.ID
	if _, err := h.openWebshareAccount(ctx, provision16Order(), sku, provision16Now); err == nil {
		t.Fatal("指定账号开号失败时应报错，不能换号重开")
	}

	if got := fake.callsFor("tok16-b"); got != 1 {
		t.Fatalf("指定账号应被尝试 1 次，实际 %d", got)
	}
	if got := fake.callsFor("tok16-a") + fake.callsFor("tok16-c"); got != 0 {
		t.Fatalf("备选账号不应被尝试，实际建号请求 %d 次", got)
	}
	// 指定账号失败也要记状态；没被碰到的两个保持 pending。
	got2, err := h.store.ProviderByID(ctx, p2.ID)
	if err != nil {
		t.Fatalf("读指定 provider 失败: %v", err)
	}
	if got2.LastStatus != "failed" {
		t.Fatalf("指定账号应标 failed，实际 %q", got2.LastStatus)
	}
	for _, p := range []*Provider{p1, p3} {
		got, err := h.store.ProviderByID(ctx, p.ID)
		if err != nil {
			t.Fatalf("读 provider %d 失败: %v", p.ID, err)
		}
		if got.LastStatus != "pending" {
			t.Fatalf("未参与的 provider %d 状态应保持 pending，实际 %q", p.ID, got.LastStatus)
		}
		accounts, err := h.store.ProviderAccounts(ctx, p.ID)
		if err != nil {
			t.Fatalf("读账号列表失败: %v", err)
		}
		if len(accounts) != 0 {
			t.Fatalf("未参与的 provider %d 不应有账号，实际 %d", p.ID, len(accounts))
		}
	}
}

// TestProvision16PickBySubuserCount：A 有 10 个子用户、B 有 2 个、C 有 5 个 → 优先选 B。
func TestProvision16PickBySubuserCount(t *testing.T) {
	h, fake := newProvision16Env(t)
	ctx := context.Background()

	pa := provision16CreateProvider(t, h, "acc-A", "tok16-a")
	pb := provision16CreateProvider(t, h, "acc-B", "tok16-b")
	pc := provision16CreateProvider(t, h, "acc-C", "tok16-c")
	fake.setSubusers("tok16-a", 10)
	fake.setSubusers("tok16-b", 2)
	fake.setSubusers("tok16-c", 5)

	candidates, err := h.pickProviders(ctx, provision16SKU())
	if err != nil {
		t.Fatalf("pickProviders 失败: %v", err)
	}
	got := make([]uint64, 0, len(candidates))
	for _, p := range candidates {
		got = append(got, p.ID)
	}
	want := []uint64{pb.ID, pc.ID, pa.ID}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("候选顺序应按子账号数升序 B(%d) C(%d) A(%d)，实际 %v", pb.ID, pc.ID, pa.ID, got)
	}
}

// TestProvision16PickCache：连续两次 pickProviders，ListSubusers 只打一次上游。
func TestProvision16PickCache(t *testing.T) {
	h, fake := newProvision16Env(t)
	ctx := context.Background()

	provision16CreateProvider(t, h, "acc-only", "tok16-a")
	fake.setSubusers("tok16-a", 4)

	for i := 0; i < 2; i++ {
		candidates, err := h.pickProviders(ctx, provision16SKU())
		if err != nil {
			t.Fatalf("第 %d 次 pickProviders 失败: %v", i+1, err)
		}
		if len(candidates) != 1 {
			t.Fatalf("第 %d 次候选数应为 1，实际 %d", i+1, len(candidates))
		}
	}
	if list, _ := fake.calls(); list != 1 {
		t.Fatalf("60 秒内 ListSubusers 应只调 1 次，实际 %d 次", list)
	}
}
