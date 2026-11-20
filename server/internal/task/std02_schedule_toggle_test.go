package task

// std02_schedule_toggle_test.go 覆盖调度层与开关（规范化-02 场景 1/2/8）：
//
//	1. 开关读取：键不存在 → 开启；0/false/off/no → 关闭；读库报错 → 按开启处理；
//	2. tick 遇关闭开关：任务不跑、**不写运行记录**；
//	3. when 条件不满足 / 报错：整条跳过（不是报错、不写记录）；
//	4. withoutOverlapping：上一轮没跑完跳过本轮，跑完自动放开；
//	5. 运行记录：成功后 finished、失败后 failed + error 文案 + duration_ms=0，每命令只留 5 条；
//	6. ban-check 跑完写 worker 心跳（系统状态页据此显示 worker 活着）。
//
// 全部打真库（生产 schema）：要验证的正是 site_configs / scheduled_task_runs / cache 的 SQL 语义。

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// std02Scheduler 造一个时钟可控、日志收进切片的调度器。
func std02Scheduler(t *testing.T, db *sql.DB, now time.Time, toggle ToggleFunc) (*Scheduler, *[]string) {
	t.Helper()
	var logs []string
	s := NewScheduler(Options{
		Now:    func() time.Time { return now },
		DB:     db,
		Toggle: toggle,
		Logf: func(format string, args ...any) {
			logs = append(logs, format)
		},
	})
	return s, &logs
}

// ─────────── 1. 开关读取（真库） ───────────

// TestStd02ToggleReadsSiteConfigs：缺省开 / 关值识别 / 读库报错按开启处理。
func TestStd02ToggleReadsSiteConfigs(t *testing.T) {
	db := newStd02TaskDB(t, "toggle")
	ctx := context.Background()
	toggle := NewSQLToggle(db)

	// 键不存在 = 开启（老库不用迁移）。
	if on, err := toggle(ctx, NameDBBackup); err != nil || !on {
		t.Fatalf("未配置时应为开启: on=%v err=%v", on, err)
	}

	for _, off := range []string{"0", "false", "FALSE", " off ", "no"} {
		mustExecTask(t, db,
			"INSERT INTO site_configs (`key`, `value`, created_at, updated_at) VALUES (?, ?, NOW(), NOW()) "+
				"ON DUPLICATE KEY UPDATE `value` = VALUES(`value`)",
			TaskToggleKey(NameDBBackup), off)
		if on, err := toggle(ctx, NameDBBackup); err != nil || on {
			t.Errorf("值 %q 应判为关闭: on=%v err=%v", off, on, err)
		}
	}

	for _, onVal := range []string{"1", "true", "yes", "anything", ""} {
		mustExecTask(t, db,
			"INSERT INTO site_configs (`key`, `value`, created_at, updated_at) VALUES (?, ?, NOW(), NOW()) "+
				"ON DUPLICATE KEY UPDATE `value` = VALUES(`value`)",
			TaskToggleKey(NameDBBackup), onVal)
		if on, err := toggle(ctx, NameDBBackup); err != nil || !on {
			t.Errorf("值 %q 应判为开启: on=%v err=%v", onVal, on, err)
		}
	}

	// 读库报错（表没了）→ 按开启处理 + 返回错误（不阻断任务）。
	mustExecTask(t, db, "DROP TABLE site_configs")
	on, err := toggle(ctx, NameDBBackup)
	if err == nil {
		t.Error("表不存在时必须返回错误（便于日志留痕）")
	}
	if !on {
		t.Error("读开关报错时必须按「开启」处理，否则一次 DB 抖动会把所有定时任务静默停掉")
	}
}

// ─────────── 2/3. tick：开关、条件 ───────────

// TestStd02TickSkipsWhenToggleOff：关闭的调度项整条不跑、不写运行记录。
func TestStd02TickSkipsWhenToggleOff(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	db := newStd02TaskDB(t, "tick_off")
	var ran int64
	toggle := func(context.Context, string) (bool, error) { return false, nil }
	s, _ := std02Scheduler(t, db, now, toggle)

	s.tick(context.Background(), Job{
		Name: NameTrafficSync,
		Run:  func(context.Context) error { atomic.AddInt64(&ran, 1); return nil },
	})

	if ran != 0 {
		t.Fatal("开关关闭时任务不许跑")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM scheduled_task_runs"); n != 0 {
		t.Fatalf("开关关闭时不许写运行记录，实际 %d 条", n)
	}
}

