package api

// 静态 IP 套餐购买流程（#静态IP-02）的集成测试：真连 MySQL，独立库 ch_region31_*，自建自删。
//
// 为什么要连真库：这条链路横跨三个包、五张表 —— 支付域（afterPaid 分支）、
// 调度器（分类前置节点分配）、管理端（webshare 开号落 proxy_accounts）。
// 用 fake store 只能证明「参数传下去了」，证明不了「SQL 写到了正确的行」。
// 上游 webshare 用 fake：不发外网请求，还能指定「哪个账号开号失败」。
//
// 变异测试锚点：把 staticChainProvisioner.Provision 里「已有账号 → 续费」这一支删掉
// （改成每次都 OpenProfileAccount），TestStaticChainRenewKeepsSingleAccount 必须变红。

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/scheduler"
)

// region31AppKey 是 32 字节 AES key（admin store 加密上游凭据/密码用）。
const region31AppKey = "0123456789abcdef0123456789abcdef"

// region31Now 是固定时钟：让到期时间的断言可预期（不与真实时间赛跑）。
var region31Now = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func region31DSN() string {
	if dsn := os.Getenv("REGION31_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

func region31WithSchema(dsn, schema string) string {
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

// region31Schema 拼测试库名（带 pid：同机多任务并行跑 go test 时互不 DROP）。
func region31Schema(caseName string) string {
	return fmt.Sprintf("ch_region31_%s_%d", caseName, os.Getpid())
}

// region31DDL 只建本流程读写的表，列类型照 server/schema/schema.sql 与 schema.go。
var region31DDL = []string{
	`CREATE TABLE users (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		email varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		password varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		token varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		protocol varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'vless',
		plan_id bigint unsigned DEFAULT NULL,
		used_invite_code_id bigint unsigned DEFAULT NULL,
		traffic_limit bigint NOT NULL DEFAULT 0,
		traffic_used bigint NOT NULL DEFAULT 0,
		monthly_traffic_used bigint NOT NULL DEFAULT 0,
		monthly_traffic_limit bigint NOT NULL DEFAULT 0,
		next_traffic_reset_at timestamp NULL DEFAULT NULL,
		expired_at datetime DEFAULT NULL,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE plans (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		price decimal(10,2) NOT NULL DEFAULT '0.00',
		reset_price decimal(10,2) NOT NULL DEFAULT '0.00',
		type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
		months int DEFAULT NULL,
		monthly_traffic bigint DEFAULT NULL,
		period_traffic bigint DEFAULT NULL,
		total_traffic bigint DEFAULT NULL,
		is_active tinyint(1) NOT NULL DEFAULT 1,
		include_local tinyint(1) NOT NULL DEFAULT 1,
		third_party_sub_ids text COLLATE utf8mb4_unicode_ci,
		region_ids json DEFAULT NULL,
		exit_type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'direct',
		exit_profile_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE orders (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		order_no varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
		user_id bigint unsigned NOT NULL,
		order_type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'plan',
		plan_id bigint unsigned DEFAULT NULL,
		proxy_sku_id bigint unsigned DEFAULT NULL,
		proxy_renewal tinyint(1) NOT NULL DEFAULT 0,
		amount decimal(10,2) NOT NULL,
		original_amount decimal(10,2) DEFAULT NULL,
		discount_code_id bigint unsigned DEFAULT NULL,
		discount_amount decimal(10,2) DEFAULT NULL,
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending',
		payment_config_id bigint unsigned DEFAULT NULL,
		trade_no varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		pay_url varchar(2048) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		pay_expires_at datetime DEFAULT NULL,
		paid_at timestamp NULL DEFAULT NULL,
		pay_ip varchar(45) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY orders_order_no_unique (order_no)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE discount_codes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		code varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		user_id bigint unsigned DEFAULT NULL,
		source varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'admin',
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE regions (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		sort int NOT NULL DEFAULT 0,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '127.0.0.1',
		port int NOT NULL DEFAULT 443,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'offline',
		region_id bigint unsigned DEFAULT NULL,
		capacity int DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE exit_profiles (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		provider_id bigint unsigned NOT NULL,
		traffic_gb int NOT NULL DEFAULT 0,
		duration_days int NOT NULL DEFAULT 30,
		max_thread_count int NOT NULL DEFAULT 0,
		socks_host varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		socks_port int NOT NULL DEFAULT 1080,
		enabled tinyint NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY exit_profiles_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE exit_profile_nodes (
		profile_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (profile_id, node_id),
		KEY idx_exit_profile_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_providers (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		driver varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'webshare',
		api_token text COLLATE utf8mb4_unicode_ci NOT NULL,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		last_status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending',
		last_error varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		last_checked_at timestamp NULL DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// profile_id 列（并行任务「静态IP-32」加）故意不建：本任务的代码不能依赖它存在。
	`CREATE TABLE proxy_accounts (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		provider_id bigint unsigned NOT NULL,
		upstream_id varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		user_id bigint unsigned DEFAULT NULL,
		sku_id bigint unsigned DEFAULT NULL,
		label varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		username varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		password text COLLATE utf8mb4_unicode_ci,
		proxy_limit decimal(8,2) NOT NULL DEFAULT '0.00',
		traffic_limit_bytes bigint unsigned DEFAULT NULL,
		expires_at timestamp NULL DEFAULT NULL,
		order_id bigint unsigned DEFAULT NULL,
		max_thread_count int unsigned NOT NULL DEFAULT '0',
		proxy_countries json DEFAULT NULL,
		used_bytes bigint unsigned NOT NULL DEFAULT '0',
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active',
		last_synced_at timestamp NULL DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY proxy_accounts_provider_id_upstream_id_unique (provider_id, upstream_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newRegion31DB 建一个干净的测试库 + 全部表，跑完自动 DROP（连不上 MySQL 就 skip）。
func newRegion31DB(t *testing.T, caseName string) *sql.DB {
	t.Helper()
	schema := region31Schema(caseName)

	adminDB, err := sql.Open("mysql", region31DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过静态 IP 购买集成测试", err)
	}
	if err := adminDB.Ping(); err != nil {
		adminDB.Close()
		t.Skipf("连不上 MySQL（%v），跳过静态 IP 购买集成测试", err)
	}
	t.Cleanup(func() { adminDB.Close() })

	if _, err := adminDB.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := adminDB.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("mysql", region31DSN())
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region31WithSchema(region31DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("测试库 ping 失败: %v", err)
	}
	for _, ddl := range region31DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	// 列探测是进程级缓存：每个用例都是新库（profile_id 不存在），必须清掉上一条用例的结果。
	resetRegion31ColumnCaches()
	return db
}

// resetRegion31ColumnCaches 清支付包「plans 有没有 exit 列」的进程内缓存。
func resetRegion31ColumnCaches() {
	planExitColumnsMu.Lock()
	planExitColumnsKnown = false
	planExitColumnsValue = false
	planExitColumnsMu.Unlock()
}

// fakeRegion31Webshare 是 admin.UpstreamClient 的假实现。
type fakeRegion31Webshare struct {
	mu          sync.Mutex
	createCalls int
	fail        bool
	seq         int
}

func (f *fakeRegion31Webshare) ListPlans(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}

func (f *fakeRegion31Webshare) ListSubusers(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}

func (f *fakeRegion31Webshare) ListProxies(context.Context, string, map[string]string) (map[string]any, error) {
	return nil, nil
}

func (f *fakeRegion31Webshare) GetSubuser(context.Context, string, string) (map[string]any, error) {
	return nil, nil
}

func (f *fakeRegion31Webshare) CreateSubuser(_ context.Context, token string, attrs map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.fail {
		return nil, &admin.WebshareError{Msg: "子用户额度已用尽（4xx）", Status: 400}
	}
	f.seq++
	return map[string]any{
		"id":               fmt.Sprintf("sub31-%d", f.seq),
		"label":            attrs["label"],
		"username":         "ws-user",
		"password":         "ws-pass",
		"proxy_limit":      attrs["proxy_limit"],
		"max_thread_count": attrs["max_thread_count"],
	}, nil
}

func (f *fakeRegion31Webshare) UpdateSubuser(context.Context, string, string, map[string]any) (map[string]any, error) {
	return nil, nil
}

func (f *fakeRegion31Webshare) DeleteSubuser(context.Context, string, string) error { return nil }

func (f *fakeRegion31Webshare) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

// region31Locker 是进程内锁（测试不需要 Redis）。
type region31Locker struct {
	mu   sync.Mutex
	held map[string]bool
}

func newRegion31Locker() *region31Locker { return &region31Locker{held: map[string]bool{}} }

func (l *region31Locker) Lock(_ context.Context, key string, _ time.Duration) (func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[key] {
		return nil, false, nil
	}
	l.held[key] = true
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, key)
	}, true, nil
}

// region31Env 是一套「真库 + 假上游 + 真调度器」的支付服务。
type region31Env struct {
	db     *sql.DB
	svc    *PaymentService
	fake   *fakeRegion31Webshare
	logs   *bytes.Buffer
	locker *region31Locker
}

func newRegion31Env(t *testing.T, caseName string) *region31Env {
	t.Helper()
	db := newRegion31DB(t, caseName)

	fake := &fakeRegion31Webshare{}
	logs := &bytes.Buffer{}
	logger := log.New(logs, "", 0)
	adminHandler := admin.New(admin.Deps{
		Store:    admin.NewSQLStore(db, []byte(region31AppKey)),
		Upstream: fake,
		Logger:   logger,
	})
	locker := newRegion31Locker()
	sched := scheduler.New(db, locker)

	prov := NewStaticChainProvisioner(db, adminHandler, sched, func() time.Time { return region31Now }, logger)
	payStore := NewPaymentSQLStore(db)
	svc := NewPaymentService(payStore, PaymentServiceOptions{
		Now: func() time.Time { return region31Now },
		AssignUserNodes: func(ctx context.Context, userID uint64) error {
			_, err := sched.Assign(ctx, userID)
			return err
		},
		PruneUserNodes: sched.PrunePlanRegions,
		StaticChain:    prov,
	}, logger)

	return &region31Env{db: db, svc: svc, fake: fake, logs: logs, locker: locker}
}

// mustExec31 执行一条 SQL，失败直接 Fatal。
func mustExec31(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("执行失败: %v\nSQL: %s", err, query)
	}
}

// scalar31 跑一个返回单列整数的查询。
func scalar31(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询失败: %v\nSQL: %s", err, query)
	}
	return n
}

// seedRegion31 铺一套数据：用户、套餐、分类、账号、节点。
//
//	exit_type 决定套餐类型（direct / static_chain）；
//	static_chain 的分类固定 id=31，前置节点由调用方通过 nodeIDs 指定。
func seedRegion31(t *testing.T, db *sql.DB, exitType string, nodeIDs []int64) {
	t.Helper()

	// 用户 3100 是被测用户；1..20 是「已经绑在节点上」的其他用户（构造负载）。
	for i := 1; i <= 20; i++ {
		mustExec31(t, db, `INSERT INTO users (id, email, token, enabled) VALUES (?, ?, ?, 1)`,
			i, fmt.Sprintf("u%d@example.test", i), fmt.Sprintf("tok-%d", i))
	}
	mustExec31(t, db, `INSERT INTO users (id, email, token, enabled) VALUES (3100, 'ch3100@example.test', 'tok-3100', 1)`)

	// 地区 7 + 一台直连节点（回归用：direct 套餐要按地区分到它）。
	mustExec31(t, db, `INSERT INTO regions (id, name, sort) VALUES (7, '香港', 1)`)
	mustExec31(t, db, `INSERT INTO nodes (id, name, host, enabled, status, region_id, capacity)
		VALUES (7001, 'direct-hk', '10.0.0.1', 1, 'online', 7, 100)`)

	// 出口分类 31：webshare 账号 1，模板 100G / 30 天 / 5 并发。
	mustExec31(t, db, `INSERT INTO proxy_providers (id, name, api_token, enabled) VALUES (1, 'ws-main', 'tok-ws', 1)`)
	mustExec31(t, db, `INSERT INTO exit_profiles (id, name, provider_id, traffic_gb, duration_days, max_thread_count, socks_host, socks_port, enabled)
		VALUES (31, '美国静态', 1, 100, 30, 5, 'p.webshare.io', 1080, 1)`)

	// 前置节点：容量都 100，已绑用户数由 nodeIDs 之外的调用方铺（见 seedProfileNodes）。
	for _, id := range nodeIDs {
		mustExec31(t, db, `INSERT INTO nodes (id, name, host, enabled, status, capacity)
			VALUES (?, ?, '10.0.1.1', 1, 'online', 100)`, id, fmt.Sprintf("front-%d", id))
		mustExec31(t, db, `INSERT INTO exit_profile_nodes (profile_id, node_id) VALUES (31, ?)`, id)
	}

	profileID := sql.NullInt64{}
	if exitType == "static_chain" {
		profileID = sql.NullInt64{Int64: 31, Valid: true}
	}
	mustExec31(t, db, `INSERT INTO plans (id, name, price, type, months, monthly_traffic, period_traffic,
		is_active, include_local, exit_type, exit_profile_id)
		VALUES (310, '静态IP月付', '50.00', 'period', 1, ?, ?, 1, 1, ?, ?)`,
		100*1024*1024*1024, 100*1024*1024*1024, exitType, profileID)
}

// bind31 把 [fromID, fromID+count) 的用户绑到 nodeID 上（构造节点负载）。
func bind31(t *testing.T, db *sql.DB, nodeID int64, fromID, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		mustExec31(t, db, `INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)`, fromID+i, nodeID)
	}
}

// placeOrder31 落一笔 pending 套餐订单并返回订单行。
func placeOrder31(t *testing.T, db *sql.DB, orderNo string, userID uint64, planID uint64) *Order {
	t.Helper()
	res, err := db.Exec(`INSERT INTO orders (order_no, user_id, order_type, plan_id, amount, status, created_at, updated_at)
		VALUES (?, ?, 'plan', ?, '50.00', 'pending', NOW(), NOW())`, orderNo, userID, planID)
	if err != nil {
		t.Fatalf("建订单失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取订单 id 失败: %v", err)
	}
	return &Order{
		ID:        uint64(id),
		OrderNo:   orderNo,
		UserID:    userID,
		OrderType: "plan",
		PlanID:    sql.NullInt64{Int64: int64(planID), Valid: true},
		Amount:    "50.00",
		Status:    "pending",
	}
}

// orderStatus31 读订单状态。
func orderStatus31(t *testing.T, db *sql.DB, orderNo string) string {
	t.Helper()
	var s string
	if err := db.QueryRow("SELECT status FROM orders WHERE order_no = ?", orderNo).Scan(&s); err != nil {
		t.Fatalf("读订单状态失败: %v", err)
	}
	return s
}

// TestStaticChainPurchaseOpensAccountAndBindsFrontNode：买 static_chain 套餐 →
// proxy_accounts 有 1 条（用户名/密码非空、sku_id 为 NULL、order_id 记本单）、
// user_nodes 有 1 条且落在「已绑 0 人」的前置节点上。
func TestStaticChainPurchaseOpensAccountAndBindsFrontNode(t *testing.T) {
	env := newRegion31Env(t, "purchase")
	seedRegion31(t, env.db, "static_chain", []int64{3101, 3102, 3103})

	// 三个前置节点容量都 100，已绑 10 / 5 / 0 人。
	bind31(t, env.db, 3101, 1, 10)
	bind31(t, env.db, 3102, 11, 5)

	order := placeOrder31(t, env.db, "SC31-0001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("完成订单失败: %v", err)
	}

	if got := orderStatus31(t, env.db, "SC31-0001"); got != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", got)
	}

	var (
		username string
		password sql.NullString
		skuID    sql.NullInt64
		orderID  sql.NullInt64
	)
	if err := env.db.QueryRow(`SELECT username, password, sku_id, order_id FROM proxy_accounts WHERE user_id = 3100`).
		Scan(&username, &password, &skuID, &orderID); err != nil {
		t.Fatalf("proxy_accounts 应有 1 条记录，查询失败: %v", err)
	}
	if username == "" {
		t.Fatalf("username 不能为空")
	}
	if !password.Valid || password.String == "" {
		t.Fatalf("password 不能为空，实际 %+v", password)
	}
	if skuID.Valid {
		t.Fatalf("静态 IP 开号的 sku_id 必须为 NULL，实际 %d", skuID.Int64)
	}
	if !orderID.Valid || orderID.Int64 != int64(order.ID) {
		t.Fatalf("order_id 应记本单 %d，实际 %+v", order.ID, orderID)
	}

	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100"); n != 1 {
		t.Fatalf("user_nodes 应只有 1 条，实际 %d 条", n)
	}
	var nodeID int64
	if err := env.db.QueryRow("SELECT node_id FROM user_nodes WHERE user_id = 3100").Scan(&nodeID); err != nil {
		t.Fatalf("读绑定失败: %v", err)
	}
	if nodeID != 3103 {
		t.Fatalf("应分到已绑 0 人的节点 3103，实际 %d", nodeID)
	}

	// 分类里的前置节点之外不应有任何地区绑定（链式出口不能走地区分配）。
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100 AND node_id = 7001"); n != 0 {
		t.Fatalf("static_chain 用户不应被绑到地区直连节点，实际 %d 条", n)
	}
}

