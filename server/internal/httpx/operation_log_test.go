package httpx

// 操作审计中间件的「跳过明细」钩子单测（issue #142 第 3 条）。
//
// 背景：原来管理端登录失败逐条落 operation_logs，实测 18,756 行失败登录占全表 92.5%，
// 真正的系统事件被埋。现在 handler 自己按来源 IP 的失败窗口聚合（只落一条汇总），
// 逐条明细由 OpLogSkipKey 跳过。这里只验一件事：标记置了就必须一条也不写、
// 没置的写请求照旧写。
//
// 不连真库：挂一个最小 database/sql 假驱动，只统计 INSERT 次数。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// ── 最小假库（只认 INSERT INTO operation_logs）──

type oplogFakeState struct {
	mu      sync.Mutex
	inserts int
}

func (s *oplogFakeState) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inserts
}

func (s *oplogFakeState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserts = 0
}

var (
	oplogFake     = &oplogFakeState{}
	oplogFakeOnce sync.Once
)

const oplogFakeDriverName = "httpxOpLogFake"

func openOpLogFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	oplogFakeOnce.Do(func() { sql.Register(oplogFakeDriverName, oplogFakeDriver{}) })
	db, err := sql.Open(oplogFakeDriverName, "fake")
	if err != nil {
		t.Fatalf("打开假库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type oplogFakeDriver struct{}

func (oplogFakeDriver) Open(string) (driver.Conn, error) { return oplogFakeConn{}, nil }

type oplogFakeConn struct{}

func (oplogFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("假驱动只支持 ExecContext")
}
func (oplogFakeConn) Close() error              { return nil }
func (oplogFakeConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("假驱动不支持事务") }

func (oplogFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if !hasPrefix(query, "INSERT INTO operation_logs") {
		return nil, fmt.Errorf("假库未识别的写入: %s", query)
	}
	oplogFake.mu.Lock()
	oplogFake.inserts++
	oplogFake.mu.Unlock()
	return driver.RowsAffected(1), nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// ── 用例 ──

func TestOperationLogSkipsFlaggedRequests(t *testing.T) {
	oplogFake.reset()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(NewOperationLogMiddleware(openOpLogFakeDB(t), log.New(io.Discard, "", 0)).Handler())
	r.POST("/admin-api/login", func(c *gin.Context) {
		switch c.GetHeader("X-Test-Case") {
		case "skip":
			// 与 admin.Login 的失败/限流路径同口径：自己聚合审计 → 跳过明细。
			c.Set(OpLogSkipKey, true)
			c.Data(http.StatusTooManyRequests, "application/json; charset=utf-8",
				[]byte(`{"code":429,"msg":"登录失败次数过多","data":null}`))
		case "fail":
			c.JSON(http.StatusOK, gin.H{"code": 401, "msg": "账号或密码错误"})
		default:
			c.JSON(http.StatusOK, gin.H{"code": 0, "msg": "登录成功"})
		}
	})

	do := func(kind string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin-api/login", nil)
		req.Header.Set("X-Test-Case", kind)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
	}

	do("skip")
	if n := oplogFake.count(); n != 0 {
		t.Fatalf("标记跳过的请求不应落审计明细，实际 %d 条", n)
	}

	do("fail")
	if n := oplogFake.count(); n != 1 {
		t.Fatalf("未标记的失败写请求仍应落一条，实际 %d 条", n)
	}

	do("ok")
	if n := oplogFake.count(); n != 2 {
		t.Fatalf("成功登录应落一条审计，实际 %d 条", n)
	}
}
