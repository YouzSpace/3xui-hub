package admin

// 管理端邮件域（P8c）的类型与依赖。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/EmailController.php 与
// backend/app/Services/{MailNotifyService,MailScheduleService,MailBatchService,SimpleMailerService}.php。

import (
	"context"
	"log"
	"time"
)

// ---- SiteConfig 键（EmailController::KEYS + MailNotifyService::CONFIG_KEYS + MailScheduleService）----

// emailConfigKeys 对齐 EmailController::KEYS（show/save 的完整键集，保序）。
var emailConfigKeys = []string{
	"smtp_host",
	"smtp_port",
	"smtp_username",
	"smtp_password", // 加密存储
	"smtp_encryption",
	"smtp_from_name",
	"email_template",
	"register_email_verify",
	"mail_rate_limit_enabled",
	"mail_rate_per_minute",
	"mail_send_interval_ms", // 串行 worker 两封之间的间隔（毫秒，默认 0 = 不等）
	"rate_code_interval",
	"rate_code_per_day",
	"rate_code_ip_hourly",
	"rate_verify_max_attempts",
	"rate_verify_lock_seconds",
	"rate_login_max_attempts",
	"rate_login_lock_seconds",
	"rate_register_ip_hourly",
}

// mailNotifyScenes 对齐 MailNotifyService::SCENES（保序）。
var mailNotifyScenes = []string{"traffic_exhausted", "traffic_almost", "expiring", "expired"}

// mailNotifySceneDefaults 是每个场景的出厂标题/正文/收件人。
var mailNotifySceneDefaults = map[string]struct {
	Label string
	Title string
	Body  string
	To    string
}{
	"traffic_exhausted": {
		Label: "流量用尽",
		Title: "您的流量已用尽",
		Body:  "<p>您好，</p><p>您的流量已用尽，流量已被暂停。请续费或重置流量后继续使用。</p>",
		To:    "user",
	},
	"traffic_almost": {
		Label: "流量即将用尽",
		Title: "您的流量即将用尽",
		Body:  "<p>您好，</p><p>您的流量已使用 {{percent}}%，请及时续费，避免服务中断。</p>",
		To:    "user",
	},
	"expiring": {
		Label: "套餐即将到期",
		Title: "您的套餐即将到期",
		Body:  "<p>您好，</p><p>您的套餐将于 {{expire_date}} 到期（剩余 {{days_left}} 天），请及时续费。</p>",
		To:    "user",
	},
	"expired": {
		Label: "套餐已到期",
		Title: "您的套餐已到期",
		Body:  "<p>您好，</p><p>您的套餐已于 {{expire_date}} 到期，流量已关闭。续费后可继续使用。</p>",
		To:    "user",
	},
}

// MailLog type 常量（对齐 MailLog 模型）。
const (
	mailLogTypeNotify   = "notify"
	mailLogTypeBatch    = "batch"
	mailLogTypeTest     = "test"
	mailLogTypeSchedule = "schedule"

	mailLogStatusSent   = "sent"
	mailLogStatusFailed = "failed"
)

// MailUser 是收件人解析需要的用户列（对齐 MailBatchService::recipientQuery 的 with('plan')）。
type MailUser struct {
	ID                  uint64
	Email               string
	Token               string
	PlanName            *string
	TrafficLimit        int64
	TrafficUsed         int64
	MonthlyTrafficUsed  int64
	MonthlyTrafficLimit int64
	ExpiredAt           *time.Time
}

// MailLogRow 是 mail_logs 一行。
//
// ToUserID 按模型 cast 输出 integer；列本身是 string，空串视作 null。
type MailLogRow struct {
	ID        uint64
	Type      string
	Status    string
	ToEmail   *string
	ToUserID  *int64
	Subject   *string
	Scene     *string
	BatchID   *int64
	Error     *string
	CreatedAt *time.Time
}

// MailLogFilter 是发信日志的筛选（GET /admin-api/email/logs）。
type MailLogFilter struct {
	Type   string // notify|batch|test|schedule，空 = 不筛
	Status string // sent|failed，空 = 不筛
}

// MailRecipientFilter 是批量发信的收件人筛选（mode=all|filter）。
type MailRecipientFilter struct {
	PlanID     *int64
	UserStatus string // normal|over|expired，空按 normal
}

// MailBatchProgress 是一个批次的发送进度（GET /admin-api/email/batch-progress）。
//
// 分母没法从库里反推得完全准确（收件人集合会随用户状态变化），所以两套都给：
// Sent+Failed 是库里的既成事实，Total/Remaining 是本批入队时的 total 与队列内存水位。
type MailBatchProgress struct {
	BatchID int64
	// Total 是入队时算出的总封数（前端发 batch-send 时拿到的 total，可传回来做分母）。
	Total int64
	// Sent / Failed 是库里该批次已落库的封数。
	Sent   int64
	Failed int64
	// FirstAt / LastAt 是该批次首/末封的落库时刻。
	FirstAt *time.Time
	LastAt  *time.Time
	// Queued 是队列里还没取出的封数（进程内存水位，重启归零）。
	Queued int64
	// Sending 是正在发的那封（0/1）。
	Sending int64
	// QueueFailed 是本次进程启动以来发信失败的封数（含非本批）。
	QueueFailed int64
}

