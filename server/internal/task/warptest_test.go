package task

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// 测试库走本地便携 MariaDB（127.0.0.1:3307，root 无密码），未起时跳过。
// 每个用例建一个独立临时库，用完即删，不碰 controlhub 真库。
const (
	testDSNBase = "root@tcp(127.0.0.1:3307)/?parseTime=true&loc=UTC"
	testDSNTpl  = "root@tcp(127.0.0.1:3307)/%s?parseTime=true&loc=UTC"
)

func openWarpTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("SKIP_DB_TESTS") == "1" {
		t.Skip("SKIP_DB_TESTS=1")
	}
	admin, err := sql.Open("mysql", testDSNBase)
	if err != nil {
		t.Skipf("连不上本地测试库（127.0.0.1:3307）: %v", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("本地测试库未启动（127.0.0.1:3307）: %v", err)
	}

	name := fmt.Sprintf("warp_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("建临时库失败: %v", err)
	}

	db, err := sql.Open("mysql", fmt.Sprintf(testDSNTpl, name))
	if err != nil {
		t.Fatalf("连临时库失败: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name)
		admin.Close()
	})

	// xray:warp-rotate 只碰这两张表：账户表 + JOIN 的 nodes。
	stmts := []string{
		`CREATE TABLE nodes (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			enabled TINYINT(1) NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE node_warp_accounts (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			node_id BIGINT UNSIGNED NOT NULL,
			auto_rotate_hours SMALLINT UNSIGNED NOT NULL DEFAULT 0,
			last_rotate_at TIMESTAMP NULL DEFAULT NULL,
			enabled TINYINT(1) NOT NULL DEFAULT 0,
			created_at TIMESTAMP NULL DEFAULT NULL,
			updated_at TIMESTAMP NULL DEFAULT NULL,
			UNIQUE KEY node_warp_accounts_node_id_unique (node_id)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}
	return db
}

func seedRotateAccount(t *testing.T, db *sql.DB, nodeID int64, name string, hours int, lastRotate time.Time) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO nodes (id, name, enabled) VALUES (?, ?, 1)", nodeID, name); err != nil {
		t.Fatalf("插节点失败: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_warp_accounts (node_id, auto_rotate_hours, last_rotate_at, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		nodeID, hours, lastRotate, lastRotate, lastRotate); err != nil {
		t.Fatalf("插 WARP 账户失败: %v", err)
	}
}