// TestStaticChainRenewKeepsSingleAccount：续费（同 user + 同分类）不重新开号，
// proxy_accounts 仍 1 条、expires_at 往后延、节点绑定不变。
func TestStaticChainRenewKeepsSingleAccount(t *testing.T) {
	env := newRegion31Env(t, "renew")
	seedRegion31(t, env.db, "static_chain", []int64{3101})

	first := placeOrder31(t, env.db, "SC31-R001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), first, sql.NullString{}); err != nil {
		t.Fatalf("首单完成失败: %v", err)
	}
	var firstExpiry time.Time
	if err := env.db.QueryRow("SELECT expires_at FROM proxy_accounts WHERE user_id = 3100").Scan(&firstExpiry); err != nil {
		t.Fatalf("读首单到期时间失败: %v", err)
	}
	if calls := env.fake.createCount(); calls != 1 {
		t.Fatalf("首单应开号 1 次，实际 %d 次", calls)
	}

	second := placeOrder31(t, env.db, "SC31-R002", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), second, sql.NullString{}); err != nil {
		t.Fatalf("续费单完成失败: %v", err)
	}

	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM proxy_accounts WHERE user_id = 3100"); n != 1 {
		t.Fatalf("续费后 proxy_accounts 仍应只有 1 条，实际 %d 条", n)
	}
	if calls := env.fake.createCount(); calls != 1 {
		t.Fatalf("续费不应重新开号（上游建号仍应只有 1 次），实际 %d 次", calls)
	}
	var secondExpiry time.Time
	if err := env.db.QueryRow("SELECT expires_at FROM proxy_accounts WHERE user_id = 3100").Scan(&secondExpiry); err != nil {
		t.Fatalf("读续费后到期时间失败: %v", err)
	}
	if !secondExpiry.After(firstExpiry) {
		t.Fatalf("续费后 expires_at 应往后延：first=%v second=%v", firstExpiry, secondExpiry)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100"); n != 1 {
		t.Fatalf("续费不应新增节点绑定，实际 %d 条", n)
	}
}

