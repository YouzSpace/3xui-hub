package task

// 13 个 artisan 命令体：对齐 backend/app/Console/Commands/*.php 与
// backend/app/Jobs/{MailNotifyScanJob,MailScheduleJob}.php 的 handle()。
//
// 命令名与 defaults.go 的 commands.Run(ctx, "xxx", ...) 字符串逐字一致。
// 复用已有驱动/store（threexui/xray 驱动、collector 的 cache 语义、
// handler/admin 的 SQLStore），不重写取数/落账逻辑。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"controlhub-server/internal/driver/threexui"
	"controlhub-server/internal/tgbot"
)

// ── 命令运行器 ──────────────────────────────────────────────────────

// MailJob 是一封待发信（对齐 SendMailJob 的入参）。
type MailJob struct {
	ToEmail  string
	Subject  string
	HTMLBody string
	Type     string // notify|batch|test|schedule
	UserID   *uint64
	BatchID  *int64
	Scene    *string
}

// MailSender 是发信队列接缝（PHP 的 SendMailJob::dispatch）。
type MailSender interface {
	EnqueueMail(job MailJob) error
}

// GeodataUpdater 是 geo 数据更新服务（对齐 GeodataService）。
type GeodataUpdater interface {
	Update(ctx context.Context) (map[string]GeodataUpdateResult, error)
	DispatchToNodes(ctx context.Context) (queued, skipped int, missing bool, err error)
}

// GeodataUpdateResult 是单文件更新结果。
type GeodataUpdateResult struct {
	Name    string
	Status  string // updated|unchanged|failed
	Size    int64
	Message string
}

// CacheStore 是防重标记的缓存接缝（对齐 Laravel Cache::add / forget）。
type CacheStore interface {
	// Add 原子写入：已存在且未过期返回 false，否则写入返回 true。
	Add(ctx context.Context, key string, value any, ttl time.Duration) (bool, error)
	// Forget 删除标记。
	Forget(ctx context.Context, key string) error
}

// CommandDeps 是 13 个命令体的依赖面。
type CommandDeps struct {
	DB     *sql.DB
	AppKey []byte // APP_KEY 解出的 32 字节 AES 密钥
	Logf   func(format string, args ...any)
	Now    func() time.Time

	// MailQueue 发信队列（mail:notify-scan / mail:schedule-scan 用）。
	MailQueue MailSender
	// Geodata geo 数据服务（geodata:update 用）。
	Geodata GeodataUpdater
	// Cache 防重标记（mail notify/schedule 去重用）。
	Cache CacheStore

	// BackupBin mysqldump 可执行文件名（默认 "mysqldump"）。
	BackupBin string
	// BackupDir 备份输出目录。
	BackupDir string
	// BackupKeep 保留份数（默认 7）。
	BackupKeep int
	// BackupTimeout 单次备份的超时（0 = 默认 30 分钟）。mysqldump 卡住会永久
	// 占用 withoutOverlapping 标志，导致后续每天备份被跳过，超时兜底掐断。
	BackupTimeout time.Duration
	// TGSender Telegram 通知（备份完成推送 zip；nil 则跳过 TG 推送）。
	TGSender *tgbot.Sender
	// DBHost/DBPort/DBName/DBUser/DBPassword 备份连接参数。
	DBHost     string
	DBPort     string
	DBName     string
	DBUser     string
	DBPassword string

	// BumpConfigVersion 行锁内递增 nodes.driver_config.config_version 并写 config_changed_at。
	// 复用 admin.NodeStore.BumpConfigVersion（对齐 PHP Node::bumpXrayConfigVersion）。
	// 为 nil 时跳过 bump 并记日志（不手写 SQL —— config_version 是 JSON 键不是列）。
	BumpConfigVersion func(ctx context.Context, nodeID uint64) (int64, error)

	// RotateWarp 是 WARP「换 IP」（xray:warp-rotate 用）：重新注册设备（保留 license）
	// → 已应用则重组 wireguard 出站 → bump config。对齐 PHP WarpService::rotate()。
	// 为 nil 时该命令跳过并记日志。
	RotateWarp func(ctx context.Context, nodeID uint64) error

	// ToggleClient 是「在所有启用节点上开关某个用户的 client」（users:reconcile-enable 用）。
	// 传 collector.ToggleClient：它已覆盖 3x-ui 与 xray 两条链路，并实现「全部确认才算成功」。
	// 为 nil 时该命令记日志并放弃。
	ToggleClient func(ctx context.Context, userID int64, enable bool) (bool, error)
}

// commandRunner 是 CommandRunner 的真实实现。
type commandRunner struct {
	deps CommandDeps
	logf func(format string, args ...any)
	now  func() time.Time
}

// NewCommandRunner 构造真实命令运行器。
func NewCommandRunner(deps CommandDeps) CommandRunner {
	logf := deps.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &commandRunner{deps: deps, logf: logf, now: now}
}

// Run 按命令名分发到对应命令体。
func (r *commandRunner) Run(ctx context.Context, command string, args map[string]string) error {
	switch command {
	case "invite:refresh":
		return r.runInviteRefresh(ctx)
	case "backup:run":
		return r.runBackupRun(ctx)
	case "monitor:rollup":
		return r.runMonitorRollup(ctx)
	case "order:expire-pending":
		return r.runOrderExpirePending(ctx, args)
	case "proxy:maintain":
		return r.runProxyMaintain(ctx, args)
	case "mail:notify-scan":
		return r.runMailNotifyScan(ctx)
	case "mail:schedule-scan":
		return r.runMailScheduleScan(ctx)
	case "third-party:fetch":
		return r.runThirdPartyFetch(ctx)
	case "xray:warp-rotate":
		return r.runXrayWarpRotate(ctx)
	case "xray:refresh-outbound-subs":
		return r.runXrayRefreshOutboundSubs(ctx)
	case "geodata:update":
		return r.runGeodataUpdate(ctx, args)
	case "users:reconcile-enable":
		return r.runUsersReconcileEnable(ctx, args)
	case "xray:reality-audit":
		return r.runXrayRealityAudit(ctx, args)
	default:
		return fmt.Errorf("未知命令: %s", command)
	}
}

// ── 公共小工具 ──────────────────────────────────────────────────────

