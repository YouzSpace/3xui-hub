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
	// inflightDeliveryUsers 是「当前有在途 user_delivery 任务」的用户 id（模拟 async_tasks）。
	inflightDeliveryUsers []int64
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
	case strings.Contains(q, "FROM async_tasks"):
		return c.inflightDeliveryRows()
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

// inflightDeliveryRows 返回「在途 user_delivery 任务」的 subject_id（模拟 async_tasks 查询）。
func (c *fakeConn) inflightDeliveryRows() (driver.Rows, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	var vals [][]driver.Value
	for _, id := range c.st.inflightDeliveryUsers {
		vals = append(vals, []driver.Value{id})
	}
	return &fakeRows{cols: []string{"subject_id"}, vals: vals}, nil
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
	// 多轮直读复核之间不必真等：测试里把间隔压到 0（生产默认 1s）。
	t.Setenv("RECONCILE_DRIFT_REVERIFY_GAP_MS", "0")
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
				"obj":     map[string]any{"client": map[string]any{"email": email, "id": "uuid-5", "enable": true, "totalGB": 500, "expiryTime": 0}},
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
	// autoFixDrift 真写时会调 UpdateClient；这里让它失败，验证写回失败落一条「3x-ui 写回失败」审计。
	entry := nodeSnapshotEntry{Enable: true, TotalGB: 500, ExpiryMs: 0}
	if err := c.autoFixDrift(context.Background(), client, node, u, entry); err == nil {
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

// ── 用例 5：可判定的字段漂移（total>0）→ 自动修复并复核 ─────────

// TestReconcileAutoFixTotalTypeDrift 复刻 218 的真实形态：
// 节点上是上一个「total 型套餐」的残留（total=48GiB 具体值、expiry=0 不过期），面板已换成
// 2GiB 的新套餐 → 必须自动修复为面板值（total=面板值、expiry 跟着写成面板的 0）。
// 这验证「expiry=0 也能修」：只要节点 total>0 就归面板真源。
func TestReconcileAutoFixTotalTypeDrift(t *testing.T) {
	const (
		nodeID = int64(7)
		userID = int64(5)
		email  = "ch_user_5"
	)
	panelLimit := int64(1000) // 面板新套餐 1000 字节（模拟 2GiB 缩小）
	var mu sync.Mutex
	nodeLimit := int64(500) // 节点旧套餐残留：具体值 500
	nodeExpiry := int64(0)   // total 型套餐 expiry=0（不过期）—— 这正是 218 的形态
	updates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/panel/api/inbounds/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": []any{map[string]any{
					"id": 1,
					"settings": map[string]any{"clients": []any{map[string]any{
						"email": email, "id": "uuid-5", "enable": true,
						"totalGB": nodeLimit, "expiryTime": nodeExpiry,
					}}},
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/get/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": map[string]any{"client": map[string]any{
					"email": email, "id": "uuid-5", "enable": true,
					"totalGB": nodeLimit, "expiryTime": nodeExpiry,
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/update/"):
			updates++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if c, ok := body["client"].(map[string]any); ok {
				body = c
			}
			if f, ok := body["totalGB"].(float64); ok {
				nodeLimit = int64(f)
			}
			if f, ok := body["expiryTime"].(float64); ok {
				nodeExpiry = int64(f)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "unexpected " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.ParseInt(portStr, 10, 64)
	apiKeyCipher, _ := crypto.EncryptString("test-token", testAppKey)
	st := &fakeStore{
		users: []fakeUser{{ID: userID, UUID: "uuid-5", Protocol: "vless", Enabled: 1,
			Limit: panelLimit, PlanID: 9, PlanType: "total", Email: "alice@example.com"}},
		nodes:    []fakeNodeRow{{ID: nodeID, Name: testNodeName, Host: host, Port: port, Scheme: "http", Status: "online", APIKeyCipher: apiKeyCipher}},
		inbounds: []fakeInbound{{Protocol: "vless", ID: 1}},
	}
	c := newAuditTestCollector(t, st, nil)

	rep, err := c.ReconcileNodes(context.Background(), false, 50)
	if err != nil {
		t.Fatalf("ReconcileNodes 返回错误: %v", err)
	}
	if rep.DriftFixed != 1 {
		t.Fatalf("total>0 的残留漂移应自动修复 1 条，实际 DriftFixed=%d（report=%+v）", rep.DriftFixed, rep)
	}
	if updates == 0 {
		t.Fatal("期望至少调用一次 UpdateClient，实际 0 次")
	}
	mu.Lock()
	gotLimit, gotExpiry := nodeLimit, nodeExpiry
	mu.Unlock()
	if gotLimit != panelLimit || gotExpiry != 0 {
		t.Fatalf("修复后节点应=面板值(total=%d expiry=0)，实际 total=%d expiry=%d", panelLimit, gotLimit, gotExpiry)
	}
	audits := st.snapshotAudits()
	if len(audits) != 1 || audits[0].Action != driftFixedAction {
		t.Fatalf("应落 1 条 %q 审计，实际 %+v", driftFixedAction, audits)
	}
}

// ── 用例 6：节点「不限」形态（total==0）不可判定 → 只报告、不改节点 ────────────

func TestReconcileDoesNotAutoFixUnlimitedDrift(t *testing.T) {
	const (
		nodeID = int64(7)
		userID = int64(5)
		email  = "ch_user_5"
	)
	// 节点 total=0（不限）、面板 Limit=1000 → Safe（不可判定方向，只报告）。
	_ = email

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/panel/api/inbounds/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": []any{map[string]any{
					"id": 1,
					"settings": map[string]any{"clients": []any{map[string]any{
						"email": "ch_user_5", "id": "uuid-5", "enable": true,
						"totalGB": 0, "expiryTime": 0, // 节点「不限/不过期」
					}}},
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/update/"):
			t.Errorf("不可判定的漂移（total==0）绝不应调用 UpdateClient，实际被调用")
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "unexpected " + r.URL.Path})
		}
	}))
	t.Cleanup(srv2.Close)

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv2.URL, "http://"))
	port, _ := strconv.ParseInt(portStr, 10, 64)
	apiKeyCipher, _ := crypto.EncryptString("test-token", testAppKey)
	st := &fakeStore{
		users: []fakeUser{{ID: userID, UUID: "uuid-5", Protocol: "vless", Enabled: 1,
			Limit: 1000, PlanID: 9, PlanType: "period", Email: "alice@example.com"}},
		nodes:    []fakeNodeRow{{ID: nodeID, Name: testNodeName, Host: host, Port: port, Scheme: "http", Status: "online", APIKeyCipher: apiKeyCipher}},
		inbounds: []fakeInbound{{Protocol: "vless", ID: 1}},
	}
	c := newAuditTestCollector(t, st, nil)

	rep, err := c.ReconcileNodes(context.Background(), false, 50)
	if err != nil {
		t.Fatalf("ReconcileNodes 返回错误: %v", err)
	}
	if rep.DriftFixed != 0 {
		t.Fatalf("total==0 不可判定，不应修复：DriftFixed=%d", rep.DriftFixed)
	}
	if rep.Drift != 1 {
		t.Fatalf("应记 1 条漂移（只报告），实际 Drift=%d", rep.Drift)
	}
	audits := st.snapshotAudits()
	if len(audits) != 1 || audits[0].Action == driftFixedAction {
		t.Fatalf("应落「待人工」审计而非「已修复」，实际 %+v", audits)
	}
}