// TestStaticChainOpenFailureKeepsOrderPaid：上游开号失败 → 订单仍 paid、
// 无 proxy_accounts 记录、日志里能看到失败原因（由管理员补开）。
func TestStaticChainOpenFailureKeepsOrderPaid(t *testing.T) {
	env := newRegion31Env(t, "openfail")
	seedRegion31(t, env.db, "static_chain", []int64{3101})
	env.fake.fail = true

	order := placeOrder31(t, env.db, "SC31-F001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("开号失败不能影响订单完成，实际返回错误: %v", err)
	}

	if got := orderStatus31(t, env.db, "SC31-F001"); got != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", got)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM proxy_accounts"); n != 0 {
		t.Fatalf("开号失败不应落 proxy_accounts，实际 %d 条", n)
	}
	if !strings.Contains(env.logs.String(), "开 webshare 子账号失败") {
		t.Fatalf("日志里应记录开号失败，实际日志:\n%s", env.logs.String())
	}
}

// TestStaticChainNoFrontNodeKeepsOrderPaid：分类没有可用前置节点 → 订单 paid、
// 不落 proxy_accounts（也不会白占一个上游子账号）、日志有记录。
func TestStaticChainNoFrontNodeKeepsOrderPaid(t *testing.T) {
	env := newRegion31Env(t, "nonode")
	seedRegion31(t, env.db, "static_chain", nil)
	// 分类里挂一台离线节点：候选必须有 enabled=1 且 status='online'，这台不合格。
	mustExec31(t, env.db, `INSERT INTO nodes (id, name, host, enabled, status, capacity)
		VALUES (3109, 'front-offline', '10.0.1.9', 1, 'offline', 100)`)
	mustExec31(t, env.db, `INSERT INTO exit_profile_nodes (profile_id, node_id) VALUES (31, 3109)`)

	order := placeOrder31(t, env.db, "SC31-N001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("无可用节点不能影响订单完成，实际返回错误: %v", err)
	}

	if got := orderStatus31(t, env.db, "SC31-N001"); got != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", got)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM proxy_accounts"); n != 0 {
		t.Fatalf("无可用节点时不应开号，实际 %d 条", n)
	}
	if calls := env.fake.createCount(); calls != 0 {
		t.Fatalf("无可用节点时不应打上游建号，实际 %d 次", calls)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100"); n != 0 {
		t.Fatalf("无可用节点时不应写绑定，实际 %d 条", n)
	}
	if !strings.Contains(env.logs.String(), "分配前置节点失败") {
		t.Fatalf("日志里应记录分配失败，实际日志:\n%s", env.logs.String())
	}
}

