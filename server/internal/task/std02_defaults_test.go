package task

// std02_defaults_test.go 覆盖调度登记表与小工具（规范化-02 场景 5/6 的接线层）。
//
// 关键口径（这些错了用户立刻感知，但平时没有任何用例盯着）：
//   - console.php 里的每一条定时任务都必须在 DefaultJobs 里登记，名字唯一、cron 合法；
//   - 带条件的任务在 Lookups=nil 时「按不跑处理」（页面隐藏 + 调度跳过同源）；
//   - 流量月重置 / 全量封禁扫描必须真的调到采集层对应方法（5/6 两个场景的接线验证）；
//   - 依赖缺失时给明确错误（collector 未配置 / ErrUnimplemented），不许假装成功。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"controlhub-server/internal/collector"
)

// ── 测试替身 ──────────────────────────────────────────────

// fakeCollector 记录每类定时任务被调用了几次。
type fakeCollector struct {
	mu        sync.Mutex
	traffic   int
	bans      int
	health    int
	reset     int
	lastReset time.Time
}

func (f *fakeCollector) RunTrafficSync(context.Context) (collector.TrafficSyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traffic++
	return collector.TrafficSyncResult{}, nil
}

func (f *fakeCollector) ScanBans(context.Context) (collector.BanScanResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bans++
	return collector.BanScanResult{}, nil
}

func (f *fakeCollector) CheckAllEnabled(context.Context) (collector.CheckManyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health++
	return collector.CheckManyResult{}, nil
}

func (f *fakeCollector) MonthlyReset(context.Context) (collector.MonthlyResetResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset++
	f.lastReset = time.Now()
	return collector.MonthlyResetResult{}, nil
}

// fakeCommands 记录被派发的 artisan 命令名。
type fakeCommands struct {
	mu   sync.Mutex
	seen []string
	args []map[string]string
	err  error
}

func (f *fakeCommands) Run(_ context.Context, command string, args map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, command)
	f.args = append(f.args, args)
	return f.err
}

func (f *fakeCommands) contains(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.seen {
		if s == name {
			return true
		}
	}
	return false
}

// fakeTasks 记录超时清扫调用。
type fakeTasks struct{ calls, minutes int }

func (f *fakeTasks) TimeoutStale(_ context.Context, minutes int) (int, error) {
	f.calls++
	f.minutes = minutes
	return 0, nil
}

func jobByName(t *testing.T, jobs []Job, name string) Job {
	t.Helper()
	for _, j := range jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("DefaultJobs 里没有调度项 %q", name)
	return Job{}
}

// TestStd02DefaultJobsRegistry：名字唯一、cron 可解析、全部带 withoutOverlapping。
func TestStd02DefaultJobsRegistry(t *testing.T) {
	jobs := DefaultJobs(Deps{})
	if len(jobs) < 19 {
		t.Fatalf("调度项只有 %d 条，代理商城维护任务下线后应 ≥19 条", len(jobs))
	}

	seen := map[string]bool{}
	for _, j := range jobs {
		if j.Name == "" {
			t.Fatal("存在没有 name 的调度项（withoutOverlapping 的键必须唯一）")
		}
		if seen[j.Name] {
			t.Fatalf("调度项 %q 重复登记", j.Name)
		}
		seen[j.Name] = true
		if _, err := cron.ParseStandard(j.Spec); err != nil {
			t.Fatalf("调度项 %s 的 cron 表达式 %q 非法: %v", j.Name, j.Spec, err)
		}
		if j.Run == nil {
			t.Fatalf("调度项 %s 没有 Run", j.Name)
		}
	}

	// 用户直接感知的几条必须在，并且档位与 console.php 对齐。
	want := map[string]string{
		NameTrafficSync:          SpecEveryFiveMinutes,
		NameBanCheck:             SpecEveryFiveMinutes,
		NameHealthCheck:          SpecEveryFiveMinutes,
		NameMonthlyResetTraffic:  SpecDaily0000,
		NameInviteCodeRefresh:    SpecDaily0010,
		NameDBBackup:             SpecDaily0330,
		NameMonitorRollup:        SpecDaily0430,
		NameDataRetention:        SpecDaily0400,
		NameExpirePendingOrders:  SpecHourly,
		NameMailNotifyScan:       SpecEveryFiveMinutes,
		NameMailScheduleScan:     SpecEveryFiveMinutes,
		NameThirdPartyFetch:      SpecEveryTwoHours,
		NameXrayWarpRotate:       SpecEveryFiveMinutes,
		NameXrayRefreshOutbound:  SpecEveryFiveMinutes,
		NameGeodataUpdate:        SpecWeeklyMon0445,
		NameUsersReconcileEnable: SpecEveryFiveMinutes,
		NameRealityAudit:         SpecDaily0410,
		NameXrayCertificateSync:  SpecEverySixHours,
		NameXrayCertificateRenew: SpecDaily0520,
	}
	for name, spec := range want {
		j := jobByName(t, jobs, name)
		if j.Spec != spec {
			t.Errorf("%s 的 cron = %q，期望 %q（与 console.php 逐条对齐）", name, j.Spec, spec)
		}
		if !j.WithoutOverlap {
			t.Errorf("%s 必须带 withoutOverlining（上一轮没跑完不能叠跑）", name)
		}
	}
}