// TestStd02TickToggleErrorStillRuns：开关查询报错时按开启处理（跑 + 记一条）。
func TestStd02TickToggleErrorStillRuns(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	db := newStd02TaskDB(t, "tick_toggle_err")
	var ran int64
	toggle := func(context.Context, string) (bool, error) { return true, context.DeadlineExceeded }
	s, logs := std02Scheduler(t, db, now, toggle)

	s.tick(context.Background(), Job{
		Name: NameTrafficSync,
		Run:  func(context.Context) error { atomic.AddInt64(&ran, 1); return nil },
	})

	if ran != 1 {
		t.Fatal("开关读取失败时必须照常执行（不能因一次抖动停掉所有任务）")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM scheduled_task_runs"); n != 1 {
		t.Fatalf("应写 1 条运行记录，实际 %d", n)
	}
	if len(*logs) == 0 {
		t.Error("开关读取失败必须留日志")
	}
}

// TestStd02TickWhenGatesAndErrors：when=false 跳过；when 报错也跳过（同向 PHP）。
func TestStd02TickWhenGatesAndErrors(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	db := newStd02TaskDB(t, "tick_when")
	s, _ := std02Scheduler(t, db, now, nil)

	var ran int64
	job := Job{
		Name: NameMailNotifyScan,
		Run:  func(context.Context) error { atomic.AddInt64(&ran, 1); return nil },
	}

	job.When = func(context.Context) (bool, error) { return false, nil }
	s.tick(context.Background(), job)
	if ran != 0 {
		t.Error("条件不满足时任务不许跑（条件不满足 ≠ 报错）")
	}

	job.When = func(context.Context) (bool, error) { return true, context.Canceled }
	s.tick(context.Background(), job)
	if ran != 0 {
		t.Error("条件查询报错时应跳过（与 PHP 条件抛错不跑同向）")
	}

	job.When = func(context.Context) (bool, error) { return true, nil }
	s.tick(context.Background(), job)
	if ran != 1 {
		t.Error("条件满足时应执行")
	}

	if n := countRows(t, db, "SELECT COUNT(*) FROM scheduled_task_runs"); n != 1 {
		t.Fatalf("只有真正执行的那次该写记录，实际 %d 条", n)
	}
}

// ─────────── 4. withoutOverlapping ───────────

// TestStd02TickWithoutOverlapping：上一轮没跑完跳过本轮，跑完自动放开。
func TestStd02TickWithoutOverlapping(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	db := newStd02TaskDB(t, "overlap")
	s, _ := std02Scheduler(t, db, now, nil)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int64
	job := Job{
		Name:           NameTrafficSync,
		WithoutOverlap: true,
		Run: func(context.Context) error {
			atomic.AddInt64(&calls, 1)
			close(started)
			<-release
			return nil
		},
	}

	done := make(chan struct{})
	go func() { s.tick(context.Background(), job); close(done) }()
	<-started

	// 第二轮必须被跳过（此时第一轮还没结束）。
	s.tick(context.Background(), job)
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("上一轮未结束时第二轮也跑了（calls=%d）", got)
	}

	close(release)
	<-done
	// 跑完之后锁要自动放开。
	s.tick(context.Background(), Job{
		Name:           NameTrafficSync,
		WithoutOverlap: true,
		Run:            func(context.Context) error { atomic.AddInt64(&calls, 1); return nil },
	})
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("上一轮结束后应能再跑（calls=%d，期望 2）", got)
	}
}

// ─────────── 5. 运行记录 ───────────

// TestStd02RunRecordsSuccessAndFailure：成功 finished / 失败 failed + error 文案 + duration_ms=0。
func TestStd02RunRecordsSuccessAndFailure(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	db := newStd02TaskDB(t, "record")
	s, _ := std02Scheduler(t, db, now, nil)
	ctx := context.Background()

	s.tick(ctx, Job{Name: "ok-task", Run: func(context.Context) error { return nil }})
	s.tick(ctx, Job{Name: "bad-task", Run: func(context.Context) error {
		return errStd02("面板 502（超长错误会被截到 1000 字符）")
	}})

	var status string
	var errText sql.NullString
	var dur int64
	if err := db.QueryRow("SELECT status, error, duration_ms FROM scheduled_task_runs WHERE command = 'ok-task'").Scan(&status, &errText, &dur); err != nil {
		t.Fatal(err)
	}
	if status != "finished" || errText.Valid {
		t.Errorf("成功记录应为 finished 且 error=NULL，实际 %s/%v", status, errText)
	}

	if err := db.QueryRow("SELECT status, error, duration_ms FROM scheduled_task_runs WHERE command = 'bad-task'").Scan(&status, &errText, &dur); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("失败记录状态 = %s，期望 failed", status)
	}
	if !errText.Valid || !strings.Contains(errText.String, "502") {
		t.Errorf("失败记录应带错误文案，实际 %v", errText)
	}
	if dur != 0 {
		t.Errorf("失败记录 duration_ms 应为 0（PHP 同口径），实际 %d", dur)
	}
}