// ── 用例 7：有在途 user_delivery 的用户本轮跳过比对（不发假漂移）──────────────

// TestReconcileSkipsUserWithInflightDelivery 验证：有在途/刚下发完的用户本轮跳过比对。
func TestReconcileSkipsUserWithInflightDelivery(t *testing.T) {
	const (
		nodeID = int64(7)
		userID = int64(5)
		email  = "ch_user_5"
	)
	// 面板侧 Limit=1000，节点 total=0 → 本会产生 1 条漂移；但该用户有在途下发 → 应跳过。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/panel/api/inbounds/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": []any{map[string]any{
					"id": 1,
					"settings": map[string]any{"clients": []any{map[string]any{
						"email": email, "id": "uuid-5", "enable": true, "totalGB": 0, "expiryTime": 0,
					}}},
				}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "unexpected " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.ParseInt(portStr, 10, 64)
	apiKeyCipher, _ := crypto.EncryptString("test-token", testAppKey)
	st := &fakeStore{
		users: []fakeUser{{ID: userID, UUID: "uuid-5", Protocol: "vless", Enabled: 1,
			Limit: 1000, PlanID: 9, PlanType: "period", Email: "alice@example.com"}},
		nodes:                 []fakeNodeRow{{ID: nodeID, Name: testNodeName, Host: host, Port: port, Scheme: "http", Status: "online", APIKeyCipher: apiKeyCipher}},
		inbounds:              []fakeInbound{{Protocol: "vless", ID: 1}},
		inflightDeliveryUsers: []int64{userID},
	}
	c := newAuditTestCollector(t, st, nil)

	rep, err := c.ReconcileNodes(context.Background(), false, 50)
	if err != nil {
		t.Fatalf("ReconcileNodes 返回错误: %v", err)
	}
	if rep.Drift != 0 {
		t.Fatalf("在途下发的用户本轮应跳过比对、不产生漂移，实际 Drift=%d", rep.Drift)
	}
	if audits := st.snapshotAudits(); len(audits) != 0 {
		t.Fatalf("跳过比对时不应落任何漂移审计，实际 %+v", audits)
	}
}