// TestStd02DefaultJobsCollectorWiring：五条采集任务必须调到采集层对应方法。
//
// 场景 5（流量重置）与场景 6（到期/超量的封禁扫描）在这一层只验证「接线不接反」；
// 具体的 SQL 行为见 std02_reset_ban_test.go（真库跑 collector）。
func TestStd02DefaultJobsCollectorWiring(t *testing.T) {
	fc := &fakeCollector{}
	cmd := &fakeCommands{}
	tasks := &fakeTasks{}
	jobs := DefaultJobs(Deps{Collector: fc, Commands: cmd, Tasks: tasks, Logf: func(string, ...any) {}})
	ctx := context.Background()

	cases := []struct {
		job  string
		want func() int
		what string
	}{
		{NameTrafficSync, func() int { return fc.traffic }, "RunTrafficSync"},
		{NameBanCheck, func() int { return fc.bans }, "ScanBans"},
		{NameHealthCheck, func() int { return fc.health }, "CheckAllEnabled"},
		{NameMonthlyResetTraffic, func() int { return fc.reset }, "MonthlyReset"},
	}
	for _, c := range cases {
		before := c.want()
		if err := jobByName(t, jobs, c.job).Run(ctx); err != nil {
			t.Fatalf("%s 执行失败: %v", c.job, err)
		}
		if c.want() != before+1 {
			t.Errorf("%s 没有调用采集层的 %s（接线断了）", c.job, c.what)
		}
	}

	if err := jobByName(t, jobs, NameAsyncTaskTimeout).Run(ctx); err != nil {
		t.Fatalf("async-task-timeout 执行失败: %v", err)
	}
	if tasks.calls != 1 || tasks.minutes != defaultStaleAfterMinutes {
		t.Errorf("async-task-timeout 调用了 %d 次、阈值 %d 分钟，期望 1 次 / %d 分钟",
			tasks.calls, tasks.minutes, defaultStaleAfterMinutes)
	}

	// 命令类任务：命令名必须逐字一致（写错一个字符就是静默不跑）。
	for job, command := range map[string]string{
		NameInviteCodeRefresh:    "invite:refresh",
		NameDBBackup:             "backup:run",
		NameMonitorRollup:        "monitor:rollup",
		NameDataRetention:        "retention:purge",
		NameExpirePendingOrders:  "order:expire-pending",
		NameMailNotifyScan:       "mail:notify-scan",
		NameMailScheduleScan:     "mail:schedule-scan",
		NameThirdPartyFetch:      "third-party:fetch",
		NameXrayWarpRotate:       "xray:warp-rotate",
		NameXrayRefreshOutbound:  "xray:refresh-outbound-subs",
		NameUsersReconcileEnable: "users:reconcile-enable",
		NameRealityAudit:         "xray:reality-audit",
	} {
		if err := jobByName(t, jobs, job).Run(ctx); err != nil {
			t.Fatalf("%s 执行失败: %v", job, err)
		}
		if !cmd.contains(command) {
			t.Errorf("%s 没有派发命令 %q，实际派发了 %v", job, command, cmd.seen)
		}
	}

	// geodata 带 --dispatch 参数。
	if err := jobByName(t, jobs, NameGeodataUpdate).Run(ctx); err != nil {
		t.Fatalf("geodata-update 执行失败: %v", err)
	}
	if !cmd.contains("geodata:update") {
		t.Error("geodata-update 没有派发 geodata:update")
	}
	found := false
	for i, name := range cmd.seen {
		if name == "geodata:update" && cmd.args[i]["--dispatch"] == "1" {
			found = true
		}
	}
	if !found {
		t.Error("geodata-update 必须带 --dispatch=1 参数")
	}
}