// TestStd02RunRecordsKeepLatestFive：每个命令只保留最近 5 条（PHP KEEP=5）。
func TestStd02RunRecordsKeepLatestFive(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	db := newStd02TaskDB(t, "record_keep")
	s, _ := std02Scheduler(t, db, now, nil)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		s.tick(ctx, Job{Name: NameTrafficSync, Run: func(context.Context) error { return nil }})
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM scheduled_task_runs WHERE command = ?", NameTrafficSync); n != 5 {
		t.Fatalf("运行记录 = %d 条，期望只留 5 条", n)
	}
	// 别的命令不受影响（每命令独立计数）。
	s.tick(ctx, Job{Name: NameDBBackup, Run: func(context.Context) error { return nil }})
	if n := countRows(t, db, "SELECT COUNT(*) FROM scheduled_task_runs WHERE command = ?", NameDBBackup); n != 1 {
		t.Fatalf("另一个命令的记录数 = %d，期望 1", n)
	}
}

// TestStd02BanCheckWritesHeartbeat：ban-check 跑完必须写 worker 心跳（20 分钟 TTL）。
func TestStd02BanCheckWritesHeartbeat(t *testing.T) {
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	db := newStd02TaskDB(t, "heartbeat")
	s, _ := std02Scheduler(t, db, now, nil)

	s.tick(context.Background(), Job{Name: NameBanCheck, Run: func(context.Context) error { return nil }})

	var val string
	var exp int64
	if err := db.QueryRow("SELECT `value`, expiration FROM cache WHERE `key` = 'controlhub:worker-heartbeat'").Scan(&val, &exp); err != nil {
		t.Fatalf("没有写 worker 心跳: %v", err)
	}
	if val != "2026-11-17 03:00:00" {
		t.Errorf("心跳值 = %q，应为 Y-m-d H:i:s（PHP toDateTimeString 口径）", val)
	}
	if want := now.Add(20 * time.Minute).Unix(); exp != want {
		t.Errorf("心跳过期时间 = %d，期望 %d（20 分钟 TTL）", exp, want)
	}

	// 第二次必须覆盖而不是报重复键。
	s.tick(context.Background(), Job{Name: NameBanCheck, Run: func(context.Context) error { return nil }})
	if n := countRows(t, db, "SELECT COUNT(*) FROM cache WHERE `key` = 'controlhub:worker-heartbeat'"); n != 1 {
		t.Fatalf("心跳行数 = %d，期望 1（ON DUPLICATE KEY UPDATE）", n)
	}
}

// TestStd02RegisterValidation：登记校验（缺 name / 缺 run / cron 非法 / 名字重复允许覆盖）。
func TestStd02RegisterValidation(t *testing.T) {
	s := NewScheduler(Options{Logf: func(string, ...any) {}})
	if err := s.Register(Job{Run: func(context.Context) error { return nil }}); err == nil {
		t.Error("缺 name 必须报错（withoutOverlapping 的键不能为空）")
	}
	if err := s.Register(Job{Name: "x"}); err == nil {
		t.Error("缺 run 必须报错")
	}
	if err := s.Register(Job{Name: "x", Spec: "not-a-cron", Run: func(context.Context) error { return nil }}); err == nil {
		t.Error("非法 cron 必须报错")
	}
	if err := s.Register(Job{Name: "x", Spec: SpecDaily0000, Run: func(context.Context) error { return nil }}); err != nil {
		t.Fatalf("合法登记失败: %v", err)
	}
	if got := len(s.Entries()); got != 1 {
		t.Fatalf("已登记 %d 条，期望 1", got)
	}
}