// TestReconcileIgnoresStaleSnapshotViaLiveReverify 是「读太快」根因的回归锁：
// inbounds/list 快照回的是**下发前的旧值**（total=500），但 client 直读已是面板值（total=1000）
// —— 模拟「下发早已完成、快照还是旧的」。此时 reconcile 必须先直读复核，发现已一致 → 不记漂移、
// 不调用 UpdateClient、不落任何审计。这是 218 那条假漂移的根因场景。
func TestReconcileIgnoresStaleSnapshotViaLiveReverify(t *testing.T) {
	const (
		nodeID     = int64(7)
		userID     = int64(5)
		email      = "ch_user_5"
		panelLimit = int64(1000)
	)
	var updates int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/panel/api/inbounds/list":
			// 旧快照：total=500（下发前的值）
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": []any{map[string]any{
					"id": 1,
					"settings": map[string]any{"clients": []any{map[string]any{
						"email": email, "id": "uuid-5", "enable": true, "totalGB": 500, "expiryTime": 0,
					}}},
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/get/"):
			// 直读：已是面板新值（total=1000）—— 下发其实已完成
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": map[string]any{"client": map[string]any{
					"email": email, "id": "uuid-5", "enable": true, "totalGB": panelLimit, "expiryTime": 0,
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/update/"):
			updates++
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "unexpected " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.ParseInt(portStr, 10, 64)
	apiKeyCipher, _ := crypto.EncryptString("test-token", testAppKey)
	st := &fakeStore{
		users: []fakeUser{{ID: userID, UUID: "uuid-5", Protocol: "vless", Enabled: 1,
			Limit: panelLimit, PlanID: 9, PlanType: "total", Email: "alice@example.com"}},
		nodes:    []fakeNodeRow{{ID: nodeID, Name: testNodeName, Host: host, Port: port, Scheme: "http", Status: "online", APIKeyCipher: apiKeyCipher}},
		inbounds: []fakeInbound{{Protocol: "vless", ID: 1}},
	}
	c := newAuditTestCollector(t, st, nil)

	rep, err := c.ReconcileNodes(context.Background(), false, 50)
	if err != nil {
		t.Fatalf("ReconcileNodes 返回错误: %v", err)
	}
	if rep.Drift != 0 {
		t.Fatalf("快照旧、直读已一致 → 不应记漂移（这是读取时序假象），实际 Drift=%d", rep.Drift)
	}
	if rep.DriftFixed != 0 {
		t.Fatalf("不应修复，实际 DriftFixed=%d", rep.DriftFixed)
	}
	if updates != 0 {
		t.Fatalf("不应调用 UpdateClient，实际 %d 次", updates)
	}
	if audits := st.snapshotAudits(); len(audits) != 0 {
		t.Fatalf("不应落任何审计，实际 %+v", audits)
	}
}

