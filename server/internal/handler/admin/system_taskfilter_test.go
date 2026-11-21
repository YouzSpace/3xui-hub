package admin

// system_taskfilter_test.go 直接测 taskStatuses 的过滤逻辑，不依赖 HTTP/登录：
//   - 被开关关掉的任务（DisabledTaskNames）→ 不出现；
//   - 条件不满足的任务（condition 返回 false）→ 不出现；
//   - 其余任务正常出现。
// 这是「页面显示 == 任务真的会跑」这条约束的核心护栏。

import (
	"context"
	"log"
	"testing"
	"time"
)

// fakeSystemStore 只实现 taskStatuses 用到的方法。
type fakeSystemStore struct {
	disabled map[string]bool
}

func (f *fakeSystemStore) OperationLogsRecent(context.Context, int64) ([]OperationLogRow, error) {
	return nil, nil
}
func (f *fakeSystemStore) OperationLogPage(context.Context, AuditLogFilter, int64, int64) ([]OperationLogRow, int64, error) {
	return nil, 0, nil
}
func (f *fakeSystemStore) ScheduledTaskRunLast(context.Context, string) (*ScheduledTaskRun, error) {
	return nil, nil
}
func (f *fakeSystemStore) ScheduledTaskRunsRecent(context.Context, int64) ([]ScheduledTaskRun, error) {
	return nil, nil
}
func (f *fakeSystemStore) DisabledTaskNames(context.Context) (map[string]bool, error) {
	return f.disabled, nil
}
func (f *fakeSystemStore) WorkerHeartbeat(context.Context) (*time.Time, error) { return nil, nil }
func (f *fakeSystemStore) QueueCounts(context.Context) (int64, int64, int64, error) {
	return 0, 0, 0, nil
}
func (f *fakeSystemStore) FailedJobsRecent(context.Context, int64) ([]SystemFailedJob, error) {
	return nil, nil
}

// eventCommands 把 taskStatuses 返回的 events 里的 command 抽出来。
func eventCommands(t *testing.T, obj *jsObj) []string {
	t.Helper()
	var eventsVal any
	found := false
	keys := obj.Keys()
	vals := obj.Vals()
	for i, k := range keys {
		if k == "events" {
			eventsVal = vals[i]
			found = true
			break
		}
	}
	if !found {
		t.Fatal("taskStatuses 返回里没有 events")
	}
	arr, ok := eventsVal.(jsArr)
	if !ok {
		t.Fatalf("events 不是数组：%T", eventsVal)
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		o, ok := it.(*jsObj)
		if !ok {
			continue
		}
		keys := o.Keys()
		vals := o.Vals()
		for i, k := range keys {
			if k == "command" {
				if s, ok := vals[i].(string); ok {
					out = append(out, s)
				}
				break
			}
		}
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestTaskStatuses_FiltersDisabledAndCondition(t *testing.T) {
	schedule := []ScheduledEvent{
		{Command: "traffic-sync", Expression: "*/5 * * * *"},       // 无条件，永远显示
		{Command: "db-backup", Expression: "30 3 * * *"},           // 下面设为被开关关闭
		{Command: "xray-warp-rotate", Expression: "*/5 * * * *"},   // 下面设为条件不满足
		{Command: "mail-schedule-scan", Expression: "*/5 * * * *"}, // 下面设为条件满足
	}

	d := &SystemDomain{
		store:    &fakeSystemStore{disabled: map[string]bool{"db-backup": true}},
		schedule: schedule,
		now:      func() time.Time { return time.Unix(0, 0).UTC() },
		logger:   log.Default(),
		condition: func(_ context.Context, name string) (bool, error) {
			switch name {
			case "xray-warp-rotate":
				return false, nil // 没开定时换 IP → 不满足
			case "mail-schedule-scan":
				return true, nil // 开了定时邮件 → 满足
			default:
				return true, nil
			}
		},
	}

	obj, err := d.taskStatuses(context.Background())
	if err != nil {
		t.Fatalf("taskStatuses 出错: %v", err)
	}
	got := eventCommands(t, obj)

	if !contains(got, "traffic-sync") {
		t.Errorf("无条件任务 traffic-sync 应出现，实际 %v", got)
	}
	if contains(got, "db-backup") {
		t.Errorf("被开关关闭的 db-backup 不应出现，实际 %v", got)
	}
	if contains(got, "xray-warp-rotate") {
		t.Errorf("条件不满足的 xray-warp-rotate 不应出现，实际 %v", got)
	}
	if !contains(got, "mail-schedule-scan") {
		t.Errorf("条件满足的 mail-schedule-scan 应出现，实际 %v", got)
	}
}

// TestTaskStatuses_NoConditionKeepsAll：未注入 condition 时不做条件过滤（只按开关过滤）。
func TestTaskStatuses_NoConditionKeepsAll(t *testing.T) {
	schedule := []ScheduledEvent{
		{Command: "xray-warp-rotate", Expression: "*/5 * * * *"},
	}
	d := &SystemDomain{
		store:    &fakeSystemStore{disabled: map[string]bool{}},
		schedule: schedule,
		now:      func() time.Time { return time.Unix(0, 0).UTC() },
		logger:   log.Default(),
		// condition 为 nil
	}
	obj, err := d.taskStatuses(context.Background())
	if err != nil {
		t.Fatalf("taskStatuses 出错: %v", err)
	}
	if got := eventCommands(t, obj); !contains(got, "xray-warp-rotate") {
		t.Errorf("无 condition 时不应隐藏任务，实际 %v", got)
	}
}

// 防呆：确保上面用到的 jsObj.get 存在（编译期即可发现 API 变化）。
