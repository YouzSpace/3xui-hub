package api

// 支付链路上的余额分支单测（payment_balance.go / payment_service.go 的 CompleteOrder）。
//
// fakePaymentStore 只实现流程真正会用到的那几个方法，其余由嵌入的 nil 接口兜底：
// 一旦被测代码多调了一个方法，测试会直接 panic —— 正好暴露意料之外的依赖。

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

type fakePaymentStore struct {
	PaymentStore // 未实现的方法被调用时 panic（嵌入 nil 接口）

	plan     *PlanRow
	user     *User
	applied  []PlanFields
	marked   []uint64 // MarkOrderPaid 的订单 id
	enableds []bool
	created  []NewOrderInput
}

func (f *fakePaymentStore) PlanForPayment(_ context.Context, id uint64) (*PlanRow, error) {
	if f.plan == nil || f.plan.ID != id {
		return nil, sql.ErrNoRows
	}
	return f.plan, nil
}

func (f *fakePaymentStore) UserByIDPayment(_ context.Context, id uint64) (*User, error) {
	if f.user == nil || f.user.ID != id {
		return nil, sql.ErrNoRows
	}
	return f.user, nil
}

func (f *fakePaymentStore) ApplyPlanFields(_ context.Context, _ uint64, fields PlanFields) error {
	f.applied = append(f.applied, fields)
	return nil
}

func (f *fakePaymentStore) SetUserEnabled(_ context.Context, _ uint64, enabled bool) error {
	f.enableds = append(f.enableds, enabled)
	return nil
}

func (f *fakePaymentStore) MarkOrderPaid(_ context.Context, id uint64, _ sql.NullString, _ time.Time) error {
	f.marked = append(f.marked, id)
	return nil
}

func (f *fakePaymentStore) CreateOrder(_ context.Context, in NewOrderInput) (*Order, error) {
	f.created = append(f.created, in)
	return &Order{
		ID:        uint64(len(f.created)),
		OrderNo:   in.OrderNo,
		UserID:    in.UserID,
		OrderType: in.OrderType,
		Amount:    in.Amount,
		Status:    in.Status,
	}, nil
}

// testPlan 是周期套餐：50 元 / 100G 月流量 / 1 个月。
func testPlan() *PlanRow {
	return &PlanRow{
		ID:             3,
		Name:           "月付套餐",
		Type:           "period",
		IsActive:       true,
		Months:         sql.NullInt64{Int64: 1, Valid: true},
		MonthlyTraffic: sql.NullInt64{Int64: 100 * 1024 * 1024 * 1024, Valid: true},
		PeriodTraffic:  sql.NullInt64{Int64: 100 * 1024 * 1024 * 1024, Valid: true},
		Price:          "50.00",
		ResetPrice:     "5.00",
		IncludeLocal:   true,
	}
}

// newBalancePayService 组装「支付服务 + 余额服务 + 内存 store」。
func newBalancePayService(payStore PaymentStore, balanceStore BalanceStore) *PaymentService {
	svc := NewPaymentService(payStore, PaymentServiceOptions{
		Now: func() time.Time { return fixedNow },
	}, nil)
	svc.balance = NewBalanceService(balanceStore, func() time.Time { return fixedNow }, nil)
	return svc
}

// TestPayPlanWithBalanceInsufficient 余额不足时：必须返回「余额不足」，且不扣款、不建单、不开套餐。
// 变异测试靶子之二（与 TestDeductForOrderInsufficientBalance 同源）：把 checkBalance 的判据改成
// 恒放行，本用例会看到扣款成功/订单被建 → 变红。
func TestPayPlanWithBalanceInsufficient(t *testing.T) {
	payStore := &fakePaymentStore{plan: testPlan(), user: &User{ID: 7, Enabled: true}}
	balanceStore := newFakeBalanceStore()
	balanceStore.balances[7] = 1200 // 12 元，买不起 50 元的套餐
	svc := newBalancePayService(payStore, balanceStore)

	_, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4")
	if err == nil {
		t.Fatalf("余额不足却支付成功")
	}
	if !IsPaymentError(err) || err.Error() != "余额不足" {
		t.Fatalf("错误 = %v, 期望业务错误「余额不足」", err)
	}
	if balanceStore.balances[7] != 1200 {
		t.Fatalf("余额不能变，实际 %d", balanceStore.balances[7])
	}
	if len(balanceStore.orders) != 0 {
		t.Fatalf("不能建订单，实际 %d 笔", len(balanceStore.orders))
	}
	if len(payStore.applied) != 0 {
		t.Fatalf("不能开通套餐，实际 %d 次", len(payStore.applied))
	}
}

