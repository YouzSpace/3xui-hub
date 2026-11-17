package api

// 独占 socks 池（地区功能-17）在「支付回调开通」这条真实链路上的集成测试。
//
// 为什么要单独测一遍：支付完成后的开通走的是 internal/handler/api 的
// proxyCompleter（不是管理端的 provision.go），两处各写一遍 SQL，只有这条
// 用例能证明用户真正下单付款时拿到的是一条专属线路。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"controlhub-server/internal/sockpool"
)

const socks17apiDSNEnv = "REGION17_MYSQL_DSN"

func socks17apiDSN() string {
	if dsn := os.Getenv(socks17apiDSNEnv); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

func socks17apiWithSchema(dsn, schema string) string {
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

var socks17apiDDL = []string{
	`CREATE TABLE proxy_skus (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL,
		source_type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'webshare',
		allocation varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'exclusive',
		source_id bigint unsigned DEFAULT NULL,
		duration_days int unsigned NOT NULL DEFAULT 30,
		stock_locked int unsigned NOT NULL DEFAULT 0,
		stock_sold int unsigned NOT NULL DEFAULT 0,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_socks (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int unsigned NOT NULL,
		is_active tinyint(1) NOT NULL DEFAULT 1,
		allocation varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'shared',
		sold_to bigint unsigned DEFAULT NULL,
		sold_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
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

func newSocks17apiDB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", socks17apiDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过独占 socks 支付链路测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过独占 socks 支付链路测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", socks17apiDSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", socks17apiWithSchema(socks17apiDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range socks17apiDDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// TestCompleter17ExclusiveOneEach：支付完成开通独占规格 → 每个用户拿到不同线路，
// 池空时报「独占 socks 池已空」，续费沿用原来那条。
func TestCompleter17ExclusiveOneEach(t *testing.T) {
	schema := "ch_region17_api_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	db := newSocks17apiDB(t, schema)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	completer := &proxyCompleter{db: db, logger: log.New(io.Discard, "", 0), now: func() time.Time { return now }}
	ctx := context.Background()

	insertSock := func(host string) uint64 {
		res, err := db.Exec(`INSERT INTO proxy_socks (name, host, port, is_active, allocation)
			VALUES (?, ?, 1080, 1, 'exclusive')`, host, host)
		if err != nil {
			t.Fatalf("建 socks 失败: %v", err)
		}
		id, _ := res.LastInsertId()
		return uint64(id)
	}
	s1 := insertSock("api-ex-1.example.com")
	s2 := insertSock("api-ex-2.example.com")

	// 独占规格不指定货源：下单时从整个池子里挑。
	res, err := db.Exec(`INSERT INTO proxy_skus (name, source_type, allocation, source_id, duration_days)
		VALUES ('独占100G', 'socks', 'exclusive', NULL, 30)`)
	if err != nil {
		t.Fatalf("建规格失败: %v", err)
	}
	skuID, _ := res.LastInsertId()

	order := func(orderID, userID uint64) *Order {
		return &Order{
			ID:         orderID,
			OrderNo:    fmt.Sprintf("POAPI-%04d", orderID),
			UserID:     userID,
			OrderType:  "proxy",
			ProxySkuID: sql.NullInt64{Int64: skuID, Valid: true},
			Status:     "paid",
		}
	}
	sockOf := func(orderID uint64) *uint64 {
		var id sql.NullInt64
		if err := db.QueryRow("SELECT socks_id FROM proxy_entitlements WHERE order_id = ?", orderID).Scan(&id); err != nil {
			t.Fatalf("读权益失败 order=%d: %v", orderID, err)
		}
		if !id.Valid {
			return nil
		}
		v := uint64(id.Int64)
		return &v
	}

	if err := completer.CompleteOrder(ctx, order(7001, 701)); err != nil {
		t.Fatalf("用户 701 开通应成功: %v", err)
	}
	if got := sockOf(7001); got == nil || *got != s1 {
		t.Fatalf("用户 701 应拿到 socks#%d，实际 %s", s1, fmtSock17(got))
	}
	if err := completer.CompleteOrder(ctx, order(7002, 702)); err != nil {
		t.Fatalf("用户 702 开通应成功: %v", err)
	}
	if got := sockOf(7002); got == nil || *got != s2 {
		t.Fatalf("用户 702 应拿到 socks#%d，实际 %s", s2, fmtSock17(got))
	}

	// 池空：第 3 个用户绝对不能和前面共用一条。
	err = completer.CompleteOrder(ctx, order(7003, 703))
	if !errors.Is(err, sockpool.ErrPoolEmpty) {
		t.Fatalf("池空应返回 ErrPoolEmpty，实际 %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM proxy_entitlements WHERE order_id = 7003").Scan(&n); err != nil {
		t.Fatalf("统计权益失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("池空时不该写权益，实际 %d 行", n)
	}

	// 续费：仍是原来那条，不占新线路。
	if err := completer.CompleteOrder(ctx, order(7004, 701)); err != nil {
		t.Fatalf("用户 701 续费应成功: %v", err)
	}
	var renewed sql.NullInt64
	if err := db.QueryRow("SELECT socks_id FROM proxy_entitlements WHERE user_id = 701 ORDER BY id DESC LIMIT 1").Scan(&renewed); err != nil {
		t.Fatalf("读续费权益失败: %v", err)
	}
	if !renewed.Valid || uint64(renewed.Int64) != s1 {
		t.Fatalf("续费应沿用 socks#%d，实际 %v", s1, renewed)
	}
}

// fmtSock17 把 socks id 打成 "NULL" 或 "#N"。
func fmtSock17(v *uint64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("#%d", *v)
}
