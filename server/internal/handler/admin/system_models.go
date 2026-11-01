package admin

// 管理端系统域（P8c）的类型与依赖。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/SystemStatusController.php 与
// GeodataController.php（+ GeodataService）。

import (
	"context"
	"log"
	"time"
)

// OperationLogRow 是 operation_logs 一行（系统状态/审计页用）。
type OperationLogRow struct {
	ID         uint64
	Type       string
	ActorName  *string
	Action     string
	Method     *string
	Path       *string
	Status     *int64
	Error      *string
	IP         *string
	CreatedAt  *time.Time
}

// AuditLogFilter 是审计日志的筛选（GET /admin-api/audit-logs）。
type AuditLogFilter struct {
	Type   string // admin|user|system，空 = 不筛
	Status string // ok|error，空 = 不筛
	Search string
}

// ScheduledTaskRun 是 scheduled_task_runs 一行。
type ScheduledTaskRun struct {
	Command    string
	Status     string
	Error      *string
	DurationMS *int64
	RanAt      *time.Time
}

// ScheduledEvent 是一条定时任务定义（对齐 Laravel Schedule::events()）。
type ScheduledEvent struct {
	Command    string
	Expression string
	HasMutex   bool
}

// SystemWorkerStatus 是 worker 区块的展示结构。
type SystemWorkerStatus struct {
	Alive        bool
	LastSeen     *time.Time
	Pending      int64
	Running      int64
	FailedCount  int64
	RecentFailed []SystemFailedJob
}

// SystemFailedJob 是 failed_jobs 最近一条的展示结构。
type SystemFailedJob struct {
	Queue        string
	Name         *string
	Attempts     *int64 // failed_jobs 无 attempts 列，拿不到就 null，不伪造 0
	ErrorSummary *string
	FailedAt     *time.Time
}

// SystemStore 是系统域对数据库的全部依赖。
type SystemStore interface {
	// OperationLogsRecent 对齐 status() 里的最近 100 条。
	OperationLogsRecent(ctx context.Context, limit int64) ([]OperationLogRow, error)
	// OperationLogPage 对齐 auditLogs 的分页查询。
	OperationLogPage(ctx context.Context, f AuditLogFilter, page, perPage int64) ([]OperationLogRow, int64, error)

	// ScheduledTaskRunLast / Recent 对齐 scheduled_task_runs 查询。
	ScheduledTaskRunLast(ctx context.Context, command string) (*ScheduledTaskRun, error)
	ScheduledTaskRunsRecent(ctx context.Context, limit int64) ([]ScheduledTaskRun, error)

	// WorkerHeartbeat 对齐 Cache::get('controlhub:worker-heartbeat')。
	WorkerHeartbeat(ctx context.Context) (*time.Time, error)
	// QueueCounts 返回 pending / running / failed。
	//
	// ⚠️ 这是 **Laravel jobs/failed_jobs 表** 的口径；Go 侧队列走进程内 goroutine，
	// 纯 Go 部署下这两张表恒为 0 行，不能代表真实排队情况。真实水位由
	// MailQueueStater 上报，见 workerStatus 的 queueStats。
	QueueCounts(ctx context.Context) (pending, running, failed int64, err error)
	// FailedJobsRecent 最近 N 条 failed_jobs。
	FailedJobsRecent(ctx context.Context, limit int64) ([]SystemFailedJob, error)
}

// MailQueueCounter 是系统域读取邮件队列水位的接缝（可选注入）。
//
// goroutine 队列不落库，监控页要显示「排队中/运行中/失败」只能由队列自己报数。
// ok=false 表示该实现不报数（如测试注入的假队列），系统域回退到 jobs 表口径。
type MailQueueCounter interface {
	QueueStats() (MailQueueStats, bool)
}

// TaskQueueCounter 是系统域读取**节点任务队列**水位的接缝（可选注入）。
//
// 与 MailQueueCounter 并列而非合并：两者是两条独立的 goroutine 队列（邮件单线程串行、
// 节点任务多 worker），水位必须分别取；只能有一个接缝的话，监控页就只能显示其中一条。
// ok=false 表示该实现不报数（测试注入的假队列），系统域回退到 jobs 表口径。
type TaskQueueCounter interface {
	Stats() (pending, running, failed int64, ok bool)
}

// PHPStatus 是 PHP 运行时体检结果（php2shell CVE 判定）。
type PHPStatus struct {
	Checked  bool
	Version  string
	SAPI     string
	Patched  bool
	FixedIn  *string
	Mitigated bool
	IniFile  string
	Error    string
}

// PHPStatusAuditor 对齐 PhpRuntimeAuditor::audit（只读、不执行 shell 的接缝）。
type PHPStatusAuditor interface {
	Audit() PHPStatus
}

// goRuntimeAuditor 是切 Go 后的 PHP 体检器：面板跑在 Go 上，没有 PHP runtime，
// 对齐 PHP 的返回结构给出「无 PHP = 无漏洞」的合理状态（CVE-2026-109618/109619 只影响 PHP）。
type goRuntimeAuditor struct{}

