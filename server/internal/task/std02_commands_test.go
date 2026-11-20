package task

// std02_commands_test.go 覆盖命令体（定时任务真正干活的地方）：
//
//	邀请码刷新 / 监控降采样 / 超时订单作废 / 第三方订阅拉取 / 出站订阅刷新 /
//	备份（失败路径）/ 代理维护 / 对账（未接实现）/ geo 更新（未接实现）/ cache 与 site_config 小工具。
//
// 口径：真库 + httptest 假上游。断言的都是「用户或运维能看到的结果」——
// 库里的行、状态字段、错误文案，而不是函数被调用过。

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// std02Runner 造一个时钟固定的命令运行器。
func std02Runner(t *testing.T, db *sql.DB, now time.Time, deps CommandDeps) *commandRunner {
	t.Helper()
	deps.DB = db
	deps.Now = func() time.Time { return now }
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	return NewCommandRunner(deps).(*commandRunner)
}

// ─────────── 邀请码刷新 ───────────

// TestStd02InviteRefreshRewritesSameRow：到期的邀请码在原行上换码文本、清使用次数、推下次刷新。
//
// 产品口径（2026-11-06 定稿）：只换码文本、不新增行 —— 返佣链靠 id 保持。
func TestStd02InviteRefreshRewritesSameRow(t *testing.T) {
	db := newStd02TaskDB(t, "invite")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := std02Runner(t, db, now, CommandDeps{})

	mustExecTask(t, db, "INSERT INTO users (id, email, token, uuid, protocol, enabled) VALUES (900,'u900@example.com','tok900','11111111-1111-1111-1111-111111111111','vless',1)")
	// 一张到期该刷新的、一张还没到期的。
	mustExecTask(t, db, `INSERT INTO discount_codes (id, code, source, user_id, discount, max_uses, used_count, max_uses_per_user, status, next_refresh_at, created_at, updated_at)
		VALUES (1,'INVOLDCODE','invite',900,0.90,5,4,1,'active',?,?,?)`, now.Add(-time.Hour), now.Add(-40*24*time.Hour), now.Add(-time.Hour))
	mustExecTask(t, db, `INSERT INTO discount_codes (id, code, source, user_id, discount, max_uses, used_count, max_uses_per_user, status, next_refresh_at, created_at, updated_at)
		VALUES (2,'INVFUTURE1','invite',900,0.90,5,1,1,'active',?,?,?)`, now.Add(48*time.Hour), now, now)

	if err := r.runInviteRefresh(ctx); err != nil {
		t.Fatalf("invite:refresh 失败: %v", err)
	}

	var code, status string
	var usedCount int
	var nextRefresh, refreshedAt sql.NullTime
	if err := db.QueryRow("SELECT code, status, used_count, next_refresh_at, refreshed_at FROM discount_codes WHERE id = 1").
		Scan(&code, &status, &usedCount, &nextRefresh, &refreshedAt); err != nil {
		t.Fatal(err)
	}
	if code == "INVOLDCODE" {
		t.Error("到期的邀请码没有换码文本")
	}
	if !strings.HasPrefix(code, "INV") || len(code) != 11 {
		t.Errorf("新码格式 = %q，应为 INV + 8 位", code)
	}
	if usedCount != 0 {
		t.Errorf("刷新后 used_count = %d，必须清零（旧码的使用次数不能算到新码头）", usedCount)
	}
	if status != "active" {
		t.Errorf("刷新后状态 = %s，期望 active", status)
	}
	if !nextRefresh.Valid || !nextRefresh.Time.After(now) {
		t.Errorf("下次刷新时间 = %v，应被推到未来", nextRefresh)
	}
	if !refreshedAt.Valid {
		t.Error("refreshed_at 应写入")
	}
	// 行数不变（绝不新增行）。
	if n := countRows(t, db, "SELECT COUNT(*) FROM discount_codes"); n != 2 {
		t.Fatalf("邀请码行数 = %d，刷新只换文本不新增行", n)
	}
	// 没到期的不动。
	var futureCode string
	if err := db.QueryRow("SELECT code FROM discount_codes WHERE id = 2").Scan(&futureCode); err != nil {
		t.Fatal(err)
	}
	if futureCode != "INVFUTURE1" {
		t.Error("未到期的邀请码被刷新了")
	}
}

