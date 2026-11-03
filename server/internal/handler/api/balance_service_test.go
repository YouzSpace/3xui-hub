package api

// 余额域单测：只用内存实现，不连库（与 P2/P7 的单测口径一致）。
//
// 覆盖点（对齐 docs/balance/implementation-plan.md §3.1.1/§3.1.2/§3.1.3/§3.1.4 与 §4）：
//   - 查余额 + 流水（倒序、分页）
//   - 兑换码：码归一化、四种失败原因、成功后余额与流水
//   - 余额扣款：余额不足必须拒（**变异测试的靶子**）、余额足够时扣款+流水+订单
//   - 充值到账：幂等（重复回调不重复加钱）

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeBalanceStore 是 BalanceStore 的内存实现。
//
// 刻意**不**复刻 SQL 里的 `WHERE balance >= ?` 条件更新兜底：那一道是并发兜底，
// 单测要验的是 service 层判据（checkBalance）。如果 fake 自己再拦一次，
// 把 checkBalance 改成恒真也测不出来（变异测试会假绿）。
type fakeBalanceStore struct {
	balances    map[uint64]int64
	logs        []BalanceLog
	cards       map[string]*fakeGiftcard
	orders      []*Order
	orderStatus map[uint64]string
	lastPay     BalancePayInput
	nextLogID   int64
}

type fakeGiftcard struct {
	id      int64
	value   int64
	status  string
	expires sql.NullTime
}

func newFakeBalanceStore() *fakeBalanceStore {
	return &fakeBalanceStore{
		balances:    map[uint64]int64{},
		cards:       map[string]*fakeGiftcard{},
		orderStatus: map[uint64]string{},
	}
}

func (f *fakeBalanceStore) UserBalance(_ context.Context, userID uint64) (int64, error) {
	return f.balances[userID], nil
}