// TestDirectPlanRegression：direct 套餐行为与改动前完全一致 ——
// 按地区由 scheduler 正常分配、不开 webshare 子账号。
func TestDirectPlanRegression(t *testing.T) {
	env := newRegion31Env(t, "direct")
	seedRegion31(t, env.db, "direct", nil)

	order := placeOrder31(t, env.db, "SC31-D001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("direct 套餐完成失败: %v", err)
	}

	if got := orderStatus31(t, env.db, "SC31-D001"); got != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", got)
	}
	var nodeID int64
	if err := env.db.QueryRow("SELECT node_id FROM user_nodes WHERE user_id = 3100").Scan(&nodeID); err != nil {
		t.Fatalf("direct 套餐应按地区绑定节点，查询失败: %v", err)
	}
	if nodeID != 7001 {
		t.Fatalf("direct 套餐应分到地区节点 7001，实际 %d", nodeID)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM proxy_accounts"); n != 0 {
		t.Fatalf("direct 套餐不应开 webshare 子账号，实际 %d 条", n)
	}
	if calls := env.fake.createCount(); calls != 0 {
		t.Fatalf("direct 套餐不应打上游建号，实际 %d 次", calls)
	}
}

// TestStaticChainUnknownExitTypeFallsBackToDirect：exit_type 为空/未知 → 按 direct 处理
// （安全降级：不开号、不报错、地区分配照常）。
func TestStaticChainUnknownExitTypeFallsBackToDirect(t *testing.T) {
	env := newRegion31Env(t, "unknown")
	seedRegion31(t, env.db, "static_socks", nil)

	order := placeOrder31(t, env.db, "SC31-U001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("static_socks 应按 direct 处理，实际返回错误: %v", err)
	}
	if got := orderStatus31(t, env.db, "SC31-U001"); got != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", got)
	}
	if calls := env.fake.createCount(); calls != 0 {
		t.Fatalf("static_socks 不应开 webshare 子账号，实际建号 %d 次", calls)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100"); n != 1 {
		t.Fatalf("static_socks 应按 direct 走地区分配（1 条绑定），实际 %d 条", n)
	}
}