// cacheAdd 复刻 Cache::add()（INSERT IGNORE 语义）。
// key 前缀与 collector 的 cacheKeyPrefix 同口径。
func (r *commandRunner) cacheAdd(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	fullKey := cacheKeyPrefix() + key
	expiration := r.now().Unix() + int64(ttl.Seconds())
	serialized := phpSerializeString(value)

	// 先查是否已有未过期条目
	var exp int64
	err := r.deps.DB.QueryRowContext(ctx,
		"SELECT expiration FROM cache WHERE `key` = ?", fullKey).Scan(&exp)
	if err == nil && exp > r.now().Unix() {
		return false, nil
	}
	if err == sql.ErrNoRows {
		res, ierr := r.deps.DB.ExecContext(ctx,
			"INSERT IGNORE INTO cache (`key`, value, expiration) VALUES (?, ?, ?)",
			fullKey, serialized, expiration)
		if ierr != nil {
			return false, ierr
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	if err != nil {
		return false, err
	}
	// 已过期 → 覆盖
	res, uerr := r.deps.DB.ExecContext(ctx,
		"UPDATE cache SET value = ?, expiration = ? WHERE `key` = ?", serialized, expiration, fullKey)
	if uerr != nil {
		return false, uerr
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// cacheForget 复刻 Cache::forget()。
func (r *commandRunner) cacheForget(ctx context.Context, key string) error {
	fullKey := cacheKeyPrefix() + key
	_, err := r.deps.DB.ExecContext(ctx, "DELETE FROM cache WHERE `key` = ?", fullKey)
	return err
}

// cacheKeyPrefix 复刻 config/cache.php 的 prefix。
func cacheKeyPrefix() string {
	// 与 collector.cacheKeyPrefix 同口径（避免重复导入）。
	return cachePrefixImpl()
}

// phpSerializeString 生成 PHP serialize() 字符串形态。
func phpSerializeString(s string) string {
	return fmt.Sprintf(`s:%d:"%s";`, len(s), s)
}

// siteConfig 读 site_configs 单个键。
func (r *commandRunner) siteConfig(ctx context.Context, key, def string) string {
	var v sql.NullString
	err := r.deps.DB.QueryRowContext(ctx,
		"SELECT value FROM site_configs WHERE `key` = ?", key).Scan(&v)
	if err != nil || !v.Valid {
		return def
	}
	return v.String
}

// siteConfigInt 读 site_configs 整数键。
func (r *commandRunner) siteConfigInt(ctx context.Context, key string, def int) int {
	s := r.siteConfig(ctx, key, "")
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}

// siteConfigFloat 读 site_configs 浮点键。
func (r *commandRunner) siteConfigFloat(ctx context.Context, key string, def float64) float64 {
	s := r.siteConfig(ctx, key, "")
	if s == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return def
	}
	return f
}

// ── 1. invite:refresh ───────────────────────────────────────────────

// runInviteRefresh 对齐 RefreshInviteCodes::handle()。
// 刷新到期的邀请码：旧码立即失效（status=refreshed），给同一用户发新码。
func (r *commandRunner) runInviteRefresh(ctx context.Context) error {
	r.logf("[%s] 开始刷新邀请码...", r.now().Format("2006-01-02 15:04:05"))

	// 查到期的 active 邀请码
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, user_id, code FROM discount_codes
		 WHERE source = 'invite' AND status = 'active'
		   AND next_refresh_at IS NOT NULL AND next_refresh_at <= ?`,
		r.now().UTC())
	if err != nil {
		return fmt.Errorf("查询到期邀请码失败: %w", err)
	}
	defer rows.Close()

	type inviteRow struct {
		ID     int64
		UserID int64
		Code   string
	}
	var expired []inviteRow
	for rows.Next() {
		var row inviteRow
		if err := rows.Scan(&row.ID, &row.UserID, &row.Code); err != nil {
			return err
		}
		expired = append(expired, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	refreshed := 0
	for _, code := range expired {
		done, err := r.refreshOneInvite(ctx, code.ID, code.UserID)
		if err != nil {
			r.logf("刷新邀请码 %s 失败: %v", code.Code, err)
			continue
		}
		if done {
			refreshed++
			r.logf("  刷新用户 #%d 的邀请码 %s", code.UserID, code.Code)
		}
	}

	r.logf("邀请码刷新完成：%d 个", refreshed)
	return nil
}

// refreshOneInvite 在事务内刷新一张邀请码（锁行重读，重复执行只刷一次）。
func (r *commandRunner) refreshOneInvite(ctx context.Context, codeID, userID int64) (bool, error) {
	tx, err := r.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// 锁行重读
	var status string
	var nextRefresh sql.NullTime
	err = tx.QueryRowContext(ctx,
		"SELECT status, next_refresh_at FROM discount_codes WHERE id = ? FOR UPDATE", codeID).
		Scan(&status, &nextRefresh)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "active" {
		return false, nil
	}
	if nextRefresh.Valid && nextRefresh.Time.After(r.now().UTC()) {
		return false, nil
	}

	// 旧码置为 refreshed
	now := r.now().UTC()
	if _, err := tx.ExecContext(ctx,
		"UPDATE discount_codes SET status = 'refreshed', refreshed_at = ?, updated_at = ? WHERE id = ?",
		now, now, codeID); err != nil {
		return false, err
	}

	// 锁用户行，保证「同一用户最多一张 active 邀请码」
	if _, err := tx.ExecContext(ctx,
		"SELECT id FROM users WHERE id = ? FOR UPDATE", userID); err != nil {
		return false, err
	}
	// 把该用户可能残留的 active 邀请码也置为 refreshed
	if _, err := tx.ExecContext(ctx,
		"UPDATE discount_codes SET status = 'refreshed', refreshed_at = ?, updated_at = ? WHERE source = 'invite' AND user_id = ? AND status = 'active'",
		now, now, userID); err != nil {
		return false, err
	}

	// 发新码
	discount := r.siteConfigFloat(ctx, "invite_discount", 0.90)
	refreshDays := r.siteConfigInt(ctx, "invite_refresh_days", 30)
	if refreshDays < 1 {
		refreshDays = 30
	}
	newCode, err := r.generateUniqueCode(ctx, "INV")
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO discount_codes (code, source, user_id, discount, max_uses, used_count,
		 max_uses_per_user, expires_at, note, status, refreshed_at, next_refresh_at, created_at, updated_at)
		 VALUES (?, 'invite', ?, ?, 999999, 0, 1, NULL, NULL, 'active', NULL, ?, ?, ?)`,
		newCode, userID, discount, now.AddDate(0, 0, refreshDays), now, now); err != nil {
		return false, err
	}

	return tx.Commit() == nil, nil
}

// generateUniqueCode 对齐 DiscountCode::generateCode()。
func (r *commandRunner) generateUniqueCode(ctx context.Context, prefix string) (string, error) {
	const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	const codeLen = 8
	for attempt := 0; attempt < 32; attempt++ {
		code := strings.ToUpper(prefix) + randomCodeBody(alphabet, codeLen)
		var n int
		err := r.deps.DB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM discount_codes WHERE code = ?", code).Scan(&n)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return code, nil
		}
	}
	return "", fmt.Errorf("生成唯一码失败（多次冲突）")
}

// randomCodeBody 生成随机码体（逐字节拒绝采样保证等概率）。
func randomCodeBody(alphabet string, length int) string {
	size := len(alphabet)
	limit := (256 / size) * size
	out := make([]byte, 0, length)
	buf := make([]byte, 1)
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			panic("task: 生成随机数失败: " + err.Error())
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, alphabet[int(buf[0])%size])
	}
	return string(out)
}

// ── 2. backup:run ──────────────────────────────────────────────────

