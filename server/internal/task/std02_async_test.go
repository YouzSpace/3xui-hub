package task

// std02_async_test.go 是异步任务体系的状态机回归（真库，生产 schema）。
//
// 这些口径一旦破了，后台「异步任务」页会永远「执行中」或者把终态任务复活：
//   - 终态粘性：已 succeeded 的子项 / 已终态的任务不许被迟到的单元改写；
//   - 聚合：全部子项收尾才判终态，有失败 → failed（error 用类型文案）；
//   - 超时清扫：任务行与子项两条判据取并集，队列在途的子项豁免；
//   - 重试：只允许失败任务重试，失败子项回到 pending。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// newStd02Service 造一个带可控时钟的服务 + 空库。
func newStd02Service(t *testing.T, caseName string, now time.Time) (*Service, *sql.DB) {
	t.Helper()
	db := newStd02TaskDB(t, caseName)
	svc := NewService(db, nil)
	svc.Now = func() time.Time { return now }
	return svc, db
}

// createTask 建一条任务并返回 id（用例里几乎每条都要）。
func createTask(t *testing.T, svc *Service, typ string, keys []string, maxAttempts int) *Task {
	t.Helper()
	task, err := svc.Create(context.Background(), CreateInput{
		Type:        typ,
		Total:       len(keys),
		ItemKeys:    keys,
		MaxAttempts: maxAttempts,
		Meta:        json.RawMessage(`{"user_id":7}`),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	return task
}

// ───────────── 1. 创建 / 子项 / 聚合 ─────────────

// TestStd02CreateAndAggregateComplete：全部子项成功 → 任务 succeeded、计数正确。
func TestStd02CreateAndAggregateComplete(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, db := newStd02Service(t, "aggregate_ok", now)
	ctx := context.Background()

	task := createTask(t, svc, TypeTrafficSync, []string{"node:1", "node:2", "node:3"}, 3)
	if task.Total != 3 {
		t.Fatalf("total = %d，期望 3", task.Total)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM async_task_items WHERE task_id = ?", task.ID); got != 3 {
		t.Fatalf("子项行数 = %d，期望 3", got)
	}

	for _, key := range []string{"node:1", "node:2", "node:3"} {
		ok, err := svc.ClaimItem(ctx, task.ID, key)
		if err != nil || !ok {
			t.Fatalf("认领 %s 失败: ok=%v err=%v", key, ok, err)
		}
		if _, err := svc.CompleteItem(ctx, task.ID, key); err != nil {
			t.Fatalf("完成 %s 失败: %v", key, err)
		}
	}

	got, err := svc.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded {
		t.Fatalf("全部子项完成后任务状态 = %s，期望 succeeded", got.Status)
	}
	if got.Completed != 3 || got.Failed != 0 || got.Total != 3 {
		t.Fatalf("聚合计数 total/completed/failed = %d/%d/%d，期望 3/3/0", got.Total, got.Completed, got.Failed)
	}
	if got.FinishedAt == nil {
		t.Fatal("终态任务必须有 finished_at")
	}
}

// TestStd02AggregatePartialFailure：有一个子项失败 → 任务 failed，且 error 用类型文案。
func TestStd02AggregatePartialFailure(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, _ := newStd02Service(t, "aggregate_fail", now)
	ctx := context.Background()

	task := createTask(t, svc, TypeTrafficSync, []string{"node:1", "node:2"}, 2)
	if _, err := svc.ClaimItem(ctx, task.ID, "node:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteItem(ctx, task.ID, "node:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClaimItem(ctx, task.ID, "node:2"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FailItem(ctx, task.ID, "node:2", "节点已删除，跳过同步"); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("有失败子项时任务状态 = %s，期望 failed", got.Status)
	}
	if got.Failed != 1 || got.Completed != 1 {
		t.Fatalf("completed/failed = %d/%d，期望 1/1", got.Completed, got.Failed)
	}
	if got.Error == nil || *got.Error == "" {
		t.Fatal("失败任务必须有 error 文案（否则后台只显示红色没有原因）")
	}
}

// TestStd02TerminalSticky：终态任务 / 已成功子项不许被迟到的单元复活。
//
// 场景：队列单元超时被放弃，但 handler 之后才返回并调 CompleteItem ——
// 如果不是终态粘性，用户会看到任务在失败后跳回成功。
func TestStd02TerminalSticky(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, _ := newStd02Service(t, "sticky", now)
	ctx := context.Background()

	task := createTask(t, svc, TypeTrafficSync, []string{"node:1"}, 1)
	if _, err := svc.ClaimItem(ctx, task.ID, "node:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteItem(ctx, task.ID, "node:1"); err != nil {
		t.Fatal(err)
	}

	// 已 succeeded 的子项不许再认领（重复执行 = 重复计费）。
	ok, err := svc.ClaimItem(ctx, task.ID, "node:1")
	if err != nil {
		t.Fatalf("认领已完成子项不该报错: %v", err)
	}
	if ok {
		t.Fatal("已 succeeded 的子项被再次认领（终态粘性破了）")
	}
	// 终态任务的迟到失败不许改写状态。
	got, err := svc.FailItem(ctx, task.ID, "node:1", "迟到的失败")
	if err != nil {
		t.Fatalf("FailItem: %v", err)
	}
	if got.Status != StatusSucceeded {
		t.Fatalf("终态任务被失败回调改成了 %s（必须是粘性 succeeded）", got.Status)
	}
}

// TestStd02ClaimDecisionMatrix：认领判定的纯函数矩阵（上限 / 终态 / 已完成）。
func TestStd02ClaimDecisionMatrix(t *testing.T) {
	cases := []struct {
		name        string
		taskStatus  string
		itemStatus  string
		attempts    int
		maxAttempts int
		wantClaim   bool
		wantErr     bool
	}{
		{"正常认领", StatusPending, StatusPending, 0, 3, true, false},
		{"子项已完成", StatusRunning, StatusSucceeded, 0, 3, false, false},
		{"任务成功", StatusSucceeded, StatusPending, 0, 3, false, false},
		{"任务失败", StatusFailed, StatusPending, 0, 3, false, false},
		{"达上限", StatusPending, StatusPending, 3, 3, false, true},
		{"running 复用不判上限", StatusRunning, StatusFailed, 5, 3, true, false},
	}
	for _, c := range cases {
		got, err := ClaimDecision(c.taskStatus, c.itemStatus, c.attempts, c.maxAttempts)
		if got != c.wantClaim {
			t.Errorf("%s: claim=%v，期望 %v", c.name, got, c.wantClaim)
		}
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v，期望报错=%v", c.name, err, c.wantErr)
		}
	}

	// 聚合判定：还有活跃子项就不判终态；失败优先于成功。
	if _, finish := AggregateDecision(StatusRunning, 1, 0, 1); finish {
		t.Error("还有 active 子项时不该判终态")
	}
	if st, finish := AggregateDecision(StatusRunning, 1, 1, 0); !finish || st != StatusFailed {
		t.Errorf("有失败应判 failed，得到 %s finish=%v", st, finish)
	}
	if st, finish := AggregateDecision(StatusRunning, 2, 0, 0); !finish || st != StatusSucceeded {
		t.Errorf("全成功应判 succeeded，得到 %s finish=%v", st, finish)
	}
	if _, finish := AggregateDecision(StatusSucceeded, 2, 0, 0); finish {
		t.Error("已经是终态的任务不该再判（终态粘性）")
	}
}

// TestStd02CompleteEmpty：0 个子项的任务直接判成功（不留在 pending）。
func TestStd02CompleteEmpty(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, _ := newStd02Service(t, "empty", now)
	ctx := context.Background()

	task := createTask(t, svc, TypeNodeInit, nil, 1)
	got, err := svc.CompleteEmpty(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || got.Total != 0 {
		t.Fatalf("空任务应 succeeded/total=0，实际 %s/%d", got.Status, got.Total)
	}
}

// TestStd02AddItemsAndNormalize：重复键去重、total 回填，空键按位置补 node:{i}。
func TestStd02AddItemsAndNormalize(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, db := newStd02Service(t, "add_items", now)
	ctx := context.Background()

	task := createTask(t, svc, TypeTrafficSync, []string{"node:1"}, 3)

	// ⚠️ 已核实的生产代码 bug（交付报告 待拍板 #3）：
	// AddItems 的 UPDATE 语句只有 2 个占位符（total 的子查询 + updated_at），
	// 但传了 3 个参数（taskID, now, now）—— 第三个参数（时间）被绑到 id 上，
	// MySQL 报 1292 Truncated incorrect DOUBLE value，整个事务回滚。
	// 当前仓库里没有任何生产调用点，所以线上没炸；一旦有人接上「追加子项」就会必现。
	//
	// 这里锁现状：修好之后本用例会红，届时把断言改成「成功 + total=3」即可。
	err := svc.AddItems(ctx, task.ID, []string{"node:2", "node:2", "node:3"})
	if err == nil {
		t.Fatal("AddItems 当前应报错（SQL 参数个数与占位符不匹配，见代码注释）；若已修复请更新本用例")
	}
	if !strings.Contains(err.Error(), "1292") && !strings.Contains(err.Error(), "DOUBLE") {
		t.Logf("AddItems 报错文案变了（%v），但确实是错误路径", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM async_task_items WHERE task_id = ?", task.ID); got != 1 {
		t.Fatalf("AddItems 失败后子项数应保持 1，实际 %d（事务没回滚）", got)
	}

	// 去重归一本身是对的（Create 走的是同一条 normalizeItemKeys）。
	if keys := normalizeItemKeys([]string{"a", "a", "b"}, 0); len(keys) != 2 {
		t.Errorf("去重保序失败: %v", keys)
	}
	if keys := normalizeItemKeys(nil, 2); len(keys) != 2 || keys[0] != "item:1" || keys[1] != "item:2" {
		t.Errorf("未给 key 时应补 item:1..N，得到 %v", keys)
	}
	if keys := normalizeItemKeys(nil, 0); len(keys) != 0 {
		t.Errorf("空输入应返回空切片，得到 %v", keys)
	}
}

// TestStd02TimeoutStaleSweeps：超阈值未终态的任务与僵尸子项都被收尾，在途子项豁免。
func TestStd02TimeoutStaleSweeps(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, db := newStd02Service(t, "stale", now)
	ctx := context.Background()

	// 任务 A：整条任务很久没动（任务行判据）。
	taskA := createTask(t, svc, TypeTrafficSync, []string{"node:1"}, 1)
	mustExecTask(t, db, "UPDATE async_tasks SET status = ?, updated_at = ? WHERE id = ?",
		StatusRunning, now.Add(-30*time.Minute), taskA.ID)

	// 任务 B：任务行很新，但子项僵尸（子项级判据 —— 2026-10-31 的盲点）。
	taskB := createTask(t, svc, TypeTrafficSync, []string{"node:2"}, 1)
	if _, err := svc.ClaimItem(ctx, taskB.ID, "node:2"); err != nil {
		t.Fatal(err)
	}
	mustExecTask(t, db, "UPDATE async_tasks SET updated_at = ? WHERE id = ?", now, taskB.ID)
	mustExecTask(t, db, "UPDATE async_task_items SET updated_at = ? WHERE task_id = ?",
		now.Add(-30*time.Minute), taskB.ID)

	// 任务 C：子项僵尸，但队列说它还在管（等退避）→ 必须豁免。
	taskC := createTask(t, svc, TypeTrafficSync, []string{"node:3"}, 1)
	if _, err := svc.ClaimItem(ctx, taskC.ID, "node:3"); err != nil {
		t.Fatal(err)
	}
	mustExecTask(t, db, "UPDATE async_tasks SET updated_at = ? WHERE id = ?", now, taskC.ID)
	mustExecTask(t, db, "UPDATE async_task_items SET updated_at = ? WHERE task_id = ?",
		now.Add(-30*time.Minute), taskC.ID)
	q := NewQueue(nil, QueueOptions{Workers: 1, Retries: -1, Backoff: []time.Duration{time.Millisecond}, Sleep: func(time.Duration) {}, Logf: func(string, ...any) {}})
	q.markInflight(inflightKey(taskC.ID, "node:3"), 1)
	svc.Queue = q

	n, err := svc.TimeoutStale(ctx, 10)
	if err != nil {
		t.Fatalf("TimeoutStale: %v", err)
	}
	if n != 2 {
		t.Fatalf("清扫任务数 = %d，期望 2（A 与 B；C 在途豁免）", n)
	}
	for _, id := range []int64{taskA.ID, taskB.ID} {
		got, err := svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusFailed {
			t.Errorf("任务 %d 应被清扫为 failed，实际 %s", id, got.Status)
		}
	}
	got, err := svc.Get(ctx, taskC.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == StatusFailed {
		t.Errorf("在途任务被误清成 failed（队列还在等退避，不许动）")
	}
}

// TestStd02RetryResetsFailedItems：只有 failed 任务能重试，失败项回到 pending。
func TestStd02RetryResetsFailedItems(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, db := newStd02Service(t, "retry", now)
	ctx := context.Background()

	task := createTask(t, svc, TypeTrafficSync, []string{"node:1"}, 2)
	if _, err := svc.ClaimItem(ctx, task.ID, "node:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FailItem(ctx, task.ID, "node:1", "面板 502"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Retry(ctx, task.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	var status string
	if err := db.QueryRow("SELECT status FROM async_task_items WHERE task_id = ?", task.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusPending {
		t.Fatalf("重试后子项状态 = %s，期望 pending", status)
	}

	// 成功任务不许重试（PHP 抛异常，Go 侧返回错误）。
	okTask := createTask(t, svc, TypeTrafficSync, []string{"node:9"}, 1)
	if _, err := svc.ClaimItem(ctx, okTask.ID, "node:9"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteItem(ctx, okTask.ID, "node:9"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Retry(ctx, okTask.ID); err == nil {
		t.Fatal("成功任务不应允许重试")
	}
}

// TestStd02DispatchEnqueuesAndPersists：Dispatch 必须落库 + 交给队列。
func TestStd02DispatchEnqueuesAndPersists(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, _ := newStd02Service(t, "dispatch", now)
	ctx := context.Background()

	rec := &recordingDispatcher{}
	svc.Queue = rec
	task := createTask(t, svc, TypeTrafficSync, []string{"node:1", "node:2"}, 2)
	if err := svc.Dispatch(ctx, task, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(rec.jobs) != 2 {
		t.Fatalf("派发了 %d 个单元，期望 2", len(rec.jobs))
	}
	for _, job := range rec.jobs {
		if job.Kind != JobSyncNodeTraffic || job.TaskID != task.ID || job.NodeID == 0 {
			t.Errorf("派发单元不对: %+v", job)
		}
	}
}

// recordingDispatcher 记录派发出去的单元。
type recordingDispatcher struct{ jobs []JobPayload }

func (r *recordingDispatcher) Enqueue(job JobPayload) { r.jobs = append(r.jobs, job) }

// TestStd02UserItemBatches：用户批处理切片（node_init 批量下发）。
func TestStd02UserItemBatches(t *testing.T) {
	keys := []string{"user:1", "user:2", "user:3", "user:4", "user:5"}
	batches := UserItemBatches(keys, 2)
	if len(batches) != 3 {
		t.Fatalf("批次数 = %d，期望 3", len(batches))
	}
	total := 0
	for i, b := range batches {
		if i < 2 && len(b.ItemKeys) != 2 {
			t.Errorf("第 %d 批大小 = %d，期望 2", i, len(b.ItemKeys))
		}
		for j, id := range b.UserIDs {
			if id != int64(i*2+j+1) {
				t.Errorf("第 %d 批第 %d 个用户 id = %d", i, j, id)
			}
		}
		total += len(b.ItemKeys)
	}
	if total != len(keys) {
		t.Fatalf("批次覆盖 %d 个键，期望 %d", total, len(keys))
	}
	if got := UserItemBatches(nil, 3); len(got) != 0 {
		t.Errorf("空输入应返回空批次，得到 %v", got)
	}
}

// TestStd02MetaAccessorsAndErrors：meta 读取与错误文案口径。
func TestStd02MetaAccessorsAndErrors(t *testing.T) {
	task := Task{Type: TypeTrafficSync, Meta: json.RawMessage(`{"user_id":12,"node_id":"34","note":"x"}`)}
	if v := task.MetaInt("user_id"); v == nil || *v != 12 {
		t.Errorf("MetaInt(user_id) = %v", v)
	}
	if v := task.MetaInt("node_id"); v == nil || *v != 34 {
		t.Errorf("MetaInt 应兼容字符串数字，得到 %v", v)
	}
	if v := task.MetaInt("missing"); v != nil {
		t.Errorf("缺失键应返回 nil，得到 %v", v)
	}
	if v := task.MetaStr("note"); v == nil || *v != "x" {
		t.Errorf("MetaStr(note) = %v", v)
	}
	if m := (Task{}).MetaMap(); len(m) != 0 {
		t.Errorf("空 meta 应返回空 map，得到 %v", m)
	}
	if m := (Task{Meta: json.RawMessage("不是json")}).MetaMap(); len(m) != 0 {
		t.Errorf("坏 meta 应返回空 map（不 panic），得到 %v", m)
	}
	if s := SummaryFor(TypeTrafficSync); s == "" {
		t.Error("SummaryFor(traffic_sync) 不能为空（后台要显示失败原因）")
	}
	if s := SummaryFor("未定义类型"); s == "" {
		t.Error("SummaryFor 对未知类型也要给兜底文案")
	}
	task.Error = nil
	if PublicError(task) != nil {
		t.Error("无 error 的任务 PublicError 应为 nil")
	}
	msg := "boom"
	task.Error = &msg
	// 对外文案是「类型文案」而不是原始错误（原始错误可能带库名/堆栈，只给后台看）。
	if got := PublicError(task); got == nil || *got != SummaryFor(task.Type) {
		t.Errorf("PublicError = %v，期望类型文案 %q", got, SummaryFor(task.Type))
	}
	if IsInboundSync(&Task{}) {
		t.Error("空任务不该判为入站同步")
	}
}

// TestStd02CacheLock：cache_locks 互斥（跨进程锁，节点锁竞争的依据）。
func TestStd02CacheLock(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	svc, db := newStd02Service(t, "cache_lock", now)
	_ = svc
	ctx := context.Background()

	owner, ok, err := acquireCacheLock(ctx, db, "traffic-sync:node:1", 30, now)
	if err != nil || !ok {
		t.Fatalf("首次加锁失败: ok=%v err=%v", ok, err)
	}
	if owner == "" {
		t.Fatal("锁 owner 不能为空（释放要校验 owner）")
	}
	// 同一 key 第二次必须抢不到。
	if _, ok2, err := acquireCacheLock(ctx, db, "traffic-sync:node:1", 30, now); err != nil || ok2 {
		t.Fatalf("重复加锁应失败: ok=%v err=%v", ok2, err)
	}
	// 释放后可再次加锁。
	releaseCacheLock(ctx, db, "traffic-sync:node:1", owner)
	if _, ok3, err := acquireCacheLock(ctx, db, "traffic-sync:node:1", 30, now); err != nil || !ok3 {
		t.Fatalf("释放后应能重新加锁: ok=%v err=%v", ok3, err)
	}
	// 过期锁可以被抢占（PHP Cache::lock 同语义）。
	mustExecTask(t, db, "UPDATE cache_locks SET expiration = ? WHERE `key` LIKE ?", now.Add(-time.Minute).Unix(), "%traffic-sync:node:1%")
	if _, ok4, err := acquireCacheLock(ctx, db, "traffic-sync:node:1", 30, now); err != nil || !ok4 {
		t.Fatalf("过期锁应可抢占: ok=%v err=%v", ok4, err)
	}
}

// TestStd02SmallAsyncHelpers：纯工具（id 解析 / 去重 / 数值转换 / 终态判定）。
func TestStd02SmallAsyncHelpers(t *testing.T) {
	if isTerminal(StatusPending) || isTerminal(StatusRunning) {
		t.Error("pending/running 不是终态")
	}
	if !isTerminal(StatusSucceeded) || !isTerminal(StatusFailed) {
		t.Error("succeeded/failed 是终态")
	}
	// partial 不算终态：PHP in_array(status, [succeeded, failed])，partial 只是展示态。
	if isTerminal(StatusPartial) {
		t.Error("partial 不是终态")
	}
	if got := parseIDPrefix("node:42", "node:"); got != 42 {
		t.Errorf("parseIDPrefix = %d", got)
	}
	if got := parseIDPrefix("no-prefix", "node:"); got != 0 {
		t.Errorf("前缀不匹配应返回 0，得到 %d", got)
	}
	if got := uniqueInt64s([]int64{3, 1, 3, 2, 1}); len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 2 {
		t.Errorf("uniqueInt64s 保序去重失败: %v", got)
	}
	for in, want := range map[any]int64{1: 1, int64(2): 2, "3": 3, float64(4): 4, nil: 0, true: 1} {
		if got := toInt64(in); got != want {
			t.Errorf("toInt64(%#v) = %d，期望 %d", in, got, want)
		}
	}
	if got := envInt("STD02_NOT_SET_ENV", 7); got != 7 {
		t.Errorf("envInt 缺省 = %d，期望 7", got)
	}
	if err := errors.New("x"); err == nil {
		t.Fatal("unreachable")
	}
}
