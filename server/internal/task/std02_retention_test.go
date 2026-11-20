package task

// std02_retention_test.go 覆盖数据保留期清理（规范化-02 场景 7）：
//
//	只删「超过保留期」的日志/回执，不删业务数据；pending/running 一律不碰；
//	阈值优先级：环境变量 > site_configs > 代码默认；--dry-run 只统计不删。
//
// 为什么必须真库：这些逻辑就是 SQL（DELETE ... LIMIT、按主键水位删、按节点分组保留），
// 打桩只能测到自己的 if。

import (
	"context"
	"testing"
	"time"
)

// TestStd02RetentionPurgesOnlyExpired：超期行被删、未超期行与业务数据原样保留。
func TestStd02RetentionPurgesOnlyExpired(t *testing.T) {
	db := newStd02TaskDB(t, "retention")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	runner := NewCommandRunner(CommandDeps{DB: db, Now: func() time.Time { return now }, Logf: func(string, ...any) {}})
	r := runner.(*commandRunner)

	old := now.AddDate(0, 0, -400)
	recent := now.AddDate(0, 0, -1)

	// node_api_commands 有外键指向 nodes，先建节点。
	for _, id := range []int{1, 2} {
		mustExecTask(t, db, "INSERT INTO nodes (id, name, host, port, enabled, status, driver_type) VALUES (?, ?, ?, 443, 1, 'online', '3x-ui')",
			id, "n"+itoaStd02(id), "10.0.0."+itoaStd02(id))
	}

	// mail_logs：180 天保留。
	mustExecTask(t, db, "INSERT INTO mail_logs (type, status, to_email, subject, created_at, updated_at) VALUES ('batch','sent','a@example.com','old',?,?)", old, old)
	mustExecTask(t, db, "INSERT INTO mail_logs (type, status, to_email, subject, created_at, updated_at) VALUES ('batch','sent','b@example.com','new',?,?)", recent, recent)

	// node_api_commands：终态回执 30 天；pending 永不删。
	mustExecTask(t, db, "INSERT INTO node_api_commands (node_id, action, status, created_at, updated_at) VALUES (1,'client_list','success',?,?)", old, old)
	mustExecTask(t, db, "INSERT INTO node_api_commands (node_id, action, status, created_at, updated_at) VALUES (1,'client_list','pending',?,?)", old, old)
	mustExecTask(t, db, "INSERT INTO node_api_commands (node_id, action, status, created_at, updated_at) VALUES (2,'client_list','failed',?,?)", recent, recent)

	// async_tasks：终态 30 天；running 永不删。
	mustExecTask(t, db, "INSERT INTO async_tasks (type, status, total, completed, failed, attempts, max_attempts, created_at, updated_at) VALUES ('traffic_sync','succeeded',1,1,0,1,1,?,?)", old, old)
	mustExecTask(t, db, "INSERT INTO async_tasks (type, status, total, completed, failed, attempts, max_attempts, created_at, updated_at) VALUES ('traffic_sync','running',1,0,0,1,1,?,?)", old, old)

	// access_tokens：过期 7 天以上删；expires_at IS NULL 的永久凭据不能动。
	mustExecTask(t, db, "INSERT INTO users (id, email, token, uuid, protocol, enabled) VALUES (800,'u800@example.com','tok800','11111111-1111-1111-1111-111111111111','vless',1)")
	mustExecTask(t, db, "INSERT INTO access_tokens (user_id, token, expires_at, created_at) VALUES (800,'old-token',?,?)", old, old)
	mustExecTask(t, db, "INSERT INTO access_tokens (user_id, token, expires_at, created_at) VALUES (800,'forever-token',NULL,?)", old)

	// operation_logs：系统审计按 30 天；日常流水按条数（这里只放 3 条，不动）。
	mustExecTask(t, db, "INSERT INTO operation_logs (type, action, created_at) VALUES ('system','drift',?)", old)
	mustExecTask(t, db, "INSERT INTO operation_logs (type, action, created_at) VALUES ('admin','update',?)", old)

	if err := r.runRetentionPurge(ctx, nil); err != nil {
		t.Fatalf("retention:purge 失败: %v", err)
	}

	// 超期的删掉、新的留下。
	if n := countRows(t, db, "SELECT COUNT(*) FROM mail_logs WHERE subject = 'old'"); n != 0 {
		t.Errorf("超期 mail_logs 未删，剩 %d 条", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM mail_logs WHERE subject = 'new'"); n != 1 {
		t.Error("未超期 mail_logs 被误删")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM node_api_commands WHERE status = 'success'"); n != 0 {
		t.Error("超期终态回执未删")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM node_api_commands WHERE status = 'pending'"); n != 1 {
		t.Error("pending 回执被误删（业务在途数据不许碰）")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM node_api_commands WHERE status = 'failed'"); n != 1 {
		t.Error("未超期失败回执被误删")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM async_tasks WHERE status = 'succeeded'"); n != 0 {
		t.Error("超期终态任务未删")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM async_tasks WHERE status = 'running'"); n != 1 {
		t.Error("running 任务被误删（在途数据不许碰）")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM access_tokens WHERE token = 'forever-token'"); n != 1 {
		t.Error("永不过期凭据被误删（expires_at IS NULL 不清）")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM access_tokens WHERE token = 'old-token'"); n != 0 {
		t.Error("过期凭据未删")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM operation_logs WHERE type = 'system'"); n != 0 {
		t.Error("超期系统审计未删")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM operation_logs WHERE type = 'admin'"); n != 1 {
		t.Error("日常流水在条数上限内，不该被删")
	}
	// 业务数据（用户）一行都不能少。
	if n := countRows(t, db, "SELECT COUNT(*) FROM users"); n != 1 {
		t.Error("清理误伤业务数据 users")
	}
}

// TestStd02RetentionDryRunDeletesNothing：--dry-run 只统计，一行都不删。
func TestStd02RetentionDryRunDeletesNothing(t *testing.T) {
	db := newStd02TaskDB(t, "retention_dry")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := NewCommandRunner(CommandDeps{DB: db, Now: func() time.Time { return now }, Logf: func(string, ...any) {}}).(*commandRunner)

	old := now.AddDate(0, 0, -400)
	for i := 0; i < 3; i++ {
		mustExecTask(t, db, "INSERT INTO mail_logs (type, status, to_email, created_at, updated_at) VALUES ('batch','sent','a@example.com',?,?)", old, old)
	}
	if err := r.runRetentionPurge(context.Background(), map[string]string{"--dry-run": "1"}); err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM mail_logs"); n != 3 {
		t.Fatalf("dry-run 删掉了数据（剩 %d 条，期望 3）", n)
	}
}

