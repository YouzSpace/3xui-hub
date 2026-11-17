package admin

// 独占 socks 池（地区功能-17）的集成测试：真连 MySQL，独立库 ch_region17_*，跑完自动 DROP。
//
// 为什么连真库：这轮的核心是「一条让一人」和「并发不重复分配」，靠的是
// SELECT ... FOR UPDATE + UPDATE 在事务里的行为 —— fake store 证明不了锁，
// 只能证明传参。共享路径同时回归，确认老行为一点没变。
//
// 变异测试锚点：把 sockpool.Acquire 里的 FOR UPDATE 去掉，
// TestSocks17ConcurrentNoDoubleSell 必须变红（同一条线路卖给两个人）。

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
)

// socks17AppKey 是 32 字节 AES key（store 加密 socks 密码用，本用例不涉及密码也不报错）。
const socks17AppKey = "0123456789abcdef0123456789abcdef"

// socks17Now 是固定时钟，断言 sold_at 用。
var socks17Now = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// socks17DSN 连本机 MySQL（root/空密码），可用 REGION17_MYSQL_DSN 覆盖。
func socks17DSN() string {
	if dsn := os.Getenv("REGION17_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

// socks17WithSchema 把 DSN 的默认库换成 schema。
func socks17WithSchema(dsn, schema string) string {
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

// socks17DDL 只建本流程真正读写的 4 张表（列与 schema/schema.sql 对齐，
// proxy_socks 带上 #地区功能-17 新增的三列）。
var socks17DDL = []string{
	`CREATE TABLE users (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		email varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_skus (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		product_id bigint unsigned NOT NULL,
		name varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL,
		source_type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'webshare',
		allocation varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'exclusive',
		source_id bigint unsigned DEFAULT NULL,
		region varchar(32) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		traffic_gb int unsigned NOT NULL DEFAULT 0,
		duration_days int unsigned NOT NULL DEFAULT 30,
		max_thread_count int unsigned NOT NULL DEFAULT 0,
		price decimal(10,2) NOT NULL DEFAULT '0.00',
		cost_price decimal(10,2) NOT NULL DEFAULT '0.00',
		stock_total int unsigned NOT NULL DEFAULT 0,
		stock_locked int unsigned NOT NULL DEFAULT 0,
		stock_sold int unsigned NOT NULL DEFAULT 0,
		is_active tinyint(1) NOT NULL DEFAULT 1,
		sort_order int unsigned NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_socks (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int unsigned NOT NULL,
		username varchar(190) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		password text COLLATE utf8mb4_unicode_ci,
		country varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		city varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		note varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		is_active tinyint(1) NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		allocation varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'shared',
		sold_to bigint unsigned DEFAULT NULL,
		sold_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_socks_pool (allocation, is_active, sold_to)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_entitlements (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		user_id bigint unsigned NOT NULL,
		order_id bigint unsigned DEFAULT NULL,
		sku_id bigint unsigned NOT NULL,
		kind varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
		account_id bigint unsigned DEFAULT NULL,
		socks_id bigint unsigned DEFAULT NULL,
		expires_at timestamp NULL DEFAULT NULL,
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active',
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newSocks17DB 建一个干净的测试库 + 4 张表，跑完自动 DROP。
func newSocks17DB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", socks17DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过独占 socks 集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过独占 socks 集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", socks17DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", socks17WithSchema(socks17DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	// 并发用例要同时开多条连接（10 个 goroutine 抢行锁），池子别设置太小。
	db.SetMaxOpenConns(16)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range socks17DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// socks17Env 是一套「真库 + 捕获日志」的 Handler（日志用来断言「池已空」文案）。
type socks17Env struct {
	h   *Handler
	db  *sql.DB
	log *bytes.Buffer
}

func newSocks17Env(t *testing.T) *socks17Env {
	t.Helper()
	schema := "ch_region17_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	db := newSocks17DB(t, schema)
	buf := &bytes.Buffer{}
	// log.Logger 自带互斥，并发写不会串行错乱。
	h := New(Deps{
		Store:    NewSQLStore(db, []byte(socks17AppKey)),
		Now:      func() time.Time { return socks17Now },
		Logger:   log.New(buf, "", 0),
		Upstream: nil,
	})
	return &socks17Env{h: h, db: db, log: buf}
}

// gotLog 返回到目前为止的日志文本。
func (e *socks17Env) gotLog() string { return e.log.String() }

// socks17User 落一个用户（开通用例只需要 email 存在）。
func socks17User(t *testing.T, db *sql.DB, id uint64) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO users (id, email) VALUES (?, ?)", id, fmt.Sprintf("u%d@example.com", id)); err != nil {
		t.Fatalf("建用户 %d 失败: %v", id, err)
	}
}

// socks17Sock 落一条 socks，返回 id。allocation: shared / exclusive。
func socks17Sock(t *testing.T, db *sql.DB, host string, allocation string) uint64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO proxy_socks (name, host, port, is_active, allocation, created_at, updated_at)
		VALUES (?, ?, 1080, 1, ?, ?, ?)`, host, host, allocation, socks17Now, socks17Now)
	if err != nil {
		t.Fatalf("建 socks %s 失败: %v", host, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取 socks id 失败: %v", err)
	}
	return uint64(id)
}

// socks17SKU 落一个 socks 规格（sourceID 为 0 表示「不指定」，独占池模式）。
func socks17SKU(t *testing.T, db *sql.DB, allocation string, sourceID uint64) uint64 {
	t.Helper()
	var src any
	if sourceID > 0 {
		src = sourceID
	}
	res, err := db.Exec(`INSERT INTO proxy_skus
		(product_id, name, source_type, allocation, source_id, traffic_gb, duration_days, max_thread_count,
		 price, cost_price, stock_total, stock_locked, stock_sold, is_active, sort_order, created_at, updated_at)
		VALUES (1, ?, 'socks', ?, ?, 100, 30, 5, 19.90, 0.00, 10, 1, 0, 1, 0, ?, ?)`,
		"region17-"+allocation, allocation, src, socks17Now, socks17Now)
	if err != nil {
		t.Fatalf("建规格失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取规格 id 失败: %v", err)
	}
	return uint64(id)
}

// socks17Order 造一张已支付的代理订单。
func socks17Order(orderID, userID, skuID uint64, renewal bool) *Order {
	return &Order{
		ID:           orderID,
		OrderNo:      fmt.Sprintf("PO17-%04d", orderID),
		UserID:       userID,
		OrderType:    OrderTypeProxy,
		SKUID:        &skuID,
		ProxyRenewal: renewal,
		Status:       OrderStatusPaid,
	}
}

// socks17Entitlement 读某订单写下的权益（没有返回 nil）。
func socks17Entitlement(t *testing.T, db *sql.DB, orderID uint64) (socksID *uint64, kind string) {
	t.Helper()
	var id sql.NullInt64
	var k string
	err := db.QueryRow(`SELECT socks_id, kind FROM proxy_entitlements WHERE order_id = ?`, orderID).Scan(&id, &k)
	if err == sql.ErrNoRows {
		return nil, ""
	}
	if err != nil {
		t.Fatalf("读权益失败 order=%d: %v", orderID, err)
	}
	if !id.Valid {
		return nil, k
	}
	v := uint64(id.Int64)
	return &v, k
}

// socks17UserSock 读某用户最新一条 socks 权益指向的线路（续费会改写 order_id，
// 所以按 user 查而不是按订单查）。
func socks17UserSock(t *testing.T, db *sql.DB, userID uint64) *uint64 {
	t.Helper()
	var id sql.NullInt64
	if err := db.QueryRow(
		`SELECT socks_id FROM proxy_entitlements WHERE user_id = ? AND kind = 'socks' ORDER BY id DESC LIMIT 1`,
		userID).Scan(&id); err != nil {
		t.Fatalf("读用户 %d 的权益失败: %v", userID, err)
	}
	if !id.Valid {
		return nil
	}
	v := uint64(id.Int64)
	return &v
}

// socks17Ptr 把 *uint64 打成 "NULL" 或 "#N"（断言失败时看得懂，不打印地址）。
func socks17Ptr(v *uint64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("#%d", *v)
}

// socks17SockState 读一条 socks 的独占状态。
func socks17SockState(t *testing.T, db *sql.DB, sockID uint64) (soldTo *uint64, soldAt *time.Time) {
	t.Helper()
	var to sql.NullInt64
	var at sql.NullTime
	if err := db.QueryRow("SELECT sold_to, sold_at FROM proxy_socks WHERE id = ?", sockID).Scan(&to, &at); err != nil {
		t.Fatalf("读 socks %d 状态失败: %v", sockID, err)
	}
	if to.Valid {
		v := uint64(to.Int64)
		soldTo = &v
	}
	if at.Valid {
		tm := at.Time
		soldAt = &tm
	}
	return soldTo, soldAt
}

// TestSocks17SharedUnchanged：共享规格指向 sock#1，两个用户买 → 都拿到同一套凭据（现状不变）。
func TestSocks17SharedUnchanged(t *testing.T) {
	env := newSocks17Env(t)
	ctx := context.Background()

	sock := socks17Sock(t, env.db, "shared-1.example.com", "shared")
	sku := socks17SKU(t, env.db, "shared", sock)
	socks17User(t, env.db, 101)
	socks17User(t, env.db, 102)

	for i, userID := range []uint64{101, 102} {
		orderID := uint64(1000 + i)
		env.h.doCompleteOrder(ctx, socks17Order(orderID, userID, sku, false))
		got, kind := socks17Entitlement(t, env.db, orderID)
		if kind != "socks" || got == nil || *got != sock {
			t.Fatalf("用户 %d 应拿到共享 socks#%d，实际 kind=%q socks=%s", userID, sock, kind, socks17Ptr(got))
		}
	}

	// 共享线路不写独占字段。
	soldTo, soldAt := socks17SockState(t, env.db, sock)
	if soldTo != nil || soldAt != nil {
		t.Fatalf("共享线路不该有 sold_to/sold_at，实际 sold_to=%s sold_at=%v", socks17Ptr(soldTo), soldAt)
	}
	if strings.Contains(env.gotLog(), "开通失败") {
		t.Fatalf("共享开通不该报错，日志：%s", env.gotLog())
	}
}

// TestSocks17ExclusivePoolOneEach：池里 3 条独占 socks，3 个用户买 → 分别拿 1/2/3，
// sold_to 各自正确、sold_at 有值。
func TestSocks17ExclusivePoolOneEach(t *testing.T) {
	env := newSocks17Env(t)
	ctx := context.Background()

	s1 := socks17Sock(t, env.db, "ex-1.example.com", "exclusive")
	s2 := socks17Sock(t, env.db, "ex-2.example.com", "exclusive")
	s3 := socks17Sock(t, env.db, "ex-3.example.com", "exclusive")
	sku := socks17SKU(t, env.db, "exclusive", 0) // 不指定 → 从整个池子挑

	users := []uint64{201, 202, 203}
	want := []uint64{s1, s2, s3}
	for i, userID := range users {
		orderID := uint64(2000 + i)
		socks17User(t, env.db, userID)
		env.h.doCompleteOrder(ctx, socks17Order(orderID, userID, sku, false))

		got, kind := socks17Entitlement(t, env.db, orderID)
		if kind != "socks" || got == nil {
			t.Fatalf("用户 %d 应开通 socks 权益，实际 kind=%q socks=%s 日志：%s", userID, kind, socks17Ptr(got), env.gotLog())
		}
		if *got != want[i] {
			t.Fatalf("第 %d 个用户应拿到 socks#%d，实际 #%d", i+1, want[i], *got)
		}
		soldTo, soldAt := socks17SockState(t, env.db, want[i])
		if soldTo == nil || *soldTo != userID {
			t.Fatalf("socks#%d 的 sold_to 应为 %d，实际 %s", want[i], userID, socks17Ptr(soldTo))
		}
		if soldAt == nil || !soldAt.Equal(socks17Now) {
			t.Fatalf("socks#%d 的 sold_at 应为 %v，实际 %v", want[i], socks17Now, soldAt)
		}
	}
}

// TestSocks17ExclusivePoolEmpty：池里只剩 1 条，第 2 个用户买 → 报「独占 socks 池已空」，
// 订单不开通（不写权益），已售那条的归属不变。
func TestSocks17ExclusivePoolEmpty(t *testing.T) {
	env := newSocks17Env(t)
	ctx := context.Background()

	sock := socks17Sock(t, env.db, "ex-only.example.com", "exclusive")
	sku := socks17SKU(t, env.db, "exclusive", 0)
	socks17User(t, env.db, 301)
	socks17User(t, env.db, 302)

	env.h.doCompleteOrder(ctx, socks17Order(3001, 301, sku, false))
	if got, _ := socks17Entitlement(t, env.db, 3001); got == nil || *got != sock {
		t.Fatalf("用户 301 应拿到唯一的独占 socks#%d，实际 %s", sock, socks17Ptr(got))
	}

	env.h.doCompleteOrder(ctx, socks17Order(3002, 302, sku, false))
	if got, kind := socks17Entitlement(t, env.db, 3002); got != nil || kind != "" {
		t.Fatalf("池空时不该开通，实际 kind=%q socks=%s", kind, socks17Ptr(got))
	}
	if log := env.gotLog(); !strings.Contains(log, "独占 socks 池已空") {
		t.Fatalf("池空应报「独占 socks 池已空」，实际日志：%s", log)
	}
	if soldTo, _ := socks17SockState(t, env.db, sock); soldTo == nil || *soldTo != 301 {
		t.Fatalf("已售线路的归属应仍是 301，实际 %s", socks17Ptr(soldTo))
	}
}

// TestSocks17ConcurrentNoDoubleSell：10 个并发订单抢 5 条独占线路 →
// 恰好 5 个成功、5 个失败，没有任何一条线路被卖给两个用户。
//
// 变异测试锚点：去掉 sockpool.Acquire 里的 FOR UPDATE，这个用例会出现
// 「同一条 sold_to 被后写的覆盖 + 多条权益指向同一条」而变红。
func TestSocks17ConcurrentNoDoubleSell(t *testing.T) {
	env := newSocks17Env(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		socks17Sock(t, env.db, fmt.Sprintf("race-%d.example.com", i+1), "exclusive")
	}
	sku := socks17SKU(t, env.db, "exclusive", 0)
	for i := 0; i < 10; i++ {
		socks17User(t, env.db, uint64(400+i))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		userID := uint64(400 + i)
		orderID := uint64(4000 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			env.h.doCompleteOrder(ctx, socks17Order(orderID, userID, sku, false))
		}()
	}
	close(start)
	wg.Wait()

	// 每条订单最多一条权益，线路不能重复出现。
	holders := map[uint64]uint64{}
	success := 0
	for i := 0; i < 10; i++ {
		orderID := uint64(4000 + i)
		userID := uint64(400 + i)
		got, _ := socks17Entitlement(t, env.db, orderID)
		if got == nil {
			continue
		}
		success++
		if prev, dup := holders[*got]; dup {
			t.Fatalf("线路 #%d 同时卖给了用户 %d 和 %d（重复分配）", *got, prev, userID)
		}
		holders[*got] = userID
		soldTo, _ := socks17SockState(t, env.db, *got)
		if soldTo == nil || *soldTo != userID {
			t.Fatalf("线路 #%d 的 sold_to 应为 %d，实际 %s", *got, userID, socks17Ptr(soldTo))
		}
	}
	if success != 5 {
		t.Fatalf("5 条线路应恰好成交 5 单，实际 %d 单（日志：%s）", success, env.gotLog())
	}
	if n := strings.Count(env.gotLog(), "独占 socks 池已空"); n != 5 {
		t.Fatalf("应有 5 单因池空失败，实际 %d 单（日志：%s）", n, env.gotLog())
	}
}

// TestSocks17RenewalKeepsSock：续费沿用原来那条，不重新分配、不白占新线路。
func TestSocks17RenewalKeepsSock(t *testing.T) {
	env := newSocks17Env(t)
	ctx := context.Background()

	s1 := socks17Sock(t, env.db, "renew-1.example.com", "exclusive")
	s2 := socks17Sock(t, env.db, "renew-2.example.com", "exclusive")
	sku := socks17SKU(t, env.db, "exclusive", 0)
	socks17User(t, env.db, 501)

	env.h.doCompleteOrder(ctx, socks17Order(5001, 501, sku, false))
	first, _ := socks17Entitlement(t, env.db, 5001)
	if first == nil || *first != s1 {
		t.Fatalf("首单应拿到 socks#%d，实际 %s", s1, socks17Ptr(first))
	}

	// 续费单：existing 权益会被 FindRenewableEntitlement 找到 → 沿用。
	env.h.doCompleteOrder(ctx, socks17Order(5002, 501, sku, true))
	if got := socks17UserSock(t, env.db, 501); got == nil || *got != *first {
		t.Fatalf("续费后权益应仍指向 socks#%d，实际 %s（日志：%s）", *first, socks17Ptr(got), env.gotLog())
	}
	// 续费不新增权益行（同一用户只有一条）。
	var rows int
	if err := env.db.QueryRow("SELECT COUNT(*) FROM proxy_entitlements WHERE user_id = 501").Scan(&rows); err != nil {
		t.Fatalf("统计权益失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("续费应改原行而不是新增，实际 %d 行", rows)
	}
	if soldTo, _ := socks17SockState(t, env.db, s2); soldTo != nil {
		t.Fatalf("续费不该占用新线路，socks#%d 实际 sold_to=%s", s2, socks17Ptr(soldTo))
	}
}

// TestSocks17RenewalAfterReleaseTakesNewSock：老线路到期已释放并被别人买走，
// 原用户续费时不能继续挂那条（否则一条线路两个主人），应改拿池子里空闲的。
func TestSocks17RenewalAfterReleaseTakesNewSock(t *testing.T) {
	env := newSocks17Env(t)
	ctx := context.Background()

	s1 := socks17Sock(t, env.db, "rel-1.example.com", "exclusive")
	s2 := socks17Sock(t, env.db, "rel-2.example.com", "exclusive")
	sku := socks17SKU(t, env.db, "exclusive", 0)
	socks17User(t, env.db, 601)
	socks17User(t, env.db, 602)

	// A 买了 s1，到期被维护任务放回池子，然后 B 买走了 s1。
	env.h.doCompleteOrder(ctx, socks17Order(6001, 601, sku, false))
	if _, err := env.db.Exec(
		"UPDATE proxy_entitlements SET status = 'expired', expires_at = ? WHERE order_id = 6001",
		socks17Now.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("标记权益过期失败: %v", err)
	}
	if _, err := env.db.Exec(
		"UPDATE proxy_socks SET sold_to = NULL, sold_at = NULL WHERE id = ?", s1); err != nil {
		t.Fatalf("释放 s1 失败: %v", err)
	}
	env.h.doCompleteOrder(ctx, socks17Order(6002, 602, sku, false))
	if got, _ := socks17Entitlement(t, env.db, 6002); got == nil || *got != s1 {
		t.Fatalf("用户 602 应买到被释放的 socks#%d，实际 %s", s1, socks17Ptr(got))
	}

	// A 续费：老权益里的 socks_id 还是 s1，但 s1 现在归 602，必须换成 s2。
	env.h.doCompleteOrder(ctx, socks17Order(6003, 601, sku, true))
	got := socks17UserSock(t, env.db, 601)
	if got == nil || *got != s2 {
		t.Fatalf("续费应改拿空闲的 socks#%d，实际 %s（日志：%s）", s2, socks17Ptr(got), env.gotLog())
	}
	if soldTo, _ := socks17SockState(t, env.db, s1); soldTo == nil || *soldTo != 602 {
		t.Fatalf("socks#%d 应仍归 602，实际 %s", s1, socks17Ptr(soldTo))
	}
}