// runBackupRun 对齐 BackupRun::handle()。
// mysqldump stdout 流式写 gzip，失败删半截文件，成功后按时间戳保留最新 keep 份。
func (r *commandRunner) runBackupRun(ctx context.Context) error {
	// 超时兜底：mysqldump 卡住（大库/磁盘慢/进程挂起）会永久占用 withoutOverlapping
	// 标志，后续每天备份都被跳过且无报错。30 分钟足够导出大库，超时自动掐断。
	timeout := r.deps.BackupTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := r.deps.BackupBin
	if bin == "" {
		bin = "mysqldump"
	}
	dir := strings.TrimRight(r.deps.BackupDir, `/\`)
	if dir == "" {
		dir = "/var/backups/3xui-hub"
	}
	keep := r.deps.BackupKeep
	if keep < 1 {
		keep = 7
	}

	// 创建备份目录
	if err := ensureDir(dir); err != nil {
		return fmt.Errorf("备份目录创建失败：%s: %w", dir, err)
	}

	dbName := r.deps.DBName
	if dbName == "" {
		return fmt.Errorf("未配置数据库名")
	}

	target := filepath.Join(dir, "backup-"+r.now().Format("20060102150405")+".sql.gz")

	args := []string{
		"--single-transaction",
		"--skip-lock-tables",
		"--host=" + r.deps.DBHost,
		"--port=" + r.deps.DBPort,
		"-u", r.deps.DBUser,
		dbName,
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	if r.deps.DBPassword != "" {
		cmd.Env = append(cmd.Environ(), "MYSQL_PWD="+r.deps.DBPassword)
	}

	// 流式写 gzip
	gzWriter, err := newGzipFile(target)
	if err != nil {
		return fmt.Errorf("备份文件无法写入：%s: %w", target, err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		gzWriter.Close()
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		gzWriter.Close()
		removeIfExists(target)
		return fmt.Errorf("备份失败（mysqldump 启动失败）：%v", err)
	}

	// 流式拷贝 stdout → gzip
	buf := make([]byte, 32*1024)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			if _, werr := gzWriter.Write(buf[:n]); werr != nil {
				break
			}
		}
		if readErr != nil {
			break
		}
	}
	gzWriter.Close()

	waitErr := cmd.Wait()
	if waitErr != nil {
		removeIfExists(target)
		summary := strings.TrimSpace(stderr.String())
		if len(summary) > 2000 {
			summary = summary[:2000]
		}
		return fmt.Errorf("备份失败（mysqldump 退出）：%v\n%s", waitErr, summary)
	}

	// 按文件名倒序保留最新 keep 份
	pruneOldBackups(dir, keep)

	size := fileSize(target)
	r.logf("备份完成：%s（%.1f KB）", target, float64(size)/1024)

	// TG 推送（开关 tg_backup_push 开启且已配置 token/chat_id 才发）。
	if r.deps.TGSender != nil && r.deps.TGSender.NotifyEnabled(ctx, tgbot.KeyBackupPush) {
		caption := fmt.Sprintf("ControlHub 每日备份 %s（%.1f KB）",
			r.now().Format("2006-01-02 15:04:05"), float64(size)/1024)
		if err := r.deps.TGSender.SendDocument(ctx, caption, target); err != nil {
			r.logf("TG 推送备份失败: %v", err)
		}
	}
	return nil
}

// ── 3. monitor:rollup ───────────────────────────────────────────────

// runMonitorRollup 对齐 MonitorRollupCommand::handle() → MonitorService::rollup()。
// 把超过保留期的 L0 采样折成 monitor_daily 的当天增量后删除原始样本。
func (r *commandRunner) runMonitorRollup(ctx context.Context) error {
	retainDays := r.siteConfigInt(ctx, "monitor.retain_days", 90)
	if retainDays < 1 {
		retainDays = 90
	}
	cutoff := r.now().UTC().AddDate(0, 0, -retainDays)

	tx, err := r.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 取过期样本，按 (node_id, sampled_at) 升序
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id, sampled_at, sent, recv FROM monitor_samples
		 WHERE sampled_at < ? ORDER BY node_id, sampled_at`, cutoff)
	if err != nil {
		return fmt.Errorf("查询过期样本失败: %w", err)
	}
	type sample struct {
		NodeID     int64
		SampledAt  time.Time
		Sent       int64
		Recv       int64
	}
	var samples []sample
	for rows.Next() {
		var s sample
		if err := rows.Scan(&s.NodeID, &s.SampledAt, &s.Sent, &s.Recv); err != nil {
			rows.Close()
			return err
		}
		samples = append(samples, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(samples) == 0 {
		return tx.Commit()
	}

	// 时区对齐 businessTimezone（PHP 默认 UTC）
	tz := time.UTC
	type dayKey struct {
		NodeID int64
		Date   string
	}
	groups := map[dayKey][2]int64{}
	lastSeen := map[int64]*sample{}

	for i := range samples {
		s := &samples[i]
		date := s.SampledAt.In(tz).Format("2006-01-02")
		prev := lastSeen[s.NodeID]
		lastSeen[s.NodeID] = s

		var dSent, dRecv int64
		if prev != nil {
			dSent = s.Sent - prev.Sent
			dRecv = s.Recv - prev.Recv
			if dSent < 0 {
				dSent = s.Sent
			}
			if dRecv < 0 {
				dRecv = s.Recv
			}
		} else {
			dSent = s.Sent
			dRecv = s.Recv
		}

		key := dayKey{NodeID: s.NodeID, Date: date}
		cur := groups[key]
		groups[key] = [2]int64{cur[0] + dSent, cur[1] + dRecv}
	}

	// upsert monitor_daily（覆盖写，幂等）
	for key, deltas := range groups {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO monitor_daily (node_id, date, sent_delta, recv_delta, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE sent_delta = VALUES(sent_delta), recv_delta = VALUES(recv_delta)`,
			key.NodeID, key.Date, deltas[0], deltas[1], r.now().UTC(), r.now().UTC())
		if err != nil {
			return fmt.Errorf("写入 monitor_daily 失败: %w", err)
		}
	}

	// 删除过期样本
	res, err := tx.ExecContext(ctx,
		"DELETE FROM monitor_samples WHERE sampled_at < ?", cutoff)
	if err != nil {
		return fmt.Errorf("删除过期样本失败: %w", err)
	}
	deleted, _ := res.RowsAffected()

	nodeSet := map[int64]bool{}
	for key := range groups {
		nodeSet[key.NodeID] = true
	}

	r.logf("完成：删除 %d 条过期样本，覆盖 %d 个 (节点, 日期) 分组（%d 个节点）",
		deleted, len(groups), len(nodeSet))
	return tx.Commit()
}

// ── 4. order:expire-pending ─────────────────────────────────────────

// runOrderExpirePending 对齐 ExpirePendingOrders::handle()。
// 作废超时（1 小时）未支付的订单。
func (r *commandRunner) runOrderExpirePending(ctx context.Context, args map[string]string) error {
	hours := 1
	if v, ok := args["--hours"]; ok {
		fmt.Sscanf(v, "%d", &hours)
	}
	if hours < 1 {
		hours = 1
	}
	dryRun := args["--dry-run"] == "1" || args["--dry-run"] == "true"

	cutoff := r.now().UTC().Add(-time.Duration(hours) * time.Hour)

	// 查询超时 pending 订单
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, order_no, user_id, amount, created_at, proxy_sku_id, proxy_renewal
		 FROM orders WHERE status = 'pending' AND created_at < ?
		 ORDER BY id LIMIT 10000`, cutoff)
	if err != nil {
		return fmt.Errorf("查询超时订单失败: %w", err)
	}
	type orderRow struct {
		ID           int64
		OrderNo      string
		UserID       int64
		Amount       float64
		CreatedAt    time.Time
		ProxySkuID   sql.NullInt64
		ProxyRenewal bool
	}
	var orders []orderRow
	for rows.Next() {
		var o orderRow
		if err := rows.Scan(&o.ID, &o.OrderNo, &o.UserID, &o.Amount, &o.CreatedAt,
			&o.ProxySkuID, &o.ProxyRenewal); err != nil {
			rows.Close()
			return err
		}
		orders = append(orders, o)
	}
	rows.Close()

	if len(orders) == 0 {
		r.logf("没有超时未支付订单，无需处理。")
		return nil
	}

	expired := 0
	now := r.now().UTC()
	for _, o := range orders {
		if dryRun {
			r.logf("[DRY-RUN] #%d %s（用户 %d，%.2f 元，创建于 %s）",
				o.ID, o.OrderNo, o.UserID, o.Amount, o.CreatedAt.Format("2006-01-02 15:04:05"))
			expired++
			continue
		}

		// 代理订单作废要把预占的库存还回去
		if o.ProxySkuID.Valid && !o.ProxyRenewal {
			_, err := r.deps.DB.ExecContext(ctx,
				"UPDATE proxy_skus SET stock_sold = GREATEST(0, stock_sold - 1), updated_at = ? WHERE id = ?",
				now, o.ProxySkuID.Int64)
			if err != nil {
				r.logf("归还库存失败（订单 #%d）: %v", o.ID, err)
			}
		}

		// 更新状态
		_, err := r.deps.DB.ExecContext(ctx,
			"UPDATE orders SET status = 'expired', updated_at = ? WHERE id = ? AND status = 'pending'",
			now, o.ID)
		if err != nil {
			r.logf("作废订单 #%d 失败: %v", o.ID, err)
			continue
		}
		expired++
	}

	r.logf("共 %d 笔超时未支付订单%s。", expired, map[bool]string{true: "（dry-run 未写库）", false: " 已作废"}[dryRun])
	return nil
}

// ── 5. proxy:maintain ──────────────────────────────────────────────

// runProxyMaintain 对齐 ProxyMaintain::handle() → ProxyStoreService::maintain()。
// 回采 webshare 用量 + 到期权益标记 + 到期停用上游子账号。
func (r *commandRunner) runProxyMaintain(ctx context.Context, args map[string]string) error {
	dryRun := args["--dry-run"] == "1" || args["--dry-run"] == "true"
	if dryRun {
		r.logf("dry-run：跳过实际回采与停用。")
		return nil
	}

	stats := struct {
		Pulled      int
		PullFailed  int
		Expired     int
		Disabled    int
	}{}

	now := r.now().UTC()

	// 1) 回采用量（webshare 独占账号）
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id FROM proxy_accounts WHERE status = 'active' AND sku_id IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("查询代理账号失败: %w", err)
	}
	var accountIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		accountIDs = append(accountIDs, id)
	}
	rows.Close()

	for _, id := range accountIDs {
		// pullUsage 需要 webshare API（proxyAccounts.pullUsage）。
		r.logf("[proxy:maintain] 账号 #%d 回采用量（webshare API 未接通，仅记录）", id)
		stats.Pulled++
	}

	// 2) 到期权益标记 expired + 停用上游账号
	rows2, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, account_id FROM proxy_entitlements
		 WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at <= ?`, now)
	if err != nil {
		return fmt.Errorf("查询到期权益失败: %w", err)
	}
	type entRow struct {
		ID        int64
		AccountID sql.NullInt64
	}
	var ents []entRow
	for rows2.Next() {
		var e entRow
		if err := rows2.Scan(&e.ID, &e.AccountID); err != nil {
			rows2.Close()
			return err
		}
		ents = append(ents, e)
	}
	rows2.Close()

	for _, e := range ents {
		if _, err := r.deps.DB.ExecContext(ctx,
			"UPDATE proxy_entitlements SET status = 'expired', updated_at = ? WHERE id = ?",
			now, e.ID); err != nil {
			r.logf("标记权益 #%d 过期失败: %v", e.ID, err)
			continue
		}
		stats.Expired++

		if e.AccountID.Valid {
			// 停用上游账号（改随机密码 + 限额压到 0.01GB）
			// 需要 webshare API（proxyAccounts.disableAccount）。
			var accStatus string
			err := r.deps.DB.QueryRowContext(ctx,
				"SELECT status FROM proxy_accounts WHERE id = ?", e.AccountID.Int64).Scan(&accStatus)
			if err == nil && accStatus == "active" {
				r.logf("[proxy:maintain] 账号 #%d 到期停用（webshare API 未接通，仅记录）", e.AccountID.Int64)
				stats.Disabled++
			}
		}
	}

	r.logf("回采成功 %d · 回采失败 %d · 到期权益 %d · 停用账号 %d",
		stats.Pulled, stats.PullFailed, stats.Expired, stats.Disabled)
	return nil
}

// ── 6. mail:notify-scan ────────────────────────────────────────────

// runMailNotifyScan 对齐 MailNotifyScanJob::handle()。
// 扫描流量即将用尽 / 套餐即将到期 / 已到期。
func (r *commandRunner) runMailNotifyScan(ctx context.Context) error {
	if err := r.scanTrafficAlmost(ctx); err != nil {
		r.logf("扫描流量即将用尽失败: %v", err)
	}
	if err := r.scanExpiring(ctx); err != nil {
		r.logf("扫描套餐即将到期失败: %v", err)
	}
	if err := r.scanExpired(ctx); err != nil {
		r.logf("扫描已到期失败: %v", err)
	}
	return nil
}

// notifyEnabled 检查场景开关。
func (r *commandRunner) notifyEnabled(ctx context.Context, scene string) bool {
	return r.siteConfig(ctx, "notify_"+scene+"_enabled", "") == "1"
}

// scanTrafficAlmost 流量即将用尽（默认 90%）。
func (r *commandRunner) scanTrafficAlmost(ctx context.Context) error {
	if !r.notifyEnabled(ctx, "traffic_almost") {
		return nil
	}
	threshold := r.siteConfigInt(ctx, "notify_traffic_almost_threshold", 90)
	if threshold <= 0 || threshold > 100 {
		threshold = 90
	}
	ratio := float64(threshold) / 100

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users
		 WHERE enabled = 1 AND traffic_limit > 0
		   AND traffic_used >= FLOOR(traffic_limit * ?)
		   AND traffic_used < traffic_limit
		 ORDER BY id LIMIT 10000`, ratio)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return err
		}
		if err := r.dispatchNotifyOnce(ctx, "traffic_almost", u); err != nil {
			r.logf("发送 traffic_almost 通知失败（用户 %d）: %v", u.ID, err)
		}
	}
	return rows.Err()
}