func (goRuntimeAuditor) Audit() PHPStatus {
	return PHPStatus{
		Checked:   true,
		Version:   "none",
		SAPI:      "none",
		Patched:   true,
		Mitigated: true,
		IniFile:   "",
	}
}

// ---- Geo 数据 ----

// GeodataFile 是面板侧 geo 文件状态。
type GeodataFile struct {
	Name      string
	Exists    bool
	Size      int64
	SHA256    string
	UpdatedAt *string
	CheckedAt *string
}

// GeodataNodeStatus 是某节点最近一次 geodata 同步状态。
type GeodataNodeStatus struct {
	NodeID     uint64
	NodeName   string
	Enabled    bool
	Status     *string
	Result     *string
	FinishedAt *time.Time
}

// GeodataUpdateResult 是单文件更新结果。
type GeodataUpdateResult struct {
	Name    string
	Status  string // updated | unchanged | failed
	Size    int64
	SHA256  string
	Message string
}

// GeodataService 是 geo 数据管理接缝（对齐 GeodataService.php）。
type GeodataService interface {
	Status(ctx context.Context) ([]GeodataFile, error)
	NodeStatuses(ctx context.Context) ([]GeodataNodeStatus, error)
	Update(ctx context.Context) ([]GeodataUpdateResult, error)
	// DispatchToNodes 把面板当前文件分发到所有 enabled xray 节点。
	DispatchToNodes(ctx context.Context) (queued, skipped int, missing bool, err error)
}

// SystemDeps 是系统域的依赖清单。
type SystemDeps struct {
	Store SystemStore
	PHP   PHPStatusAuditor
	Geo   GeodataService
	// MailQueue 是邮件队列（可选）：注入后监控页的「排队中/运行中/失败」显示真实水位，
	// 不再读恒为 0 行的 Laravel jobs 表。
	MailQueue MailQueueCounter
	// TaskQueue 是节点任务队列（可选）：同样报内存水位，与 MailQueue 并列注入。
	TaskQueue TaskQueueCounter
	// Schedule 是定时任务定义（PHP Laravel Schedule 的静态副本；测试可注入）。
	Schedule []ScheduledEvent
	Now      func() time.Time
	Logger   loggerLike
}

// SystemDomain 是系统域的 HTTP 入口。
type SystemDomain struct {
	store     SystemStore
	php       PHPStatusAuditor
	geo       GeodataService
	mailQueue MailQueueCounter
	taskQueue TaskQueueCounter
	schedule  []ScheduledEvent
	now       func() time.Time
	logger    loggerLike
}

// NewSystemDomain 组装系统域。
func NewSystemDomain(d SystemDeps) *SystemDomain {
	if d.Store == nil {
		panic("admin: SystemStore 不能为空")
	}
	php := d.PHP
	if php == nil {
		php = goRuntimeAuditor{}
	}
	sched := d.Schedule
	if sched == nil {
		sched = defaultScheduledEvents()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &SystemDomain{
		store:     d.Store,
		php:       php,
		geo:       d.Geo,
		mailQueue: d.MailQueue,
		taskQueue: d.TaskQueue,
		schedule:  sched,
		now:       now,
		logger:    logger,
	}
}

// defaultScheduledEvents 是 backend/routes/console.php 的 Schedule 静态副本。
//
// 迁移期 PHP artisan schedule:run 仍在跑，这里给后台一份同样的任务清单；
// next_run 由系统域按 expression 估算（与 Laravel nextRunDate 同为 UTC）。
func defaultScheduledEvents() []ScheduledEvent {
	return []ScheduledEvent{
		{Command: "traffic-sync", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "ban-check", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "health-check", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "monthly-reset-traffic", Expression: "0 0 * * *", HasMutex: true},
		{Command: "invite-code-refresh", Expression: "10 0 * * *", HasMutex: true},
		{Command: "db-backup", Expression: "30 3 * * *", HasMutex: true},
		{Command: "monitor-rollup", Expression: "30 4 * * *", HasMutex: true},
		{Command: "expire-pending-orders", Expression: "0 * * * *", HasMutex: true},
		{Command: "proxy-maintain", Expression: "0 * * * *", HasMutex: true},
		{Command: "async-task-timeout", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "mail-notify-scan", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "mail-schedule-scan", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "third-party-fetch", Expression: "0 */2 * * *", HasMutex: true},
		{Command: "xray-warp-rotate", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "xray-refresh-outbound-subs", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "geodata-update", Expression: "45 4 * * 1", HasMutex: true},
		{Command: "users-reconcile-enable", Expression: "*/5 * * * *", HasMutex: true},
		{Command: "reality-audit", Expression: "10 4 * * *", HasMutex: true},
		// Go 侧新增（PHP 没有对应条目）：本机证书目录 → TLS 证书库，
		// acme.sh 续签后自动把新 PEM 刷进库，HY2 入站不用改。
		{Command: "xray-certificates-sync", Expression: "0 */6 * * *", HasMutex: true},
	}
}
