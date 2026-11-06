package collector

// 对账漂移 / 3x-ui 写回失败的审计落库单测（issue #135）。
//
// 不连真库：这里挂一个最小 database/sql 假驱动，按 SQL 文本前缀路由到内存表，
// 只为验证「写了什么、写了几条、写挂了会不会中断对账」这三件事。
// 3x-ui 侧用 httptest 起一个假面板，驱动走真实 HTTP（Bearer），不 mock 驱动层。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"controlhub-server/internal/crypto"
	"controlhub-server/internal/driver/threexui"
)

// ── 内存假库 ─────────────────────────────────────────────────

type fakeUser struct {
	ID       int64
	UUID     string
	Protocol string
	Enabled  int64
	Limit    int64
	PlanID   int64
	PlanType string
	Email    string
}

type fakeNodeRow struct {
	ID           int64
	Name         string
	Host         string
	Port         int64
	Scheme       string
	Status       string
	APIKeyCipher string
}

type fakeInbound struct {
	Protocol string
	ID       int64
}

type fakeAudit struct {
	Actor  string
	Action string
	Path   string
	Status int64
	ErrMsg string
}

type fakeStore struct {
	mu       sync.Mutex
	users    []fakeUser
	nodes    []fakeNodeRow
	inbounds []fakeInbound
	audits   []fakeAudit
	// auditQueryErr / auditInsertErr 用来模拟落库本身失败（顺序 5 的验收点）。
	auditQueryErr  error
	auditInsertErr error
}

func (s *fakeStore) snapshotAudits() []fakeAudit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fakeAudit, len(s.audits))
	copy(out, s.audits)
	return out
}

// 假驱动的全局注册表：sql.Open 只能按 dsn 拿到 store。
var (
	fakeDriverMu sync.Mutex
	fakeDriverID int
	fakeStores   = map[string]*fakeStore{}
)

func init() { sql.Register("collectorFake", fakeDriver{}) }

func openFakeDB(t *testing.T, st *fakeStore) *sql.DB {
	t.Helper()
	fakeDriverMu.Lock()
	fakeDriverID++
	dsn := strconv.Itoa(fakeDriverID)
	fakeStores[dsn] = st
	fakeDriverMu.Unlock()

	db, err := sql.Open("collectorFake", dsn)
	if err != nil {
		t.Fatalf("打开假库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	fakeDriverMu.Lock()
	st := fakeStores[dsn]
	fakeDriverMu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("未知假库 dsn=%s", dsn)
	}
	return &fakeConn{st: st}, nil
}

type fakeConn struct{ st *fakeStore }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("不支持 Prepare") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("不支持事务") }

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q := strings.TrimSpace(query)
	switch {
	case strings.Contains(q, "FROM users u LEFT JOIN plans"):
		return c.userRows(), nil
	case strings.Contains(q, "FROM nodes WHERE enabled = 1"):
		return c.nodeRows(), nil
	case strings.Contains(q, "SELECT protocol, inbound_id FROM node_inbounds"):
		return c.inboundRows(true), nil
	case strings.Contains(q, "SELECT inbound_id FROM node_inbounds"):
		return c.inboundRows(false), nil
	case strings.Contains(q, "SELECT 1 FROM operation_logs"):
		return c.auditExists(args)
	default:
		return nil, fmt.Errorf("假库未识别的查询: %s", q)
	}
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if !strings.Contains(query, "INSERT INTO operation_logs") {
		return nil, fmt.Errorf("假库未识别的写入: %s", query)
	}
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	if c.st.auditInsertErr != nil {
		return nil, c.st.auditInsertErr
	}
	if len(args) != 5 {
		return nil, fmt.Errorf("operation_logs 写入参数应为 5 个，实际 %d", len(args))
	}
	c.st.audits = append(c.st.audits, fakeAudit{
		Actor:  fmt.Sprint(args[0].Value),
		Action: fmt.Sprint(args[1].Value),
		Path:   fmt.Sprint(args[2].Value),
		Status: 500, // 假驱动只认固定 status=500 的行
		ErrMsg: fmt.Sprint(args[3].Value),
	})
	return driver.RowsAffected(1), nil
}