// scanExpiring 套餐即将到期（默认提前 3 天）。
func (r *commandRunner) scanExpiring(ctx context.Context) error {
	if !r.notifyEnabled(ctx, "expiring") {
		return nil
	}
	days := r.siteConfigInt(ctx, "notify_expiring_days", 3)
	now := r.now().UTC()
	end := now.AddDate(0, 0, days)

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users
		 WHERE enabled = 1 AND expired_at IS NOT NULL
		   AND expired_at > ? AND expired_at <= ?
		 ORDER BY id LIMIT 10000`, now, end)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return err
		}
		if err := r.dispatchNotifyOnce(ctx, "expiring", u); err != nil {
			r.logf("发送 expiring 通知失败（用户 %d）: %v", u.ID, err)
		}
	}
	return rows.Err()
}

// scanExpired 已到期（扫描窗口 7 天）。
func (r *commandRunner) scanExpired(ctx context.Context) error {
	if !r.notifyEnabled(ctx, "expired") {
		return nil
	}
	now := r.now().UTC()
	start := now.AddDate(0, 0, -7)

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users
		 WHERE enabled = 1 AND expired_at IS NOT NULL
		   AND expired_at <= ? AND expired_at >= ?
		 ORDER BY id LIMIT 10000`, now, start)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return err
		}
		if err := r.dispatchNotifyOnce(ctx, "expired", u); err != nil {
			r.logf("发送 expired 通知失败（用户 %d）: %v", u.ID, err)
		}
	}
	return rows.Err()
}

// notifyUser 是通知扫描的用户行。
type notifyUser struct {
	ID                  int64
	Email               string
	Token               string
	TrafficLimit        int64
	TrafficUsed         int64
	ExpiredAt           *time.Time
	PlanID              sql.NullInt64
	MonthlyTrafficUsed  int64
	MonthlyTrafficLimit int64
	PlanName            string
}

// scanNotifyUser 从查询结果扫一行用户。
func scanNotifyUser(rows *sql.Rows) (*notifyUser, error) {
	var u notifyUser
	var expiredAt sql.NullTime
	if err := rows.Scan(&u.ID, &u.Email, &u.Token, &u.TrafficLimit, &u.TrafficUsed,
		&expiredAt, &u.PlanID, &u.MonthlyTrafficUsed, &u.MonthlyTrafficLimit); err != nil {
		return nil, err
	}
	if expiredAt.Valid {
		t := expiredAt.Time
		u.ExpiredAt = &t
	}
	return &u, nil
}

// dispatchNotifyOnce 对齐 MailNotifyScanJob::dispatchOnce()。
func (r *commandRunner) dispatchNotifyOnce(ctx context.Context, scene string, u *notifyUser) error {
	if u.Email == "" {
		return nil
	}

	// 渲染
	rendered := r.renderNotify(ctx, scene, u)
	if rendered == nil {
		return nil
	}

	// 解析收件人
	to := rendered.ToEmail
	if rendered.ToType == "admin" {
		to = r.siteConfig(ctx, "notify_admin_email", "")
	} else {
		to = u.Email
	}
	if to == "" {
		return nil
	}

	// 防重标记
	claimKey := r.mailClaimKey(scene, rendered.ToType, to, u.ID)
	ok, err := r.cacheAdd(ctx, claimKey, "1", 365*24*time.Hour)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	// 入队
	uid := uint64(u.ID)
	job := MailJob{
		ToEmail:  to,
		Subject:  rendered.Subject,
		HTMLBody: rendered.Body,
		Type:     "notify",
		UserID:   &uid,
		Scene:    &scene,
	}
	if r.deps.MailQueue != nil {
		return r.deps.MailQueue.EnqueueMail(job)
	}
	r.logf("[mail:notify-scan] MailQueue 未配置，跳过 %s → %s", scene, to)
	return nil
}

// notifyRendered 是渲染结果。
type notifyRendered struct {
	Subject string
	Body    string
	ToType  string
	ToEmail string
}

// renderNotify 对齐 MailNotifyService::render()。
func (r *commandRunner) renderNotify(ctx context.Context, scene string, u *notifyUser) *notifyRendered {
	def := mailNotifySceneDefaults[scene]
	if def.Title == "" {
		return nil
	}

	title := r.siteConfig(ctx, "notify_"+scene+"_title", def.Title)
	if title == "" {
		title = def.Title
	}
	body := r.siteConfig(ctx, "notify_"+scene+"_body", def.Body)
	if body == "" {
		body = def.Body
	}
	toType := r.siteConfig(ctx, "notify_"+scene+"_to", def.To)
	if toType == "" {
		toType = def.To
	}

	// 渲染变量
	limit := u.TrafficLimit
	used := u.TrafficUsed
	var percent float64
	if limit > 0 {
		percent = math.Round(float64(used)/float64(limit)*100*10) / 10
	}

	expireDate := "—"
	daysLeft := 0
	if u.ExpiredAt != nil {
		expireDate = u.ExpiredAt.Format("2006-01-02")
		daysLeft = int(math.Ceil(u.ExpiredAt.Sub(r.now().UTC()).Hours() / 24))
	}

	siteTitle := r.siteConfig(ctx, "site_title", "ControlHub")
	subURL := ""
	if u.Token != "" {
		subURL = "/api/sub/" + u.Token
	}

	planName := "无"
	if u.PlanID.Valid {
		var name string
		err := r.deps.DB.QueryRowContext(ctx,
			"SELECT name FROM plans WHERE id = ?", u.PlanID.Int64).Scan(&name)
		if err == nil {
			planName = name
		}
	}

	vars := map[string]string{
		"{{email}}":         u.Email,
		"{{user_id}}":       fmt.Sprintf("%d", u.ID),
		"{{site_title}}":    siteTitle,
		"{{used}}":          formatBytes(used),
		"{{limit}}":         formatBytes(limit),
		"{{percent}}":       fmt.Sprintf("%g", percent),
		"{{plan_name}}":     planName,
		"{{expire_date}}":   expireDate,
		"{{days_left}}":     fmt.Sprintf("%d", daysLeft),
		"{{subscribe_url}}": subURL,
	}

	return &notifyRendered{
		Subject: strtr(title, vars),
		Body:    strtr(body, vars),
		ToType:  toType,
	}
}

// mailClaimKey 对齐 MailNotifyService::claimKey()。
func (r *commandRunner) mailClaimKey(scene, toType, to string, userID int64) string {
	var who string
	if toType == "admin" {
		who = fmt.Sprintf("admin:%d", userID)
	} else {
		who = "email:" + strings.ToLower(strings.TrimSpace(to))
	}
	return fmt.Sprintf("mail_notify:%s:%s", scene, who)
}

// ── 7. mail:schedule-scan ──────────────────────────────────────────

