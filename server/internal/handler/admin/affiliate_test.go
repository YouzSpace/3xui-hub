package admin

// 邀请返佣域单测：只验路由与输出形状，不连库。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// stubAffiliateStore 是 AffiliateStore 的内存实现。
type stubAffiliateStore struct {
	rows  []AffiliateRebate
	total int64
}

func (s *stubAffiliateStore) RebatePage(_ context.Context, _, _ int64) ([]AffiliateRebate, int64, error) {
	return s.rows, s.total, nil
}

// TestAffiliateRebatesRouteAndShape 钉住管理端返佣流水的路径与字段：
// 路径写错前端就是 404（没有编译期提醒）；字段名是前端契约，不能悄悄改。
func TestAffiliateRebatesRouteAndShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	createdAt := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	inviterEmail := "inviter@example.com"
	inviteeEmail := "invitee@example.com"
	store := &stubAffiliateStore{
		total: 1,
		rows: []AffiliateRebate{{
			ID: 9, InviterID: 3, InviterEmail: &inviterEmail,
			InviteeID: 7, InviteeEmail: &inviteeEmail,
			OrderID: 22, OrderNo: "20261103100000000022",
			Amount: 500, Rate: "0.1000", CreatedAt: &createdAt,
		}},
	}

	engine := gin.New()
	NewAffiliateDomain(AffiliateDeps{Store: store}).Register(engine.Group("/admin-api"))

	registered := map[string]bool{}
	for _, r := range engine.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	if !registered["GET /admin-api/affiliate/rebates"] {
		t.Fatalf("路由缺失: GET /admin-api/affiliate/rebates（已注册: %v）", registered)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin-api/affiliate/rebates?page=1&per_page=20", nil)
	engine.ServeHTTP(rec, req)

	var body struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			List []struct {
				InviterID    uint64  `json:"inviter_id"`
				InviterEmail string  `json:"inviter_email"`
				InviteeID    uint64  `json:"invitee_id"`
				InviteeEmail string  `json:"invitee_email"`
				OrderID      uint64  `json:"order_id"`
				OrderNo      string  `json:"order_no"`
				Amount       int64   `json:"amount"`
				Rate         float64 `json:"rate"`
				CreatedAt    string  `json:"created_at"`
			} `json:"list"`
			Total   int64 `json:"total"`
			Page    int64 `json:"page"`
			PerPage int64 `json:"per_page"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("code = %d, 期望 0（body=%s）", body.Code, rec.Body.String())
	}
	if body.Data.Total != 1 || body.Data.Page != 1 || body.Data.PerPage != 20 || len(body.Data.List) != 1 {
		t.Fatalf("分页字段不对: %+v", body.Data)
	}
	row := body.Data.List[0]
	if row.InviterID != 3 || row.InviterEmail != inviterEmail ||
		row.InviteeID != 7 || row.InviteeEmail != inviteeEmail ||
		row.OrderID != 22 || row.OrderNo != "20261103100000000022" ||
		row.Amount != 500 || row.Rate != 0.1 || row.CreatedAt != "2026-11-03 10:00:00" {
		t.Fatalf("行字段不对: %+v", row)
	}
}