func (f *fakeBalanceStore) BalanceLogs(_ context.Context, userID uint64, limit, offset int) ([]BalanceLog, error) {
	all := make([]BalanceLog, 0, len(f.logs))
	for i := len(f.logs) - 1; i >= 0; i-- { // 与 SQL 的 ORDER BY id DESC 同序
		if f.logs[i].UserID == int64(userID) {
			all = append(all, f.logs[i])
		}
	}
	if offset >= len(all) {
		return []BalanceLog{}, nil
	}
	all = all[offset:]
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (f *fakeBalanceStore) RedeemGiftcard(_ context.Context, in RedeemGiftcardInput) (*RedeemGiftcardResult, error) {
	card, ok := f.cards[in.Code]
	if !ok {
		return nil, ErrGiftcardNotFound
	}
	switch card.status {
	case "active":
	case "used":
		return nil, ErrGiftcardUsed
	case "disabled":
		return nil, ErrGiftcardDisabled
	default:
		return nil, ErrGiftcardNotFound
	}
	if card.expires.Valid && card.expires.Time.Before(in.Now) {
		return nil, ErrGiftcardExpired
	}

	card.status = "used"
	f.balances[in.UserID] += card.value
	balance := f.balances[in.UserID]
	f.nextLogID++
	f.logs = append(f.logs, BalanceLog{
		ID:           f.nextLogID,
		UserID:       int64(in.UserID),
		Amount:       card.value,
		Type:         "redeem",
		GiftcardID:   nullInt64(card.id),
		BalanceAfter: balance,
		Note:         sql.NullString{String: "兑换码", Valid: true},
		CreatedAt:    sql.NullTime{Time: in.Now, Valid: true},
	})
	return &RedeemGiftcardResult{GiftcardID: card.id, Added: card.value, Balance: balance}, nil
}

func (f *fakeBalanceStore) PayOrderWithBalance(_ context.Context, in BalancePayInput) (*Order, error) {
	// 不校验余额（见类型注释）：扣款是否该发生由 service 的 checkBalance 决定。
	f.balances[in.UserID] -= in.AmountCents
	balance := f.balances[in.UserID]
	f.lastPay = in

	order := &Order{
		ID:             uint64(len(f.orders) + 1),
		OrderNo:        in.OrderNo,
		UserID:         in.UserID,
		OrderType:      "plan",
		PlanID:         nullInt64(int64(in.PlanID)),
		Amount:         money{cents: in.AmountCents}.String(),
		OriginalAmount: nullStr(money{cents: in.OriginalCents}.String()),
		DiscountCodeID: in.DiscountCodeID,
		Status:         "paid",
		PaidAt:         sql.NullTime{Time: in.Now, Valid: true},
		CreatedAt:      in.Now,
	}
	f.orders = append(f.orders, order)
	f.orderStatus[order.ID] = "paid"

	if in.AmountCents > 0 {
		f.nextLogID++
		f.logs = append(f.logs, BalanceLog{
			ID:           f.nextLogID,
			UserID:       int64(in.UserID),
			Amount:       -in.AmountCents,
			Type:         "consume",
			OrderID:      nullInt64(int64(order.ID)),
			BalanceAfter: balance,
			Note:         sql.NullString{String: in.Note, Valid: true},
			CreatedAt:    sql.NullTime{Time: in.Now, Valid: true},
		})
	}
	return order, nil
}

func (f *fakeBalanceStore) CreditRechargeOrder(_ context.Context, in CreditRechargeInput) (bool, int64, error) {
	if f.orderStatus[in.OrderID] == "paid" {
		return false, f.balances[in.UserID], nil
	}
	f.orderStatus[in.OrderID] = "paid"
	f.balances[in.UserID] += in.AmountCents
	balance := f.balances[in.UserID]
	f.nextLogID++
	f.logs = append(f.logs, BalanceLog{
		ID:           f.nextLogID,
		UserID:       int64(in.UserID),
		Amount:       in.AmountCents,
		Type:         "recharge",
		OrderID:      nullInt64(int64(in.OrderID)),
		BalanceAfter: balance,
		Note:         sql.NullString{String: in.Note, Valid: true},
		CreatedAt:    sql.NullTime{Time: in.Now, Valid: true},
	})
	return true, balance, nil
}

var fixedNow = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

func newBalanceSvc(store BalanceStore) *BalanceService {
	return NewBalanceService(store, func() time.Time { return fixedNow }, nil)
}

func TestBalanceCurrent(t *testing.T) {
	store := newFakeBalanceStore()
	store.balances[7] = 5000
	store.logs = []BalanceLog{
		{ID: 1, UserID: 7, Amount: 5000, Type: "redeem", BalanceAfter: 5000},
		{ID: 2, UserID: 7, Amount: -1200, Type: "consume", BalanceAfter: 3800},
		{ID: 3, UserID: 8, Amount: 999, Type: "redeem", BalanceAfter: 999}, // 别人的流水不能混进来
	}
	svc := newBalanceSvc(store)

	balance, logs, err := svc.Current(context.Background(), 7, 20, 0)
	if err != nil {
		t.Fatalf("Current 失败: %v", err)
	}
	if balance != 5000 {
		t.Fatalf("余额 = %d, 期望 5000", balance)
	}
	if len(logs) != 2 {
		t.Fatalf("流水条数 = %d, 期望 2", len(logs))
	}
	if logs[0].ID != 2 || logs[1].ID != 1 {
		t.Fatalf("流水应倒序（最新在前），实际 id 顺序 %d,%d", logs[0].ID, logs[1].ID)
	}

	_, page2, err := svc.Current(context.Background(), 7, 1, 1)
	if err != nil {
		t.Fatalf("Current 分页失败: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != 1 {
		t.Fatalf("offset=1 应只返回第 2 条（id=1），实际 %+v", page2)
	}
}

func TestRedeemGiftcardNormalizesCode(t *testing.T) {
	store := newFakeBalanceStore()
	store.cards["ABCD-EFGH-JKLM-NPQR"] = &fakeGiftcard{id: 1, value: 5000, status: "active"}
	svc := newBalanceSvc(store)

	res, err := svc.Redeem(context.Background(), 7, "  abcd-efgh-jklm-npqr \n")
	if err != nil {
		t.Fatalf("兑换失败: %v", err)
	}
	if res.Added != 5000 || res.Balance != 5000 {
		t.Fatalf("兑换结果 = %+v, 期望 added=5000 balance=5000", res)
	}
	if store.balances[7] != 5000 {
		t.Fatalf("余额 = %d, 期望 5000", store.balances[7])
	}
	if len(store.logs) != 1 || store.logs[0].Type != "redeem" || store.logs[0].BalanceAfter != 5000 {
		t.Fatalf("流水不对: %+v", store.logs)
	}
	if store.cards["ABCD-EFGH-JKLM-NPQR"].status != "used" {
		t.Fatalf("兑换码应置 used, 实际 %s", store.cards["ABCD-EFGH-JKLM-NPQR"].status)
	}
}

func TestRedeemGiftcardErrors(t *testing.T) {
	expiredAt := fixedNow.Add(-time.Hour)
	cases := []struct {
		name    string
		code    string
		card    *fakeGiftcard
		want    error
		wantMsg string
	}{
		{name: "不存在", code: "NOPE-NOPE-NOPE-NOPE", card: nil, want: ErrGiftcardNotFound, wantMsg: "兑换码不存在"},
		{name: "已使用", code: "USED-USED-USED-USED", card: &fakeGiftcard{id: 2, value: 100, status: "used"}, want: ErrGiftcardUsed, wantMsg: "兑换码已使用"},
		{name: "已作废", code: "DISA-DISA-DISA-DISA", card: &fakeGiftcard{id: 3, value: 100, status: "disabled"}, want: ErrGiftcardDisabled, wantMsg: "兑换码已作废"},
		{name: "已过期", code: "EXPR-EXPR-EXPR-EXPR", card: &fakeGiftcard{id: 4, value: 100, status: "active", expires: sql.NullTime{Time: expiredAt, Valid: true}}, want: ErrGiftcardExpired, wantMsg: "兑换码已过期"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeBalanceStore()
			if tc.card != nil {
				store.cards[tc.code] = tc.card
			}
			_, err := newBalanceSvc(store).Redeem(context.Background(), 7, tc.code)
			if err == nil {
				t.Fatalf("期望失败，实际成功")
			}
			msg, ok := redeemErrorText(err)
			if !ok {
				t.Fatalf("错误未被识别为兑换失败: %v", err)
			}
			if msg != tc.wantMsg {
				t.Fatalf("文案 = %q, 期望 %q", msg, tc.wantMsg)
			}
			if store.balances[7] != 0 {
				t.Fatalf("失败不应加余额，实际 %d", store.balances[7])
			}
		})
	}
}

// TestDeductForOrderInsufficientBalance 是「余额不足必须拒付」的判据测试。
// 变异测试靶子：把 checkBalance 里的 balanceCents < amountCents 改成恒假（恒真放行），
// 本用例必须变红（余额会被扣成负数、订单被建出来）。
func TestDeductForOrderInsufficientBalance(t *testing.T) {
	store := newFakeBalanceStore()
	store.balances[7] = 1200 // 12 元
	svc := newBalanceSvc(store)

	_, err := svc.DeductForOrder(context.Background(), BalancePayInput{
		OrderNo:     "20261103100000000001",
		UserID:      7,
		PlanID:      3,
		AmountCents: 5000, // 50 元
		Now:         fixedNow,
	})
	if err == nil {
		t.Fatalf("余额不足却扣款成功")
	}
	if !errors.Is(err, ErrBalanceInsufficient) {
		t.Fatalf("错误 = %v, 期望 ErrBalanceInsufficient", err)
	}
	if store.balances[7] != 1200 {
		t.Fatalf("余额不足时余额不能变，实际 %d", store.balances[7])
	}
	if len(store.orders) != 0 {
		t.Fatalf("余额不足时不能建订单，实际建了 %d 笔", len(store.orders))
	}
	if len(store.logs) != 0 {
		t.Fatalf("余额不足时不能写流水，实际 %d 条", len(store.logs))
	}
}

func TestDeductForOrderSuccess(t *testing.T) {
	store := newFakeBalanceStore()
	store.balances[7] = 8000
	svc := newBalanceSvc(store)

	order, err := svc.DeductForOrder(context.Background(), BalancePayInput{
		OrderNo:       "20261103100000000002",
		UserID:        7,
		PlanID:        3,
		AmountCents:   5000,
		OriginalCents: 6000,
		Note:          "余额支付-月付套餐",
		Now:           fixedNow,
	})
	if err != nil {
		t.Fatalf("扣款失败: %v", err)
	}
	if store.balances[7] != 3000 {
		t.Fatalf("余额 = %d, 期望 3000", store.balances[7])
	}
	if order.Status != "paid" || order.Amount != "50.00" || order.OriginalAmount.String != "60.00" {
		t.Fatalf("订单不对: %+v", order)
	}
	if len(store.logs) != 1 {
		t.Fatalf("流水条数 = %d, 期望 1", len(store.logs))
	}
	log := store.logs[0]
	if log.Type != "consume" || log.Amount != -5000 || log.BalanceAfter != 3000 || !log.OrderID.Valid {
		t.Fatalf("流水不对: %+v", log)
	}
}

func TestCreditRechargeIsIdempotent(t *testing.T) {
	store := newFakeBalanceStore()
	store.orderStatus[11] = "pending"
	svc := newBalanceSvc(store)
	order := &Order{ID: 11, UserID: 7, OrderNo: "20261103100000000003", OrderType: "balance", Amount: "50.00"}

	credited, balance, err := svc.CreditRecharge(context.Background(), order)
	if err != nil || !credited {
		t.Fatalf("首次到账应成功: credited=%v err=%v", credited, err)
	}
	if balance != 5000 || store.balances[7] != 5000 {
		t.Fatalf("到账后余额 = %d/%d, 期望 5000", balance, store.balances[7])
	}

	// 重复回调（网关重推/查单再完成）：不能再加一次钱。
	credited, balance, err = svc.CreditRecharge(context.Background(), order)
	if err != nil {
		t.Fatalf("重复到账不应报错: %v", err)
	}
	if credited {
		t.Fatalf("重复到账不应再次加钱")
	}
	if balance != 5000 || store.balances[7] != 5000 {
		t.Fatalf("重复到账后余额 = %d/%d, 期望仍是 5000", balance, store.balances[7])
	}
	if len(store.logs) != 1 {
		t.Fatalf("流水应只有 1 条，实际 %d", len(store.logs))
	}
}

// TestBalanceRoutesRegistered 钉住两个余额端点的路径：它们由 UserExtras.Register 挂载
// （routes.go 里整段套 api.auth），路径写错前端就是 404，而且不会有编译错误提醒。
func TestBalanceRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// 与 cmd/server/routes.go 一致：挂在 api.auth 分组下（这里用占位中间件代替登录校验）。
	auth := engine.Group("", func(c *gin.Context) { c.Next() })
	(&UserExtras{}).Register(auth)

	registered := map[string]bool{}
	for _, r := range engine.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	for _, want := range []string{"GET /api/balance", "POST /api/redeem"} {
		if !registered[want] {
			t.Fatalf("路由缺失: %s（已注册: %v）", want, registered)
		}
	}
}

// TestBalancePageParams 分页参数口径：默认 20 条、上限 100、page 从 1 起。
func TestBalancePageParams(t *testing.T) {
	cases := []struct {
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"", balanceDefaultPerPage, 0},
		{"?per_page=5&page=3", 5, 10},
		{"?per_page=999", balanceMaxPerPage, 0},
		{"?per_page=abc&page=-2", balanceDefaultPerPage, 0},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/balance"+tc.query, nil)
		limit, offset := balancePageParams(c)
		if limit != tc.wantLimit || offset != tc.wantOffset {
			t.Fatalf("%q → limit=%d offset=%d, 期望 %d/%d", tc.query, limit, offset, tc.wantLimit, tc.wantOffset)
		}
	}
}