// TestStd02DefaultJobsMissingDeps：依赖缺失时必须「显式失败/静默跳过」，不许假装成功。
func TestStd02DefaultJobsMissingDeps(t *testing.T) {
	jobs := DefaultJobs(Deps{})
	ctx := context.Background()

	for _, name := range []string{NameTrafficSync, NameBanCheck, NameHealthCheck, NameMonthlyResetTraffic} {
		err := jobByName(t, jobs, name).Run(ctx)
		if err == nil {
			t.Errorf("%s 在 collector 缺失时必须返回错误（否则用户以为在同步，其实什么都没做）", name)
			continue
		}
		if !strings.Contains(err.Error(), "collector") {
			t.Errorf("%s 的错误信息应点明 collector 未配置，实际 %q", name, err.Error())
		}
	}

	// 命令类任务在默认 runner 下必须返回 ErrUnimplemented（可见失败 > 假装成功）。
	for _, name := range []string{NameDBBackup, NameDataRetention, NameMailNotifyScan} {
		err := jobByName(t, jobs, name).Run(ctx)
		if !errors.Is(err, ErrUnimplemented) {
			t.Errorf("%s 未接命令实现时应返回 ErrUnimplemented，实际 %v", name, err)
		}
	}

	// 证书类任务没接实现时只跳过，不报错（避免每天刷失败记录）。
	for _, name := range []string{NameXrayCertificateSync, NameXrayCertificateRenew} {
		if err := jobByName(t, jobs, name).Run(ctx); err != nil {
			t.Errorf("%s 未接实现时应静默跳过，实际 %v", name, err)
		}
	}
}

// TestStd02DefaultJobsCertAndCondition：证书任务接通后必须执行并吞掉错误外的一切；
// 带条件的任务在 Lookups 缺失时按「不跑」处理（返回 false，不是报错）。
func TestStd02DefaultJobsCertAndCondition(t *testing.T) {
	var syncCalls, renewCalls int
	jobs := DefaultJobs(Deps{
		SyncLocalCerts: func(context.Context) (LocalCertSyncSummary, error) {
			syncCalls++
			return LocalCertSyncSummary{Dir: "/etc/acme", Scanned: 3, Created: 1, Updated: 1, Skipped: 1, Bumped: 2}, nil
		},
		RenewNodeCerts: func(context.Context) (CertRenewSummary, error) {
			renewCalls++
			return CertRenewSummary{Checked: 5, Due: 2, Renewed: 1, Failed: 1, Skipped: 3, Bumped: 1}, nil
		},
		Logf: func(string, ...any) {},
	})
	ctx := context.Background()
	if err := jobByName(t, jobs, NameXrayCertificateSync).Run(ctx); err != nil {
		t.Fatalf("证书同步失败: %v", err)
	}
	if err := jobByName(t, jobs, NameXrayCertificateRenew).Run(ctx); err != nil {
		t.Fatalf("证书续签失败: %v", err)
	}
	if syncCalls != 1 || renewCalls != 1 {
		t.Fatalf("证书任务应各调用一次，实际 sync=%d renew=%d", syncCalls, renewCalls)
	}

	for _, name := range []string{NameMailNotifyScan, NameMailScheduleScan, NameThirdPartyFetch, NameXrayWarpRotate, NameXrayRefreshOutbound} {
		j := jobByName(t, jobs, name)
		if j.When == nil {
			t.Errorf("%s 缺少 when 条件（四开关全关时必须整条跳过、不空转）", name)
			continue
		}
		ok, err := j.When(ctx)
		if err != nil {
			t.Errorf("%s 在 Lookups 缺失时应返回 (false,nil)，实际 err=%v", name, err)
		}
		if ok {
			t.Errorf("%s 在 Lookups 缺失时应判「不跑」，实际 true", name)
		}
	}
}

// TestStd02ConditionAndToggleDefaults：ConditionChecker 与开关的缺省口径。
func TestStd02ConditionAndToggleDefaults(t *testing.T) {
	chk := ConditionChecker(nil)
	for _, name := range []string{NameTrafficSync, NameDataRetention, "unknown-task"} {
		ok, err := chk(context.Background(), name)
		if err != nil || !ok {
			t.Errorf("%s 无条件任务应恒 true，实际 ok=%v err=%v", name, ok, err)
		}
	}
	if TaskToggleKey(NameDBBackup) != "sched_db-backup_enabled" {
		t.Errorf("开关键 = %q，键名规则被改了会影响系统状态页隐藏", TaskToggleKey(NameDBBackup))
	}
}

