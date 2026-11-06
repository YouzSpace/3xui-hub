package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// nodesStore 是 handleNodes 的最小 fake：只实现被测路径会用到的方法。
//
// 未实现的方法靠内嵌 nil 接口「不可调用」—— 真被调到会 panic，正好暴露
// handler 多依赖了边界外的数据源。计数用于断言「无套餐时一次节点表都不查」。
type nodesStore struct {
	Store

	onlineCalls int
	onlineNodes []Node
	planCalls   int
	plan        *Plan
	subsCalls   int
	subs        []ThirdPartySub
	configs     map[string]string
}

func (s *nodesStore) OnlineNodesForProtocol(ctx context.Context, protocol string) ([]Node, error) {
	s.onlineCalls++
	return s.onlineNodes, nil
}

func (s *nodesStore) PlanByID(ctx context.Context, id uint64) (*Plan, error) {
	s.planCalls++
	if s.plan == nil {
		return nil, sql.ErrNoRows
	}
	return s.plan, nil
}

func (s *nodesStore) SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = s.configs[k]
	}
	return out, nil
}

func (s *nodesStore) ThirdPartySubsByIDs(ctx context.Context, ids []int64) ([]ThirdPartySub, error) {
	s.subsCalls++
	return s.subs, nil
}

// nodesResponse 取 GET /api/nodes 的原始 data 段（保留 []/null 的区别）。
type nodesResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// callNodes 用给定用户跑一次 GET /api/nodes。
func callNodes(t *testing.T, st Store, user *User) (*httptest.ResponseRecorder, nodesResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := &Handler{store: st, logger: log.New(io.Discard, "", 0)}
	engine := gin.New()
	engine.GET("/api/nodes", func(c *gin.Context) {
		c.Set(ctxUserKey, user)
		c.Next()
	}, h.handleNodes)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nodes", nil))

	var body nodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（原始: %s）", err, rec.Body.String())
	}
	return rec, body
}

// TestNodesWithoutPlanReturnsEmpty：plan_id 为空或 <=0 一律空数组，且不查节点表。
// 无套餐 = 没买套餐 = 没有可连节点，与订阅侧「无套餐，暂无流量」403 同口径。
func TestNodesWithoutPlanReturnsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		planID sql.NullInt64
	}{
		{"计划ID为NULL", sql.NullInt64{}},
		{"计划ID为0", sql.NullInt64{Int64: 0, Valid: true}},
		{"计划ID为负数", sql.NullInt64{Int64: -1, Valid: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &nodesStore{
				configs: map[string]string{},
				// 节点表故意放一条数据：只要被查到就会返回非空列表，断言才有意义。
				onlineNodes: []Node{{ID: 1, Name: "香港01", Host: "1.1.1.1", Port: 443, Status: "online"}},
			}
			user := &User{ID: 9, Protocol: "vless", PlanID: tc.planID}

			rec, body := callNodes(t, st, user)

			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP status = %d, 期望 200", rec.Code)
			}
			if body.Code != 0 || body.Msg != "ok" {
				t.Fatalf("响应 = {code:%d, msg:%q}, 期望 {code:0, msg:\"ok\"}", body.Code, body.Msg)
			}
			// 必须是 []（不是 null，也不是对象），前端据此走空态。
			if got := string(body.Data); got != "[]" {
				t.Fatalf("data = %s, 期望 []", got)
			}
			if st.onlineCalls != 0 {
				t.Errorf("OnlineNodesForProtocol 调用次数 = %d, 期望 0（无套餐不该查节点表）", st.onlineCalls)
			}
			if st.planCalls != 0 {
				t.Errorf("PlanByID 调用次数 = %d, 期望 0（无套餐不该查套餐表）", st.planCalls)
			}
			if st.subsCalls != 0 {
				t.Errorf("ThirdPartySubsByIDs 调用次数 = %d, 期望 0（无套餐不该注入第三方线路）", st.subsCalls)
			}
		})
	}
}

// TestNodesWithPlanUnchanged：有套餐（含本地节点）时行为保持原样 —— 查节点表并逐条输出。
func TestNodesWithPlanUnchanged(t *testing.T) {
	st := &nodesStore{
		configs: map[string]string{},
		plan:    &Plan{ID: 7, Name: "标准版", IncludeLocal: true},
		onlineNodes: []Node{
			{ID: 1, Name: "香港01", Host: "1.1.1.1", Port: 8443, Status: "online", Latency: 30},
		},
	}
	user := &User{ID: 9, Protocol: "vless", PlanID: sql.NullInt64{Int64: 7, Valid: true}}

	rec, body := callNodes(t, st, user)

	if rec.Code != http.StatusOK || body.Code != 0 {
		t.Fatalf("HTTP %d / code %d, 期望 200 / 0", rec.Code, body.Code)
	}
	if st.onlineCalls != 1 {
		t.Fatalf("OnlineNodesForProtocol 调用次数 = %d, 期望 1", st.onlineCalls)
	}
	if st.planCalls != 2 {
		// 2 次是既有行为（HEAD 即如此）：handleNodes 判 IncludeLocal 一次，
		// userPlanThirdPartyIDs 注入第三方线路再取一次，handler 未做请求内缓存。
		t.Fatalf("PlanByID 调用次数 = %d, 期望 2", st.planCalls)
	}

	var items []map[string]any
	if err := json.Unmarshal(body.Data, &items); err != nil {
		t.Fatalf("data 不是数组: %v（原始: %s）", err, string(body.Data))
	}
	if len(items) != 1 {
		t.Fatalf("节点数 = %d, 期望 1", len(items))
	}
	if items[0]["name"] != "香港01" {
		t.Errorf("name = %v, 期望 香港01", items[0]["name"])
	}
	if port, _ := items[0]["port"].(float64); port != 8443 {
		t.Errorf("port = %v, 期望 8443", items[0]["port"])
	}
	if items[0]["status"] != "online" {
		t.Errorf("status = %v, 期望 online", items[0]["status"])
	}
}

// TestNodesThirdPartyPlanUnchanged：纯第三方套餐（include_local=false）仍只回第三方线路，
// 不查本地节点表 —— 有套餐的分支未被新加的提前 return 影响。
func TestNodesThirdPartyPlanUnchanged(t *testing.T) {
	st := &nodesStore{
		configs: map[string]string{"third_party_enabled": "1"},
		plan:    &Plan{ID: 8, Name: "第三方面板", IncludeLocal: false, ThirdPartyIDs: []int64{5}},
		subs:    []ThirdPartySub{{ID: 5, CachedLinks: "vless://abc@example.com:443#%E6%97%A5%E6%9C%AC1\n"}},
		onlineNodes: []Node{
			{ID: 1, Name: "不该出现", Host: "1.1.1.1", Port: 443, Status: "online"},
		},
	}
	user := &User{ID: 9, Protocol: "vless", PlanID: sql.NullInt64{Int64: 8, Valid: true}}

	_, body := callNodes(t, st, user)

	if st.onlineCalls != 0 {
		t.Fatalf("OnlineNodesForProtocol 调用次数 = %d, 期望 0（纯第三方套餐不查本地节点）", st.onlineCalls)
	}
	var items []map[string]any
	if err := json.Unmarshal(body.Data, &items); err != nil {
		t.Fatalf("data 不是数组: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("线路数 = %d, 期望 1", len(items))
	}
	if items[0]["name"] != "日本1" {
		t.Errorf("name = %v, 期望 日本1", items[0]["name"])
	}
	if items[0]["host"] != nil {
		t.Errorf("第三方线路 host 应为 null, 实际 %v", items[0]["host"])
	}
}
