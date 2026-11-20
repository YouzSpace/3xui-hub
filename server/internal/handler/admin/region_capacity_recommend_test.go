package admin

// 容量建议接口用例（GET /admin-api/nodes/:node/capacity-recommend）。
//
// 为什么只 fake、不连库：这条接口自己不含任何算法 —— 推算全在 internal/metrics.Recommend
// （那边另有真库/纯函数用例钉口径），handler 只做三件事：解析 id、判节点是否存在、
// 把 Recommendation 摆成前端契约的 JSON（Suggest=0 → suggest:null）。
// 这三件事 fake 就能证明，而且能反过来断言「节点不存在时根本没去查指标」——
// 真库造 7 天样本反而看不出来。
//
// 变异测试锚点：
//   - 去掉 nodeCapacityRecommend 里的 NodeBinding 检查 → TestCapacityRecommendNodeNotFound 红
//     （会 200 + 「最近 7 天没有采样数据」，而不是 404）；
//   - 把 presentCapacityRecommendation 的 suggest 直接写 r.Suggest → NoSuggest 用例红（0 而非 null）；
//   - 删掉 Register 里那行 GET → TestCapacityRecommendRouteRegistered 红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"controlhub-server/internal/metrics"

	"github.com/gin-gonic/gin"
)

// ---- 假 RegionStore ----
//
// recFakeStore 只让 NodeBinding 有真行为（nodes 集合里有就返回、没有就 sql.ErrNoRows），
// 其余方法容量建议一个都不该用到，统一返回零值。

type recFakeStore struct {
	nodes map[uint64]bool
}

func (s *recFakeStore) RegionList(context.Context) ([]RegionSummary, error) { return nil, nil }

func (s *recFakeStore) RegionByID(context.Context, uint64) (*RegionRecord, error) {
	return nil, sql.ErrNoRows
}

func (s *recFakeStore) RegionCreate(context.Context, string, int64) (*RegionRecord, error) {
	return nil, sql.ErrNoRows
}

func (s *recFakeStore) RegionUpdate(context.Context, uint64, *string, *int64) (*RegionRecord, error) {
	return nil, sql.ErrNoRows
}

func (s *recFakeStore) RegionDelete(context.Context, uint64) error { return sql.ErrNoRows }

func (s *recFakeStore) RegionExists(context.Context, uint64) (bool, error) { return false, nil }

func (s *recFakeStore) RegionNameTaken(context.Context, string, uint64) (bool, error) {
	return false, nil
}

func (s *recFakeStore) RegionNodeCount(context.Context, uint64) (int64, error) { return 0, nil }

func (s *recFakeStore) RegionNodes(context.Context, uint64) ([]RegionNode, error) { return nil, nil }

func (s *recFakeStore) UnassignedNodes(context.Context) ([]RegionNode, error) { return nil, nil }

func (s *recFakeStore) NodeTotals(context.Context) (int64, int64, int64, error) { return 0, 0, 0, nil }

func (s *recFakeStore) NodeBinding(_ context.Context, nodeID uint64) (*uint64, *int64, error) {
	if !s.nodes[nodeID] {
		return nil, nil, sql.ErrNoRows
	}
	// 归属/容量与本接口无关，返回「既不属于地区也不限容量」。
	return nil, nil, nil
}

func (s *recFakeStore) NodeSetRegion(context.Context, uint64, *uint64) error { return nil }

func (s *recFakeStore) NodeSetCapacity(context.Context, uint64, *int64) error { return nil }

// ---- 假指标存储 ----

// recFakeMetrics 返回固定推荐结果；called 记下收到的 nodeID，
// 让「节点不存在时不该推算」可以反向断言。
type recFakeMetrics struct {
	rec  metrics.Recommendation
	err  error
	call []int64
}

func (m *recFakeMetrics) Recommend(_ context.Context, nodeID int64) (metrics.Recommendation, error) {
	m.call = append(m.call, nodeID)
	return m.rec, m.err
}

// recRouter 只挂地区域（与 region_api_test.go 的 newRegion09Router 同构，但不连库）。
func recRouter(store RegionStore, m capacityRecommender) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewRegionDomain(RegionDeps{Store: store, Metrics: m}).Register(engine.Group("/admin-api"))
	return engine
}

// recGet 发一个 GET 并解析 {code,msg,data}。
func recGet(t *testing.T, engine *gin.Engine, path string) region09Response {
	t.Helper()
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	var resp region09Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET %s 响应不是合法 JSON: %v（body=%s）", path, err, rec.Body.String())
	}
	return resp
}

