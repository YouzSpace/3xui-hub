package api

// 地区功能 #15：换套餐时的绑定收敛接线点（payment_service.afterPaid）。
//
// 三条断言，各守一个容易写错的边界：
//  1. 换套餐（旧 plan_id ≠ 新套餐）→ 必须先调清理器、再调分配器（顺序反了就等于没清）；
//  2. 同一套餐续费 → 不调清理器（不碰绑定，避免无谓断线/多余 DELETE）；
//  3. 清理失败只记日志：订单照样 paid、套餐照样生效、分配器照常被调
//     （钱已经收了，回调不能因为副作用失败返回 FAIL，网关会无限重推）。

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// newPruneTestService 组装「余额支付 + 内存 store」，并注入记录调用的清理器/分配器。
// planID>0 表示用户当前已持有该套餐（续费用例），0 表示还没套餐（首次购买）。
func newPruneTestService(t *testing.T, planID int64, prune UserNodePruner, assign UserNodeAssigner) (*PaymentService, *fakePaymentStore) {
	t.Helper()
	user := &User{ID: 7, Enabled: true}
	if planID > 0 {
		user.PlanID = sql.NullInt64{Int64: planID, Valid: true}
	}
	payStore := &fakePaymentStore{plan: testPlan(), user: user}
	balanceStore := newFakeBalanceStore()
	balanceStore.balances[7] = 8000 // 80 元，够买 50 元套餐
	svc := newBalancePayService(payStore, balanceStore)
	svc.pruneUserNodes = prune
	svc.assignUserNodes = assign
	return svc, payStore
}

// TestPayPlanChangePrunesThenAssigns 换套餐：先清理旧地区绑定，再按新套餐分配。
func TestPayPlanChangePrunesThenAssigns(t *testing.T) {
	var calls []string
	var prunedUser uint64
	svc, payStore := newPruneTestService(t, 2,
		func(_ context.Context, userID uint64) (int64, error) {
			calls = append(calls, "prune")
			prunedUser = userID
			return 1, nil
		},
		func(_ context.Context, _ uint64) error {
			calls = append(calls, "assign")
			return nil
		})

	res, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4")
	if err != nil {
		t.Fatalf("余额支付失败: %v", err)
	}
	if res.Status != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", res.Status)
	}
	if len(calls) != 2 || calls[0] != "prune" || calls[1] != "assign" {
		t.Fatalf("调用顺序 = %v，期望 [prune assign]", calls)
	}
	if prunedUser != 7 {
		t.Fatalf("清理器收到的用户 = %d，期望 7", prunedUser)
	}
}

// TestPayPlanRenewSamePlanSkipsPrune 同一套餐续费：不清理绑定（旧绑定本来就合规）。
func TestPayPlanRenewSamePlanSkipsPrune(t *testing.T) {
	pruned, assigned := 0, 0
	svc, payStore := newPruneTestService(t, 3,
		func(_ context.Context, _ uint64) (int64, error) {
			pruned++
			return 0, nil
		},
		func(_ context.Context, _ uint64) error {
			assigned++
			return nil
		})

	if _, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4"); err != nil {
		t.Fatalf("余额支付失败: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("同套餐续费不该调清理器，实际调了 %d 次", pruned)
	}
	if assigned != 1 {
		t.Fatalf("续费仍应调一次分配器，实际 %d 次", assigned)
	}
}

// TestPayPlanPruneFailureKeepsOrderPaid 清理失败只记日志：订单 paid、分配照做。
func TestPayPlanPruneFailureKeepsOrderPaid(t *testing.T) {
	assigned := 0
	svc, payStore := newPruneTestService(t, 2,
		func(_ context.Context, _ uint64) (int64, error) {
			return 0, errors.New("mysql: 连接断了")
		},
		func(_ context.Context, _ uint64) error {
			assigned++
			return nil
		})

	res, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4")
	if err != nil {
		t.Fatalf("清理失败不能把订单打回失败，实际返回: %v", err)
	}
	if res.Status != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", res.Status)
	}
	if len(payStore.applied) != 1 {
		t.Fatalf("套餐应生效 1 次，实际 %d 次", len(payStore.applied))
	}
	if assigned != 1 {
		t.Fatalf("清理失败后仍应继续分配，实际分配 %d 次", assigned)
	}
}