// TestStd02InviteRefreshOrphanUser：用户已注销时只置 refreshed，不产生孤儿新码。
func TestStd02InviteRefreshOrphanUser(t *testing.T) {
	db := newStd02TaskDB(t, "invite_orphan")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := std02Runner(t, db, now, CommandDeps{})

	mustExecTask(t, db, `INSERT INTO discount_codes (id, code, source, user_id, discount, max_uses, used_count, max_uses_per_user, status, next_refresh_at, created_at, updated_at)
		VALUES (7,'INVORPHAN1','invite',12345,0.90,5,0,1,'active',?,?,?)`, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Hour))

	if err := r.runInviteRefresh(ctx); err != nil {
		t.Fatalf("invite:refresh 失败: %v", err)
	}
	var status string
	if err := db.QueryRow("SELECT status FROM discount_codes WHERE id = 7").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "refreshed" {
		t.Fatalf("孤儿码状态 = %s，期望 refreshed（留证据但不复活）", status)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM discount_codes"); n != 1 {
		t.Fatalf("孤儿码刷新后行数 = %d，绝不能自我复制", n)
	}
}

// ─────────── 监控降采样 ───────────

// TestStd02MonitorRollup：超期 L0 采样折成 monitor_daily 增量后删除，未超期样本保留。
func TestStd02MonitorRollup(t *testing.T) {
	db := newStd02TaskDB(t, "rollup")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := std02Runner(t, db, now, CommandDeps{})

	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('monitor.retain_days', '90')")
	mustExecTask(t, db, "INSERT INTO nodes (id, name, host, port, enabled, status, driver_type) VALUES (1,'n1','10.0.0.1',443,1,'online','3x-ui')")
	oldDay := now.AddDate(0, 0, -100)
	// 同一天两条：第一条是当天第一份（增量=绝对值），第二条增量=差值。
	mustExecTask(t, db, "INSERT INTO monitor_samples (node_id, sent, recv, sampled_at) VALUES (1, 1000, 2000, ?)", oldDay.Add(1*time.Hour))
	mustExecTask(t, db, "INSERT INTO monitor_samples (node_id, sent, recv, sampled_at) VALUES (1, 1500, 2600, ?)", oldDay.Add(2*time.Hour))
	// 重置（计数变小）走「按新值算」分支。
	mustExecTask(t, db, "INSERT INTO monitor_samples (node_id, sent, recv, sampled_at) VALUES (1, 100, 200, ?)", oldDay.Add(3*time.Hour))
	// 未超期样本必须保留。
	mustExecTask(t, db, "INSERT INTO monitor_samples (node_id, sent, recv, sampled_at) VALUES (1, 9, 9, ?)", now.Add(-time.Hour))

	if err := r.runMonitorRollup(ctx); err != nil {
		t.Fatalf("monitor:rollup 失败: %v", err)
	}

	if n := countRows(t, db, "SELECT COUNT(*) FROM monitor_samples"); n != 1 {
		t.Fatalf("剩余采样 = %d 条，期望只留未超期的 1 条", n)
	}
	var sentDelta, recvDelta int64
	if err := db.QueryRow("SELECT sent_delta, recv_delta FROM monitor_daily WHERE node_id = 1").
		Scan(&sentDelta, &recvDelta); err != nil {
		t.Fatalf("没有写 monitor_daily: %v", err)
	}
	// 1000 + 500 + 100(重置后按新值) = 1600；2000 + 600 + 200 = 2800。
	if sentDelta != 1600 || recvDelta != 2800 {
		t.Fatalf("monitor_daily 增量 = sent %d / recv %d，期望 1600 / 2800", sentDelta, recvDelta)
	}

	// 幂等：再跑一次不应翻倍。
	if err := r.runMonitorRollup(ctx); err != nil {
		t.Fatalf("monitor:rollup 第二次失败: %v", err)
	}
	if err := db.QueryRow("SELECT sent_delta FROM monitor_daily WHERE node_id = 1").Scan(&sentDelta); err != nil {
		t.Fatal(err)
	}
	if sentDelta != 1600 {
		t.Fatalf("重复执行后 sent_delta = %d，应保持 1600（幂等）", sentDelta)
	}
}

// ─────────── 超时订单作废 ───────────