// runMailScheduleScan 对齐 MailScheduleJob::handle()。
// 按月定时发信：到点且当月没发过就把配好的那封信入队。
func (r *commandRunner) runMailScheduleScan(ctx context.Context) error {
	if r.siteConfig(ctx, "schedule_mail_enabled", "") != "1" {
		return nil
	}

	cfgDay := r.siteConfigInt(ctx, "schedule_mail_day", 1)
	cfgTime := r.siteConfig(ctx, "schedule_mail_time", "09:00")
	now := r.now().UTC()

	// 计算本月发送时刻
	lastDay := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if cfgDay < 1 || cfgDay > lastDay {
		return nil // 配置的第 N 天当月不存在
	}

	var hour, minute int
	fmt.Sscanf(cfgTime, "%d:%d", &hour, &minute)
	sendAt := time.Date(now.Year(), now.Month(), cfgDay, hour, minute, 0, 0, time.UTC)

	if now.Before(sendAt) {
		return nil // 还没到点
	}

	// 开启时间晚于本月到点时刻 → 当月不补发
	armedRaw := r.siteConfig(ctx, "schedule_mail_armed_at", "")
	if armedRaw != "" {
		if armedAt, err := time.Parse("2006-01-02 15:04:05", armedRaw); err == nil {
			if armedAt.After(sendAt) {
				return nil
			}
		}
	}

	// 当月已发过 → 跳过
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	var mailLogCount int
	err := r.deps.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mail_logs WHERE type = 'schedule' AND created_at >= ? AND created_at < ?`,
		monthStart, monthEnd).Scan(&mailLogCount)
	if err != nil {
		return err
	}
	if mailLogCount > 0 {
		return nil
	}

	// 读配置
	cfgJSON := r.siteConfig(ctx, "schedule_mail_config", "")
	if cfgJSON == "" {
		r.logf("MailScheduleJob: 定时发信未配置，跳过本轮")
		return nil
	}
	var mailCfg struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(cfgJSON), &mailCfg); err != nil {
		r.logf("MailScheduleJob: 解析定时发信配置失败: %v", err)
		return nil
	}
	if mailCfg.Subject == "" || mailCfg.Body == "" {
		r.logf("MailScheduleJob: 定时发信未配置标题/正文，跳过本轮")
		return nil
	}

	// 解析收件人
	users, err := r.resolveScheduleRecipients(ctx, cfgJSON)
	if err != nil || len(users) == 0 {
		r.logf("MailScheduleJob: 没有符合条件的收件人，跳过本轮")
		return nil
	}

	// 去重标记（入队那一刻就落）
	markKey := "mail_schedule:" + now.Format("200601")
	markTTL := time.Date(now.Year(), now.Month()+1, 1, 0, 1, 0, 0, time.UTC).Sub(now)
	ok, err := r.cacheAdd(ctx, markKey, "1", markTTL)
	if err != nil || !ok {
		return err
	}

	// 入队
	batchID := now.Unix()
	bid := batchID
	now2 := now
	for i, user := range users {
		uid := uint64(user.ID)
		job := MailJob{
			ToEmail:  user.Email,
			Subject:  mailCfg.Subject,
			HTMLBody: r.renderForSchedule(ctx, mailCfg.Body, user, now2),
			Type:     "schedule",
			UserID:   &uid,
			BatchID:  &bid,
		}
		_ = i
		if r.deps.MailQueue != nil {
			if err := r.deps.MailQueue.EnqueueMail(job); err != nil {
				r.logf("MailScheduleJob: 入队失败: %v", err)
			}
		}
	}

	r.logf("MailScheduleJob: 定时发信已入队（batch=%d, total=%d）", batchID, len(users))
	return nil
}

// resolveScheduleRecipients 解析定时发信收件人（mode=all 简化）。
func (r *commandRunner) resolveScheduleRecipients(ctx context.Context, cfgJSON string) ([]notifyUser, error) {
	var cfg struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal([]byte(cfgJSON), &cfg)

	// mode=all：全部启用且有邮箱的用户
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, email, token, traffic_limit, traffic_used, expired_at,
		        plan_id, monthly_traffic_used, monthly_traffic_limit
		 FROM users WHERE enabled = 1 AND email != '' ORDER BY id LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []notifyUser
	for rows.Next() {
		u, err := scanNotifyUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, rows.Err()
}

// renderForSchedule 渲染定时发信正文。
func (r *commandRunner) renderForSchedule(ctx context.Context, body string, u notifyUser, now time.Time) string {
	vars := map[string]string{
		"{{email}}":     u.Email,
		"{{user_id}}":   fmt.Sprintf("%d", u.ID),
		"{{site_title}}": r.siteConfig(ctx, "site_title", "ControlHub"),
		"{{used}}":      formatBytes(u.TrafficUsed),
		"{{limit}}":     formatBytes(u.TrafficLimit),
		"{{plan_name}}": "无",
	}
	if u.PlanID.Valid {
		var name string
		if err := r.deps.DB.QueryRowContext(ctx,
			"SELECT name FROM plans WHERE id = ?", u.PlanID.Int64).Scan(&name); err == nil {
			vars["{{plan_name}}"] = name
		}
	}
	return strtr(body, vars)
}

// ── 8. third-party:fetch ───────────────────────────────────────────

// runThirdPartyFetch 对齐 FetchThirdPartySubsCommand::handle()。
// 抓取所有启用的第三方订阅 → 解析 → 缓存节点链接。
func (r *commandRunner) runThirdPartyFetch(ctx context.Context) error {
	// 查启用的订阅
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, name, url FROM third_party_subs WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("查询第三方订阅失败: %w", err)
	}
	type subRow struct {
		ID   int64
		Name string
		URL  string
	}
	var subs []subRow
	for rows.Next() {
		var s subRow
		if err := rows.Scan(&s.ID, &s.Name, &s.URL); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, s)
	}
	rows.Close()

	if len(subs) == 0 {
		r.logf("没有启用的第三方订阅，跳过")
		return nil
	}

	r.logf("%s 开始拉取 %d 条第三方订阅...", r.now().Format("2006-01-02 15:04:05"), len(subs))

	for _, sub := range subs {
		// 拉取订阅（HTTP fetch + 解析）
		links, fetchErr := r.fetchThirdPartyLinks(ctx, sub.URL)
		now := r.now().UTC()
		if fetchErr != nil || len(links) == 0 {
			msg := ""
			if fetchErr != nil {
				msg = fetchErr.Error()
			} else {
				msg = "拉取成功但未解析到任何节点链接"
			}
			// 保留上次成功缓存，只记错误
			_, _ = r.deps.DB.ExecContext(ctx,
				`UPDATE third_party_subs SET last_status = 'failed', last_error = ?, last_fetched_at = ?, updated_at = ?
				 WHERE id = ?`, truncateStr(msg, 500), now, now, sub.ID)
			r.logf("  %s: 失败（%s）", sub.Name, msg)
			continue
		}

		cached := strings.Join(links, "\n")
		_, err := r.deps.DB.ExecContext(ctx,
			`UPDATE third_party_subs SET last_status = 'success', last_error = NULL,
			 node_count = ?, cached_links = ?, last_fetched_at = ?, updated_at = ?
			 WHERE id = ?`, len(links), cached, now, now, sub.ID)
		if err != nil {
			r.logf("  %s: 保存失败（%v）", sub.Name, err)
			continue
		}
		r.logf("  %s: 成功（%d 节点）", sub.Name, len(links))
	}

	r.logf("拉取完成")
	return nil
}

// fetchThirdPartyLinks 拉取第三方订阅并解析节点链接。
func (r *commandRunner) fetchThirdPartyLinks(ctx context.Context, url string) ([]string, error) {
	// 订阅内容通常是 base64 编码的 URI 列表
	body, err := httpGet(ctx, url, 15*time.Second)
	if err != nil {
		return nil, err
	}

	// 尝试 base64 解码
	decoded := tryBase64Decode(string(body))
	if decoded == "" {
		decoded = string(body)
	}

	var links []string
	for _, line := range strings.Split(decoded, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "vmess://") || strings.HasPrefix(line, "vless://") ||
			strings.HasPrefix(line, "trojan://") || strings.HasPrefix(line, "ss://") ||
			strings.HasPrefix(line, "ssr://") || strings.HasPrefix(line, "hysteria2://") ||
			strings.HasPrefix(line, "hy2://") {
			links = append(links, line)
		}
	}
	return links, nil
}

// ── 9. xray:warp-rotate ────────────────────────────────────────────

// runXrayWarpRotate 对齐 XrayWarpRotate::handle()。
// 到点条件：已启用 + last_rotate_at（或 updated_at）距今 ≥ auto_rotate_hours。
func (r *commandRunner) runXrayWarpRotate(ctx context.Context) error {
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT wa.id, wa.node_id, wa.auto_rotate_hours, wa.last_rotate_at, wa.updated_at,
		        n.name, n.enabled
		 FROM node_warp_accounts wa
		 JOIN nodes n ON n.id = wa.node_id
		 WHERE wa.auto_rotate_hours > 0 AND wa.enabled = 1 AND n.enabled = 1
		 ORDER BY wa.id`)
	if err != nil {
		return fmt.Errorf("查询 WARP 账户失败: %w", err)
	}
	type warpRow struct {
		ID              int64
		NodeID          int64
		AutoRotateHours int64
		LastRotateAt    sql.NullTime
		UpdatedAt       time.Time
		NodeName        string
	}
	var accounts []warpRow
	for rows.Next() {
		var a warpRow
		if err := rows.Scan(&a.ID, &a.NodeID, &a.AutoRotateHours, &a.LastRotateAt,
			&a.UpdatedAt, &a.NodeName, new(sql.NullInt64)); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, a)
	}
	rows.Close()

	now := r.now().UTC()
	rotated := 0
	for _, a := range accounts {
		base := a.UpdatedAt
		if a.LastRotateAt.Valid {
			base = a.LastRotateAt.Time
		}
		due := base.Add(time.Duration(a.AutoRotateHours) * time.Hour)
		if now.Before(due) {
			continue
		}

		// 换 IP：重新注册设备 → 重组 wireguard 出站 → bump config（PHP WarpService::rotate）。
		// 失败也推进 last_rotate_at，否则每 5 分钟重试一次、把 CF 接口打到限流。
		if r.deps.RotateWarp == nil {
			r.logf("RotateWarp 未配置，跳过 node=%d", a.NodeID)
			continue
		}
		if err := r.deps.RotateWarp(ctx, uint64(a.NodeID)); err != nil {
			r.logf("node#%d WARP 换 IP 失败（%s）: %v", a.NodeID, a.NodeName, err)
		} else {
			r.logf("node#%d WARP 已换 IP（%s）", a.NodeID, a.NodeName)
			rotated++
		}

		// 更新 last_rotate_at（成功失败都推进，对齐 PHP 的失败也写）
		_, _ = r.deps.DB.ExecContext(ctx,
			"UPDATE node_warp_accounts SET last_rotate_at = ?, updated_at = ? WHERE id = ?",
			now, now, a.ID)
	}

	r.logf("完成：%d/%d 个账户已换 IP", rotated, len(accounts))
	return nil
}