func (c *fakeConn) auditExists(args []driver.NamedValue) (driver.Rows, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	if c.st.auditQueryErr != nil {
		return nil, c.st.auditQueryErr
	}
	action, path := fmt.Sprint(args[0].Value), fmt.Sprint(args[1].Value)
	for _, a := range c.st.audits {
		if a.Action == action && a.Path == path {
			return &fakeRows{cols: []string{"1"}, vals: [][]driver.Value{{int64(1)}}}, nil
		}
	}
	return &fakeRows{cols: []string{"1"}}, nil
}

func (c *fakeConn) userRows() driver.Rows {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	cols := []string{"id", "uuid", "protocol", "enabled", "traffic_disabled_at", "expired_at",
		"traffic_limit", "traffic_used", "monthly_traffic_used", "monthly_traffic_limit",
		"plan_id", "type", "email"}
	var vals [][]driver.Value
	for _, u := range c.st.users {
		vals = append(vals, []driver.Value{
			u.ID, u.UUID, u.Protocol, u.Enabled, nil, nil,
			u.Limit, int64(0), int64(0), int64(0),
			u.PlanID, u.PlanType, u.Email,
		})
	}
	return &fakeRows{cols: cols, vals: vals}
}

func (c *fakeConn) nodeRows() driver.Rows {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	cols := []string{"id", "name", "host", "port", "scheme", "web_base_path", "username",
		"password", "api_key", "enabled", "verify_ssl", "driver_type", "driver_config",
		"status", "latency", "traffic_multiplier", "last_check_at", "cpu", "mem"}
	var vals [][]driver.Value
	for _, n := range c.st.nodes {
		vals = append(vals, []driver.Value{
			n.ID, n.Name, n.Host, n.Port, n.Scheme, "", "", nil, n.APIKeyCipher,
			int64(1), int64(0), "3x-ui", nil,
			n.Status, int64(0), nil, nil, nil, nil,
		})
	}
	return &fakeRows{cols: cols, vals: vals}
}

func (c *fakeConn) inboundRows(withProtocol bool) driver.Rows {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	cols := []string{"inbound_id"}
	if withProtocol {
		cols = []string{"protocol", "inbound_id"}
	}
	var vals [][]driver.Value
	for _, ib := range c.st.inbounds {
		if withProtocol {
			vals = append(vals, []driver.Value{ib.Protocol, ib.ID})
		} else {
			vals = append(vals, []driver.Value{ib.ID})
		}
	}
	return &fakeRows{cols: cols, vals: vals}
}

type fakeRows struct {
	cols []string
	vals [][]driver.Value
	pos  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.pos])
	r.pos++
	return nil
}

// ── 测试脚手架 ───────────────────────────────────────────────

var testAppKey = []byte("0123456789abcdef0123456789abcdef")

var testNow = time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)

const testNodeName = "JP1"

// newAuditTestCollector 组装一个只连假库的 Collector。
func newAuditTestCollector(t *testing.T, st *fakeStore, logs *[]string) *Collector {
	t.Helper()
	c, err := New(Options{
		DB:     openFakeDB(t, st),
		AppKey: testAppKey,
		Now:    func() time.Time { return testNow },
		Logf: func(format string, args ...any) {
			if logs != nil {
				*logs = append(*logs, fmt.Sprintf(format, args...))
			}
		},
	})
	if err != nil {
		t.Fatalf("构造 collector 失败: %v", err)
	}
	return c
}