// TestStd02SmallHelpers：命令层的纯函数逐个验一遍（错一个就是线上文案错）。
func TestStd02SmallHelpers(t *testing.T) {
	if got := formatBytes(0); got != "0 B" {
		t.Errorf("formatBytes(0) = %q", got)
	}
	if got := formatBytes(1536); !strings.Contains(got, "KB") {
		t.Errorf("formatBytes(1536) = %q，应带 KB", got)
	}
	// 注意：task 包的 clientEmail 是 "user_" 前缀（订阅/采集层是 "ch_user_"）。
	// 本包当前没有调用点（见交付报告的待拍板 #1），这里只锁现状不改行为。
	if got := clientEmail(42); got != "user_42" {
		t.Errorf("clientEmail(42) = %q", got)
	}
	// 现状是**按字节**截断（不是按字符）；中文超过上限时可能切在多字节中间，
	// 写 utf8mb4 列会报 Incorrect string value（见交付报告的待拍板 #2）。
	if got := truncateStr("中文截断测试", 2); got != string([]byte{0xe4, 0xb8}) {
		t.Errorf("truncateStr 现状应为字节截断，得到 %q", got)
	}
	if got := truncateStr("abcdef", 3); got != "abc" {
		t.Errorf("truncateStr ASCII = %q", got)
	}
	if got := truncateStr("ab", 5); got != "ab" {
		t.Errorf("未超长时必须原样返回，得到 %q", got)
	}
	if got := strtr("hi {name}", map[string]string{"{name}": "bob"}); got != "hi bob" {
		t.Errorf("strtr = %q", got)
	}
	if got := xrayHostOf("example.com:443"); got != "example.com" {
		t.Errorf("xrayHostOf 解析错: %q", got)
	}
	if got := xrayHostOf("  Example.COM  "); got != "example.com" {
		t.Errorf("xrayHostOf 应去空格并转小写，得到 %q", got)
	}
	if got := xrayHostOf("[2001:db8::1]:443"); got != "[2001:db8::1]:443" {
		t.Errorf("IPv6 字面量不该被截断，得到 %q", got)
	}
	if got := tryBase64Decode("aGVsbG8="); got != "hello" {
		t.Errorf("tryBase64Decode = %q", got)
	}
	if got := tryBase64Decode("!!!"); got != "" {
		t.Errorf("解码失败应返回空串（不是原样返回），得到 %q", got)
	}
	if got := tryBase64Decode(""); got != "" {
		t.Errorf("空输入应返回空串，得到 %q", got)
	}
	if got := simpleSHA1("abc"); len(got) != 40 {
		t.Errorf("simpleSHA1 长度 = %d，应为 40 位十六进制", len(got))
	}
	if got := toInt64Value(float64(12)); got != 12 {
		t.Errorf("toInt64Value(float64) = %d", got)
	}
	if got := toInt64Value("17"); got != 17 {
		t.Errorf("toInt64Value(string) = %d", got)
	}
	if phpSerializeString("ab") != `s:2:"ab";` {
		t.Errorf("phpSerializeString 与 PHP serialize 口径不一致: %q", phpSerializeString("ab"))
	}
	if slugifySimple("Hello World!!") != "hello-world" {
		t.Errorf("slugifySimple = %q", slugifySimple("Hello World!!"))
	}
	if cryptoSHA1("abc") != simpleSHA1("abc") {
		t.Error("cryptoSHA1 与 simpleSHA1 必须同口径（缓存键/指令签名共用）")
	}
}

// TestStd02BackupHelpersOnDisk：备份用到的小工具在真文件系统上跑一遍。
func TestStd02BackupHelpersOnDisk(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested", "deep")
	if err := ensureDir(sub); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
		t.Fatalf("ensureDir 没建出目录: %v", err)
	}

	// gzip 写一个文件 → 体积可读、能被 gzip 识别。
	path := filepath.Join(sub, "a.sql.gz")
	w, err := newGzipFile(path)
	if err != nil {
		t.Fatalf("newGzipFile: %v", err)
	}
	if _, err := w.Write([]byte(strings.Repeat("dump line\n", 100))); err != nil {
		t.Fatalf("写 gzip 失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 gzip 失败: %v", err)
	}
	if fileSize(path) <= 0 {
		t.Fatal("fileSize 读到 0")
	}

	// pruneOldBackups 只保留最新 keep 份（按 backup-*.sql.gz 文件名倒序）。
	for i := 0; i < 5; i++ {
		p := filepath.Join(sub, "backup-2026010"+itoaStd02(i)+"-000000.sql.gz")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 让修改时间有先后（prune 按 mtime 排序）。
		mt := time.Now().Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(p, mt, mt)
	}
	pruneOldBackups(sub, 2)
	backups, err := filepath.Glob(filepath.Join(sub, "backup-*.sql.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("pruneOldBackups(keep=2) 后剩 %d 份备份，期望 2", len(backups))
	}
	// 保留下来的必须是最新的两份（倒序保留）。
	for _, want := range []string{"backup-20260103-000000.sql.gz", "backup-20260104-000000.sql.gz"} {
		if _, err := os.Stat(filepath.Join(sub, want)); err != nil {
			t.Errorf("最新备份 %s 被误删: %v", want, err)
		}
	}

	removeIfExists(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("removeIfExists 没删掉文件")
	}
	removeIfExists(path) // 删不存在的文件必须静默
}

func itoaStd02(i int) string { return string(rune('0' + i)) }