// ── 10. xray:refresh-outbound-subs ─────────────────────────────────

// runXrayRefreshOutboundSubs 对齐 RefreshOutboundSubscriptions::handle()。
// 逐条检查到期项 → 拉取 → 解析 → 缓存；内容变化则 bump 节点 config_version。
func (r *commandRunner) runXrayRefreshOutboundSubs(ctx context.Context) error {
	var enabled int
	if err := r.deps.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM xray_outbound_subscriptions WHERE enabled = 1").Scan(&enabled); err != nil {
		return err
	}
	if enabled == 0 {
		r.logf("没有启用的出站订阅，跳过")
		return nil
	}

	r.logf("%s 检查 %d 条出站订阅...", r.now().Format("2006-01-02 15:04:05"), enabled)

	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, node_id, url, update_interval, last_fetched_at, content_hash, link_identities, tag_prefix
		 FROM xray_outbound_subscriptions WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return err
	}
	type subRow struct {
		ID             int64
		NodeID         int64
		URL            string
		UpdateInterval int64
		LastFetchedAt  sql.NullTime
		ContentHash    sql.NullString
		LinkIDentities sql.NullString
		TagPrefix      sql.NullString
	}
	var subs []subRow
	for rows.Next() {
		var s subRow
		if err := rows.Scan(&s.ID, &s.NodeID, &s.URL, &s.UpdateInterval,
			&s.LastFetchedAt, &s.ContentHash, &s.LinkIDentities, &s.TagPrefix); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, s)
	}
	rows.Close()

	now := r.now().UTC()
	refreshed := 0

	for _, sub := range subs {
		// 检查到期
		if sub.LastFetchedAt.Valid {
			due := sub.LastFetchedAt.Time.Add(time.Duration(sub.UpdateInterval) * time.Minute)
			if now.Before(due) {
				continue
			}
		}

		// 拉取并解析
		outbounds, fetchErr := r.fetchOutboundSub(ctx, sub.URL)
		if fetchErr != nil || len(outbounds) == 0 {
			msg := ""
			if fetchErr != nil {
				msg = fetchErr.Error()
			} else {
				msg = "拉取成功但未解析到任何可用出站"
			}
			_, _ = r.deps.DB.ExecContext(ctx,
				`UPDATE xray_outbound_subscriptions
				 SET last_status = 'failed', last_error = ?, last_fetched_at = ?, updated_at = ?
				 WHERE id = ?`, truncateStr(msg, 500), now, now, sub.ID)
			continue
		}

		// 序列化并计算哈希
		fetchedJSON, _ := json.Marshal(outbounds)
		hash := simpleSHA1(string(fetchedJSON))

		// 保存
		_, err := r.deps.DB.ExecContext(ctx,
			`UPDATE xray_outbound_subscriptions
			 SET last_status = 'success', last_error = NULL,
			     fetched = ?, content_hash = ?, node_count = ?,
			     last_fetched_at = ?, updated_at = ?
			 WHERE id = ?`, string(fetchedJSON), hash, len(outbounds), now, now, sub.ID)
		if err != nil {
			r.logf("保存出站订阅 #%d 失败: %v", sub.ID, err)
			continue
		}

		// 内容变化才 bump config_version（复用 NodeStore.BumpConfigVersion：
		// 行锁内递增 driver_config JSON 的 config_version 键 + 写 config_changed_at）。
		if !sub.ContentHash.Valid || sub.ContentHash.String != hash {
			if r.deps.BumpConfigVersion != nil {
				if _, berr := r.deps.BumpConfigVersion(ctx, uint64(sub.NodeID)); berr != nil {
					r.logf("bump config_version 失败 node=%d: %v", sub.NodeID, berr)
				}
			} else {
				r.logf("BumpConfigVersion 未配置，跳过 node=%d", sub.NodeID)
			}
		}
		refreshed++
	}

	r.logf("完成：已刷新 %d 条", refreshed)
	return nil
}

// fetchOutboundSub 拉取出站订阅并解析出站列表。
func (r *commandRunner) fetchOutboundSub(ctx context.Context, url string) ([]map[string]any, error) {
	body, err := httpGet(ctx, url, 30*time.Second)
	if err != nil {
		return nil, err
	}

	decoded := tryBase64Decode(string(body))
	if decoded == "" {
		decoded = string(body)
	}

	var outbounds []map[string]any
	for _, line := range strings.Split(decoded, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 简化解析：每行一个 URI，包装为 {tag, uri}
		if strings.Contains(line, "://") {
			outbounds = append(outbounds, map[string]any{
				"tag": fmt.Sprintf("out-%d", len(outbounds)),
				"uri": line,
			})
		}
	}
	return outbounds, nil
}

// ── 11. geodata:update ─────────────────────────────────────────────

// runGeodataUpdate 对齐 GeodataUpdateCommand::handle()。
func (r *commandRunner) runGeodataUpdate(ctx context.Context, args map[string]string) error {
	if r.deps.Geodata == nil {
		r.logf("Geodata 服务未配置，跳过")
		return nil
	}

	results, err := r.deps.Geodata.Update(ctx)
	if err != nil {
		return fmt.Errorf("geo 数据更新失败: %w", err)
	}

	updated := 0
	for name, res := range results {
		detail := res.Message
		if detail == "" {
			detail = fmt.Sprintf("%d 字节", res.Size)
		}
		if res.Status == "updated" {
			updated++
			r.logf("%s：已更新（%s）", name, detail)
		} else {
			r.logf("%s：%s（%s）", name, res.Status, detail)
		}
	}

	forceDispatch := args["--force-dispatch"] == "1"
	shouldDispatch := forceDispatch || (args["--dispatch"] == "1" && updated > 0)

	if shouldDispatch {
		queued, skipped, missing, err := r.deps.Geodata.DispatchToNodes(ctx)
		if err != nil {
			return fmt.Errorf("分发 geo 数据失败: %w", err)
		}
		if missing {
			r.logf("面板上没有 geo 文件，跳过分发。")
			return nil
		}
		r.logf("已分发到 %d 个节点（跳过 %d 个已有在途任务的节点）。", queued, skipped)
	} else {
		if updated > 0 {
			r.logf("（未分发；如需推送节点请加 --dispatch）")
		} else {
			r.logf("无更新。")
		}
	}
	return nil
}

// ── 12. users:reconcile-enable ─────────────────────────────────────

// nodeRow 是节点行（3x-ui 客户端构造用）。
type nodeRow struct {
	ID         int64
	Name       string
	Scheme     string
	Host       string
	Port       int64
	BasePath   string
	Username   string
	Password   sql.NullString
	APIKey     sql.NullString
	VerifySSL  bool
	DriverType sql.NullString
}

