package admin

// POST /admin-api/regions/backfill 的 handler 用例（fake 引擎，不连库）。
//
// 为什么 fake 就够：这条接口自己不做任何分配决策 —— 选节点、模拟计数、套餐地区过滤
// 全在 internal/scheduler（那边有真库用例钉口径）。handler 只负责三件事：
//  1. 解析 dry / limit，且 dry 缺省必须是 true（这个接口会写 user_nodes，默认最安全）；
//  2. limit 夹到 [1, 5000]，缺省 500；
//  3. 把引擎结果摆成前端契约的 JSON。
// 用 fake 能反过来断言「默认真的传了 dry=true」—— 真库用例看不出这个区别。
//
// 变异测试锚点（去掉即变红）：
//   - regionBackfill 里把 `dry := true` 改成 `dry := inputBool(in["dry"])` → TestBackfillDryDefaultsTrue 红；
//   - 去掉 limit 上限夹取 → TestBackfillLimitClampedToMax 红；
//   - 去掉 `d.backfill == nil` 的 503 分支 → nil 引擎会 panic / 回 0 让人误以为补完了。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// backfillFakeEngine 记下 handler 传来的参数，返回固定结果。
type backfillFakeEngine struct {
	dryCalls   []bool
	limitCalls []int
	report     BackfillReport
	err        error
	calls      int
}

func (e *backfillFakeEngine) Backfill(_ context.Context, dry bool, limit int) (BackfillReport, error) {
	e.calls++
	e.dryCalls = append(e.dryCalls, dry)
	e.limitCalls = append(e.limitCalls, limit)
	return e.report, e.err
}

// backfillRouter 只挂地区域（store 用 recFakeStore：本用例不碰它的任何方法）。
func backfillRouter(engine BackfillEngine) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	NewRegionDomain(RegionDeps{Store: &recFakeStore{nodes: map[uint64]bool{}}, Backfill: engine}).
		Register(e.Group("/admin-api"))
	return e
}

// backfillPost 发一个 POST 并解析统一信封。
func backfillPost(t *testing.T, engine *gin.Engine, body string) region09Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin-api/regions/backfill", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	var resp region09Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("管理端口径恒为 HTTP 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	return resp
}

// backfillPayload 是前端契约的 JSON 形状（字段名就是契约）。
type backfillPayload struct {
	Dry          bool `json:"dry"`
	Candidates   int  `json:"candidates"`
	Assigned     int  `json:"assigned"`
	UsersDone    int  `json:"users_done"`
	UsersSkipped int  `json:"users_skipped"`
	Remaining    int  `json:"remaining"`
	ByNode       []struct {
		NodeID int64 `json:"node_id"`
		Count  int   `json:"count"`
	} `json:"by_node"`
	Skipped []struct {
		RegionID   int64  `json:"region_id"`
		RegionName string `json:"region_name"`
		Reason     string `json:"reason"`
		Count      int    `json:"count"`
	} `json:"skipped"`
}

func backfillDecode(t *testing.T, raw json.RawMessage) backfillPayload {
	t.Helper()
	var data backfillPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析批量入组结果失败: %v（data=%s）", err, raw)
	}
	return data
}

