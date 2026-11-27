package task

// 定时发信收件人筛选的纯函数单测（scheduleRecipientWhere，不连库）。
//
// 回归的是本次修复的核心问题：以前 resolveScheduleRecipients 写死 `WHERE enabled = 1`
// 全量取人，页面上选的「用户状态 / 限定套餐」被完全忽略 —— 定时任务按全量发出去。
// 现在它与「批量发信」共用同一套口径，这里逐条钉住：
//   - 全量忽略套餐/状态，但仍排除封禁与空邮箱；
//   - 未购买过套餐 = plan_id IS NULL，可与套餐多选并存；
//   - normal / depleted 的状态条件与参数顺序。

import (
	"strings"
	"testing"
	"time"
)

var scheduleTestNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func assertScheduleHardConditions(t *testing.T, where string) {
	t.Helper()
	for _, must := range []string{"u.enabled = 1", "u.email IS NOT NULL", "u.email <> ''"} {
		if !strings.Contains(where, must) {
			t.Errorf("WHERE 缺少硬条件 %q：%s", must, where)
		}
	}
}

// TestScheduleRecipientWhereAll：全量不带套餐/状态条件。
func TestScheduleRecipientWhereAll(t *testing.T) {
	where, args := scheduleRecipientWhere(scheduleMailConfig{
		Mode:          "all",
		UserStatus:    "normal",
		PlanIDs:       []int64{1, 2},
		IncludeNoPlan: true,
	}, scheduleTestNow)
	assertScheduleHardConditions(t, where)
	if len(args) != 0 {
		t.Errorf("全量不该有参数（套餐/状态都要忽略），实际 %v", args)
	}
	for _, forbidden := range []string{"plan_id", "expired_at"} {
		if strings.Contains(where, forbidden) {
			t.Errorf("全量不该出现 %q：%s", forbidden, where)
		}
	}
}

// TestScheduleRecipientWhereNoPlan：只勾未购买过套餐。
func TestScheduleRecipientWhereNoPlan(t *testing.T) {
	where, args := scheduleRecipientWhere(scheduleMailConfig{
		Mode:          "filter",
		UserStatus:    "all",
		IncludeNoPlan: true,
	}, scheduleTestNow)
	assertScheduleHardConditions(t, where)
	if !strings.Contains(where, "u.plan_id IS NULL") {
		t.Errorf("应生成 plan_id IS NULL：%s", where)
	}
	if len(args) != 0 {
		t.Errorf("不该有参数，实际 %v", args)
	}
}

// TestScheduleRecipientWhereLegacyPlanID：旧配置的单值 plan_id 仍然生效。
func TestScheduleRecipientWhereLegacyPlanID(t *testing.T) {
	legacy := int64(4)
	where, args := scheduleRecipientWhere(scheduleMailConfig{
		Mode:       "filter",
		UserStatus: "all",
		PlanID:     &legacy,
	}, scheduleTestNow)
	assertScheduleHardConditions(t, where)
	if !strings.Contains(where, "u.plan_id IN (?)") {
		t.Errorf("旧 plan_id 应转成 IN (?)：%s", where)
	}
	if len(args) != 1 || args[0] != int64(4) {
		t.Errorf("参数应为 [4]，实际 %v", args)
	}
}

// TestScheduleRecipientWherePlanIDsAndNoPlan：套餐多选 + 未购买过套餐 → 并集。
func TestScheduleRecipientWherePlanIDsAndNoPlan(t *testing.T) {
	where, args := scheduleRecipientWhere(scheduleMailConfig{
		Mode:          "filter",
		UserStatus:    "normal",
		PlanIDs:       []int64{7, 9},
		IncludeNoPlan: true,
	}, scheduleTestNow)
	assertScheduleHardConditions(t, where)
	if !strings.Contains(where, "(u.plan_id IN (?,?) OR u.plan_id IS NULL)") {
		t.Errorf("应生成并集条件：%s", where)
	}
	// 套餐参数在前，now 在后。
	if len(args) != 3 || args[0] != int64(7) || args[1] != int64(9) || args[2] != scheduleTestNow {
		t.Errorf("参数应为 [7 9 now]，实际 %v", args)
	}
	if n := strings.Count(where, "?"); n != len(args) {
		t.Errorf("占位符 %d 个与参数 %d 个不匹配：%s", n, len(args), where)
	}
}

// TestScheduleRecipientWhereDepleted：已耗尽条件。
func TestScheduleRecipientWhereDepleted(t *testing.T) {
	where, args := scheduleRecipientWhere(scheduleMailConfig{
		Mode:       "filter",
		UserStatus: "depleted",
	}, scheduleTestNow)
	assertScheduleHardConditions(t, where)
	if !strings.Contains(where, "u.expired_at IS NOT NULL AND u.expired_at < ?") {
		t.Errorf("已耗尽应包含过期条件：%s", where)
	}
	if len(args) != 1 || args[0] != scheduleTestNow {
		t.Errorf("参数应为 now，实际 %v", args)
	}
}