// runUsersReconcileEnable 对齐 ReconcileClientEnableCommand::handle()。
// Re-enable clients disabled on nodes while the panel still allows them.
func (r *commandRunner) runUsersReconcileEnable(ctx context.Context, args map[string]string) error {
	dry := args["--dry-run"] == "1" || args["--dry-run"] == "true"
	nodeFilter := int64(0)
	if v, ok := args["--node"]; ok {
		fmt.Sscanf(v, "%d", &nodeFilter)
	}

	nowMs := r.now().UnixMilli()
	checked, fixed, failed, skipped := 0, 0, 0, 0

	// 查启用的非 xray 节点
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, name, scheme, host, port, web_base_path, username, password, api_key, verify_ssl, driver_type
		 FROM nodes WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return err
	}
	var nodes []nodeRow
	for rows.Next() {
		var n nodeRow
		if err := rows.Scan(&n.ID, &n.Name, &n.Scheme, &n.Host, &n.Port, &n.BasePath,
			&n.Username, &n.Password, &n.APIKey, &n.VerifySSL, &n.DriverType); err != nil {
			rows.Close()
			return err
		}
		// 跳过 xray 节点（PHP: filter(fn ($n) => ! $n->isXray())）
		if n.DriverType.Valid && n.DriverType.String == "xray" {
			continue
		}
		if nodeFilter > 0 && n.ID != nodeFilter {
			continue
		}
		nodes = append(nodes, n)
	}
	rows.Close()

	// 查符合条件的用户
	userRows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, protocol FROM users
		 WHERE plan_id IS NOT NULL AND enabled = 1 AND traffic_disabled_at IS NULL
		   AND (expired_at IS NULL OR expired_at > ?)
		   AND (traffic_limit = 0 OR traffic_used < traffic_limit)
		 ORDER BY id LIMIT 10000`, r.now().UTC())
	if err != nil {
		return err
	}
	type userRow struct {
		ID       int64
		Protocol string
	}
	var users []userRow
	for userRows.Next() {
		var u userRow
		if err := userRows.Scan(&u.ID, &u.Protocol); err != nil {
			userRows.Close()
			return err
		}
		users = append(users, u)
	}
	userRows.Close()

	for _, node := range nodes {
		client, err := r.buildThreexuiClient(node)
		if err != nil {
			r.logf("构造节点 #%d 客户端失败: %v", node.ID, err)
			continue
		}

		for _, user := range users {
			email := clientEmail(user.ID)
			traffic, err := client.GetClientTraffic(ctx, email)
			if err != nil {
				continue
			}
			if traffic == nil {
				continue
			}
			checked++

			enable, _ := traffic["enable"].(bool)
			if enable {
				continue
			}

			up := toInt64(traffic["up"])
			down := toInt64(traffic["down"])
			total := toInt64(traffic["total"])
			expiry := toInt64(traffic["expiryTime"])

			used := up + down
			if total > 0 && used >= total {
				skipped++
				continue
			}
			if expiry > 0 && expiry <= nowMs {
				skipped++
				continue
			}

			if dry {
				r.logf("[dry] %s(#%d) %s disabled on node -> re-enable", node.Name, node.ID, email)
				fixed++
				continue
			}

			// toggleClient：全部启用节点确认才算成功
			if r.toggleClientAllNodes(ctx, user.ID, user.Protocol, true) {
				fixed++
			} else {
				failed++
				r.logf("%s %s: 未全部节点确认，下一轮重试", node.Name, email)
			}
		}
	}

	prefix := ""
	if dry {
		prefix = "[dry-run] "
	}
	r.logf("%schecked=%d re-enabled=%d failed=%d skipped_over_or_expired=%d",
		prefix, checked, fixed, failed, skipped)
	return nil
}

// buildThreexuiClient 构造 3x-ui 客户端。
func (r *commandRunner) buildThreexuiClient(n nodeRow) (*threexui.Client, error) {
	row := threexui.NodeRow{
		Scheme:      n.Scheme,
		Host:        n.Host,
		Port:        int(n.Port),
		WebBasePath: n.BasePath,
		Username:    n.Username,
		VerifySSL:   n.VerifySSL,
	}
	if n.Password.Valid {
		v := n.Password.String
		row.PasswordCipher = &v
	}
	if n.APIKey.Valid {
		v := n.APIKey.String
		row.APIKeyCipher = &v
	}
	return threexui.NewForNode(row, r.deps.AppKey)
}

// toggleClientAllNodes 对齐 BanService::toggleClient：全部启用节点确认才算成功。
//
// 直接委托 collector.ToggleClient —— 那里已含 3x-ui（get+normalize+update）与 xray
// （adu/rmu 指令通道）两条分支，以及「全部确认才写 traffic_disabled_at」的判定。
// 旧实现在这里另写了一份只处理 3x-ui 的版本，遇到 xray 节点直接 continue，
// 于是 confirmed 永远小于 total、reconcile 每轮都判失败并重试。
func (r *commandRunner) toggleClientAllNodes(ctx context.Context, userID int64, protocol string, enable bool) bool {
	_ = protocol
	if r.deps.ToggleClient == nil {
		r.logf("ToggleClient 未配置，无法切换 user=%d 的 client 开关", userID)
		return false
	}
	ok, err := r.deps.ToggleClient(ctx, userID, enable)
	if err != nil {
		r.logf("切换 client 开关失败（user=%d enable=%v）: %v", userID, enable, err)
		return false
	}
	return ok
}

// ── 13. xray:reality-audit ─────────────────────────────────────────

// runXrayRealityAudit 对齐 RealityAuditCommand::handle()。
// REALITY 入站巡检：节点侧探测 dest，告警 SNI 不匹配与证书链超限。
func (r *commandRunner) runXrayRealityAudit(ctx context.Context, args map[string]string) error {
	nodeFilter := int64(0)
	if v, ok := args["--node"]; ok {
		fmt.Sscanf(v, "%d", &nodeFilter)
	}
	waitSec := 20.0
	if v, ok := args["--wait"]; ok {
		fmt.Sscanf(v, "%g", &waitSec)
	}

	// 查启用的 xray 节点
	nodeRows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, name FROM nodes WHERE enabled = 1 AND driver_type = 'xray' ORDER BY id`)
	if err != nil {
		return err
	}
	type xrayNode struct {
		ID   int64
		Name string
	}
	var xrayNodes []xrayNode
	for nodeRows.Next() {
		var n xrayNode
		if err := nodeRows.Scan(&n.ID, &n.Name); err != nil {
			nodeRows.Close()
			return err
		}
		if nodeFilter > 0 && n.ID != nodeFilter {
			continue
		}
		xrayNodes = append(xrayNodes, n)
	}
	nodeRows.Close()

	if len(xrayNodes) == 0 {
		r.logf("没有启用中的 REALITY 入站")
		return nil
	}

	// 查 REALITY 入站
	alerts := 0
	for _, node := range xrayNodes {
		ibRows, err := r.deps.DB.QueryContext(ctx,
			`SELECT id, tag, port, stream_settings FROM xray_inbounds
			 WHERE node_id = ? AND enabled = 1 ORDER BY sort, id`, node.ID)
		if err != nil {
			continue
		}

		type inbound struct {
			ID             int64
			Tag            string
			Port           int64
			StreamSettings string
		}
		var inbounds []inbound
		for ibRows.Next() {
			var ib inbound
			if err := ibRows.Scan(&ib.ID, &ib.Tag, &ib.Port, &ib.StreamSettings); err != nil {
				continue
			}
			// 检查 security == "reality"
			var stream map[string]any
			if json.Unmarshal([]byte(ib.StreamSettings), &stream) == nil {
				if sec, _ := stream["security"].(string); sec == "reality" {
					inbounds = append(inbounds, ib)
				}
			}
		}
		ibRows.Close()

		if len(inbounds) == 0 {
			continue
		}

		// 收集 dest 目标
		targets := map[string]bool{}
		for _, ib := range inbounds {
			var stream map[string]any
			if json.Unmarshal([]byte(ib.StreamSettings), &stream) != nil {
				continue
			}
			realitySettings, _ := stream["realitySettings"].(map[string]any)
			if realitySettings == nil {
				continue
			}
			dest, _ := realitySettings["dest"].(string)
			dest = strings.TrimSpace(dest)
			if dest != "" {
				host := xrayHostOf(dest)
				if host != "" {
					targets[host] = true
				}
			}
		}

		if len(targets) == 0 {
			continue
		}

		// 下发 reality_scan 指令并等回执
		targetList := make([]string, 0, len(targets))
		for t := range targets {
			targetList = append(targetList, t)
		}
		sort.Strings(targetList)

		results := r.probeRealityScan(ctx, node.ID, targetList, waitSec)
		if results == nil {
			r.logf("%s：节点未回执（离线 / agent 过旧），本轮跳过", node.Name)
			continue
		}

		for _, ib := range inbounds {
			for _, issue := range r.realityIssuesFor(ib, results) {
				alerts++
				msg := fmt.Sprintf("%s（节点 %s，入站 %s，端口 %d）",
					issue, node.Name, ib.Tag, ib.Port)
				r.logf("[WARN] %s", msg)
				r.recordRealityAlert(ctx, msg)
			}
		}
	}

	if alerts == 0 {
		r.logf("REALITY 巡检通过，未发现问题")
	} else {
		r.logf("REALITY 巡检完成，%d 项告警已记入操作日志", alerts)
	}
	return nil
}