func TestPayPlanWithBalanceSuccess(t *testing.T) {
	payStore := &fakePaymentStore{plan: testPlan(), user: &User{ID: 7, Enabled: true}}
	balanceStore := newFakeBalanceStore()
	balanceStore.balances[7] = 8000 // 80 元
	svc := newBalancePayService(payStore, balanceStore)

	result, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4")
	if err != nil {
		t.Fatalf("余额支付失败: %v", err)
	}
	if result.Status != "paid" || result.Amount.cents != 5000 || result.OrderNo == "" {
		t.Fatalf("返回不对: %+v", result)
	}
	if balanceStore.balances[7] != 3000 {
		t.Fatalf("余额 = %d, 期望 3000", balanceStore.balances[7])
	}
	if len(balanceStore.orders) != 1 || balanceStore.orders[0].Status != "paid" {
		t.Fatalf("应建 1 笔 paid 订单，实际 %+v", balanceStore.orders)
	}
	if len(balanceStore.logs) != 1 || balanceStore.logs[0].Type != "consume" || balanceStore.logs[0].Amount != -5000 {
		t.Fatalf("流水不对: %+v", balanceStore.logs)
	}
	if len(payStore.applied) != 1 {
		t.Fatalf("应开通 1 次套餐，实际 %d", len(payStore.applied))
	}
	if got := payStore.applied[0].TrafficLimit; got != 100*1024*1024*1024 {
		t.Fatalf("套餐流量限额 = %d, 期望 100G", got)
	}
}

func TestCreateRechargeOrderAmountRange(t *testing.T) {
	payStore := &fakePaymentStore{}
	svc := newBalancePayService(payStore, newFakeBalanceStore())

	for _, amount := range []int64{0, -100, rechargeMinCents - 1, rechargeMaxCents + 1} {
		_, err := svc.CreateRechargeOrder(context.Background(), &User{ID: 7}, amount, 0, "1.2.3.4")
		if err == nil || !IsPaymentError(err) {
			t.Fatalf("金额 %d 分应被拒，实际 err=%v", amount, err)
		}
	}
	if len(payStore.created) != 0 {
		t.Fatalf("金额非法时不能建单，实际 %d 笔", len(payStore.created))
	}
}

// TestCompleteOrderBalanceRecharge 充值订单到账：+余额、写 recharge 流水、订单置 paid；
// 重复完成（网关重推）不会再加钱。
func TestCompleteOrderBalanceRecharge(t *testing.T) {
	payStore := &fakePaymentStore{}
	balanceStore := newFakeBalanceStore()
	balanceStore.orderStatus[21] = "pending"
	svc := newBalancePayService(payStore, balanceStore)

	order := &Order{ID: 21, UserID: 7, OrderNo: "20261103100000000021", OrderType: "balance", Amount: "50.00", Status: "pending"}
	if err := svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("充值到账失败: %v", err)
	}
	if order.Status != "paid" {
		t.Fatalf("订单状态 = %s, 期望 paid", order.Status)
	}
	if balanceStore.balances[7] != 5000 {
		t.Fatalf("余额 = %d, 期望 5000", balanceStore.balances[7])
	}
	if len(balanceStore.logs) != 1 || balanceStore.logs[0].Type != "recharge" || balanceStore.logs[0].Amount != 5000 {
		t.Fatalf("流水不对: %+v", balanceStore.logs)
	}
	if len(payStore.marked) != 0 {
		t.Fatalf("充值单应走自己的到账事务，不该再调 MarkOrderPaid")
	}

	// 重推：拿一个仍显示 pending 的同 id 订单再完成一次（模拟回调重复到达）。
	again := &Order{ID: 21, UserID: 7, OrderNo: "20261103100000000021", OrderType: "balance", Amount: "50.00", Status: "pending"}
	if err := svc.CompleteOrder(context.Background(), again, sql.NullString{}); err != nil {
		t.Fatalf("重复到账不应报错: %v", err)
	}
	if balanceStore.balances[7] != 5000 {
		t.Fatalf("重复到账后余额 = %d, 期望仍是 5000", balanceStore.balances[7])
	}
	if len(balanceStore.logs) != 1 {
		t.Fatalf("重复到账不能再写流水，实际 %d 条", len(balanceStore.logs))
	}
}

// TestCompleteOrderPlanStillWorks 回归：普通套餐订单（第三方支付回调）仍走
// MarkOrderPaid → afterPaid（applyPlan），重构 CompleteOrder 不能把这条老路径改坏。
func TestCompleteOrderPlanStillWorks(t *testing.T) {
	payStore := &fakePaymentStore{plan: testPlan(), user: &User{ID: 7, Enabled: true}}
	svc := newBalancePayService(payStore, newFakeBalanceStore())

	order := &Order{
		ID: 22, UserID: 7, OrderNo: "20261103100000000022", OrderType: "plan",
		PlanID: sql.NullInt64{Int64: 3, Valid: true}, Amount: "50.00", Status: "pending",
	}
	if err := svc.CompleteOrder(context.Background(), order, sql.NullString{}); err != nil {
		t.Fatalf("完成套餐订单失败: %v", err)
	}
	if order.Status != "paid" {
		t.Fatalf("订单状态 = %s, 期望 paid", order.Status)
	}
	if len(payStore.marked) != 1 || payStore.marked[0] != 22 {
		t.Fatalf("应 MarkOrderPaid(22)，实际 %+v", payStore.marked)
	}
	if len(payStore.applied) != 1 {
		t.Fatalf("应 applyPlan 1 次，实际 %d", len(payStore.applied))
	}
}
