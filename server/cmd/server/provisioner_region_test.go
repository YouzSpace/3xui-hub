package main

// 建号派发范围（#地区功能 05）：只在用户绑定的节点上建号，无绑定保持全量。
//
// 三条用例实连本机 MySQL，各自在独立库 ch_region05_* 里造数据，跑完 DROP，互不影响。
// 断言只看「派发了几个 job、派到哪个节点」—— handleProvisionClient 不在本任务范围内，
// 因此测试里注册的是计数 handler，不碰 3x-ui。
//
// 为什么用 Stats().Pending 断言条数：Dispatch 是同步的，入队发生在返回之前；
// worker 未启动，所以 Pending 就是本次派发条数，没有竞态、不需要 sleep 等结果。

import (
	"context"
	"database/sql"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"controlhub-server/internal/handler/api"
	"controlhub-server/internal/task"
)

// region05DSN 连本机 MySQL（root/空密码），可用 REGION05_MYSQL_DSN 覆盖。
func region05DSN() string {
	if dsn := os.Getenv("REGION05_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// region05SchemaDSN 把 DSN 的默认库换成 schema。
func region05SchemaDSN(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	head, tail := dsn[:slash+1], ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	return head + schema + tail
}

// newRegion05Schema 建干净的测试库 + nodes / user_nodes 两表，返回连到该库的连接。
// user_nodes 的 DDL 与 cmd/server/schema.go 里 ensureSchema 建的一字不差。
func newRegion05Schema(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", region05DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过建号派发集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过建号派发集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", region05DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region05SchemaDSN(region05DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ddl := []string{
		`CREATE TABLE nodes (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(255) NOT NULL DEFAULT '',
			enabled tinyint(1) NOT NULL DEFAULT 1,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE user_nodes (
			user_id bigint unsigned NOT NULL,
			node_id bigint unsigned NOT NULL,
			PRIMARY KEY (user_id, node_id),
			KEY idx_node (node_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, stmt)
		}
	}
	return db
}

// seedRegion05 造 3 个 enabled 节点（id 1/2/3）+ 1 个 enabled=0 的节点（id 4）。
func seedRegion05(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		"INSERT INTO nodes (id, name, enabled) VALUES (1, 'hk-1', 1)",
		"INSERT INTO nodes (id, name, enabled) VALUES (2, 'jp-1', 1)",
		"INSERT INTO nodes (id, name, enabled) VALUES (3, 'us-1', 1)",
		"INSERT INTO nodes (id, name, enabled) VALUES (4, 'off-1', 0)",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("造节点失败: %v\nSQL: %s", err, stmt)
		}
	}
}

// bindRegion05 给用户绑节点。
func bindRegion05(t *testing.T, db *sql.DB, userID, nodeID int64) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)", userID, nodeID); err != nil {
		t.Fatalf("造绑定失败: %v", err)
	}
}

// dispatchRegion05 真跑一次 Dispatch，返回派发条数与目标节点 id（升序）。
func dispatchRegion05(t *testing.T, db *sql.DB, userID uint64) (int64, []int64) {
	t.Helper()

	q := task.NewQueue(nil, task.QueueOptions{
		Workers: 1,
		Buffer:  64,
		Logf:    func(string, ...any) {},
	})
	collected := make(chan int64, 64)
	q.Register(task.JobProvisionClient, func(_ context.Context, job task.JobPayload) error {
		if job.UserID == nil {
			t.Errorf("派发的 job 没带 UserID（node=%d）", job.NodeID)
		} else if *job.UserID != int64(userID) {
			t.Errorf("派发的 job UserID=%d，期望 %d", *job.UserID, userID)
		}
		collected <- job.NodeID
		return nil
	})

	p := NewQueueProvisioner(db, q, nil, log.New(io.Discard, "", 0))
	if err := p.Dispatch(context.Background(), &api.User{ID: userID}); err != nil {
		t.Fatalf("Dispatch 失败: %v", err)
	}

	// Dispatch 已返回、worker 还没启动 → 队列里的条数就是派发条数（确定值）。
	pending := q.Stats().Pending

	q.Start()
	defer q.Stop()

	ids := make([]int64, 0, pending)
	timeout := time.After(5 * time.Second)
	for int64(len(ids)) < pending {
		select {
		case id := <-collected:
			ids = append(ids, id)
		case <-timeout:
			t.Fatalf("只收到 %d 个 job，队列报告 %d 个", len(ids), pending)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return pending, ids
}

// 用例 1：用户没有任何绑定 → 保持老行为，3 个 enabled 节点各派一个。
// 同时给另一个用户造绑定，证明「无绑定」是按当前用户判的，不是全表有没有绑定。
func TestRegion05DispatchUnboundUserGetsAllEnabledNodes(t *testing.T) {
	db := newRegion05Schema(t, "ch_region05_all")
	seedRegion05(t, db)

	const userID = 1001
	// 干扰数据：另一个用户绑了 1 个节点，不能影响 userID 的全量派发。
	bindRegion05(t, db, 2002, 1)

	count, ids := dispatchRegion05(t, db, userID)
	if count != 3 {
		t.Fatalf("无绑定用户应派发 3 个 job，实际 %d 个（节点 %v）", count, ids)
	}
	want := []int64{1, 2, 3}
	if len(ids) != len(want) {
		t.Fatalf("目标节点应为 %v，实际 %v", want, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("目标节点应为 %v，实际 %v", want, ids)
		}
	}
}

// 用例 2：用户绑定了 1 个 enabled 节点 → 只派 1 个，且就是那个节点。
func TestRegion05DispatchBoundUserGetsOnlyBoundNode(t *testing.T) {
	db := newRegion05Schema(t, "ch_region05_bound")
	seedRegion05(t, db)

	const userID = 1002
	bindRegion05(t, db, userID, 2)

	count, ids := dispatchRegion05(t, db, userID)
	if count != 1 {
		t.Fatalf("绑定 1 个节点的用户应派发 1 个 job，实际 %d 个（节点 %v）", count, ids)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("应只派发节点 2，实际 %v", ids)
	}
}

// 用例 3：用户绑定的节点 enabled=0 → 一个都不派。
func TestRegion05DispatchBoundDisabledNodeGetsNothing(t *testing.T) {
	db := newRegion05Schema(t, "ch_region05_off")
	seedRegion05(t, db)

	const userID = 1003
	bindRegion05(t, db, userID, 4) // node 4 的 enabled=0

	count, ids := dispatchRegion05(t, db, userID)
	if count != 0 {
		t.Fatalf("绑定节点已停用的用户应派发 0 个 job，实际 %d 个（节点 %v）", count, ids)
	}
}
