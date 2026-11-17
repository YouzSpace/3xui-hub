package admin

// 迁移与调度接口的集成测试（地区功能 #10，实连 MySQL，独立库 ch_region10_*，自建自删）。
//
// 覆盖任务书验收第 3 条：
//   - 迁移接口返回 job_id，进度从 running → finished 且 done 正确；
//   - /migrations 按 node_id 过滤能查到；
//   - 撤销 24 小时内的批次 → user_nodes 回到迁移前状态；
//   - 撤销 25 小时前的批次 → 拒绝；
//   - 删除地区接口不受影响（路由共存回归）。
//
// 另有一条「真引擎端到端」用例（TestMigrationEndToEndWithScheduler）：用 NewSchedulerEngine
// 接 internal/scheduler 真跑一遍「迁移 → 打批次号 → 撤销」，证明接线不是只有 fake 能过。
//
// 为什么必须有 fake：进度用例要观察「running 且 done=0」这一瞬间，真引擎搬 3 个人是毫秒级，
// 轮询永远只能看到 finished。fake 用一道 gate 把引擎按住在原地，测完再放行。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// region10DSN 连本机 MySQL（root/空密码），可用 REGION10_MYSQL_DSN 覆盖。
func region10DSN() string {
	if dsn := os.Getenv("REGION10_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// region10WithSchema 把 DSN 的默认库换成 schema，并补齐生产同款参数
// （parseTime=true&loc=UTC 与 config.DBConfig.DSN() 一致；不补 parseTime 的话
// created_at 扫不进 sql.NullTime）。
func region10WithSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	sep := "?"
	if strings.Contains(tail, "?") {
		sep = "&"
	}
	if tail == "" {
		tail = "?timeout=3s"
		sep = "&"
	}
	return head + schema + tail + sep + "parseTime=true&loc=UTC"
}

// region10DDL 只建这些接口真正读写的 5 张表（列与 schema.go 一致，省掉无关列）。
var region10DDL = []string{
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
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		driver_type varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '3x-ui',
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'online',
		enabled tinyint(1) NOT NULL DEFAULT 1,
		capacity int DEFAULT NULL,
		region_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_nodes_region (region_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE users (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		email varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	// batch_id 是本任务唯一新增的列（ensureSchema 里幂等加列），这里按同样的定义建。
	`CREATE TABLE user_node_migrations (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		user_id bigint unsigned NOT NULL,
		from_node_id bigint unsigned DEFAULT NULL,
		to_node_id bigint unsigned NOT NULL,
		reason varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
		operator varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		batch_id varchar(36) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_user (user_id),
		KEY idx_batch (batch_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newRegion10DB 建一个干净的测试库 + 5 张表，跑完自动 DROP。
// 库名带进程号：多个 worker 可能同时在同一工作区跑 go test，固定库名会互相 DROP。
func newRegion10DB(t *testing.T, schema string) *sql.DB {
	t.Helper()
	schema = fmt.Sprintf("%s_%d", schema, os.Getpid())

	admin, err := sql.Open("mysql", region10DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过迁移接口集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过迁移接口集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", region10DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region10WithSchema(region10DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range region10DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// newMigrationRouter 只挂迁移域（不挂 AdminAuth：鉴权由 routes.go 的分组负责）。
func newMigrationRouter(t *testing.T, db *sql.DB, engine MigrationEngine) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewMigrationDomain(MigrationDeps{
		DB:     db,
		Engine: engine,
		Logger: log.New(io.Discard, "", 0),
	}).Register(r.Group("/admin-api"))
	return r
}

// migrationResp 是统一的 {code,msg,data} 响应。
type migrationResp struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// migrationCall 发一个 JSON 请求并解析响应；body 为空串表示不带请求体。
func migrationCall(t *testing.T, r *gin.Engine, method, path, body string) migrationResp {
	t.Helper()
	var reader io.Reader = strings.NewReader("")
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var resp migrationResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s %s 响应不是合法 JSON: %v（body=%s）", method, path, err, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s HTTP 状态 = %d，管理端口径恒为 200（body=%s）", method, path, rec.Code, rec.Body.String())
	}
	return resp
}

// dataMap 把 data 解析成 map。
func (r migrationResp) dataMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatalf("解析 data 失败: %v（raw=%s）", err, string(r.Data))
	}
	return m
}

// num 从 map 里取数字（JSON 解出来是 float64）。
func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("data 里没有字段 %q（%v）", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("字段 %q 不是数字: %T %v", key, v, v)
	}
	return f
}

// ---- fake 引擎 ----

// fakeMigrationEngine 是可控的假引擎：
//   - Migrate 第一次调用会等 gate 关闭（gate=nil 则不等），用来观察 running 状态；
//   - 每次返回 min(count, pool)，并把 pool 减掉，模拟「源节点上的人被搬走」。
type fakeMigrationEngine struct {
	mu   sync.Mutex
	pool int
	gate chan struct{}

	gated  bool
	calls  []int
	from   int64
	to     int64
	plan   []MigrationMove
	moved  int
	dryOn  bool
	region int64
	limit  int
}

func (f *fakeMigrationEngine) Migrate(ctx context.Context, from, to int64, count int) (int, error) {
	f.mu.Lock()
	f.from, f.to = from, to
	f.calls = append(f.calls, count)
	gate := f.gate
	first := !f.gated
	f.gated = true
	f.mu.Unlock()

	if gate != nil && first {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	f.mu.Lock()
	n := count
	if count <= 0 || n > f.pool {
		n = f.pool
	}
	f.pool -= n
	f.mu.Unlock()
	return n, nil
}

func (f *fakeMigrationEngine) Balance(_ context.Context, regionID int64, dry bool, limit int) (int, []MigrationMove, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.region, f.dryOn, f.limit = regionID, dry, limit
	return f.moved, f.plan, nil
}

// waitJob 轮询任务进度直到 done 达到 wantDone 且不再 running（最多 5 秒）。
func waitJob(t *testing.T, r *gin.Engine, jobID string, wantStatus string, wantDone int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		resp := migrationCall(t, r, http.MethodGet, "/admin-api/migrations/jobs/"+jobID, "")
		if resp.Code != 0 {
			t.Fatalf("查任务进度失败: code=%d msg=%s", resp.Code, resp.Msg)
		}
		last = resp.dataMap(t)
		if last["status"] == wantStatus && int(num(t, last, "done")) == wantDone {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务 %s 在 5 秒内没有到达 status=%s done=%d，最后一次快照: %v", jobID, wantStatus, wantDone, last)
	return nil
}

// seedNodesUsers 建节点 1/2 与 3 个用户（都绑在节点 1 上）。
func seedNodesUsers(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec10(t, db, "INSERT INTO nodes (id, name, host) VALUES (1, 'A', 'a.example.com'), (2, 'B', 'b.example.com')")
	for id := 1; id <= 3; id++ {
		mustExec10(t, db, "INSERT INTO users (id, email) VALUES (?, ?)", id, fmt.Sprintf("u%d@example.com", id))
		mustExec10(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (?, 1)", id)
	}
}

func mustExec10(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("准备数据失败: %v\nSQL: %s", err, query)
	}
}

// ---- 用例 1：job_id + 进度 running → finished ----

func TestMigrationJobProgressRunningToFinished(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_progress")
	seedNodesUsers(t, db)

	gate := make(chan struct{})
	engine := &fakeMigrationEngine{pool: 3, gate: gate}
	r := newMigrationRouter(t, db, engine)

	resp := migrationCall(t, r, http.MethodPost, "/admin-api/nodes/1/migrate", `{"to_node_id":2,"count":3}`)
	if resp.Code != 0 {
		t.Fatalf("发起迁移失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := resp.dataMap(t)
	jobID, _ := data["job_id"].(string)
	if jobID == "" {
		t.Fatalf("响应里没有 job_id: %v", data)
	}
	if batchID, _ := data["batch_id"].(string); batchID == "" {
		t.Fatalf("响应里没有 batch_id: %v", data)
	}

	// 引擎被 gate 按在原地：此刻必须是 running 且 done=0。
	running := migrationCall(t, r, http.MethodGet, "/admin-api/migrations/jobs/"+jobID, "").dataMap(t)
	if running["status"] != migrationJobRunning {
		t.Fatalf("刚发起的任务应处于 running，实际 %v", running["status"])
	}
	if got := num(t, running, "total"); got != 3 {
		t.Fatalf("total 应为请求的 3，实际 %v", got)
	}
	if got := num(t, running, "done"); got != 0 {
		t.Fatalf("放行前 done 应为 0（引擎还没返回），实际 %v", got)
	}
	if running["finished_at"] != nil {
		t.Fatalf("未结束的任务 finished_at 应为 null，实际 %v", running["finished_at"])
	}

	close(gate) // 放行

	done := waitJob(t, r, jobID, migrationJobFinished, 3)
	if got := num(t, done, "total"); got != 3 {
		t.Fatalf("结束时 total 应为 3，实际 %v", got)
	}
	if got := num(t, done, "failed"); got != 0 {
		t.Fatalf("结束时 failed 应为 0，实际 %v", got)
	}
	if done["finished_at"] == nil {
		t.Fatalf("结束的任务必须有 finished_at，实际 null")
	}
	if done["error"] != nil {
		t.Fatalf("成功任务 error 应为 null，实际 %v", done["error"])
	}

	engine.mu.Lock()
	from, to := engine.from, engine.to
	engine.mu.Unlock()
	if from != 1 || to != 2 {
		t.Fatalf("引擎收到的节点对应为 (1,2)，实际 (%d,%d)", from, to)
	}
}

// ---- 用例 2：/migrations 过滤 ----

func TestMigrationListFiltersByNode(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_list")
	seedNodesUsers(t, db)

	// user1: 1→2；user2: 2→3；user3: 3→1（三条不同节点对，便于验证 from/to 双向命中）
	mustExec10(t, db, "INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, batch_id) VALUES (1, 1, 2, 'failover', 'b1')")
	mustExec10(t, db, "INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, batch_id) VALUES (2, 2, 3, 'balance', 'b2')")
	mustExec10(t, db, "INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, batch_id) VALUES (3, 3, 1, 'failover', 'b3')")

	r := newMigrationRouter(t, db, &fakeMigrationEngine{})

	countItems := func(query string) float64 {
		t.Helper()
		resp := migrationCall(t, r, http.MethodGet, "/admin-api/migrations"+query, "")
		if resp.Code != 0 {
			t.Fatalf("查历史失败: code=%d msg=%s", resp.Code, resp.Msg)
		}
		return num(t, resp.dataMap(t), "total")
	}

	// node_id=2：既是 user1 的 to，也是 user2 的 from → 2 条（双向命中）。
	if got := countItems("?node_id=2"); got != 2 {
		t.Fatalf("node_id=2 应命中 2 条（from 或 to），实际 %v", got)
	}
	// node_id=3：user2 的 to + user3 的 from → 2 条。
	if got := countItems("?node_id=3"); got != 2 {
		t.Fatalf("node_id=3 应命中 2 条，实际 %v", got)
	}
	// node_id=9：没有节点碰过 → 0。
	if got := countItems("?node_id=9"); got != 0 {
		t.Fatalf("node_id=9 应命中 0 条，实际 %v", got)
	}
	// reason=failover → 2 条。
	if got := countItems("?reason=failover"); got != 2 {
		t.Fatalf("reason=failover 应命中 2 条，实际 %v", got)
	}
	// user_id=2 → 1 条。
	if got := countItems("?user_id=2"); got != 1 {
		t.Fatalf("user_id=2 应命中 1 条，实际 %v", got)
	}
	// 分页：per_page=2 → 第 1 页 2 条，total 仍是 3。
	resp := migrationCall(t, r, http.MethodGet, "/admin-api/migrations?per_page=2&page=1", "")
	d := resp.dataMap(t)
	items, _ := d["items"].([]any)
	if len(items) != 2 || num(t, d, "total") != 3 || num(t, d, "per_page") != 2 {
		t.Fatalf("per_page=2 第 1 页应为 2 条 / total 3 / per_page 2，实际 items=%d total=%v per_page=%v",
			len(items), d["total"], d["per_page"])
	}

	// 校验返回字段形状（前端要按这些键渲染）。
	first, _ := items[0].(map[string]any)
	for _, key := range []string{"id", "user_id", "from_node_id", "to_node_id", "reason", "operator", "batch_id", "created_at"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("历史行缺字段 %q: %v", key, first)
		}
	}
}

// ---- 用例 3：撤销 24 小时内的批次 ----

func TestMigrationUndoWithinWindow(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_undo_ok")
	seedNodesUsers(t, db)

	// 迁移后的状态：user1/user2 已经在节点 2 上；历史记录 1 小时前写的。
	mustExec10(t, db, "UPDATE user_nodes SET node_id = 2 WHERE user_id IN (1,2)")
	mustExec10(t, db, "INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, batch_id, created_at) "+
		"VALUES (1, 1, 2, 'failover', 'batch-ok', NOW() - INTERVAL 1 HOUR)")
	mustExec10(t, db, "INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, batch_id, created_at) "+
		"VALUES (2, 1, 2, 'failover', 'batch-ok', NOW() - INTERVAL 1 HOUR)")

	r := newMigrationRouter(t, db, &fakeMigrationEngine{})
	resp := migrationCall(t, r, http.MethodPost, "/admin-api/migrations/batch-ok/undo", "")
	if resp.Code != 0 {
		t.Fatalf("撤销 1 小时前的批次应成功，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := resp.dataMap(t)
	if got := num(t, data, "undone"); got != 2 {
		t.Fatalf("undone 应为 2，实际 %v", got)
	}
	if got := num(t, data, "rebound"); got != 2 {
		t.Fatalf("rebound 应为 2，实际 %v", got)
	}

	// user_nodes 回到迁移前：两个用户都在节点 1，节点 2 上没有他们。
	for uid := 1; uid <= 2; uid++ {
		var onNode1, onNode2 int
		if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE user_id = ? AND node_id = 1", uid).Scan(&onNode1); err != nil {
			t.Fatalf("查绑定失败: %v", err)
		}
		if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE user_id = ? AND node_id = 2", uid).Scan(&onNode2); err != nil {
			t.Fatalf("查绑定失败: %v", err)
		}
		if onNode1 != 1 || onNode2 != 0 {
			t.Fatalf("user %d 撤销后应在节点 1（现在 node1=%d node2=%d）", uid, onNode1, onNode2)
		}
	}

	// users.updated_at 必须被刷新（否则节点不会重拉配置，用户在新节点上建不出来）。
	var refreshed int
	if err := db.QueryRow("SELECT COUNT(*) FROM users WHERE id IN (1,2) AND updated_at IS NOT NULL").Scan(&refreshed); err != nil {
		t.Fatalf("查 users 失败: %v", err)
	}
	if refreshed != 2 {
		t.Fatalf("撤销后 2 个用户的 updated_at 都应被刷新，实际 %d", refreshed)
	}
}

// ---- 用例 4：撤销 25 小时前的批次 → 拒绝 ----

func TestMigrationUndoOlderThanWindowRejected(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_undo_old")
	seedNodesUsers(t, db)

	mustExec10(t, db, "UPDATE user_nodes SET node_id = 2 WHERE user_id = 1")
	mustExec10(t, db, "INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, batch_id, created_at) "+
		"VALUES (1, 1, 2, 'failover', 'batch-old', NOW() - INTERVAL 25 HOUR)")

	r := newMigrationRouter(t, db, &fakeMigrationEngine{})
	resp := migrationCall(t, r, http.MethodPost, "/admin-api/migrations/batch-old/undo", "")
	if resp.Code == 0 {
		t.Fatalf("撤销 25 小时前的批次必须被拒绝，实际成功: %s", string(resp.Data))
	}
	if !strings.Contains(resp.Msg, "24") {
		t.Fatalf("拒绝文案必须说明 24 小时窗口，实际: %s", resp.Msg)
	}
	if !strings.Contains(resp.Msg, "超过") {
		t.Fatalf("拒绝文案必须说明原因（超过窗口），实际: %s", resp.Msg)
	}

	// 被拒绝时不能动数据：user1 仍在节点 2。
	var onNode2 int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE user_id = 1 AND node_id = 2").Scan(&onNode2); err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if onNode2 != 1 {
		t.Fatalf("撤销被拒后数据不应变化，实际 user1 已不在节点 2")
	}
}

// ---- 用例 5：真引擎端到端（迁移 → 打批次号 → 撤销） ----

func TestMigrationEndToEndWithScheduler(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_e2e")
	seedNodesUsers(t, db)

	r := newMigrationRouter(t, db, NewSchedulerEngine(db, nil))

	resp := migrationCall(t, r, http.MethodPost, "/admin-api/nodes/1/migrate", `{"to_node_id":2,"count":0}`)
	if resp.Code != 0 {
		t.Fatalf("发起迁移失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := resp.dataMap(t)
	jobID, _ := data["job_id"].(string)
	batchID, _ := data["batch_id"].(string)
	if jobID == "" || batchID == "" {
		t.Fatalf("响应必须同时带 job_id 与 batch_id: %v", data)
	}

	waitJob(t, r, jobID, migrationJobFinished, 3)

	// 真引擎搬完后：3 个用户都在节点 2 上。
	var onNode2 int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE node_id = 2").Scan(&onNode2); err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if onNode2 != 3 {
		t.Fatalf("迁移后节点 2 应有 3 个绑定，实际 %d", onNode2)
	}

	// 引擎写的迁移记录必须已被本任务打上批次号（scheduler 自己不写 batch_id）。
	var stamped int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM user_node_migrations WHERE batch_id = ? AND reason = 'failover'", batchID).Scan(&stamped); err != nil {
		t.Fatalf("查批次失败: %v", err)
	}
	if stamped != 3 {
		t.Fatalf("3 条迁移记录都应打上批次号 %s，实际 %d 条", batchID, stamped)
	}

	// 按批次撤销 → 3 个用户回到节点 1。
	undo := migrationCall(t, r, http.MethodPost, "/admin-api/migrations/"+batchID+"/undo", "")
	if undo.Code != 0 {
		t.Fatalf("撤销批次失败: code=%d msg=%s", undo.Code, undo.Msg)
	}
	if got := num(t, undo.dataMap(t), "undone"); got != 3 {
		t.Fatalf("undone 应为 3，实际 %v", got)
	}
	var back int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE node_id = 1").Scan(&back); err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	var left int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE node_id = 2").Scan(&left); err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if back != 3 || left != 0 {
		t.Fatalf("撤销后应 3 个都在节点 1、节点 2 清空，实际 node1=%d node2=%d", back, left)
	}
}

// ---- 用例 6：dry 均衡只出计划 ----

func TestMigrationBalanceDryRunReturnsPlan(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_balance")
	seedNodesUsers(t, db)

	// 地区 1：节点 1 容量 2 但绑了 3 个人（超容量），节点 2 容量 10 空着。
	mustExec10(t, db, "INSERT INTO regions (id, name) VALUES (1, 'HK')")
	mustExec10(t, db, "UPDATE nodes SET region_id = 1, capacity = 2, status = 'online', enabled = 1 WHERE id = 1")
	mustExec10(t, db, "UPDATE nodes SET region_id = 1, capacity = 10, status = 'online', enabled = 1 WHERE id = 2")

	r := newMigrationRouter(t, db, NewSchedulerEngine(db, nil))
	resp := migrationCall(t, r, http.MethodPost, "/admin-api/regions/1/balance", `{"dry":true}`)
	if resp.Code != 0 {
		t.Fatalf("dry 均衡失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := resp.dataMap(t)
	plan, _ := data["plan"].([]any)
	if len(plan) == 0 {
		t.Fatalf("超容量节点应产出至少 1 条计划，实际 %v", data)
	}
	if data["dry"] != true {
		t.Fatalf("dry 响应必须回显 dry=true，实际 %v", data["dry"])
	}

	// dry 不写库：3 个绑定仍全在节点 1。
	var stillOnNode1 int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE node_id = 1").Scan(&stillOnNode1); err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if stillOnNode1 != 3 {
		t.Fatalf("dry=true 不允许写库，节点 1 应仍有 3 个绑定，实际 %d", stillOnNode1)
	}

	// 计划项形状：user_id / from_node_id / to_node_id。
	first, _ := plan[0].(map[string]any)
	for _, key := range []string{"user_id", "from_node_id", "to_node_id"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("计划项缺字段 %q: %v", key, first)
		}
	}
}

// ---- 用例 6b：dry=false 走后台任务 ----

func TestMigrationBalanceBackgroundJob(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_balance_job")
	seedNodesUsers(t, db)

	// 同一份数据：节点 1 容量 2 却绑了 3 个人，节点 2 容量 10 空着 → 应搬 1 个人。
	mustExec10(t, db, "INSERT INTO regions (id, name) VALUES (1, 'HK')")
	mustExec10(t, db, "UPDATE nodes SET region_id = 1, capacity = 2, status = 'online', enabled = 1 WHERE id = 1")
	mustExec10(t, db, "UPDATE nodes SET region_id = 1, capacity = 10, status = 'online', enabled = 1 WHERE id = 2")

	r := newMigrationRouter(t, db, NewSchedulerEngine(db, nil))
	resp := migrationCall(t, r, http.MethodPost, "/admin-api/regions/1/balance", `{"dry":false}`)
	if resp.Code != 0 {
		t.Fatalf("发起均衡任务失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := resp.dataMap(t)
	jobID, _ := data["job_id"].(string)
	batchID, _ := data["batch_id"].(string)
	if jobID == "" || batchID == "" {
		t.Fatalf("响应必须带 job_id 与 batch_id: %v", data)
	}

	done := waitJob(t, r, jobID, migrationJobFinished, 1)
	if done["kind"] != migrationJobKindBalance {
		t.Fatalf("任务种类应为 %s，实际 %v", migrationJobKindBalance, done["kind"])
	}

	// 数据库真的动了：节点 1 只剩 2 个，节点 2 有 1 个。
	var onNode1, onNode2 int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE node_id = 1").Scan(&onNode1); err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE node_id = 2").Scan(&onNode2); err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	if onNode1 != 2 || onNode2 != 1 {
		t.Fatalf("均衡后应 node1=2 node2=1，实际 %d / %d", onNode1, onNode2)
	}

	// 均衡记录也要打上批次号（reason='balance'），否则这批搬移无法一键撤销。
	var stamped int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM user_node_migrations WHERE batch_id = ? AND reason = 'balance'", batchID).Scan(&stamped); err != nil {
		t.Fatalf("查批次失败: %v", err)
	}
	if stamped != 1 {
		t.Fatalf("均衡的 1 条迁移记录应打上批次号 %s，实际 %d 条", batchID, stamped)
	}
}

// ---- 用例 7：引擎未接线时明确报错，读接口不受影响 ----
func TestMigrationEngineNotReady(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_noengine")
	seedNodesUsers(t, db)

	r := newMigrationRouter(t, db, nil)

	resp := migrationCall(t, r, http.MethodPost, "/admin-api/nodes/1/migrate", `{"to_node_id":2,"count":0}`)
	if resp.Code == 0 {
		t.Fatalf("引擎为 nil 时迁移必须明确失败，实际成功")
	}
	if !strings.Contains(resp.Msg, "未就绪") {
		t.Fatalf("失败文案必须说明引擎未就绪，实际: %s", resp.Msg)
	}

	// 只读接口照常可用。
	list := migrationCall(t, r, http.MethodGet, "/admin-api/migrations", "")
	if list.Code != 0 {
		t.Fatalf("引擎未接线不影响历史查询，实际 code=%d msg=%s", list.Code, list.Msg)
	}
}

// ---- 用例 8：与地区 CRUD 域共存（删除地区接口回归） ----

func TestMigrationRoutesCoexistWithRegionDomain(t *testing.T) {
	db := newRegion10DB(t, "ch_region10_coexist")
	mustExec10(t, db, "INSERT INTO regions (id, name) VALUES (7, 'JP')")

	// 同一个 /admin-api 分组里挂两个域：地区域（/regions/:region、/nodes/:node/...）与迁移域。
	// 迁移域若把通配名写成 :id，这里会在 Register 阶段直接 panic —— 这正是要防的回归。
	gin.SetMode(gin.TestMode)
	r := gin.New()
	group := r.Group("/admin-api")
	NewRegionDomain(RegionDeps{Store: NewRegionSQLStore(db), Logger: log.New(io.Discard, "", 0)}).Register(group)
	NewMigrationDomain(MigrationDeps{DB: db, Engine: &fakeMigrationEngine{}, Logger: log.New(io.Discard, "", 0)}).Register(group)

	// ① 删除空地区仍然成功。
	del := migrationCall(t, r, http.MethodDelete, "/admin-api/regions/7", "")
	if del.Code != 0 {
		t.Fatalf("删除空地区应成功，实际 code=%d msg=%s", del.Code, del.Msg)
	}
	var left int
	if err := db.QueryRow("SELECT COUNT(*) FROM regions WHERE id = 7").Scan(&left); err != nil {
		t.Fatalf("查地区失败: %v", err)
	}
	if left != 0 {
		t.Fatalf("地区 7 应已删除，实际还在")
	}

	// ② 迁移域的路由也还在同一个 router 上可用。
	if resp := migrationCall(t, r, http.MethodGet, "/admin-api/migrations", ""); resp.Code != 0 {
		t.Fatalf("同 router 下迁移历史应可用，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
}