// TestStd02SQLLookups：五条 when 条件（系统状态页与调度同源）。
func TestStd02SQLLookups(t *testing.T) {
	db := newStd02TaskDB(t, "lookups")
	ctx := context.Background()
	l := &SQLLookups{DB: db}

	// 全关 → 都不跑。
	if on, err := l.MailNotifyAnyEnabled(ctx); err != nil || on {
		t.Errorf("通知开关全关时应为 false: %v/%v", on, err)
	}
	if on, err := l.MailScheduleEnabled(ctx); err != nil || on {
		t.Errorf("定时发信开关缺失时应为 false: %v/%v", on, err)
	}
	if on, err := l.ThirdPartyFetchEnabled(ctx); err != nil || on {
		t.Errorf("第三方拉取未开时应为 false: %v/%v", on, err)
	}
	if on, err := l.WarpRotateEnabled(ctx); err != nil || on {
		t.Errorf("没有 WARP 账户时应为 false: %v/%v", on, err)
	}
	if on, err := l.OutboundSubsEnabled(ctx); err != nil || on {
		t.Errorf("没有出站订阅时应为 false: %v/%v", on, err)
	}

	// 开一个通知开关（traffic_almost）→ true；只开 traffic_exhausted → 仍 false（不在扫描范围）。
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('notify_traffic_exhausted_enabled', '1')")
	if on, _ := l.MailNotifyAnyEnabled(ctx); on {
		t.Error("只开「流量用尽」不该让扫描任务跑（它不扫这个场景）")
	}
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('notify_traffic_almost_enabled', '1')")
	if on, err := l.MailNotifyAnyEnabled(ctx); err != nil || !on {
		t.Errorf("开了「即将用尽」应为 true: %v/%v", on, err)
	}

	// 严格字符串比较：'true' 不算开。
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('schedule_mail_enabled', 'true')")
	if on, _ := l.MailScheduleEnabled(ctx); on {
		t.Error("schedule_mail_enabled 必须 === '1' 才算开")
	}

	// 第三方：总开关 + 至少一条启用的订阅。
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('third_party_enabled', '1')")
	if on, _ := l.ThirdPartyFetchEnabled(ctx); on {
		t.Error("没有启用的第三方订阅时应为 false")
	}
	mustExecTask(t, db, "INSERT INTO third_party_subs (name, url, enabled, created_at, updated_at) VALUES ('sub', 'https://example.com/a', 1, NOW(), NOW())")
	if on, err := l.ThirdPartyFetchEnabled(ctx); err != nil || !on {
		t.Errorf("总开关开 + 有启用订阅应为 true: %v/%v", on, err)
	}

	// WARP：账户启用 + 间隔>0 + 节点本身启用。
	mustExecTask(t, db, `INSERT INTO nodes (id, name, host, port, enabled, status, driver_type)
		VALUES (900, 'n900', '10.0.0.9', 443, 1, 'online', 'xray')`)
	mustExecTask(t, db, "INSERT INTO node_warp_accounts (node_id, enabled, auto_rotate_hours, created_at, updated_at) VALUES (900, 1, 0, NOW(), NOW())")
	if on, _ := l.WarpRotateEnabled(ctx); on {
		t.Error("auto_rotate_hours=0 时应为 false")
	}
	mustExecTask(t, db, "UPDATE node_warp_accounts SET auto_rotate_hours = 6 WHERE node_id = 900")
	if on, err := l.WarpRotateEnabled(ctx); err != nil || !on {
		t.Errorf("账户启用 + 间隔 6 小时 + 节点启用应为 true: %v/%v", on, err)
	}
	mustExecTask(t, db, "UPDATE nodes SET enabled = 0 WHERE id = 900")
	if on, _ := l.WarpRotateEnabled(ctx); on {
		t.Error("节点被禁用时为 false（判据必须与 runXrayWarpRotate 一致）")
	}
}

// TestStd02ConditionCheckerWithSQLLookups：条件判断与库联动（缺省不隐藏、条件不满足才隐藏）。
func TestStd02ConditionCheckerWithSQLLookups(t *testing.T) {
	db := newStd02TaskDB(t, "condition_sql")
	ctx := context.Background()
	chk := ConditionChecker(&SQLLookups{DB: db})

	for _, name := range []string{NameTrafficSync, NameBanCheck, NameDataRetention} {
		ok, err := chk(ctx, name)
		if err != nil || !ok {
			t.Errorf("%s 无条件任务必须恒 true（不能被状态页隐藏），实际 %v/%v", name, ok, err)
		}
	}
	if ok, err := chk(ctx, NameMailNotifyScan); err != nil || ok {
		t.Errorf("通知全关时 mail-notify-scan 应判不跑（隐藏），实际 %v/%v", ok, err)
	}
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('notify_expired_enabled', '1')")
	if ok, err := chk(ctx, NameMailNotifyScan); err != nil || !ok {
		t.Errorf("开了任一通知场景后应为 true，实际 %v/%v", ok, err)
	}
}

// errStd02 是最小错误类型（测试里只想给一段文案）。
type errStd02 string

func (e errStd02) Error() string { return string(e) }