// TestStd02RetentionThresholds：阈值优先级 环境变量 > site_configs > 默认；配 0 = 关闭该项。
func TestStd02RetentionThresholds(t *testing.T) {
	db := newStd02TaskDB(t, "retention_threshold")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := NewCommandRunner(CommandDeps{DB: db, Now: func() time.Time { return now }, Logf: func(string, ...any) {}}).(*commandRunner)

	// site_configs 把 mail_logs 保留期改成 10 天 → 20 天前的行该删（默认 180 天本不会删）。
	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('mail.retain_days', '10')")
	twentyDays := now.AddDate(0, 0, -20)
	mustExecTask(t, db, "INSERT INTO mail_logs (type, status, to_email, created_at, updated_at) VALUES ('batch','sent','a@example.com',?,?)", twentyDays, twentyDays)
	if err := r.runRetentionPurge(context.Background(), nil); err != nil {
		t.Fatalf("retention 失败: %v", err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM mail_logs"); n != 0 {
		t.Fatal("site_configs 的保留期没生效（配 10 天应删掉 20 天前的行）")
	}

	// site_configs 配 0 → 关闭该项清理（谁都不删）。
	mustExecTask(t, db, "UPDATE site_configs SET `value` = '0' WHERE `key` = 'mail.retain_days'")
	mustExecTask(t, db, "INSERT INTO mail_logs (type, status, to_email, created_at, updated_at) VALUES ('batch','sent','a@example.com',?,?)", now.AddDate(0, 0, -999), now.AddDate(0, 0, -999))
	if err := r.runRetentionPurge(context.Background(), nil); err != nil {
		t.Fatalf("retention 失败: %v", err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM mail_logs"); n != 1 {
		t.Fatal("保留期配 0 应关闭清理，但行被删了")
	}

	// 环境变量优先级最高：设 1 天 → 5 天前的行被删（site_configs 此时是 0 = 关闭）。
	t.Setenv("MAIL_LOG_RETAIN_DAYS", "1")
	fiveDays := now.AddDate(0, 0, -5)
	mustExecTask(t, db, "INSERT INTO mail_logs (type, status, to_email, created_at, updated_at) VALUES ('batch','sent','b@example.com',?,?)", fiveDays, fiveDays)
	if err := r.runRetentionPurge(context.Background(), nil); err != nil {
		t.Fatalf("retention 失败: %v", err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM mail_logs WHERE to_email = 'b@example.com'"); n != 0 {
		t.Fatal("环境变量阈值没生效（MAIL_LOG_RETAIN_DAYS=1 应删掉 5 天前的行）")
	}
	if v, ok := lookupEnvInt("MAIL_LOG_RETAIN_DAYS"); !ok || v != 1 {
		t.Errorf("lookupEnvInt = %d/%v，期望 1/true", v, ok)
	}
	if _, ok := lookupEnvInt("STD02_不存在的变量"); ok {
		t.Error("未设置的环境变量应返回 false")
	}
}

// TestStd02RetentionOperationLogKeepRows：operation_logs 按条数保留（超出上限才删，且不删系统审计）。
func TestStd02RetentionOperationLogKeepRows(t *testing.T) {
	db := newStd02TaskDB(t, "retention_keep")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := NewCommandRunner(CommandDeps{DB: db, Now: func() time.Time { return now }, Logf: func(string, ...any) {}}).(*commandRunner)

	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('operation_log.keep_rows', '5')")
	for i := 0; i < 9; i++ {
		mustExecTask(t, db, "INSERT INTO operation_logs (type, action, created_at) VALUES ('admin','update',?)", now)
	}
	mustExecTask(t, db, "INSERT INTO operation_logs (type, action, created_at) VALUES ('system','drift',?)", now)

	if err := r.runRetentionPurge(context.Background(), nil); err != nil {
		t.Fatalf("retention 失败: %v", err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM operation_logs WHERE type = 'admin'"); n != 5 {
		t.Fatalf("日常流水 = %d 条，期望保留 5 条", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM operation_logs WHERE type = 'system'"); n != 1 {
		t.Fatal("系统审计被按条数清了（它只按时间保留，不能被日常操作挤掉）")
	}
}

// TestStd02RetentionClientListPerNode：client_list 回执按「每节点最近 N 条」清，别的 action 不动。
func TestStd02RetentionClientListPerNode(t *testing.T) {
	db := newStd02TaskDB(t, "retention_clientlist")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := NewCommandRunner(CommandDeps{DB: db, Now: func() time.Time { return now }, Logf: func(string, ...any) {}}).(*commandRunner)

	mustExecTask(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('node_command.client_list_keep', '3')")
	for _, id := range []int{1, 2} {
		mustExecTask(t, db, "INSERT INTO nodes (id, name, host, port, enabled, status, driver_type) VALUES (?, ?, ?, 443, 1, 'online', '3x-ui')",
			id, "n"+itoaStd02(id), "10.0.0."+itoaStd02(id))
	}
	// 节点 1 有 7 条 client_list 回执 + 2 条别的 action；节点 2 只有 2 条（不够 keep）。
	for i := 0; i < 7; i++ {
		mustExecTask(t, db, "INSERT INTO node_api_commands (node_id, action, status, created_at, updated_at) VALUES (1,'client_list','success',?,?)", now, now)
	}
	for i := 0; i < 2; i++ {
		mustExecTask(t, db, "INSERT INTO node_api_commands (node_id, action, status, created_at, updated_at) VALUES (1,'xray_config_reload','success',?,?)", now, now)
	}
	for i := 0; i < 2; i++ {
		mustExecTask(t, db, "INSERT INTO node_api_commands (node_id, action, status, created_at, updated_at) VALUES (2,'client_list','success',?,?)", now, now)
	}

	if err := r.runRetentionPurge(context.Background(), nil); err != nil {
		t.Fatalf("retention 失败: %v", err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM node_api_commands WHERE node_id = 1 AND action = 'client_list'"); n != 3 {
		t.Fatalf("节点 1 的 client_list 回执 = %d 条，期望 3 条", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM node_api_commands WHERE node_id = 2 AND action = 'client_list'"); n != 2 {
		t.Fatal("节点 2 没攒够 keep 条，不该删")
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM node_api_commands WHERE action = 'xray_config_reload'"); n != 2 {
		t.Fatal("非 client_list 的回执被误删")
	}
}
