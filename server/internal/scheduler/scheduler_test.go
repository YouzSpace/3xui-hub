package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ───────────────────── 集成测试底座（实连 MySQL） ─────────────────────
//
// 每个用例自建 ch_region08_scheduler 库、跑完 DROP，不碰本机任何已有库；
// 连不上 MySQL 时 skip 而不是 fail（与仓库其他集成测试一致）。
// 分布式锁用 fake，不解 Redis。

// 库名带进程号：多个 worker 同时在同一个工作区跑 go test 时，各自的库互不覆盖
// （固定库名会被另一个进程的 DROP DATABASE 打断 —— 实测过「Unknown database」）。
var testSchema = fmt.Sprintf("ch_region08_scheduler_%d", os.Getpid())

// testDSN 连本机 MySQL（root / 空密码），可用 REGION08_MYSQL_DSN 覆盖。
func testDSN() string {
	if dsn := os.Getenv("REGION08_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// withSchema 把 DSN 的默认库换成 schema，并补齐生产同款参数
// （parseTime=true&loc=UTC 与 config.DBConfig.DSN() 一致）。
func withSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	return dsn[:slash+1] + schema + "?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=UTC&timeout=3s"
}

// testSchemaDDL 只建本包用到的 5 张表，列类型照 server/schema/schema.sql。
var testSchemaDDL = []string{
	`CREATE TABLE regions (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		sort int NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY regions_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		port int NOT NULL DEFAULT '443',
		enabled tinyint(1) NOT NULL DEFAULT '1',
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'offline',
		cpu decimal(5,2) DEFAULT NULL,
		mem decimal(5,2) DEFAULT NULL,
		region_id bigint unsigned DEFAULT NULL,
		capacity int DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_nodes_region (region_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE users (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		token varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		uuid char(36) COLLATE utf8mb4_unicode_ci NOT NULL,
		enabled tinyint(1) NOT NULL DEFAULT '1',
		plan_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY users_updated_at_index (updated_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// plans 只有分配要读的两列（#地区功能-15 的套餐地区过滤会 JOIN 它）。NULL region_ids = 不限地区。
	`CREATE TABLE plans (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		region_ids json DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_node_migrations (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		user_id bigint unsigned NOT NULL,
		from_node_id bigint unsigned DEFAULT NULL,
		to_node_id bigint unsigned NOT NULL,
		reason varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		operator varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_user (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newTestScheduler 建干净测试库 + 5 张表，返回 Scheduler（锁定注入 fake）。
func newTestScheduler(t *testing.T, locker Locker) (*Scheduler, *sql.DB) {
	t.Helper()

	admin, err := sql.Open("mysql", testDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过 scheduler 集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过 scheduler 集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	mustExec(t, admin, "DROP DATABASE IF EXISTS `"+testSchema+"`")
	mustExec(t, admin, "CREATE DATABASE `"+testSchema+"` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci")
	t.Cleanup(func() {
		a, err := sql.Open("mysql", testDSN())
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec("DROP DATABASE IF EXISTS `" + testSchema + "`")
	})

	db, err := sql.Open("mysql", withSchema(testDSN(), testSchema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range testSchemaDDL {
		mustExec(t, db, stmt)
	}
	return New(db, locker), db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("执行失败: %v\nSQL: %s", err, query)
	}
}

// scalarInt 跑一个返回单列整数的查询。
func scalarInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询失败: %v\nSQL: %s", err, query)
	}
	return n
}

// addRegion 建一个地区（显式 id，便于断言）。
func addRegion(t *testing.T, db *sql.DB, id int64, name string, sort int) {
	t.Helper()
	mustExec(t, db, "INSERT INTO regions (id, name, sort) VALUES (?, ?, ?)", id, name, sort)
}

// addNode 建一个节点：regionID=0 表示 NULL（不参与地区），capacity<0 表示 NULL（不限）。
func addNode(t *testing.T, db *sql.DB, id, regionID, capacity int64, enabled bool, status string) {
	t.Helper()
	var regionArg, capacityArg any
	if regionID > 0 {
		regionArg = regionID
	}
	if capacity >= 0 {
		capacityArg = capacity
	}
	mustExec(t, db,
		"INSERT INTO nodes (id, name, host, port, enabled, status, region_id, capacity) VALUES (?, ?, ?, 443, ?, ?, ?, ?)",
		id, fmt.Sprintf("node-%d", id), "127.0.0.1", enabled, status, regionArg, capacityArg)
}

// addUsers 建 count 个用户（id 从 fromID 开始），updated_at 固定成 updatedAt。
func addUsers(t *testing.T, db *sql.DB, fromID, count int, updatedAt string) {
	t.Helper()
	const chunk = 500
	for start := 0; start < count; start += chunk {
		end := start + chunk
		if end > count {
			end = count
		}
		var b strings.Builder
		b.WriteString("INSERT INTO users (id, token, uuid, updated_at) VALUES ")
		args := make([]any, 0, (end-start)*4)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString("(?,?,?,?)")
			id := fromID + i
			args = append(args, id, fmt.Sprintf("token-%d", id), fmt.Sprintf("00000000-0000-0000-0000-%012d", id), updatedAt)
		}
		mustExec(t, db, b.String(), args...)
	}
}

// bindUsers 把 count 个用户（id 从 fromID 开始）绑到 nodeID 上。
func bindUsers(t *testing.T, db *sql.DB, nodeID int64, fromID, count int) {
	t.Helper()
	const chunk = 500
	for start := 0; start < count; start += chunk {
		end := start + chunk
		if end > count {
			end = count
		}
		var b strings.Builder
		b.WriteString("INSERT INTO user_nodes (user_id, node_id) VALUES ")
		args := make([]any, 0, (end-start)*2)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			b.WriteString("(?,?)")
			args = append(args, fromID+i, nodeID)
		}
		mustExec(t, db, b.String(), args...)
	}
}

// ───────────────────── fake 分布式锁 ─────────────────────

// fakeLocker 记录每次抢锁，可模拟「前 failures 次抢不到」「底层报错」。
type fakeLocker struct {
	mu       sync.Mutex
	failures int   // 前 failures 次抢锁返回 ok=false
	err      error // 非 nil 时 Lock 直接返回该错误

	calls   int
	keys    []string
	ttls    []time.Duration
	unlocks int
}

func (f *fakeLocker) Lock(_ context.Context, key string, ttl time.Duration) (func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.keys = append(f.keys, key)
	f.ttls = append(f.ttls, ttl)
	if f.err != nil {
		return nil, false, f.err
	}
	if f.calls <= f.failures {
		return nil, false, nil
	}
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unlocks++
	}, true, nil
}

func (f *fakeLocker) stats() (calls, unlocks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.unlocks
}

func (f *fakeLocker) lastCall() (string, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keys) == 0 {
		return "", 0
	}
	return f.keys[len(f.keys)-1], f.ttls[len(f.ttls)-1]
}