// TestStd02OrderExpirePending：支付窗口已过的 pending 单变 expired、清 pay_url、代理单还库存。
func TestStd02OrderExpirePending(t *testing.T) {
	db := newStd02TaskDB(t, "orders")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := std02Runner(t, db, now, CommandDeps{})

	// schema.sql 是「全新安装」形态，pay_url / pay_expires_at 由启动时 ensureSchema 补列。
	mustExecTask(t, db, "ALTER TABLE orders ADD COLUMN pay_url varchar(2048) NULL, ADD COLUMN pay_expires_at datetime NULL")

	mustExecTask(t, db, "INSERT INTO users (id, email, token, uuid, protocol, enabled) VALUES (901,'u901@example.com','tok901','11111111-1111-1111-1111-111111111111','vless',1)")

	// 1) 已到 pay_expires_at、还挂着支付链接的单 → 作废。
	mustExecTask(t, db, `INSERT INTO orders (id, order_no, user_id, order_type, amount, original_amount, discount_amount, currency, status, pay_url, pay_expires_at, created_at, updated_at)
		VALUES (1,'CH20261117001',901,'recharge',9.90,9.90,0,'CNY','pending','https://pay.example.com/1',?,?,?)`,
		now.Add(-time.Minute), now.Add(-30*time.Minute), now)
	// 2) 老单没有 pay_expires_at，但 created_at 超过 TTL → 也要作废。
	mustExecTask(t, db, `INSERT INTO orders (id, order_no, user_id, order_type, amount, original_amount, discount_amount, currency, status, created_at, updated_at)
		VALUES (2,'CH20261117002',901,'recharge',5.00,5.00,0,'CNY','pending',?,?)`,
		now.Add(-24*time.Hour), now.Add(-24*time.Hour))
	// 3) 还在支付窗口内的单 → 不动。
	mustExecTask(t, db, `INSERT INTO orders (id, order_no, user_id, order_type, amount, original_amount, discount_amount, currency, status, pay_expires_at, created_at, updated_at)
		VALUES (3,'CH20261117003',901,'recharge',5.00,5.00,0,'CNY','pending',?,?,?)`,
		now.Add(10*time.Minute), now, now)
	// 4) 非 pending 单上残留的支付链接 → 兜底清掉。
	mustExecTask(t, db, `INSERT INTO orders (id, order_no, user_id, order_type, amount, original_amount, discount_amount, currency, status, pay_url, created_at, updated_at)
		VALUES (4,'CH20261117004',901,'recharge',5.00,5.00,0,'CNY','expired','https://pay.example.com/4',?,?)`, now, now)

	if err := r.runOrderExpirePending(ctx, nil); err != nil {
		t.Fatalf("order:expire-pending 失败: %v", err)
	}

	var status string
	var payURL sql.NullString
	if err := db.QueryRow("SELECT status, pay_url FROM orders WHERE id = 1").Scan(&status, &payURL); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || payURL.Valid {
		t.Errorf("订单 1 = %s/pay_url=%v，期望 expired 且 pay_url 清空", status, payURL)
	}
	if err := db.QueryRow("SELECT status FROM orders WHERE id = 2").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Errorf("老单（无 pay_expires_at）状态 = %s，期望 expired", status)
	}
	if err := db.QueryRow("SELECT status FROM orders WHERE id = 3").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("还在窗口内的订单被作废了（%s）", status)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM orders WHERE pay_url IS NOT NULL"); n != 0 {
		t.Errorf("非待支付单上的支付链接没清干净，剩 %d 条", n)
	}

	// dry-run 不写库。
	mustExecTask(t, db, `INSERT INTO orders (id, order_no, user_id, order_type, amount, original_amount, discount_amount, currency, status, pay_expires_at, created_at, updated_at)
		VALUES (5,'CH20261117005',901,'recharge',5.00,5.00,0,'CNY','pending',?,?,?)`, now.Add(-time.Minute), now, now)
	if err := r.runOrderExpirePending(ctx, map[string]string{"--dry-run": "1"}); err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	if err := db.QueryRow("SELECT status FROM orders WHERE id = 5").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Error("dry-run 把订单作废了")
	}
}

// ─────────── 第三方订阅拉取 ───────────