// TestStaticChainPlanWithoutProfileSkipsProvision：static_chain 但没绑分类 →
// 不开号、不绑节点、不开号失败（订单 paid，日志说明原因）。
func TestStaticChainPlanWithoutProfileSkipsProvision(t *testing.T) {
	env := newRegion31Env(t, "noprofile")
	seedRegion31(t, env.db, "static_chain", []int64{3101})
	mustExec31(t, env.db, "UPDATE plans SET exit_profile_id = NULL WHERE id = 310")

	order := placeOrder31(t, env.db, "SC31-P001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("完成订单失败: %v", err)
	}
	if got := orderStatus31(t, env.db, "SC31-P001"); got != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", got)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100"); n != 0 {
		t.Fatalf("未绑分类时不应写任何绑定，实际 %d 条（也不能退回地区分配）", n)
	}
	if !strings.Contains(env.logs.String(), "未绑定出口分类") {
		t.Fatalf("日志里应说明未绑分类，实际日志:\n%s", env.logs.String())
	}
}

// TestStaticChainSwitchFromDirectPrunesRegionBindings：用户原本是 direct 套餐（有地区
// 直连节点绑定），改买 static_chain 套餐后，旧绑定的直连节点必须被清掉 —— 否则订阅里
// 仍会下发直连入口，用户会连到一个走不了链式出站的节点。
func TestStaticChainSwitchFromDirectPrunesRegionBindings(t *testing.T) {
	env := newRegion31Env(t, "switch")
	seedRegion31(t, env.db, "static_chain", []int64{3101})

	// 模拟用户之前的直连套餐留下的绑定。
	mustExec31(t, env.db, `INSERT INTO user_nodes (user_id, node_id) VALUES (3100, 7001)`)

	order := placeOrder31(t, env.db, "SC31-S001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("完成订单失败: %v", err)
	}

	var nodeID int64
	if err := env.db.QueryRow("SELECT node_id FROM user_nodes WHERE user_id = 3100").Scan(&nodeID); err != nil {
		t.Fatalf("应有一条分类前置节点绑定，查询失败: %v", err)
	}
	if nodeID != 3101 {
		t.Fatalf("应绑到分类前置节点 3101，实际 %d", nodeID)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100 AND node_id = 7001"); n != 0 {
		t.Fatalf("旧地区直连绑定必须被清掉，实际还剩 %d 条", n)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 3100"); n != 1 {
		t.Fatalf("切换后应只剩 1 条绑定，实际 %d 条", n)
	}
}

