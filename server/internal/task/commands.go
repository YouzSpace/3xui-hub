package task

// 13 个 artisan 命令体：对齐 backend/app/Console/Commands/*.php 与
// backend/app/Jobs/{MailNotifyScanJob,MailScheduleJob}.php 的 handle()。
//
// 命令名与 defaults.go 的 commands.Run(ctx, "xxx", ...) 字符串逐字一致。
// 复用已有驱动/store（threexui/xray 驱动、collector 的 cache 语义、
// handler/admin 的 SQLStore），不重写取数/落账逻辑。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"controlhub-server/internal/tgbot"
)

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

	// ReconcileNodes 是「节点用户全量比对 + 兜底修复」（users:reconcile-enable 用）。
	// 传 collector.ReconcileNodes 的适配：读节点全量快照 → 按面板意图比对 → 缺人补注册 /
	// enable 开合 / 内容对不上删除重建 / 未知不动；只处理我们自己建的 ch_user_{id}。
	// 为 nil 时该命令记日志并跳过（不影响其它调度）。
	ReconcileNodes func(ctx context.Context, dry bool, maxWritesPerNode int) (ReconcileSummary, error)
}

// ReconcileSummary 是对账命令需要的最小统计（刻意不引用 collector 的类型，避免包耦合）。
type ReconcileSummary struct {
	Nodes, Users, Probed int
	// ProbeFailed 是探测本身失败（超时/5xx/登录态失效）的条数，与 Unknown（探测到了但读不全）分开。
	ProbeFailed                int
	Registered, Fixed, Drift   int
	DriftFixed                 int // 漂移里被自动修复（写回节点并复核一致）的条数
	DriftSafe, DriftEarlyCut   int
	DriftLoose                 int
	Opened, Closed, Unchanged  int
	// Attached 是「client 已存在但没挂到该协议全部入站」→ 补挂的条数（入站维度对账）。
	Attached                   int
	Unknown, Deferred, Foreign int
	ForeignNames, OfflineNodes []string
	SnapshotBy                 string
	// 裸内核（driver_type=xray）对账：读法是 client_list 指令回执，
	// 修法是 bump config_version（让 agent 重拉全量配置）。见 collector/reconcile_bare.go。
	//
	// BareMismatch 只统计会真实影响用户的差异（缺人 / 该开被关 / 不该连却在）；
	// BareBumped 是因此触发重拉的节点数；BareDrift（字段不一致）与 BareStale
	// （已禁用的残留）只报告，不触发重拉。
	BareNodes, BareMismatch, BareBumped, BareDrift, BareStale int
	ElapsedMs                                                int64
}

// DriftEarlyCutWarning 是「有用户正被节点提前掐」的告警判定（drift_early_cut>0）。
//
// 单独抽成函数是为了让「日志升级 WARN」这件事可被测试断言 —— 否则只能靠 grep 日志，
// 而日志格式一改测试就静默通过。
func (s ReconcileSummary) DriftEarlyCutWarning() bool { return s.DriftEarlyCut > 0 }

// ProbeStalled 是「整轮什么都没探到」的空转判定：候选非空、却一条都没探测成功。
//
// 这是旧实现 checked=0 空转 143 轮的复现条件 —— probed=0 本身也可能是「全员一致」，
// 所以必须要求 probe_failed>0 才判空转（没探测失败、也没探测成功 = 面板没有可比对的候选）。
func (s ReconcileSummary) ProbeStalled() bool {
	return s.Probed == 0 && s.Users > 0 && s.ProbeFailed > 0
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
	case "retention:purge":
		return r.runRetentionPurge(ctx, args)
	case "order:expire-pending":
		return r.runOrderExpirePending(ctx, args)
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
