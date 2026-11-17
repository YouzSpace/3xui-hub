package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// 本用例连本机 MySQL（root/空密码，可用 REGION11_MYSQL_DSN 覆盖），
// 每次跑都自建自删独立库，不碰任何现有库。
// 连不上就 skip（这里要验证的是真实 SQL 行为，不能打桩）。
//
// 库名带进程号：多份工作区/并行 go test 同时跑本包时，各自的库互不覆盖
// （固定库名会被另一个进程的 DROP DATABASE 打断）。
var testSchema = fmt.Sprintf("ch_region11_metrics_%d", os.Getpid())

// testSchemaDDL 与 cmd/server/schema.go 里 node_metrics 的定义逐列对齐。
const testSchemaDDL = `CREATE TABLE node_metrics (
	id bigint unsigned NOT NULL AUTO_INCREMENT,
	node_id bigint unsigned NOT NULL,
	ts datetime NOT NULL,
	users int NOT NULL DEFAULT 0,
	mem_used int NOT NULL DEFAULT 0,
	mem_total int NOT NULL DEFAULT 0,
	cpu decimal(5,2) DEFAULT NULL,
	conns int NOT NULL DEFAULT 0,
	PRIMARY KEY (id),
	KEY idx_node_ts (node_id, ts)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// testDSN 在 REGION11_MYSQL_DSN（默认本机 root 空密码）基础上换掉库名。
func testDSN(schema string) string {
	dsn := os.Getenv("REGION11_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
	}
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := ""
	if q := strings.Index(dsn, "?"); q > slash {
		tail = dsn[q:]
	}
	return head + schema + tail
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("执行 %q 失败: %v", query, err)
	}
}

// openTestDB 建一个干净的同构库并返回连到该库的连接。
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", testDSN(""))
	if err != nil {
		t.Skipf("打不开 MySQL 连接（%v），跳过集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过集成测试", err)
	}
	mustExec(t, admin, "DROP DATABASE IF EXISTS "+testSchema)
	mustExec(t, admin, "CREATE DATABASE "+testSchema+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + testSchema)
		admin.Close()
	})

	db, err := sql.Open("mysql", testDSN(testSchema))
	if err != nil {
		t.Fatalf("连测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, testSchemaDDL)
	return db
}

// insertSample 直接写一条历史样本（模拟过去采集到的数据）。
func insertSample(t *testing.T, db *sql.DB, nodeID int64, ts time.Time, users, memUsed, memTotal int) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO node_metrics (node_id, ts, users, mem_used, mem_total, cpu, conns) VALUES (?, ?, ?, ?, ?, ?, ?)",
		nodeID, ts.UTC().Truncate(time.Second), users, memUsed, memTotal, 10.5, 0)
	if err != nil {
		t.Fatalf("写入样本失败: %v", err)
	}
}

// TestRecommendNormalFit 正常拟合：24 条样本、用户数 10 万→20 万、内存 800→1150 MB。
// 期望每用户 ≈ (1150-800)/100000 MB = 3.58 KB，置信度 high，建议容量 30-36 万。
func TestRecommendNormalFit(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	const nodeID = 11

	base := time.Now().UTC().Add(-6 * 24 * time.Hour)
	for i := 0; i < 24; i++ {
		users := 100000 + 100000*i/23
		memUsed := 800 + 350*i/23
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*6*time.Hour), users, memUsed, 2048)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	t.Logf("实际结果：perUserKB=%.4f confidence=%s samples=%d suggest=%d reason=%s",
		rec.PerUserKB, rec.Confidence, rec.Samples, rec.Suggest, rec.Reason)

	if rec.Samples != 24 {
		t.Fatalf("样本数应为 24，实际 %d", rec.Samples)
	}
	if math.Abs(rec.PerUserKB-3.5) > 0.3 {
		t.Fatalf("每用户内存应 ≈3.5 KB，实际 %.4f KB", rec.PerUserKB)
	}
	if rec.Confidence != ConfidenceHigh {
		t.Fatalf("置信度应为 high，实际 %s", rec.Confidence)
	}
	if rec.Suggest < 300000 || rec.Suggest > 360000 {
		t.Fatalf("建议容量应在 30-36 万之间，实际 %d", rec.Suggest)
	}
	if rec.Suggest%1000 != 0 {
		t.Fatalf("建议容量应向上取整到 1000 的倍数，实际 %d", rec.Suggest)
	}
}

// TestRecommendInsufficientSamples 样本只有 3 条：退回默认 5.0 KB/人、置信度 low。
func TestRecommendInsufficientSamples(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	const nodeID = 12

	base := time.Now().UTC().Add(-3 * time.Hour)
	for i := 0; i < 3; i++ {
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*time.Hour), 50000+i*10000, 700+i*50, 2048)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	t.Logf("实际结果：perUserKB=%.4f confidence=%s samples=%d suggest=%d reason=%s",
		rec.PerUserKB, rec.Confidence, rec.Samples, rec.Suggest, rec.Reason)

	if rec.Samples != 3 {
		t.Fatalf("样本数应为 3，实际 %d", rec.Samples)
	}
	if rec.Confidence != ConfidenceLow {
		t.Fatalf("置信度应为 low，实际 %s", rec.Confidence)
	}
	if rec.PerUserKB != DefaultPerUserKB {
		t.Fatalf("样本不足时应退回默认 %.1f KB/人，实际 %.4f", DefaultPerUserKB, rec.PerUserKB)
	}
	if !strings.Contains(rec.Reason, "数据不足") {
		t.Fatalf("Reason 应说明数据不足，实际 %q", rec.Reason)
	}
}

// TestRecommendSmallUserSpread 样本 20 条但用户数只从 100 变到 150：
// 跨度不足 1000，无法拟合，置信度 low。
func TestRecommendSmallUserSpread(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	const nodeID = 13

	base := time.Now().UTC().Add(-20 * time.Hour)
	for i := 0; i < 20; i++ {
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*time.Hour), 100+i*50/19, 300+i*20/19, 2048)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	t.Logf("实际结果：perUserKB=%.4f confidence=%s samples=%d suggest=%d reason=%s",
		rec.PerUserKB, rec.Confidence, rec.Samples, rec.Suggest, rec.Reason)

	if rec.Samples != 20 {
		t.Fatalf("样本数应为 20，实际 %d", rec.Samples)
	}
	if rec.Confidence != ConfidenceLow {
		t.Fatalf("用户数跨度太小时置信度应为 low，实际 %s", rec.Confidence)
	}
	if rec.PerUserKB != DefaultPerUserKB {
		t.Fatalf("跨度不足时应退回默认 %.1f KB/人，实际 %.4f", DefaultPerUserKB, rec.PerUserKB)
	}
}

// TestRecommendOverCapacity 内存已经用到 80% 上限以上：建议容量低于当前用户数，
// 结果照常返回，Reason 必须写明「已超建议容量」。
func TestRecommendOverCapacity(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	const nodeID = 14

	base := time.Now().UTC().Add(-6 * 24 * time.Hour)
	for i := 0; i < 24; i++ {
		users := 100000 + 100000*i/23
		memUsed := 800 + 1200*i/23 // 最后一条 2000 MB，已超过 2048*80%=1638 MB
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*6*time.Hour), users, memUsed, 2048)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	t.Logf("实际结果：perUserKB=%.4f confidence=%s samples=%d suggest=%d reason=%s",
		rec.PerUserKB, rec.Confidence, rec.Samples, rec.Suggest, rec.Reason)

	if rec.Suggest > 200000 {
		t.Fatalf("内存已超上限时建议容量应 ≤ 当前用户数 200000，实际 %d", rec.Suggest)
	}
	if !strings.Contains(rec.Reason, "已超建议容量") {
		t.Fatalf("Reason 应含「已超建议容量」，实际 %q", rec.Reason)
	}
}

// TestRecordWritesAndCleansUp Record 要能落库，且每 100 次写会把 30 天前的数据清掉。
func TestRecordWritesAndCleansUp(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()
	const nodeID = 15

	if err := store.Record(ctx, Snapshot{NodeID: nodeID, Users: 1234, MemUsed: 900, MemTotal: 2048, CPU: 33.5, Conns: 42}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	var (
		ts       time.Time
		users    int
		memUsed  int
		memTotal int
		cpu      float64
		conns    int
	)
	if err := db.QueryRow(
		"SELECT ts, users, mem_used, mem_total, cpu, conns FROM node_metrics WHERE node_id = ?", nodeID).
		Scan(&ts, &users, &memUsed, &memTotal, &cpu, &conns); err != nil {
		t.Fatalf("Record 之后查不到数据: %v", err)
	}
	if users != 1234 || memUsed != 900 || memTotal != 2048 || conns != 42 || math.Abs(cpu-33.5) > 0.01 {
		t.Fatalf("写入内容不符：users=%d mem=%d/%d cpu=%.2f conns=%d", users, memUsed, memTotal, cpu, conns)
	}
	if ts.IsZero() {
		t.Fatal("ts 不应为零值")
	}

	// 造一条 31 天前（该被清）和一条 1 天前（该保留）的数据。
	insertSample(t, db, nodeID, time.Now().UTC().AddDate(0, 0, -31), 1111, 111, 2048)
	insertSample(t, db, nodeID, time.Now().UTC().AddDate(0, 0, -1), 2222, 222, 2048)

	// 补到第 100 次写（上面已经写过 1 次），触发清理。
	for i := 0; i < cleanupEvery-1; i++ {
		if err := store.Record(ctx, Snapshot{NodeID: 999, Users: i, MemUsed: 100, MemTotal: 2048}); err != nil {
			t.Fatalf("第 %d 次 Record 失败: %v", i+2, err)
		}
	}

	var oldCount, keptCount int
	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	if err := db.QueryRow("SELECT COUNT(*) FROM node_metrics WHERE node_id = ? AND ts < ?", nodeID, cutoff).Scan(&oldCount); err != nil {
		t.Fatalf("统计旧数据失败: %v", err)
	}
	if oldCount != 0 {
		t.Fatalf("31 天前的数据应被清理，实际还剩 %d 条", oldCount)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM node_metrics WHERE node_id = ?", nodeID).Scan(&keptCount); err != nil {
		t.Fatalf("统计保留数据失败: %v", err)
	}
	if keptCount != 2 {
		t.Fatalf("应保留 2 条（本次采样 + 1 天前那条），实际 %d 条", keptCount)
	}
}
