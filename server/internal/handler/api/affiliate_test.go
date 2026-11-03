package api

// 邀请返佣单测：只用内存实现，不连库。
//
// 覆盖点（对齐 prompts/affiliate-backend-20261103.txt）：
//   - 返佣比例读取（默认 0.10 / 显式值 / 非法值回默认 / 0 = 暂停）
//   - 金额 = 实付分 × 比例，四舍五入到整数分，> 0 才发
//   - 两个触发点：afterPaid（套餐消费）与 completeRechargeOrder（充值到账）
//   - 同一订单只返一次（store 的 order_id 唯一键；这里用假实现复刻同一判据）
//   - GET /api/affiliate 路由已注册、返回结构（比例 / 累计返佣 / 邀请人数 / 名单）
//
// 变异测试靶子：把 Rate 的默认值或 Grant 的比例计算改成恒 0，
// TestAfterPaidGrantsRebate / TestRechargeGrantsRebate 必须变红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeAffiliateStore 是 AffiliateStore 的内存实现。
//
// granted 复刻 affiliate_rebates.order_id 唯一键：同一笔订单第二次 GrantRebate 返回 false，
// 不加钱也不记流水（SQL 侧由 1062 撞键实现同一语义）。
type fakeAffiliateStore struct {
	relations map[uint64]uint64 // 被邀请人 → 邀请人
	grants    []GrantRebateInput
	granted   map[uint64]bool
	total     int64
	invitees  []AffiliateInvitee
}

func (f *fakeAffiliateStore) InviterOf(_ context.Context, userID uint64) (uint64, error) {
	return f.relations[userID], nil
}

func (f *fakeAffiliateStore) GrantRebate(_ context.Context, in GrantRebateInput) (bool, error) {
	if f.granted == nil {
		f.granted = map[uint64]bool{}
	}
	if f.granted[in.OrderID] {
		return false, nil
	}
	f.granted[in.OrderID] = true
	f.grants = append(f.grants, in)
	f.total += in.AmountCents
	return true, nil
}

func (f *fakeAffiliateStore) RebateSummary(_ context.Context, _ uint64) (int64, []AffiliateInvitee, error) {
	return f.total, f.invitees, nil
}

// fakeSiteConfigReader 是 SiteConfigReader 的内存实现（只返回请求的键）。
type fakeSiteConfigReader map[string]string

