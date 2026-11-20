package task

// std02_helpers_test.go 是「规范化-02：补关键路径测试」共用的测试基建。
//
// 口径（与项目既有集成测试一致）：
//   - 连本机 MySQL（root/空密码），可用 TEST_DSN 覆盖；连不上就 t.Skip；
//   - 每个用例建独立库 ch_std02_task_<用例>_<pid>，t.Cleanup 里 DROP；
//   - 表结构直接加载生产 schema（server/schema/schema.sql），不手抄 DDL ——
//     手抄会随迁移漂移，测出来的「通过」可能是假通过。
//
// 为什么不用 mock：本包要验证的就是 SQL 语义（条件更新、聚合、保留期删除），
// 打桩只能测到自己的 if。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// std02AdminDSN 返回不含库名的管理连接 DSN。
func std02AdminDSN() string {
	if dsn := os.Getenv("TEST_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// std02DSNForSchema 在管理 DSN 上挂库名（保留原有 query 参数）。
func std02DSNForSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	q := strings.Index(dsn, "?")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := "?charset=utf8mb4&parseTime=true&loc=UTC&timeout=5s"
	if q > slash {
		tail = dsn[q:]
		if !strings.Contains(tail, "parseTime") {
			tail += "&parseTime=true&loc=UTC"
		}
	}
	return head + schema + tail
}

// std02SchemaSQL 定位并读取生产 schema 文件（server/schema/schema.sql）。
func std02SchemaSQL(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败，拿不到本测试文件路径")
	}
	// <server>/internal/task/std02_helpers_test.go → <server>/schema/schema.sql
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "schema.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读生产 schema 失败（%s）: %v", path, err)
	}
	var out []string
	var cur []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur = append(cur, line)
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSpace(strings.Join(cur, "\n"))
			stmt = strings.TrimSuffix(stmt, ";")
			if stmt != "" {
				out = append(out, stmt)
			}
			cur = nil
		}
	}
	if len(out) < 40 {
		t.Fatalf("schema.sql 解析出 %d 条语句，明显不对（期望 50+）", len(out))
	}
	return out
}

// newStd02TaskDB 建一个干净的测试库（生产 schema 全表），返回连接。
// t.Cleanup 负责删库；连不上 MySQL 时整条用例 Skip。
func newStd02TaskDB(t *testing.T, caseName string) *sql.DB {
	t.Helper()

	adminDSN := std02AdminDSN()
	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过任务集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过任务集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	schema := fmt.Sprintf("ch_std02_task_%s_%d", caseName, os.Getpid())
	if len(schema) > 64 {
		schema = schema[:64]
	}
	for _, q := range []string{
		"DROP DATABASE IF EXISTS `" + schema + "`",
		"CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci",
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("准备测试库失败（%q）: %v", q, err)
		}
	}
	t.Cleanup(func() {
		a, err := sql.Open("mysql", adminDSN)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", std02DSNForSchema(adminDSN, schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })

	for _, stmt := range std02SchemaSQL(t) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建表失败: %v\n语句: %s", err, stmt)
		}
	}
	return db
}

// mustExecTask 是「插数据失败即 Fatal」的简写。
func mustExecTask(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("执行失败: %v\nSQL: %s", err, query)
	}
}

// countRows 数一行 COUNT(*)，断言用。
func countRows(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("计数失败: %v\nSQL: %s", err, query)
	}
	return n
}

// TestStd02SchemaProbe 自检：测试库建得起来、关键表都在。
// 这条用例同时是大批集成用例的「探针」—— 它 Skip，说明本机没有 MySQL。
func TestStd02SchemaProbe(t *testing.T) {
	db := newStd02TaskDB(t, "probe")
	for _, table := range []string{
		"users", "site_configs", "async_tasks", "async_task_items", "scheduled_task_runs",
		"cache", "mail_logs", "node_api_commands", "access_tokens", "operation_logs",
		"traffic_snapshots", "orders", "discount_codes", "nodes",
	} {
		var n int64
		err := db.QueryRow(
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&n)
		if err != nil {
			t.Fatalf("查表 %s 失败: %v", table, err)
		}
		if n == 0 {
			t.Fatalf("生产 schema 里没有表 %s", table)
		}
	}
}