// MailStore 是邮件域对数据库的全部依赖。
type MailStore interface {
	// SiteConfig 读写（键值表；缺失键读出空串，与 SiteConfig::getMany 一致）
	MailConfigMany(ctx context.Context, keys []string) (map[string]string, error)
	MailConfigSetMany(ctx context.Context, kv map[string]string) error

	// 收件人解析
	MailUserByLogin(ctx context.Context, query string) (*MailUser, error) // 邮箱精确或 ID 精确
	MailRecipients(ctx context.Context, f MailRecipientFilter, now time.Time) ([]MailUser, error)

	// 发信日志
	MailLogPage(ctx context.Context, f MailLogFilter, page, perPage int64) ([]MailLogRow, int64, error)
	CreateMailLog(ctx context.Context, row MailLogRow) error
	// MailBatchProgress 按 batch_id 聚合已发/失败（批量发信进度条用）。
	MailBatchProgress(ctx context.Context, batchID int64) (*MailBatchProgress, error)

	// SMTP 账号池
	SMTPAccountStore
}

// SMTPSettings 是一次发信要使用的 SMTP 配置（密码已解密）。
type SMTPSettings struct {
	Host       string
	Port       int
	Username   string
	Password   string
	Encryption string // tls|ssl|none
	FromName   string
}

// MailTransport 是发信接缝（对齐 SimpleMailerService::send / Mail::html）。
//
// Go 侧「forgetMailers」的等价物：**每次发送都由调用方把新解出来的 SMTPSettings 传进来**，
// 不在 Transport 内缓存连接配置 —— 改 mail 配置后下一封信立即生效，不会复用旧 mailer。
type MailTransport interface {
	Send(cfg SMTPSettings, to, subject, htmlBody string) error
}

// MailJob 是一封待发信（对齐 SendMailJob 的入参）。
type MailJob struct {
	ToEmail      string
	Subject      string
	HTMLBody     string
	Type         string // notify|batch|test|schedule
	UserID       *uint64
	BatchID      *int64
	Scene        *string
	// DelaySeconds 是「相对入队时刻的最早可发偏移」（秒），不是每封之间要睡的秒数。
	DelaySeconds int
	// EnqueuedAt 是入队时刻，worker 用它把 DelaySeconds 换算成绝对时刻，避免延迟逐封累加。
	// 由队列在 Enqueue 时补（调用方零值即可）。
	EnqueuedAt time.Time
}

// MailQueue 是队列接缝（PHP 的 SendMailJob::dispatch）。
type MailQueue interface {
	Enqueue(job MailJob) error
}

// MailQueueStater 是可选接口：队列若能报实时水位就实现它（系统监控页用）。
//
// goroutine 队列不落库（jobs 表恒 0 行），所以监控页的数字只能由队列自己报。
type MailQueueStater interface {
	Stats() MailQueueStats
}

// MailDeps 是邮件域的依赖清单。
type MailDeps struct {
	Store     MailStore
	Transport MailTransport
	Queue     MailQueue
	// AppKey 用于 Crypt::encryptString / decryptString（smtp_password）。
	AppKey []byte
	Now    func() time.Time
	Logger loggerLike
}

// MailDomain 是邮件域的 HTTP 入口。
type MailDomain struct {
	store     MailStore
	transport MailTransport
	queue     MailQueue
	appKey    []byte
	now       func() time.Time
	logger    loggerLike
}

// NewMailDomain 组装邮件域。Transport / Queue 为空时用真实实现（不是 noop 假成功）：
//   - Transport 空 → NewSMTPTransport（net/smtp 真实发信）；
//   - Queue 空 → newMailQueue（单 goroutine 串行 + DelaySeconds + mail_send_interval_ms 封间隔，
//     执行体是 m.deliver）。
//
// 测试可注入 mock 覆盖两者；nil 不再退化为「记日志后假装成功」。
func NewMailDomain(d MailDeps) *MailDomain {
	if d.Store == nil {
		panic("admin: MailStore 不能为空")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	transport := d.Transport
	if transport == nil {
		transport = NewSMTPTransport(logger)
	}
	m := &MailDomain{
		store:     d.Store,
		transport: transport,
		appKey:    d.AppKey,
		now:       now,
		logger:    logger,
	}
	queue := d.Queue
	if queue == nil {
		queue = newMailQueue(m.deliver, logger, m.mailSendInterval)
	}
	m.queue = queue
	return m
}

// MailQueue 暴露邮件队列（单 goroutine 串行），供 commandRunner 的定时通知邮件
// （mail:notify-scan / mail:schedule-scan）复用同一队列，避免各自起 worker 并发发信。
func (m *MailDomain) MailQueue() MailQueue { return m.queue }

// QueueStats 报告邮件队列实时水位（实现 MailQueueCounter，供系统监控页显示）。
//
// 队列不是 goroutine 实现（测试注入的 mock）时返回零值 + false，调用方可回退到库表口径。
func (m *MailDomain) QueueStats() (MailQueueStats, bool) {
	if stater, ok := m.queue.(MailQueueStater); ok {
		return stater.Stats(), true
	}
	return MailQueueStats{}, false
}