// TestStaticChainWritesProfileIDWhenColumnExists：并行任务「静态IP-32」把
// proxy_accounts.profile_id 加上后，开号/续费必须写这一列做关联；
// 列不存在时（其余用例的库）不写也不失败。
func TestStaticChainWritesProfileIDWhenColumnExists(t *testing.T) {
	env := newRegion31Env(t, "profile_col")
	seedRegion31(t, env.db, "static_chain", []int64{3101})
	mustExec31(t, env.db, "ALTER TABLE proxy_accounts ADD COLUMN profile_id bigint unsigned DEFAULT NULL")

	order := placeOrder31(t, env.db, "SC31-C001", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("完成订单失败: %v", err)
	}

	var profileID sql.NullInt64
	if err := env.db.QueryRow("SELECT profile_id FROM proxy_accounts WHERE user_id = 3100").Scan(&profileID); err != nil {
		t.Fatalf("读 profile_id 失败: %v", err)
	}
	if !profileID.Valid || profileID.Int64 != 31 {
		t.Fatalf("proxy_accounts.profile_id 应写 31，实际 %+v", profileID)
	}

	// 续费仍走「已有账号」判定（该判定此时按 profile_id 查），记录数不变。
	renew := placeOrder31(t, env.db, "SC31-C002", 3100, 310)
	if err := env.svc.CompleteOrder(context.Background(), renew, sql.NullString{}); err != nil {
		t.Fatalf("续费失败: %v", err)
	}
	if n := scalar31(t, env.db, "SELECT COUNT(*) FROM proxy_accounts WHERE user_id = 3100"); n != 1 {
		t.Fatalf("续费后仍应只有 1 条，实际 %d 条", n)
	}
	if calls := env.fake.createCount(); calls != 1 {
		t.Fatalf("续费不应重新开号，上游建号 %d 次", calls)
	}
}
