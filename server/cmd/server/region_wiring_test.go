package main

// 地区功能 #14 接线验收：Redis 缺位时的进程内锁 + 调度器写绑定。
//
// 这两条用例覆盖「支付成功后自动分配」这一步真正落地的那一小段：
//   - localLocker 必须真的互斥（互斥坏了 = 同一用户被并发绑到两台节点）；
//   - scheduler.Assign 在 localLocker 下必须真的往 user_nodes 写一行，
//     并且重复调用幂等（续费/换套餐会重复触发分配）。
//
// 为什么不连 Redis：本机没有 Redis（P7 验收时已确认），而接线恰恰要求「没 Redis 也能用」。
// 用例自建 schema（ch_region14_*），跑完 DROP，不碰任何现有库。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"controlhub-server/internal/scheduler"
)

// TestLocalLocker_MutualExclusion 验证进程内锁的三条语义：
// 同名 key 第二次抢不到、释放后可以再抢、ttl 到期后能被接管。
func TestLocalLocker_MutualExclusion(t *testing.T) {
	l := newLocalLocker()
	ctx := context.Background()

	unlock1, ok, err := l.Lock(ctx, "ch:lock:assign:region:1", 3*time.Second)
	if err != nil || !ok {
		t.Fatalf("第一次抢锁应当成功，ok=%v err=%v", ok, err)
	}

	if _, ok, err := l.Lock(ctx, "ch:lock:assign:region:1", 3*time.Second); ok || err != nil {
		t.Fatalf("同名 key 第二次抢锁必须失败（ok=%v err=%v）", ok, err)
	}

	unlock1()
	unlock1() // 幂等：重复释放不能 panic、也不能删掉别人后来拿到的锁

	unlock2, ok, err := l.Lock(ctx, "ch:lock:assign:region:1", 3*time.Second)
	if err != nil || !ok {
		t.Fatalf("释放后应当能重新抢到，ok=%v err=%v", ok, err)
	}
	unlock2()

	// ttl 极短：睡着之后锁自动过期，另一个调用方可以接管（对齐 Redis 的 PX 语义）。
	if _, ok, err := l.Lock(ctx, "ch:lock:assign:region:2", 20*time.Millisecond); err != nil || !ok {
		t.Fatalf("短 ttl 抢锁应当成功，ok=%v err=%v", ok, err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok, err := l.Lock(ctx, "ch:lock:assign:region:2", time.Second); err != nil || !ok {
		t.Fatalf("ttl 过期后应当能接管，ok=%v err=%v", ok, err)
	}
}

// wiringSchemaDDL 是 scheduler.Assign 需要的最小表集合（列名与 ensureSchema 一致）。
const wiringSchemaDDL = `
CREATE TABLE regions (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  name varchar(64) NOT NULL,
  sort int NOT NULL DEFAULT 0,
  created_at timestamp NULL DEFAULT NULL,
  updated_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY regions_name_unique (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE nodes (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  name varchar(255) NOT NULL,
  enabled tinyint(1) NOT NULL DEFAULT 1,
  status varchar(16) NOT NULL DEFAULT 'offline',
  capacity int DEFAULT NULL,
  region_id bigint unsigned DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE user_nodes (
  user_id bigint unsigned NOT NULL,
  node_id bigint unsigned NOT NULL,
  PRIMARY KEY (user_id, node_id),
  KEY idx_node (node_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE users (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  plan_id bigint unsigned DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE plans (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  name varchar(64) NOT NULL,
  region_ids json DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

// openWiringDB 建一个独立库并建好五张表（连不上 MySQL 就 skip）。
func openWiringDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("REGION14_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
	}
	withSchema := func(schema string) string {
		slash := strings.LastIndex(dsn, "/")
		if slash < 0 {
			return dsn
		}
		head, tail := dsn[:slash+1], ""
		if q := strings.Index(dsn, "?"); q > slash {
			tail = dsn[q:]
		}
		return head + schema + tail
	}

	schema := fmt.Sprintf("ch_region14_wiring_%d", os.Getpid())
	admin, err := sql.Open("mysql", withSchema(""))
	if err != nil {
		t.Skipf("打不开 MySQL 连接（%v），跳过集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过集成测试", err)
	}
	for _, q := range []string{
		"DROP DATABASE IF EXISTS " + schema,
		"CREATE DATABASE " + schema + " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
	} {
		if _, err := admin.Exec(q); err != nil {
			admin.Close()
			t.Fatalf("准备测试库失败（%q）: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + schema)
		admin.Close()
	})

	db, err := sql.Open("mysql", withSchema(schema))
	if err != nil {
		t.Fatalf("连测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range strings.Split(wiringSchemaDDL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}
	return db
}

// TestAssignWritesUserNodesWithoutRedis 复刻路由层接线的那三行：
//
//	locker := newLocalLocker()      // Redis 缺位时 routes.go 走这一支
//	sched  := scheduler.New(db, locker)
//	sched.Assign(ctx, userID)       // PaymentService 在套餐生效后调它
//
// 断言：新用户第一次购买 → user_nodes 多一行；再调一次（续费/换套餐）→ 不多行。
func TestAssignWritesUserNodesWithoutRedis(t *testing.T) {
	db := openWiringDB(t)

	// 一个地区、两台在线节点：按「已绑/capacity」升序选，两台 capacity 都 NULL → 取 id 最小的。
	if _, err := db.Exec("INSERT INTO regions (id, name, sort) VALUES (1, '默认', 0)"); err != nil {
		t.Fatalf("插入地区失败: %v", err)
	}
	for _, q := range []string{
		"INSERT INTO nodes (id, name, enabled, status, capacity, region_id) VALUES (11, 'HK-1', 1, 'online', NULL, 1)",
		"INSERT INTO nodes (id, name, enabled, status, capacity, region_id) VALUES (12, 'HK-2', 1, 'online', NULL, 1)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("插入节点失败: %v", err)
		}
	}

	sched := scheduler.New(db, newLocalLocker())
	const userID = uint64(42)

	assigned, err := sched.Assign(context.Background(), userID)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if len(assigned) != 1 || assigned[0].RegionID != 1 || assigned[0].NodeID != 11 {
		t.Fatalf("分配结果 = %+v，期望 [{RegionID:1 NodeID:11}]（平局取 id 最小的节点）", assigned)
	}

	var nodeID int64
	if err := db.QueryRow("SELECT node_id FROM user_nodes WHERE user_id = ?", userID).Scan(&nodeID); err != nil {
		t.Fatalf("user_nodes 里没有绑定记录（接线没生效）: %v", err)
	}
	if nodeID != 11 {
		t.Fatalf("绑定节点 = %d，期望 11", nodeID)
	}
	t.Logf("第一次分配后 user_nodes: user_id=%d node_id=%d", userID, nodeID)

	// 幂等：续费/换套餐会再调一次，不能再插一行、也不能换节点。
	if _, err := sched.Assign(context.Background(), userID); err != nil {
		t.Fatalf("重复分配失败: %v", err)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE user_id = ?", userID).Scan(&rows); err != nil {
		t.Fatalf("统计绑定失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("重复分配后 user_nodes 行数 = %d，期望 1（必须幂等）", rows)
	}
	t.Logf("重复分配后 user_nodes 行数 = %d（幂等）", rows)
}