// TestBackfillDryDefaultsTrue 钉住「不传 dry == 预览」：真实执行必须显式传 false。
func TestBackfillDryDefaultsTrue(t *testing.T) {
	engine := &backfillFakeEngine{report: BackfillReport{Dry: true}}
	r := backfillRouter(engine)

	// 无请求体 ✅ 与空对象 ✅ 都必须按 dry=true 处理
	for _, body := range []string{"", "{}", `{"dry":null}`} {
		if resp := backfillPost(t, r, body); resp.Code != 0 {
			t.Fatalf("body=%q 应成功，实际 code=%d msg=%s", body, resp.Code, resp.Msg)
		}
	}
	// 显式 false 才真写
	if resp := backfillPost(t, r, `{"dry":false}`); resp.Code != 0 {
		t.Fatalf("dry=false 应成功，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}

	want := []bool{true, true, true, false}
	if len(engine.dryCalls) != len(want) {
		t.Fatalf("引擎调用次数不对: %v", engine.dryCalls)
	}
	for i := range want {
		if engine.dryCalls[i] != want[i] {
			t.Fatalf("第 %d 次调用 dry=%v，期望 %v（全部：%v）", i+1, engine.dryCalls[i], want[i], engine.dryCalls)
		}
	}
}

// TestBackfillLimitDefaultsAndClamp 默认 500；负数/0 仍走默认；超上限夹到 5000；正常值原样透传。
func TestBackfillLimitDefaultsAndClamp(t *testing.T) {
	engine := &backfillFakeEngine{report: BackfillReport{Dry: true}}
	r := backfillRouter(engine)

	for _, body := range []string{"", "{}", `{"limit":0}`, `{"limit":-3}`} {
		backfillPost(t, r, body)
	}
	backfillPost(t, r, `{"limit":2000}`)
	backfillPost(t, r, `{"limit":999999}`)

	want := []int{500, 500, 500, 500, 2000, 5000}
	if len(engine.limitCalls) != len(want) {
		t.Fatalf("引擎调用次数不对: %v", engine.limitCalls)
	}
	for i := range want {
		if engine.limitCalls[i] != want[i] {
			t.Fatalf("第 %d 次调用 limit=%d，期望 %d（全部：%v）", i+1, engine.limitCalls[i], want[i], engine.limitCalls)
		}
	}
}

// TestBackfillResponseShape 钉住响应字段名/取值（前端按这些字段渲染，改名先改前端）。
func TestBackfillResponseShape(t *testing.T) {
	engine := &backfillFakeEngine{report: BackfillReport{
		Dry:          false,
		Candidates:   12,
		Assigned:     9,
		UsersDone:    4,
		UsersSkipped: 1,
		Remaining:    8,
		ByNode:       []BackfillNodeCount{{NodeID: 101, Count: 5}, {NodeID: 102, Count: 4}},
		Skipped:      []BackfillSkipRow{{RegionID: 3, RegionName: "美国", Reason: "该地区暂无可分配节点", Count: 1}},
	}}
	r := backfillRouter(engine)

	resp := backfillPost(t, r, `{"dry":false}`)
	if resp.Code != 0 {
		t.Fatalf("应成功，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := backfillDecode(t, resp.Data)
	if data.Dry || data.Candidates != 12 || data.Assigned != 9 || data.UsersDone != 4 ||
		data.UsersSkipped != 1 || data.Remaining != 8 {
		t.Fatalf("标量字段不对: %s", resp.Data)
	}
	if len(data.ByNode) != 2 || data.ByNode[0].NodeID != 101 || data.ByNode[0].Count != 5 ||
		data.ByNode[1].NodeID != 102 || data.ByNode[1].Count != 4 {
		t.Fatalf("by_node 不对: %s", resp.Data)
	}
	if len(data.Skipped) != 1 || data.Skipped[0].RegionID != 3 ||
		data.Skipped[0].RegionName != "美国" || data.Skipped[0].Count != 1 ||
		data.Skipped[0].Reason == "" {
		t.Fatalf("skipped 不对: %s", resp.Data)
	}
	// 空数组也要输出 []（不是 null），前端不必再判空类型。
	if !strings.Contains(string(resp.Data), `"by_node":[`) || !strings.Contains(string(resp.Data), `"skipped":[`) {
		t.Fatalf("by_node/skipped 必须是数组，实际 %s", resp.Data)
	}
}

// TestBackfillEmptyListsAsArrays 引擎没给分布（例如没有候选用户）时输出空数组而不是 null。
func TestBackfillEmptyListsAsArrays(t *testing.T) {
	engine := &backfillFakeEngine{report: BackfillReport{Dry: true}}
	r := backfillRouter(engine)

	resp := backfillPost(t, r, "")
	if !strings.Contains(string(resp.Data), `"by_node":[]`) || !strings.Contains(string(resp.Data), `"skipped":[]`) {
		t.Fatalf("空结果应输出 []，实际 %s", resp.Data)
	}
}

// TestBackfillEngineMissing 没注入引擎（裁剪部署 / 只有 fake Store）→ 503 明确「未就绪」，
// 不能回 code=0 + 0 人（那会被当成「已经补完了」）。
func TestBackfillEngineMissing(t *testing.T) {
	r := backfillRouter(nil)

	resp := backfillPost(t, r, "")
	if resp.Code != 503 {
		t.Fatalf("没引擎应回 503，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	if !strings.Contains(resp.Msg, "未就绪") {
		t.Fatalf("文案应说明引擎未就绪，实际 %q", resp.Msg)
	}
}

// TestBackfillRouteRegistered 钉住路由：写错路径前端就是 404，编译期不会提醒。
func TestBackfillRouteRegistered(t *testing.T) {
	r := backfillRouter(&backfillFakeEngine{})
	for _, route := range r.Routes() {
		if route.Method == http.MethodPost && route.Path == "/admin-api/regions/backfill" {
			return
		}
	}
	t.Fatalf("路由 POST /admin-api/regions/backfill 没注册（已注册 %d 条）", len(r.Routes()))
}

// backfillFallbackStore 模拟生产接线：RegionDeps 不注入 Backfill，引擎由 Store 自己给出。
type backfillFallbackStore struct {
	RegionStore
	engine BackfillEngine
}

func (s backfillFallbackStore) backfillEngine() BackfillEngine { return s.engine }

// TestBackfillUsesStoreFallbackEngine 钉住生产路径（routes.go 不注入 Backfill，只靠 Store 兜底）。
//
// 回归护栏：修复前 handler 把兜底结果赋给局部变量 engine，真正调用的却是 d.backfill（nil）→
// nil pointer panic；fake 注入路径测不到这一段，所以必须有这条用例。
func TestBackfillUsesStoreFallbackEngine(t *testing.T) {
	fake := &backfillFakeEngine{report: BackfillReport{Dry: true, Candidates: 1, Remaining: 1}}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewRegionDomain(RegionDeps{Store: backfillFallbackStore{engine: fake}}).Register(r.Group("/admin-api"))

	resp := backfillPost(t, r, "{}")
	if resp.Code != 0 {
		t.Fatalf("兜底引擎路径应成功，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	if fake.calls != 1 {
		t.Fatalf("兜底引擎应被调用 1 次，实际 %d", fake.calls)
	}
	if len(fake.dryCalls) != 1 || !fake.dryCalls[0] {
		t.Fatalf("缺省必须是 dry=true，实际 %v", fake.dryCalls)
	}
}