// TestStd02ThirdPartyFetch：成功缓存链接 / 失败只记错误且保留旧缓存。
func TestStd02ThirdPartyFetch(t *testing.T) {
	db := newStd02TaskDB(t, "thirdparty")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 订阅正文：base64 编码的链接列表（真实机场的常见形态）。
		body := "dmxlc3M6Ly91dWlkQGV4YW1wbGUuY29tOjQ0Mz9zZWN1cml0eT10bHMjSlAx" +
			"CnRyb2phbjovL3Bhc3NAZXhhbXBsZS5vcmc6NDQzI1VTMQo="
		fmt.Fprint(w, body)
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer bad.Close()

	r := std02Runner(t, db, now, CommandDeps{})
	mustExecTask(t, db, "INSERT INTO third_party_subs (id, name, url, enabled, node_count, last_status, cached_links) VALUES (1,'good',?,1,0,'pending',NULL)", good.URL)
	mustExecTask(t, db, "INSERT INTO third_party_subs (id, name, url, enabled, node_count, last_status, cached_links) VALUES (2,'bad',?,1,0,'success','stale-link')", bad.URL)
	mustExecTask(t, db, "INSERT INTO third_party_subs (id, name, url, enabled, node_count, last_status) VALUES (3,'disabled',?,0,0,'pending')", bad.URL)

	if err := r.runThirdPartyFetch(ctx); err != nil {
		t.Fatalf("third-party:fetch 失败: %v", err)
	}

	var status string
	var nodeCount int
	var cached sql.NullString
	if err := db.QueryRow("SELECT last_status, node_count, cached_links FROM third_party_subs WHERE id = 1").
		Scan(&status, &nodeCount, &cached); err != nil {
		t.Fatal(err)
	}
	if status != "success" || nodeCount != 2 {
		t.Errorf("订阅 1 = %s/%d 节点，期望 success/2", status, nodeCount)
	}
	if !cached.Valid || !strings.Contains(cached.String, "vless://") || !strings.Contains(cached.String, "trojan://") {
		t.Errorf("缓存内容不对: %v", cached)
	}

	var lastErr sql.NullString
	if err := db.QueryRow("SELECT last_status, last_error, cached_links FROM third_party_subs WHERE id = 2").
		Scan(&status, &lastErr, &cached); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !lastErr.Valid {
		t.Errorf("订阅 2 = %s/err=%v，期望 failed 且有错误文案", status, lastErr)
	}
	if !cached.Valid || cached.String != "stale-link" {
		t.Errorf("拉取失败时必须保留上次成功缓存，实际 %v", cached)
	}

	// 禁用的订阅不参与。
	if err := db.QueryRow("SELECT last_status FROM third_party_subs WHERE id = 3").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("禁用的订阅被处理了（%s）", status)
	}
}

// ─────────── 出站订阅刷新 ───────────

// TestStd02RefreshOutboundSubs：内容变化才 bump config_version；未到期跳过。
func TestStd02RefreshOutboundSubs(t *testing.T) {
	db := newStd02TaskDB(t, "outbound")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 出站订阅的真实形态：每行一个 URI（base64 或明文）。
		fmt.Fprint(w, "vless://uuid-1@jp.example.com:443?security=tls#JP1"+"\n"+"trojan://pass@us.example.com:443#US1"+"\n")
	}))
	defer srv.Close()

	var bumped []uint64
	r := std02Runner(t, db, now, CommandDeps{
		BumpConfigVersion: func(_ context.Context, nodeID uint64) (int64, error) {
			bumped = append(bumped, nodeID)
			return 7, nil
		},
	})
	mustExecTask(t, db, "INSERT INTO nodes (id, name, host, port, enabled, status, driver_type) VALUES (901,'n901','10.0.0.1',443,1,'online','xray')")
	// 1 号：内容变化（之前 hash 是旧的）→ 应该 bump。
	mustExecTask(t, db, `INSERT INTO xray_outbound_subscriptions (id, node_id, url, update_interval, enabled, content_hash, last_fetched_at, created_at, updated_at)
		VALUES (1, 901, ?, 60, 1, 'oldhash', ?, NOW(), NOW())`, srv.URL, now.Add(-2*time.Hour))
	// 2 号：刚拉过，没到期 → 跳过。
	mustExecTask(t, db, `INSERT INTO xray_outbound_subscriptions (id, node_id, url, update_interval, enabled, last_fetched_at, created_at, updated_at)
		VALUES (2, 901, ?, 60, 1, ?, NOW(), NOW())`, srv.URL, now.Add(-time.Minute))

	if err := r.runXrayRefreshOutboundSubs(ctx); err != nil {
		t.Fatalf("xray:refresh-outbound-subs 失败: %v", err)
	}

	var status string
	var hash sql.NullString
	var nodeCount int
	if err := db.QueryRow("SELECT last_status, content_hash, node_count FROM xray_outbound_subscriptions WHERE id = 1").
		Scan(&status, &hash, &nodeCount); err != nil {
		t.Fatal(err)
	}
	if status != "success" || !hash.Valid || hash.String == "oldhash" {
		t.Errorf("订阅 1 = %s/hash=%v，期望 success 且 hash 已更新", status, hash)
	}
	if nodeCount != 2 {
		t.Errorf("node_count = %d，期望 2（两行 URI）", nodeCount)
	}
	if len(bumped) != 1 || bumped[0] != 901 {
		t.Errorf("bump 调用 = %v，期望只有 node 901（内容变化才 bump）", bumped)
	}
}

