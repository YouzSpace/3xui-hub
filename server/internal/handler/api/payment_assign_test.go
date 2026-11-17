package api

// 地区功能 #14：购买流程的接线点。
//
// 两条断言，各守一个容易写错的边界：
//  1. 套餐生效后**必须**调用分配器（漏调 = 用户买了套餐却没有任何地区绑定，
//     订阅渲染/nodeapi 过滤全部按「无绑定 = 老行为」处理，功能静默不生效）；
//  2. 分配失败**绝不能**让订单失败（钱已经收了，回调返回失败会被网关无限重推，
//     用户还会以为没付成功）。失败只记日志，交给管理端事后补绑。

import (
	"context"
	"errors"
	"testing"
)

// newAssignTestService 组装「余额支付 + 内存 store」，并注入一个记录调用的分配器。
func newAssignTestService(t *testing.T, assign UserNodeAssigner) (*PaymentService, *fakePaymentStore) {
	t.Helper()
	payStore := &fakePaymentStore{plan: testPlan(), user: &User{ID: 7, Enabled: true}}
	balanceStore := newFakeBalanceStore()
	balanceStore.balances[7] = 8000 // 80 元，够买 50 元套餐
	svc := newBalancePayService(payStore, balanceStore)
	svc.assignUserNodes = assign
	return svc, payStore
}

// TestPayPlanCallsAssignUserNodes 套餐生效后必须调一次分配器，且传的是下单用户。
func TestPayPlanCallsAssignUserNodes(t *testing.T) {
	var got []uint64
	svc, payStore := newAssignTestService(t, func(_ context.Context, userID uint64) error {
		got = append(got, userID)
		return nil
	})

	res, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4")
	if err != nil {
		t.Fatalf("余额支付失败: %v", err)
	}
	if res.Status != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", res.Status)
	}
	if len(payStore.applied) != 1 {
		t.Fatalf("套餐应生效 1 次，实际 %d 次", len(payStore.applied))
	}
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("分配器调用 = %v，期望 [7]（套餐生效后必须调一次）", got)
	}
}

// TestPayPlanAssignFailureKeepsOrderPaid 分配失败（地区没节点/锁被占）只记日志：
// 订单照样 paid、套餐照样生效、函数不返回错误。
//
// 变异测试靶子：把 afterPaid 里的分配错误改成 return err，本用例立刻变红。
func TestPayPlanAssignFailureKeepsOrderPaid(t *testing.T) {
	called := 0
	svc, payStore := newAssignTestService(t, func(_ context.Context, _ uint64) error {
		called++
		return errors.New("scheduler: 该地区暂无可分配节点")
	})

	res, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4")
	if err != nil {
		t.Fatalf("分配失败不能影响订单完成，实际返回错误: %v", err)
	}
	if res == nil || res.Status != "paid" {
		t.Fatalf("订单必须仍是 paid，实际 %+v", res)
	}
	if len(payStore.applied) != 1 {
		t.Fatalf("套餐必须已生效，实际 %d 次", len(payStore.applied))
	}
	if called != 1 {
		t.Fatalf("分配器调用次数 = %d，期望 1", called)
	}
}

// TestPayPlanWithoutAssigner 分配器未接线（纯 PHP 对齐期/单测环境）时不能 panic。
func TestPayPlanWithoutAssigner(t *testing.T) {
	payStore := &fakePaymentStore{plan: testPlan(), user: &User{ID: 7, Enabled: true}}
	balanceStore := newFakeBalanceStore()
	balanceStore.balances[7] = 8000
	svc := newBalancePayService(payStore, balanceStore)

	res, err := svc.PayPlanWithBalance(context.Background(), payStore.user, 3, "", "1.2.3.4")
	if err != nil {
		t.Fatalf("未接分配器时支付失败: %v", err)
	}
	if res.Status != "paid" {
		t.Fatalf("订单状态 = %q，期望 paid", res.Status)
	}
}