// TestReconcileMultiRoundReverifyConvergesToFix 验证「多轮读都不一致才修」：
// 快照旧值 total=500；直读前两次仍返回旧值 500（模拟下发还在收尾），第三次起是面板值 1000。
// 期望：多轮直读中最终读到一致 → 判假象、不记漂移、不修。这覆盖了「读几次才稳定」的场景。
func TestReconcileMultiRoundReverifyConvergesToFix(t *testing.T) {
	const (
		nodeID     = int64(7)
		userID     = int64(5)
		email      = "ch_user_5"
		panelLimit = int64(1000)
	)
	var mu sync.Mutex
	getCount := 0
	updates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/panel/api/inbounds/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": []any{map[string]any{"id": 1, "settings": map[string]any{"clients": []any{map[string]any{
					"email": email, "id": "uuid-5", "enable": true, "totalGB": 500, "expiryTime": 0,
				}}}}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/get/"):
			getCount++
			// 前 2 次仍读到旧值 500（还没写完），第 3 次起读到面板值 1000。
			cur := int64(500)
			if getCount >= 3 {
				cur = panelLimit
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"obj": map[string]any{"client": map[string]any{
					"email": email, "id": "uuid-5", "enable": true, "totalGB": cur, "expiryTime": 0,
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/panel/api/clients/update/"):
			updates++
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "unexpected " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.ParseInt(portStr, 10, 64)
	apiKeyCipher, _ := crypto.EncryptString("test-token", testAppKey)
	st := &fakeStore{
		users: []fakeUser{{ID: userID, UUID: "uuid-5", Protocol: "vless", Enabled: 1,
			Limit: panelLimit, PlanID: 9, PlanType: "total", Email: "alice@example.com"}},
		nodes:    []fakeNodeRow{{ID: nodeID, Name: testNodeName, Host: host, Port: port, Scheme: "http", Status: "online", APIKeyCipher: apiKeyCipher}},
		inbounds: []fakeInbound{{Protocol: "vless", ID: 1}},
	}
	c := newAuditTestCollector(t, st, nil)

	rep, err := c.ReconcileNodes(context.Background(), false, 50)
	if err != nil {
		t.Fatalf("ReconcileNodes 返回错误: %v", err)
	}
	if rep.Drift != 0 || rep.DriftFixed != 0 {
		t.Fatalf("多轮直读最终一致 → 应判假象（Drift=0、DriftFixed=0），实际 Drift=%d DriftFixed=%d", rep.Drift, rep.DriftFixed)
	}
	if updates != 0 {
		t.Fatalf("不应调用 UpdateClient，实际 %d 次", updates)
	}
	if audits := st.snapshotAudits(); len(audits) != 0 {
		t.Fatalf("不应落任何审计，实际 %+v", audits)
	}
	mu.Lock()
	gc := getCount
	mu.Unlock()
	if gc < 3 {
		t.Fatalf("应发生多轮直读（读到第 3 次才一致），实际 getCount=%d", gc)
	}
}

// TestDeliveryGraceExceedsSnapshotTTL 锁死「下发宽限期必须 > 快照 TTL」这条不变式。
//
// 根因：reconcile 读节点走 inbound/list 共享快照（TTL 75s），下发是另一进程直接写节点、
// 不清本进程缓存 —— 下发完成后若立即比对，会读到旧快照里的下发前值，判成假漂移。
// 所以宽限期必须比 TTL 长，等旧快照自然过期。改短了这条会失败，防止回归。
func TestDeliveryGraceExceedsSnapshotTTL(t *testing.T) {
	if deliveryGraceSeconds() <= int(defaultInboundSnapshotTTL/time.Second) {
		t.Fatalf("下发宽限期 %ds 必须大于 inbound 快照 TTL %v，否则下发完成后的旧快照仍会导致假漂移",
			deliveryGraceSeconds(), defaultInboundSnapshotTTL)
	}
}