// ─────────── 备份（失败路径） ───────────

// TestStd02BackupFailsLoudly：mysqldump 不存在 / 未配库名时必须返回明确错误，不留半截文件。
func TestStd02BackupFailsLoudly(t *testing.T) {
	db := newStd02TaskDB(t, "backup")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 3, 30, 0, 0, time.UTC)

	// 没有数据库名 → 直接报错（不能导出空文件当成功）。
	r := std02Runner(t, db, now, CommandDeps{BackupDir: t.TempDir()})
	if err := r.runBackupRun(ctx); err == nil || !strings.Contains(err.Error(), "数据库名") {
		t.Errorf("未配置库名时应报错，实际 %v", err)
	}

	// mysqldump 不存在 → 报「启动失败」并清掉半截文件。
	dir := t.TempDir()
	r2 := std02Runner(t, db, now, CommandDeps{
		BackupBin:  "std02-mysqldump-does-not-exist",
		BackupDir:  dir,
		BackupKeep: 7,
		DBName:     "controlhub",
		DBHost:     "127.0.0.1",
		DBPort:     "3306",
		DBUser:     "root",
	})
	err := r2.runBackupRun(ctx)
	if err == nil || !strings.Contains(err.Error(), "mysqldump") {
		t.Fatalf("备份二进制缺失时应报错，实际 %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "backup-*.sql.gz"))
	if len(backups) != 0 {
		t.Errorf("失败后不该留半截备份文件，实际 %v", backups)
	}
}

// ─────────── 未接实现 / dry-run 分支 ───────────

// TestStd02CommandsNoopBranches：依赖没接时要「明确跳过」，不许假装成功也不许报错。
func TestStd02CommandsNoopBranches(t *testing.T) {
	db := newStd02TaskDB(t, "noop")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	r := std02Runner(t, db, now, CommandDeps{})

	if err := r.runGeodataUpdate(ctx, map[string]string{"--dispatch": "1"}); err != nil {
		t.Errorf("Geodata 未配置时应静默跳过，实际 %v", err)
	}
	if err := r.runUsersReconcileEnable(ctx, nil); err != nil {
		t.Errorf("ReconcileNodes 未配置时应静默跳过，实际 %v", err)
	}
	// 未知命令必须报错（可见失败）。
	if err := r.Run(ctx, "no:such:command", nil); err == nil {
		t.Error("未知命令必须返回错误")
	}
	// 没有启用订阅时两条拉取命令都应安静跳过。
	if err := r.runThirdPartyFetch(ctx); err != nil {
		t.Errorf("没有第三方订阅时应跳过，实际 %v", err)
	}
	if err := r.runXrayRefreshOutboundSubs(ctx); err != nil {
		t.Errorf("没有出站订阅时应跳过，实际 %v", err)
	}
}

// TestStd02ReconcileEnableLogsSummary：对账返回统计后必须把关键告警写进日志。
func TestStd02ReconcileEnableLogsSummary(t *testing.T) {
	db := newStd02TaskDB(t, "reconcile")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	var logs []string
	r := std02Runner(t, db, now, CommandDeps{
		ReconcileNodes: func(context.Context, bool, int) (ReconcileSummary, error) {
			return ReconcileSummary{
				Nodes: 3, Users: 10, Probed: 0, ProbeFailed: 2,
				DriftEarlyCut: 1, DriftFixed: 1, BareBumped: 1, BareMismatch: 1,
				ForeignNames: []string{"someoneelse"}, SnapshotBy: "commands",
			}, nil
		},
		Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	if err := r.runUsersReconcileEnable(ctx, nil); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"probe_failed=2", "drift_early_cut", "WARN", "someoneelse"} {
		if !strings.Contains(joined, want) {
			t.Errorf("日志里缺 %q（运维靠这些字段定位）:\n%s", want, joined)
		}
	}
	// dry-run 必须带前缀。
	logs = nil
	if err := r.runUsersReconcileEnable(ctx, map[string]string{"--dry-run": "1"}); err != nil {
		t.Fatalf("对账 dry-run 失败: %v", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "[dry-run]") {
		t.Error("dry-run 日志应带 [dry-run] 前缀")
	}
}

// ─────────── cache / site_config 小工具 ───────────

// TestStd02CacheAndSiteConfig：Cache::add 语义（已存在未过期返回 false）+ 配置读取兜底。
func TestStd02CacheAndSiteConfig(t *testing.T) {
	db := newStd02TaskDB(t, "cache")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	r := std02Runner(t, db, now, CommandDeps{})

	ok, err := r.cacheAdd(ctx, "notify:1", "1", time.Hour)
	if err != nil || !ok {
		t.Fatalf("首次 cacheAdd 应成功: ok=%v err=%v", ok, err)
	}
	// 再写同 key → 已存在未过期，返回 false（这就是防重标记的作用）。
	if ok, err := r.cacheAdd(ctx, "notify:1", "1", time.Hour); err != nil || ok {
		t.Fatalf("重复 cacheAdd 应返回 false: ok=%v err=%v", ok, err)
	}
	// 过期后可以重新写入。
	mustExecTask(t, db, "UPDATE cache SET expiration = ? WHERE `key` LIKE ?", now.Add(-time.Hour).Unix(), "%notify:1")
	if ok, err := r.cacheAdd(ctx, "notify:1", "1", time.Hour); err != nil || !ok {
		t.Fatalf("过期后 cacheAdd 应成功: ok=%v err=%v", ok, err)
	}
	// forget 后可再写。
	if err := r.cacheForget(ctx, "notify:1"); err != nil {
		t.Fatalf("cacheForget: %v", err)
	}
	if ok, err := r.cacheAdd(ctx, "notify:1", "1", time.Hour); err != nil || !ok {
		t.Fatalf("forget 后 cacheAdd 应成功: ok=%v err=%v", ok, err)
	}

	// site_config 读取：缺失/存在/非法数字。
	if got := r.siteConfig(ctx, "missing.key", "def"); got != "def" {
		t.Errorf("缺省值 = %q", got)
	}
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('invite_refresh_days', '15')")
	if got := r.siteConfigInt(ctx, "invite_refresh_days", 30); got != 15 {
		t.Errorf("siteConfigInt = %d，期望 15", got)
	}
	mustExecTask(t, db, "UPDATE site_configs SET `value` = 'not-a-number' WHERE `key` = 'invite_refresh_days'")
	if got := r.siteConfigInt(ctx, "invite_refresh_days", 30); got != 30 {
		t.Errorf("非法数字应回落默认值，得到 %d", got)
	}
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('default_node_multiplier', '1.25')")
	if got := r.siteConfigFloat(ctx, "default_node_multiplier", 1.0); got != 1.25 {
		t.Errorf("siteConfigFloat = %v，期望 1.25", got)
	}
	if got := r.siteConfigFloat(ctx, "missing.float", 0.5); got != 0.5 {
		t.Errorf("缺失键应回落默认值，得到 %v", got)
	}
	// 随机码只出现在允许字符集里。
	code := randomCodeBody("23456789ABCDEFGHJKMNPQRSTUVWXYZ", 8)
	if len(code) != 8 || strings.ContainsAny(code, "01IO L") {
		t.Errorf("随机码体 = %q（长度 8、不含易混字符）", code)
	}
}
