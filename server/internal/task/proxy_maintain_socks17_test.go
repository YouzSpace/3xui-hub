package task

// 独占 socks 到期释放（地区功能-17）：proxy:maintain 把到期权益标 expired 时，
// 必须把那条线路放回池子（sold_to/sold_at 清空），否则这条永远卖不出去。
//
// 连真库的理由：这是 UPDATE + 释放两段 SQL 的顺序问题，fake 证明不了
// 「清完之后真的能被下一个人买到」—— 最后一条断言直接用 sockpool.Acquire 复核。

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"controlhub-server/internal/sockpool"
)

func socks17taskDSN() string {
	if dsn := os.Getenv("REGION17_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

func socks17taskWithSchema(dsn, schema string) string {
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

var socks17taskDDL = []string{
	`CREATE TABLE proxy_accounts (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		upstream_id varchar(64) NOT NULL DEFAULT '',
		sku_id bigint unsigned DEFAULT NULL,
		status varchar(16) NOT NULL DEFAULT 'active',
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_socks (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(100) NOT NULL,
		host varchar(190) NOT NULL,
		port int unsigned NOT NULL,
		is_active tinyint(1) NOT NULL DEFAULT 1,
		allocation varchar(16) NOT NULL DEFAULT 'shared',
		sold_to bigint unsigned DEFAULT NULL,
		sold_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE proxy_entitlements (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		user_id bigint unsigned NOT NULL,
		order_id bigint unsigned DEFAULT NULL,
		sku_id bigint unsigned NOT NULL,
		kind varchar(16) NOT NULL,
		account_id bigint unsigned DEFAULT NULL,
		socks_id bigint unsigned DEFAULT NULL,
		expires_at timestamp NULL DEFAULT NULL,
		status varchar(16) NOT NULL DEFAULT 'active',
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

func newSocks17taskDB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", socks17taskDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过到期释放集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过到期释放集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", socks17taskDSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", socks17taskWithSchema(socks17taskDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range socks17taskDDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// TestProxyMaintain17ReleasesExclusiveSock：到期权益标 expired 的同时，
// 独占线路回到池子（能再次被买），共享线路不被碰。
func TestProxyMaintain17ReleasesExclusiveSock(t *testing.T) {
	schema := "ch_region17_task_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	db := newSocks17taskDB(t, schema)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	insertSock := func(host, allocation string, soldTo any) uint64 {
		res, err := db.Exec(`INSERT INTO proxy_socks (name, host, port, is_active, allocation, sold_to, sold_at)
			VALUES (?, ?, 1080, 1, ?, ?, ?)`, host, host, allocation, soldTo, soldTo)
		if err != nil {
			t.Fatalf("建 socks 失败: %v", err)
		}
		id, _ := res.LastInsertId()
		return uint64(id)
	}
	exclusive := insertSock("task-ex.example.com", "exclusive", 801)
	shared := insertSock("task-sh.example.com", "shared", nil)

	// 一条到期的独占权益 + 一条到期的共享权益。
	if _, err := db.Exec(`INSERT INTO proxy_entitlements (user_id, order_id, sku_id, kind, socks_id, expires_at, status)
		VALUES (801, 8101, 1, 'socks', ?, ?, 'active')`, exclusive, now.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("建独占权益失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO proxy_entitlements (user_id, order_id, sku_id, kind, socks_id, expires_at, status)
		VALUES (802, 8102, 1, 'socks', ?, ?, 'active')`, shared, now.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("建共享权益失败: %v", err)
	}

	runner := NewCommandRunner(CommandDeps{DB: db, Now: func() time.Time { return now }})
	if err := runner.Run(ctx, "proxy:maintain", nil); err != nil {
		t.Fatalf("proxy:maintain 执行失败: %v", err)
	}

	var status string
	if err := db.QueryRow("SELECT status FROM proxy_entitlements WHERE order_id = 8101").Scan(&status); err != nil {
		t.Fatalf("读权益状态失败: %v", err)
	}
	if status != "expired" {
		t.Fatalf("到期权益应标 expired，实际 %q", status)
	}

	var soldTo sql.NullInt64
	var soldAt sql.NullTime
	if err := db.QueryRow("SELECT sold_to, sold_at FROM proxy_socks WHERE id = ?", exclusive).Scan(&soldTo, &soldAt); err != nil {
		t.Fatalf("读独占线路状态失败: %v", err)
	}
	if soldTo.Valid || soldAt.Valid {
		t.Fatalf("独占线路应回到池子（sold_to/sold_at 清空），实际 sold_to=%v sold_at=%v", soldTo, soldAt)
	}

	// 真的能再卖一次：拿别的用户去池子里挑，应该挑到这条。
	got, err := sockpool.Acquire(ctx, db, nil, nil, 999, now)
	if err != nil {
		t.Fatalf("释放后应能再次分配: %v", err)
	}
	if got != exclusive {
		t.Fatalf("重新分配应拿到被释放的 #%d，实际 #%d", exclusive, got)
	}
	if err := sockpool.Release(ctx, db, got, now); err != nil {
		t.Fatalf("二次释放失败: %v", err)
	}

	// 共享线路不参与独占池：sold_to 本来就是 NULL，不该被写。
	var sharedSold sql.NullInt64
	if err := db.QueryRow("SELECT sold_to FROM proxy_socks WHERE id = ?", shared).Scan(&sharedSold); err != nil {
		t.Fatalf("读共享线路失败: %v", err)
	}
	if sharedSold.Valid {
		t.Fatalf("共享线路不该有 sold_to，实际 %v", sharedSold)
	}
}