func (f fakeSiteConfigReader) SiteConfigMany(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := f[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// affiliatePaymentStore 同时实现 PaymentStore 与 AffiliateStore，
// 用来验证 NewPaymentService 的「从 store 取出返佣能力」这段装配。
type affiliatePaymentStore struct {
	*fakePaymentStore
	*fakeAffiliateStore
}

func newAffiliateSvc(store AffiliateStore, cfg map[string]string) *AffiliateService {
	return NewAffiliateService(store, fakeSiteConfigReader(cfg), func() time.Time { return fixedNow }, nil)
}

func TestRebateCentsRounding(t *testing.T) {
	cases := []struct {
		orderCents int64
		rate       float64
		want       int64
	}{
		{5000, 0.10, 500},
		{9850, 0.10, 985}, // 9850×0.1 在 float 里是 985.0000000000001
		{333, 0.10, 33},   // 33.3 → 33
		{335, 0.10, 34},   // 33.5 → 34（四舍五入）
		{1, 0.10, 0},      // 0.1 分 → 0，不发
		{5000, 0, 0},      // 比例 0 = 暂停
		{0, 0.10, 0},      // 金额 0 不发
		{-5000, 0.10, 0},  // 负数不发
		{100000, 0.055, 5500},
	}
	for _, tc := range cases {
		if got := rebateCents(tc.orderCents, tc.rate); got != tc.want {
			t.Fatalf("rebateCents(%d, %v) = %d, 期望 %d", tc.orderCents, tc.rate, got, tc.want)
		}
	}
}

func TestRebateRateConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]string
		want float64
	}{
		{"没配过用默认 0.10", map[string]string{}, 0.10},
		{"空串用默认 0.10", map[string]string{affiliateRebateRateKey: "  "}, 0.10},
		{"显式 15%", map[string]string{affiliateRebateRateKey: "0.15"}, 0.15},
		{"显式 0 表示暂停", map[string]string{affiliateRebateRateKey: "0"}, 0},
		{"非数字回默认", map[string]string{affiliateRebateRateKey: "abc"}, 0.10},
		{"负数回默认", map[string]string{affiliateRebateRateKey: "-0.5"}, 0.10},
		{"大于 1 回默认", map[string]string{affiliateRebateRateKey: "10"}, 0.10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newAffiliateSvc(&fakeAffiliateStore{}, tc.cfg).Rate(context.Background())
			if got != tc.want {
				t.Fatalf("Rate = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// TestAfterPaidGrantsRebate 套餐消费：afterPaid 给邀请人返 10%（50.00 → 500 分）。
func TestAfterPaidGrantsRebate(t *testing.T) {
	payStore := &fakePaymentStore{plan: testPlan(), user: &User{ID: 7, Enabled: true}}
	affiliate := &fakeAffiliateStore{relations: map[uint64]uint64{7: 3}} // 7 是被邀请人，3 是邀请人
	svc := NewPaymentService(&affiliatePaymentStore{fakePaymentStore: payStore, fakeAffiliateStore: affiliate},
		PaymentServiceOptions{
			Now:         func() time.Time { return fixedNow },
			SiteConfigs: fakeSiteConfigReader{affiliateRebateRateKey: "0.10"},
		}, nil)

	order := &Order{
		ID: 22, UserID: 7, OrderNo: "20261103100000000022", OrderType: "plan",
		PlanID: sql.NullInt64{Int64: 3, Valid: true}, Amount: "50.00", Status: "paid",
	}
	if err := svc.afterPaid(context.Background(), order); err != nil {
		t.Fatalf("afterPaid 失败: %v", err)
	}
	if len(affiliate.grants) != 1 {
		t.Fatalf("应发 1 笔返佣，实际 %d 笔", len(affiliate.grants))
	}
	g := affiliate.grants[0]
	if g.InviterID != 3 || g.InviteeID != 7 || g.OrderID != 22 {
		t.Fatalf("返佣对象不对: %+v", g)
	}
	if g.AmountCents != 500 {
		t.Fatalf("返佣金额 = %d 分, 期望 500", g.AmountCents)
	}
	if g.Rate != "0.1000" || !g.Now.Equal(fixedNow) {
		t.Fatalf("比例/时间不对: rate=%s now=%v", g.Rate, g.Now)
	}
}

// TestRechargeNoRebate 充值到账不返佣（返佣只在购买套餐消费时发，充值不返）。
func TestRechargeNoRebate(t *testing.T) {
	payStore := &fakePaymentStore{}
	balanceStore := newFakeBalanceStore()
	balanceStore.orderStatus[21] = "pending"
	svc := newBalancePayService(payStore, balanceStore)

	affiliate := &fakeAffiliateStore{relations: map[uint64]uint64{7: 3}}
	svc.affiliate = NewAffiliateService(affiliate, fakeSiteConfigReader{affiliateRebateRateKey: "0.10"},
		func() time.Time { return fixedNow }, nil)

	order := &Order{ID: 21, UserID: 7, OrderNo: "20261103100000000021", OrderType: "balance", Amount: "50.00", Status: "pending"}
	if err := svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("充值到账失败: %v", err)
	}
	if len(affiliate.grants) != 0 {
		t.Fatalf("充值不该返佣，实际 %d 笔", len(affiliate.grants))
	}
}

// TestRebateOncePerOrder 同一笔订单的副作用被重复执行（例如回调与查单都完成一次），
// 返佣仍只发一笔 —— 幂等闸门复刻 SQL 里的 order_id 唯一键。
func TestRebateOncePerOrder(t *testing.T) {
	payStore := &fakePaymentStore{plan: testPlan(), user: &User{ID: 7, Enabled: true}}
	affiliate := &fakeAffiliateStore{relations: map[uint64]uint64{7: 3}}
	svc := NewPaymentService(&affiliatePaymentStore{fakePaymentStore: payStore, fakeAffiliateStore: affiliate},
		PaymentServiceOptions{
			Now:         func() time.Time { return fixedNow },
			SiteConfigs: fakeSiteConfigReader{affiliateRebateRateKey: "0.10"},
		}, nil)

	order := &Order{
		ID: 22, UserID: 7, OrderNo: "20261103100000000022", OrderType: "plan",
		PlanID: sql.NullInt64{Int64: 3, Valid: true}, Amount: "50.00", Status: "paid",
	}
	for i := 0; i < 3; i++ {
		if err := svc.afterPaid(context.Background(), order); err != nil {
			t.Fatalf("第 %d 次 afterPaid 失败: %v", i+1, err)
		}
	}
	if len(affiliate.grants) != 1 {
		t.Fatalf("同一订单只能返一次，实际 %d 笔", len(affiliate.grants))
	}
}

// TestNoRebateWithoutInviteRelation 没邀请关系 / 自己邀自己 / 比例为 0 / 金额不足 1 分都不发。
func TestNoRebateWithoutInviteRelation(t *testing.T) {
	cases := []struct {
		name      string
		relations map[uint64]uint64
		cfg       map[string]string
		amount    string
	}{
		{"没有邀请关系", map[uint64]uint64{}, map[string]string{affiliateRebateRateKey: "0.10"}, "50.00"},
		{"自己邀自己", map[uint64]uint64{7: 7}, map[string]string{affiliateRebateRateKey: "0.10"}, "50.00"},
		{"比例为 0（暂停返佣）", map[uint64]uint64{7: 3}, map[string]string{affiliateRebateRateKey: "0"}, "50.00"},
		{"返佣金额不足 1 分", map[uint64]uint64{7: 3}, map[string]string{affiliateRebateRateKey: "0.10"}, "0.04"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			affiliate := &fakeAffiliateStore{relations: tc.relations}
			svc := newAffiliateSvc(affiliate, tc.cfg)
			order := &Order{ID: 22, UserID: 7, OrderNo: "20261103100000000022", Amount: tc.amount, Status: "paid"}
			if err := svc.Grant(context.Background(), order); err != nil {
				t.Fatalf("Grant 失败: %v", err)
			}
			if len(affiliate.grants) != 0 {
				t.Fatalf("不该发返佣，实际 %+v", affiliate.grants)
			}
		})
	}
}

// TestAffiliateSummary 汇总：比例 + 累计返佣 + 被邀请人名单。
func TestAffiliateSummary(t *testing.T) {
	invitedAt := fixedNow.Add(-24 * time.Hour)
	affiliate := &fakeAffiliateStore{
		total: 1500,
		invitees: []AffiliateInvitee{
			{UserID: 8, Email: sql.NullString{String: "friend@example.com", Valid: true}, Rebate: 1500, InvitedAt: sql.NullTime{Time: invitedAt, Valid: true}},
			{UserID: 9, Rebate: 0},
		},
	}
	svc := newAffiliateSvc(affiliate, map[string]string{affiliateRebateRateKey: "0.15"})

	sum, err := svc.Summary(context.Background(), 3)
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Rate != 0.15 || sum.TotalRebate != 1500 || len(sum.Invitees) != 2 {
		t.Fatalf("汇总不对: %+v", sum)
	}
	if got := maskEmail(sum.Invitees[0].Email); got != "fr***@example.com" {
		t.Fatalf("邮箱打码 = %v, 期望 fr***@example.com", got)
	}
}

// TestMaskEmail 邮箱打码：用户端不展示被邀请人完整邮箱。
func TestMaskEmail(t *testing.T) {
	cases := []struct {
		in   sql.NullString
		want any
	}{
		{sql.NullString{}, nil},
		{sql.NullString{String: "", Valid: true}, nil},
		{sql.NullString{String: "zhangsan@qq.com", Valid: true}, "zh***@qq.com"},
		{sql.NullString{String: "a@qq.com", Valid: true}, "a***@qq.com"},
		{sql.NullString{String: "  ab@qq.com  ", Valid: true}, "ab***@qq.com"},
		{sql.NullString{String: "nodomain", Valid: true}, "no***"},
	}
	for _, tc := range cases {
		got := maskEmail(tc.in)
		if got != tc.want {
			t.Fatalf("maskEmail(%q) = %v, 期望 %v", tc.in.String, got, tc.want)
		}
	}
}

// TestAffiliateRouteRegistered 钉住端点路径：由 UserExtras.Register 挂载，
// 路径写错前端就是 404，而不会有编译错误提醒。
func TestAffiliateRouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	auth := engine.Group("", func(c *gin.Context) { c.Next() })
	(&UserExtras{}).Register(auth)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/affiliate", nil)

	registered := map[string]bool{}
	for _, r := range engine.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	if !registered["GET /api/affiliate"] {
		t.Fatalf("路由缺失: GET /api/affiliate（已注册: %v）", registered)
	}
}

// TestAffiliateHandlerShape 钉住 GET /api/affiliate 的响应契约（前端按这些字段读）：
// {rate, total_rebate, invited_count, invitees:[{user_id, email(打码), rebate, invited_at}]}
func TestAffiliateHandlerShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	affiliate := NewAffiliateService(&fakeAffiliateStore{
		total: 1500,
		invitees: []AffiliateInvitee{{
			UserID:    8,
			Email:     sql.NullString{String: "friend@example.com", Valid: true},
			Rebate:    1500,
			InvitedAt: sql.NullTime{Time: fixedNow, Valid: true},
		}},
	}, fakeSiteConfigReader{affiliateRebateRateKey: "0.10"}, func() time.Time { return fixedNow }, nil)

	extras := &UserExtras{
		handler:   &Handler{logger: log.New(io.Discard, "", 0)},
		affiliate: affiliate,
	}
	auth := engine.Group("", func(c *gin.Context) {
		c.Set(ctxUserKey, &User{ID: 3})
		c.Next()
	})
	extras.Register(auth)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/affiliate", nil))

	var body struct {
		Code int `json:"code"`
		Data struct {
			Rate         float64 `json:"rate"`
			TotalRebate  int64   `json:"total_rebate"`
			InvitedCount int     `json:"invited_count"`
			Invitees     []struct {
				UserID    uint64  `json:"user_id"`
				Email     *string `json:"email"`
				Rebate    int64   `json:"rebate"`
				InvitedAt *string `json:"invited_at"`
			} `json:"invitees"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("code = %d, 期望 0（body=%s）", body.Code, rec.Body.String())
	}
	if body.Data.Rate != 0.1 || body.Data.TotalRebate != 1500 || body.Data.InvitedCount != 1 {
		t.Fatalf("汇总字段不对: %+v", body.Data)
	}
	if len(body.Data.Invitees) != 1 {
		t.Fatalf("invitees 条数 = %d, 期望 1", len(body.Data.Invitees))
	}
	it := body.Data.Invitees[0]
	if it.UserID != 8 || it.Rebate != 1500 {
		t.Fatalf("被邀请人字段不对: %+v", it)
	}
	if it.Email == nil || *it.Email != "fr***@example.com" {
		t.Fatalf("email 应打码成 fr***@example.com, 实际 %v", it.Email)
	}
	if it.InvitedAt == nil || *it.InvitedAt != "2026-11-03T10:00:00+00:00" {
		t.Fatalf("invited_at = %v, 期望 2026-11-03T10:00:00+00:00", it.InvitedAt)
	}
}