// newFakePanel 起一个假 3x-ui 面板：inbounds/list 回一条带漂移的 client。
//
// clientStats: total=500（节点），面板侧 Limit=1000 → driftEarlyCut。
func newFakePanel(t *testing.T, email string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/panel/api/inbounds/list":
			resp := map[string]any{
				"success": true,
				"obj": []any{
					map[string]any{
						"id": 1,
						"clientStats": []any{
							map[string]any{"email": email, "enable": true, "total": 500, "expiry_time": 0},
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "unexpected " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// baseStore 建一个「1 台节点 + 1 个用户」的最小库，节点指向给定面板地址。
func baseStore(t *testing.T, panelURL, email string, userID, nodeID int64, panelLimit int64) *fakeStore {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(panelURL, "http://"))
	if err != nil {
		t.Fatalf("解析假面板地址失败: %v", err)
	}
	port, _ := strconv.ParseInt(portStr, 10, 64)
	apiKeyCipher, err := crypto.EncryptString("test-token", testAppKey)
	if err != nil {
		t.Fatalf("加密 api_key 失败: %v", err)
	}
	return &fakeStore{
		users: []fakeUser{{
			ID: userID, UUID: "uuid-" + strconv.FormatInt(userID, 10), Protocol: "vless",
			Enabled: 1, Limit: panelLimit, PlanID: 9, PlanType: "total", Email: email,
		}},
		nodes: []fakeNodeRow{{
			ID: nodeID, Name: testNodeName, Host: host, Port: port, Scheme: "http",
			Status: "online", APIKeyCipher: apiKeyCipher,
		}},
		inbounds: []fakeInbound{{Protocol: "vless", ID: 1}},
	}
}

// ── 用例 1：early-cut 漂移落库 ────────────────────────────────

func TestReconcileDriftRecordedAsEarlyCut(t *testing.T) {
	const (
		nodeID = int64(7)
		userID = int64(5)
		email  = "alice@example.com"
	)
	srv := newFakePanel(t, "ch_user_5")
	st := baseStore(t, srv.URL, email, userID, nodeID, 1000) // 面板 1000，节点 500 → earlyCut
	col := newAuditTestCollector(t, st, nil)

	rep, err := col.ReconcileNodes(context.Background(), true, 50)
	if err != nil {
		t.Fatalf("ReconcileNodes 返回错误: %v", err)
	}
	if rep.DriftEarlyCut != 1 {
		t.Fatalf("DriftEarlyCut 期望 1，实际 %d（report=%+v）", rep.DriftEarlyCut, rep)
	}

	audits := st.snapshotAudits()
	if len(audits) != 1 {
		t.Fatalf("期望落库 1 条，实际 %d 条: %+v", len(audits), audits)
	}
	got := audits[0]
	if got.Actor != "reconcile" {
		t.Errorf("actor_name 期望 reconcile，实际 %q", got.Actor)
	}
	if got.Action != "对账漂移·会提前掐人" {
		t.Errorf("action 期望 对账漂移·会提前掐人，实际 %q", got.Action)
	}
	wantPath := fmt.Sprintf("node#%d/user#%d", nodeID, userID)
	if got.Path != wantPath {
		t.Errorf("path 期望 %q，实际 %q", wantPath, got.Path)
	}
	// error 必须能看出字段、两侧值、节点名与用户 email。
	for _, want := range []string{"total", "节点=500", "面板=1000", testNodeName, email} {
		if !strings.Contains(got.ErrMsg, want) {
			t.Errorf("error 里缺少 %q：%s", want, got.ErrMsg)
		}
	}
}

// ── 用例 2：同 node+user+action 24h 内只留一条 ────────────────

func TestReconcileDriftDedupedWithin24h(t *testing.T) {
	const (
		nodeID = int64(7)
		userID = int64(5)
		email  = "alice@example.com"
	)
	srv := newFakePanel(t, "ch_user_5")
	st := baseStore(t, srv.URL, email, userID, nodeID, 1000)
	col := newAuditTestCollector(t, st, nil)

	for i := 0; i < 2; i++ {
		rep, err := col.ReconcileNodes(context.Background(), true, 50)
		if err != nil {
			t.Fatalf("第 %d 轮 ReconcileNodes 返回错误: %v", i+1, err)
		}
		if rep.DriftEarlyCut != 1 {
			t.Fatalf("第 %d 轮 DriftEarlyCut 期望 1，实际 %d", i+1, rep.DriftEarlyCut)
		}
	}

	if audits := st.snapshotAudits(); len(audits) != 1 {
		t.Fatalf("两轮同一 node+user 只应落 1 条，实际 %d 条: %+v", len(audits), audits)
	}
}

// ── 用例 3：3x-ui 写回失败落一行 ─────────────────────────────

func TestUpdateClientWritebackFailureRecorded(t *testing.T) {
	const (
		nodeID = int64(7)
		userID = int64(5)
		email  = "ch_user_5"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/get/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj":     map[string]any{"client": map[string]any{"email": email, "id": "uuid-5", "enable": true}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/update/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "database is locked"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "unexpected " + r.URL.Path})
		}
	}))
	defer srv.Close()

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	client := threexui.New(threexui.Config{Scheme: "http", Host: host, Port: port, APIKey: "k"})

	st := &fakeStore{inbounds: []fakeInbound{{Protocol: "vless", ID: 1}}}
	c, err := New(Options{DB: openFakeDB(t, st), AppKey: testAppKey,
		Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("构造 collector 失败: %v", err)
	}

	node := nodeRow{ID: nodeID, Name: testNodeName}
	u := reconcileUser{ID: userID, Protocol: "vless", Limit: 1000}
	if err := c.updateClientOnNode(context.Background(), client, node, u); err == nil {
		t.Fatal("期望 UpdateClient 失败并返回错误")
	}

	audits := st.snapshotAudits()
	if len(audits) != 1 {
		t.Fatalf("期望落库 1 条，实际 %d 条: %+v", len(audits), audits)
	}
	got := audits[0]
	if got.Action != "3x-ui 写回失败" {
		t.Errorf("action 期望 3x-ui 写回失败，实际 %q", got.Action)
	}
	if want := fmt.Sprintf("node#%d/user#%s", nodeID, email); got.Path != want {
		t.Errorf("path 期望 %q，实际 %q", want, got.Path)
	}
	if !strings.Contains(got.ErrMsg, "database is locked") {
		t.Errorf("error 应是错误原文，实际 %q", got.ErrMsg)
	}
	if got.Actor != "reconcile" {
		t.Errorf("actor_name 期望 reconcile，实际 %q", got.Actor)
	}
}

// ── 用例 4：落库报错不中断对账 ───────────────────────────────

func TestReconcileSurvivesAuditFailure(t *testing.T) {
	const (
		nodeID = int64(7)
		userID = int64(5)
		email  = "alice@example.com"
	)
	srv := newFakePanel(t, "ch_user_5")
	st := baseStore(t, srv.URL, email, userID, nodeID, 1000)
	st.auditQueryErr = fmt.Errorf("operation_logs 不可用（模拟）")
	var logs []string
	col := newAuditTestCollector(t, st, &logs)

	rep, err := col.ReconcileNodes(context.Background(), true, 50)
	if err != nil {
		t.Fatalf("落库报错不应让 ReconcileNodes 返回错误，实际: %v", err)
	}
	if rep.DriftEarlyCut != 1 {
		t.Fatalf("漂移计数应照常：期望 DriftEarlyCut=1，实际 %d（report=%+v）", rep.DriftEarlyCut, rep)
	}
	if rep.ElapsedMs < 0 {
		t.Fatalf("报告应正常返回，实际 %+v", rep)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "漂移审计落库失败") {
		t.Fatalf("落库失败应打日志，实际日志:\n%s", joined)
	}
	if audits := st.snapshotAudits(); len(audits) != 0 {
		t.Fatalf("查询失败时不应有落库行，实际 %+v", audits)
	}
}
