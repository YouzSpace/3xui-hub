package admin

// 批量发信收件人筛选的纯函数单测（mailRecipientWhere，不连库）。
//
// 覆盖产品口径（用户 2026-11-27 定稿）：
//   - 全量 = 所有已启用账号（含未购买过套餐 / 已过期 / 已超量）；
//   - 「未购买过套餐」= plan_id IS NULL，可与套餐多选并存（并集）；
//   - 被封禁（enabled=0）与空邮箱的账号**永不**进收件人集合，所有分支都成立。
//
// 这里只验 SQL 片段与参数顺序；真正的取数语义由集成测试（真库）覆盖。

import (
	"strings"
	"testing"
	"time"
)

var recipientTestNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// 断言：任何筛选组合都必须带上「已启用」与「有邮箱」两条硬条件。
func assertRecipientHardConditions(t *testing.T, where string) {
	t.Helper()
	for _, must := range []string{"u.enabled = 1", "u.email IS NOT NULL", "u.email <> ''"} {
		if !strings.Contains(where, must) {
			t.Errorf("WHERE 缺少硬条件 %q：%s", must, where)
		}
	}
}

// TestMailRecipientWhereAll：全量不带任何套餐/状态条件，但仍排除封禁与空邮箱。
func TestMailRecipientWhereAll(t *testing.T) {
	where, args := mailRecipientWhere(MailRecipientFilter{UserStatus: "all"}, recipientTestNow)
	assertRecipientHardConditions(t, where)
	if len(args) != 0 {
		t.Errorf("全量不该有参数，实际 %v", args)
	}
	for _, forbidden := range []string{"plan_id", "expired_at", "traffic_used"} {
		if strings.Contains(where, forbidden) {
			t.Errorf("全量不该出现 %q：%s", forbidden, where)
		}
	}
}

// TestMailRecipientWhereNoPlan：只勾「未购买过套餐」→ plan_id IS NULL。
func TestMailRecipientWhereNoPlan(t *testing.T) {
	where, args := mailRecipientWhere(MailRecipientFilter{
		UserStatus: "all",
		NoPlan:     true,
	}, recipientTestNow)
	assertRecipientHardConditions(t, where)
	if !strings.Contains(where, "u.plan_id IS NULL") {
		t.Errorf("未购买过套餐应生成 plan_id IS NULL：%s", where)
	}
	if strings.Contains(where, "IN (") {
		t.Errorf("没勾具体套餐时不该出现 IN：%s", where)
	}
	if len(args) != 0 {
		t.Errorf("未购买过套餐不该有参数，实际 %v", args)
	}
}

// TestMailRecipientWherePlanIDs：多选套餐 → 参数化的 IN，顺序与入参一致。
func TestMailRecipientWherePlanIDs(t *testing.T) {
	where, args := mailRecipientWhere(MailRecipientFilter{
		UserStatus: "all",
		PlanIDs:    []int64{7, 9},
	}, recipientTestNow)
	assertRecipientHardConditions(t, where)
	if !strings.Contains(where, "u.plan_id IN (?,?)") {
		t.Errorf("多选套餐应生成 IN (?,?)：%s", where)
	}
	if len(args) != 2 || args[0] != int64(7) || args[1] != int64(9) {
		t.Errorf("套餐参数应为 [7 9]，实际 %v", args)
	}
}

// TestMailRecipientWherePlanIDsAndNoPlan：套餐多选 + 未购买过套餐 → 并集。
func TestMailRecipientWherePlanIDsAndNoPlan(t *testing.T) {
	where, args := mailRecipientWhere(MailRecipientFilter{
		UserStatus: "all",
		PlanIDs:    []int64{7},
		NoPlan:     true,
	}, recipientTestNow)
	assertRecipientHardConditions(t, where)
	if !strings.Contains(where, "(u.plan_id IN (?) OR u.plan_id IS NULL)") {
		t.Errorf("应生成并集条件：%s", where)
	}
	if len(args) != 1 || args[0] != int64(7) {
		t.Errorf("参数应为 [7]，实际 %v", args)
	}
}

// TestMailRecipientWhereStatus：normal / depleted 的状态条件与参数。
func TestMailRecipientWhereStatus(t *testing.T) {
	cases := []struct {
		status  string
		want    []string
		argNow  bool
		comment string
	}{
		{
			status:  "normal",
			want:    []string{"u.expired_at IS NULL OR u.expired_at > ?"},
			argNow:  true,
			comment: "正常 = 未过期",
		},
		{
			status:  "depleted",
			want:    []string{"u.expired_at IS NOT NULL AND u.expired_at < ?"},
			argNow:  true,
			comment: "已耗尽 = 过期或超量",
		},
		{
			status:  "",
			want:    []string{"u.expired_at IS NULL OR u.expired_at > ?"},
			argNow:  true,
			comment: "空值按 normal",
		},
	}
	for _, tc := range cases {
		where, args := mailRecipientWhere(MailRecipientFilter{UserStatus: tc.status}, recipientTestNow)
		assertRecipientHardConditions(t, where)
		for _, want := range tc.want {
			if !strings.Contains(where, want) {
				t.Errorf("%s（status=%q）应包含 %q：%s", tc.comment, tc.status, want, where)
			}
		}
		if tc.argNow {
			if len(args) != 1 || args[0] != recipientTestNow {
				t.Errorf("%s 参数应为 now，实际 %v", tc.comment, args)
			}
		}
	}
}

// TestMailRecipientWhereArgsOrder：套餐参数在前、now 在后（占位符与参数必须同序）。
func TestMailRecipientWhereArgsOrder(t *testing.T) {
	where, args := mailRecipientWhere(MailRecipientFilter{
		UserStatus: "depleted",
		PlanIDs:    []int64{3, 5},
	}, recipientTestNow)
	if len(args) != 3 {
		t.Fatalf("参数个数应为 3（2 套餐 + now），实际 %d: %v", len(args), args)
	}
	if args[0] != int64(3) || args[1] != int64(5) || args[2] != recipientTestNow {
		t.Errorf("参数顺序应为 [3 5 now]，实际 %v", args)
	}
	// 占位符总数必须等于参数个数，否则 SQL 一定报错。
	if n := strings.Count(where, "?"); n != len(args) {
		t.Errorf("占位符 %d 个与参数 %d 个不匹配：%s", n, len(args), where)
	}
}