// realityIssuesFor 检查单条入站的问题清单。
func (r *commandRunner) realityIssuesFor(ib struct {
	ID             int64
	Tag            string
	Port           int64
	StreamSettings string
}, results map[string]map[string]any) []string {
	var stream map[string]any
	if json.Unmarshal([]byte(ib.StreamSettings), &stream) != nil {
		return nil
	}
	realitySettings, _ := stream["realitySettings"].(map[string]any)
	if realitySettings == nil {
		return nil
	}

	dest, _ := realitySettings["dest"].(string)
	dest = strings.TrimSpace(dest)
	host := xrayHostOf(dest)
	row := results[host]

	var issues []string
	if row == nil {
		return []string{fmt.Sprintf("dest 探测未返回结果：%s", dest)}
	}

	if feasible, _ := row["feasible"].(bool); !feasible {
		reason, _ := row["reason"].(string)
		msg := "dest 无法完成 REALITY 所需的 TLS 握手"
		if reason = strings.TrimSpace(reason); reason != "" {
			msg += "：" + reason
		}
		issues = append(issues, msg)
	}

	if chainWarn, _ := row["chain_warn"].(bool); chainWarn {
		chainBytes := toInt64(row["chain_bytes"])
		issues = append(issues, fmt.Sprintf(
			"dest 证书链 %d 字节超过 REALITY 的 8192 上限，握手会被截断（Xray-core #6356）", chainBytes))
	}

	// SAN 检查
	var san []string
	if sanArr, ok := row["server_names"].([]any); ok {
		for _, name := range sanArr {
			if s, ok := name.(string); ok {
				s = strings.ToLower(strings.TrimSpace(s))
				if s != "" {
					san = append(san, s)
				}
			}
		}
	}

	if len(san) > 0 {
		if snArr, ok := realitySettings["serverNames"].([]any); ok {
			for _, sn := range snArr {
				s, ok := sn.(string)
				if !ok {
					continue
				}
				s = strings.ToLower(strings.TrimSpace(s))
				if s == "" {
					continue
				}
				found := false
				for _, name := range san {
					if name == s {
						found = true
						break
					}
				}
				if !found {
					issues = append(issues, fmt.Sprintf(
						"serverName %s 不在 dest 证书覆盖范围内（被分到它的用户会握手失败）", s))
				}
			}
		}
	}

	return issues
}

// probeRealityScan 下发 reality_scan 指令并等回执。
func (r *commandRunner) probeRealityScan(ctx context.Context, nodeID int64, targets []string, waitSec float64) map[string]map[string]any {
	// 创建指令
	payload, _ := json.Marshal(map[string]any{"targets": targets})
	var cmdID int64
	err := r.deps.DB.QueryRowContext(ctx,
		`INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at)
		 VALUES (?, NULL, 'reality_scan', ?, 'pending', ?, ?)`,
		nodeID, string(payload), r.now().UTC(), r.now().UTC()).Scan(&cmdID)
	if err != nil {
		// MySQL 不支持 RETURNING，用 LastInsertId
		res, ierr := r.deps.DB.ExecContext(ctx,
			`INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at)
			 VALUES (?, NULL, 'reality_scan', ?, 'pending', ?, ?)`,
			nodeID, string(payload), r.now().UTC(), r.now().UTC())
		if ierr != nil {
			return nil
		}
		cmdID, _ = res.LastInsertId()
	}

	deadline := time.Now().Add(time.Duration(waitSec * float64(time.Second)))
	for time.Now().Before(deadline) {
		var status, result string
		err := r.deps.DB.QueryRowContext(ctx,
			"SELECT status, COALESCE(result, '') FROM node_api_commands WHERE id = ?", cmdID).
			Scan(&status, &result)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}

		if status == "failed" {
			if !strings.Contains(result, "unknown action") {
				r.logf("扫描失败 %s", result)
			}
			return nil
		}

		if status == "success" {
			var rows []map[string]any
			if json.Unmarshal([]byte(result), &rows) != nil {
				return nil
			}
			resultMap := map[string]map[string]any{}
			for _, row := range rows {
				target, _ := row["target"].(string)
				key := xrayHostOf(target)
				if key != "" {
					resultMap[key] = row
				}
			}
			return resultMap
		}

		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

// recordRealityAlert 写操作日志（同一问题 24h 内只留一条）。
func (r *commandRunner) recordRealityAlert(ctx context.Context, message string) {
	var n int
	err := r.deps.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM operation_logs
		 WHERE type = 'system' AND action = 'REALITY 巡检' AND error = ?
		   AND created_at >= ?`,
		message, r.now().UTC().AddDate(0, 0, -1)).Scan(&n)
	if err != nil || n > 0 {
		return
	}

	_, _ = r.deps.DB.ExecContext(ctx,
		`INSERT INTO operation_logs (type, actor_name, action, method, path, status, error, created_at)
		 VALUES ('system', 'reality-audit', 'REALITY 巡检', 'CLI', 'xray:reality-audit', 500, ?, ?)`,
		message, r.now().UTC())
}

// ── 辅助工具 ───────────────────────────────────────────────────────

// mailNotifySceneDefaults 是每个场景的出厂标题/正文/收件人。
var mailNotifySceneDefaults = map[string]struct {
	Title string
	Body  string
	To    string
}{
	"traffic_exhausted": {
		Title: "您的流量已用尽",
		Body:  "<p>您好，</p><p>您的流量已用尽，流量已被暂停。请续费或重置流量后继续使用。</p>",
		To:    "user",
	},
	"traffic_almost": {
		Title: "您的流量即将用尽",
		Body:  "<p>您好，</p><p>您的流量已使用 {{percent}}%，请及时续费，避免服务中断。</p>",
		To:    "user",
	},
	"expiring": {
		Title: "您的套餐即将到期",
		Body:  "<p>您好，</p><p>您的套餐将于 {{expire_date}} 到期（剩余 {{days_left}} 天），请及时续费。</p>",
		To:    "user",
	},
	"expired": {
		Title: "您的套餐已到期",
		Body:  "<p>您好，</p><p>您的套餐已于 {{expire_date}} 到期，流量已关闭。续费后可继续使用。</p>",
		To:    "user",
	},
}

// formatBytes 字节转可读文本。
func formatBytes(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := int(math.Log(float64(bytes)) / math.Log(1024))
	if i < 0 {
		i = 0
	}
	if i >= len(units) {
		i = len(units) - 1
	}
	return fmt.Sprintf("%.2f %s", float64(bytes)/math.Pow(1024, float64(i)), units[i])
}

// strtr 替换模板变量。
func strtr(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// truncateStr 截断字符串。
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// clientEmail 生成客户端 email 标识。
func clientEmail(userID int64) string {
	return fmt.Sprintf("user_%d", userID)
}

// xrayHostOf 提取 dest 中的主机名。
func xrayHostOf(dest string) string {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return ""
	}
	// 去掉端口
	if idx := strings.LastIndex(dest, ":"); idx > 0 {
		host := dest[:idx]
		// 确保不是 IPv6
		if !strings.Contains(host, "]") {
			return strings.ToLower(host)
		}
	}
	return strings.ToLower(dest)
}

// tryBase64Decode 尝试 base64 解码。
func tryBase64Decode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	decoded, err := decodeBase64(s)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// simpleSHA1 计算简单 SHA1 哈希（hex）。
func simpleSHA1(s string) string {
	return cryptoSHA1(s)
}

// toInt64Value 从 any 转 int64（与 async.go 的 toInt64 同义，避免重复声明）。
func toInt64Value(v any) int64 {
	return toInt64(v)
}

// ensureDir 确保目录存在。
func ensureDir(dir string) error {
	return ensureDirImpl(dir)
}

// removeIfExists 删除文件（忽略不存在）。
func removeIfExists(path string) {
	removeIfExistsImpl(path)
}

// fileSize 返回文件大小。
func fileSize(path string) int64 {
	return fileSizeImpl(path)
}

// pruneOldBackups 按文件名倒序只保留最新 keep 份。
func pruneOldBackups(dir string, keep int) {
	pruneOldBackupsImpl(dir, keep)
}

// newGzipFile 创建 gzip 文件写入器。
func newGzipFile(path string) (gzipWriteCloser, error) {
	return newGzipFileImpl(path)
}

// gzipWriteCloser 是 gzip 写入器接口。
type gzipWriteCloser interface {
	Write(p []byte) (int, error)
	Close() error
}

// httpGet 发起 HTTP GET 请求。
func httpGet(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	return httpGetImpl(ctx, url, timeout)
}

// decodeBase64 base64 解码。
func decodeBase64(s string) ([]byte, error) {
	return decodeBase64Impl(s)
}

// cryptoSHA1 计算 SHA1 hex。
func cryptoSHA1(s string) string {
	return cryptoSHA1Impl(s)
}

// cachePrefixImpl 获取缓存 key 前缀。
func cachePrefixImpl() string {
	return cachePrefixShared()
}