// recPayload 是前端契约的 JSON 形状（字段名就是契约，改名前先改前端）。
type recPayload struct {
	Suggest    *int     `json:"suggest"`
	Confidence string   `json:"confidence"`
	Basis      string   `json:"basis"`
	PerUserKB  *float64 `json:"per_user_kb"`
	Samples    int      `json:"samples"`
	Reason     string   `json:"reason"`
}

// recDecode 把 data 解成 recPayload。
func recDecode(t *testing.T, raw json.RawMessage) recPayload {
	t.Helper()
	var data recPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析容量建议失败: %v（data=%s）", err, raw)
	}
	return data
}

// TestCapacityRecommendRouteRegistered 钉住路由：路径写错前端直接 404，编译期不会提醒。
func TestCapacityRecommendRouteRegistered(t *testing.T) {
	engine := recRouter(&recFakeStore{nodes: map[uint64]bool{}}, &recFakeMetrics{})

	for _, r := range engine.Routes() {
		if r.Method == http.MethodGet && r.Path == "/admin-api/nodes/:node/capacity-recommend" {
			return
		}
	}
	t.Fatalf("路由 GET /admin-api/nodes/:node/capacity-recommend 没注册（已注册 %d 条）", len(engine.Routes()))
}

// TestCapacityRecommendNodeNotFound ①节点不存在（以及 id 不是数字）：404，且不调 Recommend。
//
// 不能回 200 + 「最近 7 天没有采样数据」：那会把打错的 id 说成「节点还没上报」。
func TestCapacityRecommendNodeNotFound(t *testing.T) {
	store := &recFakeStore{nodes: map[uint64]bool{11: true}}
	m := &recFakeMetrics{}
	engine := recRouter(store, m)

	for _, path := range []string{
		"/admin-api/nodes/999/capacity-recommend", // 节点表里没有
		"/admin-api/nodes/abc/capacity-recommend", // 非数字 id（Laravel 隐式绑定同样 404）
	} {
		resp := recGet(t, engine, path)
		if resp.Code != 404 {
			t.Fatalf("%s 应回 code=404，实际 code=%d msg=%s", path, resp.Code, resp.Msg)
		}
		if resp.Msg != "接口不存在" {
			t.Fatalf("%s 的 404 文案应与其它路由一致，实际 %q", path, resp.Msg)
		}
	}
	if len(m.call) != 0 {
		t.Fatalf("节点不存在时不该去推算容量，实际调了 Recommend(%v)", m.call)
	}

	// 存在的节点才会往下走：同一条路由、同样的 store，节点 11 是 200。
	if resp := recGet(t, engine, "/admin-api/nodes/11/capacity-recommend"); resp.Code != 0 {
		t.Fatalf("节点存在时应回 code=0，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
}

// TestCapacityRecommendNoSuggest ②推算不出数（Suggest=0）：建议值不下发（null），
// 只给 reason 说明。前端拿到 null 就整行隐藏，不会显示「建议 0 人」。
func TestCapacityRecommendNoSuggest(t *testing.T) {
	store := &recFakeStore{nodes: map[uint64]bool{11: true}}
	m := &recFakeMetrics{rec: metrics.Recommendation{
		NodeID:     11,
		Suggest:    0,
		PerUserKB:  metrics.DefaultPerUserKB,
		Basis:      metrics.BasisDefault,
		Confidence: metrics.ConfidenceLow,
		Samples:    3,
		Reason:     "最近 7 天仅 3 个数据点，数据不足，按默认 5.0 KB/用户估算（3-7 天后更准）",
	}}
	engine := recRouter(store, m)

	resp := recGet(t, engine, "/admin-api/nodes/11/capacity-recommend")
	if resp.Code != 0 {
		t.Fatalf("取容量建议失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := recDecode(t, resp.Data)
	t.Logf("实际结果：%+v（data=%s）", data, resp.Data)

	if data.Suggest != nil {
		t.Fatalf("Suggest=0 时不该下发建议值，实际 suggest=%d", *data.Suggest)
	}
	if !strings.Contains(string(resp.Data), `"suggest":null`) {
		t.Fatalf("建议值必须是 JSON null（不是 0），实际 data=%s", resp.Data)
	}
	// reason 里的中文按 PHP 口径转成 \uXXXX，这里解出来比对内容。
	if !strings.Contains(data.Reason, "数据不足") || !strings.Contains(data.Reason, "3 个数据点") {
		t.Fatalf("reason 应原样透传 Recommend 的说明，实际 %q", data.Reason)
	}
	if data.Samples != 3 || data.Confidence != metrics.ConfidenceLow || data.Basis != metrics.BasisDefault {
		t.Fatalf("samples/confidence/basis 应原样透传，实际 %+v", data)
	}
	if data.PerUserKB == nil || *data.PerUserKB != metrics.DefaultPerUserKB {
		t.Fatalf("per_user_kb 应为默认 %.1f，实际 %v", metrics.DefaultPerUserKB, data.PerUserKB)
	}
	if !strings.Contains(string(resp.Data), `"per_user_kb":5`) {
		t.Fatalf("per_user_kb 应是短浮点（5 而不是 5.000000），实际 data=%s", resp.Data)
	}
}

// TestCapacityRecommendOK ③正常：六个字段全给，且 nodeID 原样传给 Recommend。
func TestCapacityRecommendOK(t *testing.T) {
	store := &recFakeStore{nodes: map[uint64]bool{42: true}}
	m := &recFakeMetrics{rec: metrics.Recommendation{
		NodeID:     42,
		Suggest:    12000,
		PerUserKB:  0, // 百分比口径：每用户 KB 不适用
		Basis:      metrics.BasisPercent,
		Confidence: metrics.ConfidenceHigh,
		Samples:    24,
		Reason:     "最近 7 天 24 个数据点，内存占用每万用户约涨 12.5%，当前已用 40%，按 80% 上限推算",
	}}
	engine := recRouter(store, m)

	resp := recGet(t, engine, "/admin-api/nodes/42/capacity-recommend")
	if resp.Code != 0 {
		t.Fatalf("取容量建议失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := recDecode(t, resp.Data)
	t.Logf("实际结果：%+v（data=%s）", data, resp.Data)

	if data.Suggest == nil || *data.Suggest != 12000 {
		t.Fatalf("suggest 应为 12000，实际 %v", data.Suggest)
	}
	if data.Confidence != metrics.ConfidenceHigh || data.Basis != metrics.BasisPercent {
		t.Fatalf("confidence/basis 应原样透传，实际 %+v", data)
	}
	if data.PerUserKB == nil || *data.PerUserKB != 0 {
		t.Fatalf("百分比口径的 per_user_kb 应为 0，实际 %v", data.PerUserKB)
	}
	if data.Samples != 24 {
		t.Fatalf("samples 应为 24，实际 %d", data.Samples)
	}
	if !strings.HasPrefix(data.Reason, "最近 7 天 24 个数据点") {
		t.Fatalf("reason 应原样透传，实际 %q", data.Reason)
	}
	if len(m.call) != 1 || m.call[0] != 42 {
		t.Fatalf("Recommend 应收到节点 id 42，实际 %v", m.call)
	}
}

// TestCapacityRecommendMetricsMissing 没注入指标存储（测试 / 裁剪部署）：
// 回 200 + 「未启用」，不能 panic，也不能伪装成「样本不足」。
func TestCapacityRecommendMetricsMissing(t *testing.T) {
	engine := recRouter(&recFakeStore{nodes: map[uint64]bool{11: true}}, nil)

	resp := recGet(t, engine, "/admin-api/nodes/11/capacity-recommend")
	if resp.Code != 0 {
		t.Fatalf("未启用时也应回 code=0（页面降级），实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	data := recDecode(t, resp.Data)
	if data.Suggest != nil {
		t.Fatalf("未启用时不该有建议值，实际 suggest=%d", *data.Suggest)
	}
	if !strings.Contains(data.Reason, "未启用") {
		t.Fatalf("reason 应说明未启用，实际 %q", data.Reason)
	}
}

// TestCapacityRecommendStoreError 查库/推算失败：回通用 500，不把 SQL 细节透给前端。
func TestCapacityRecommendStoreError(t *testing.T) {
	store := &recFakeStore{nodes: map[uint64]bool{11: true}}
	m := &recFakeMetrics{err: errors.New("Error 1146: Table 'node_metrics' doesn't exist")}
	engine := recRouter(store, m)

	resp := recGet(t, engine, "/admin-api/nodes/11/capacity-recommend")
	if resp.Code != 500 {
		t.Fatalf("推算失败应回 code=500，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Msg != "服务器错误" {
		t.Fatalf("失败文案应是通用的「服务器错误」，实际 %q", resp.Msg)
	}
	if strings.Contains(resp.Msg, "node_metrics") {
		t.Fatalf("不该把底层错误透给前端，实际 %q", resp.Msg)
	}
}
