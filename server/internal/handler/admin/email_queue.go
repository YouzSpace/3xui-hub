package admin

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// mailQueue 是 MailQueue 的 goroutine 实现：单 goroutine 串行消费 + 按绝对时刻限速。
//
// 对齐 PHP 语义（SendMailJob::onQueue + queue:work 单线程）：
//   - 单 goroutine 一封一封发，避免并发触发 SMTP 风控/限流；
//   - DelaySeconds = (i/perMinute)*60 是「相对入队时刻的最早可发偏移」，**不是每封之间
//     要睡的秒数**。worker 只睡到「该封的最早可发时刻」为止（wait = EnqueuedAt+delay-now），
//     所以延迟不会逐封累加（旧实现直接 sleep(DelaySeconds)，第 99 封要多睡 19 分钟，
//     整批 100 封要跑 27 小时——那是 bug，不是限速）；
//   - 发信执行体由 MailDomain.deliver 提供（load 配置 + 发信 + 写 mail_logs + 失败语义）。
type mailQueue struct {
	jobs    chan MailJob
	deliver func(job MailJob)
	logger  loggerLike

	// interval 返回两封之间的间隔（读 site_config 的 mail_send_interval_ms）；
	// sleep 可注入（测试里替换成记账不真睡）；now 可注入（测试里控制时钟）。
	interval func() time.Duration
	sleep    func(time.Duration)
	now      func() time.Time

	// 运行时计数（系统监控页读它；goroutine 队列不落库，jobs 表永远是 0 行）。
	running int32 // 0/1，是否正在发某一封
	sent    int64
	failed  int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// MailQueueStats 是邮件队列的实时水位（供 /admin-api/system/status 展示）。
type MailQueueStats struct {
	// Pending 是还没取出来的封数（chan 里排队的）。
	Pending int64
	// Running 是正在发的那封（单并发，最多 1）。
	Running int64
	// Failed 是本次进程启动以来发信失败的封数。
	Failed int64
	// Sent 是本次进程启动以来发信成功的封数。
	Sent int64
}

// newMailQueue 构造并启动单 worker 的邮件队列。
//
// interval 为空时按 0 处理（不发间隔），sleep 为空时用 time.Sleep。
func newMailQueue(deliver func(MailJob), logger loggerLike, interval func() time.Duration) *mailQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &mailQueue{
		jobs:     make(chan MailJob, 256),
		deliver:  deliver,
		logger:   logger,
		interval: interval,
		sleep:    time.Sleep,
		now:      time.Now,
		ctx:      ctx,
		cancel:   cancel,
	}
	q.wg.Add(1)
	go q.worker()
	return q
}

// Enqueue 实现 MailQueue（PHP 的 SendMailJob::dispatch）。
//
// 入队时刻记进 job.EnqueuedAt：DelaySeconds 是相对它的偏移，worker 据此算「最早可发时刻」。
func (q *mailQueue) Enqueue(job MailJob) error {
	if job.EnqueuedAt.IsZero() {
		job.EnqueuedAt = q.now()
	}
	select {
	case <-q.ctx.Done():
		return fmt.Errorf("邮件队列已停止")
	case q.jobs <- job:
		return nil
	}
}

// Stats 返回队列实时水位。
func (q *mailQueue) Stats() MailQueueStats {
	return MailQueueStats{
		Pending: int64(len(q.jobs)),
		Running: int64(atomic.LoadInt32(&q.running)),
		Failed:  atomic.LoadInt64(&q.failed),
		Sent:    atomic.LoadInt64(&q.sent),
	}
}

// worker 单 goroutine 消费：等到该封的最早可发时刻 → 发 → 封间隔。
//
// 两个等待都不累加：
//   - 限速等待只等到 EnqueuedAt+DelaySeconds（已过期就立刻发，不补睡）；
//   - mail_send_interval_ms 是「发完这封、再发下一封之前」的间隔，只睡一次。
func (q *mailQueue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case job := <-q.jobs:
			if wait := q.rateWait(job); wait > 0 {
				q.sleep(wait)
			}
			atomic.StoreInt32(&q.running, 1)
			q.deliver(job)
			atomic.StoreInt32(&q.running, 0)
			if d := q.currentInterval(); d > 0 {
				q.sleep(d)
			}
		}
	}
}

// rateWait 算出「还要等多久才轮到这封」：EnqueuedAt + DelaySeconds - 现在。
//
// DelaySeconds <= 0 或已过期一律返回 0（立刻发）。这样第 i 封最早在批次开始后
// (i/perMinute) 分钟发出，而不是在「前面所有封都睡完之后」再睡。
func (q *mailQueue) rateWait(job MailJob) time.Duration {
	if job.DelaySeconds <= 0 || job.EnqueuedAt.IsZero() {
		return 0
	}
	notBefore := job.EnqueuedAt.Add(time.Duration(job.DelaySeconds) * time.Second)
	if wait := notBefore.Sub(q.now()); wait > 0 {
		return wait
	}
	return 0
}

// record 记一封的发送结果（stats 用）。
func (q *mailQueue) record(ok bool) {
	if ok {
		atomic.AddInt64(&q.sent, 1)
	} else {
		atomic.AddInt64(&q.failed, 1)
	}
}

// currentInterval 读一次 mail_send_interval_ms；空/坏值/负数按 0（不等）处理。
func (q *mailQueue) currentInterval() time.Duration {
	if q.interval == nil {
		return 0
	}
	return q.interval()
}

// Stop 停止接收并等待在发的一封结束（进程退出时由 main 调用）。
func (q *mailQueue) Stop() {
	q.cancel()
	q.wg.Wait()
}

// deliver 是单封信的发送执行体（对齐 SendMailJob::handle）：
// load 配置 → 挑号发信（多账号池，见 smtp_pool.go）→ 写 mail_logs（成功 sent / 失败 failed+error）。
//
// SMTP 4xx/5xx 与网络错误都归为 failed 并落日志（PHP 侧 UnexpectedResponseException
// 400~599 判永久失败不重试、网络错误 releaseClaim 下轮补发 —— 这里单队列一次送达，
// 失败即记 failed，不无限重试，避免死信反复占用队列）。
//
// 账号池只在「有可用号」时生效；池空时回落到站点配置的 smtp_*，行为与上线前一致。
func (d *MailDomain) deliver(job MailJob) {
	ctx := context.Background()
	cfg, err := d.store.MailConfigMany(ctx, emailConfigKeys)
	if err != nil {
		d.logger.Printf("admin mail deliver 读配置失败: %v", err)
		d.recordMailResult(false)
		return
	}
	result := d.deliverWithPool(ctx, job, cfg)
	sendErr := result.sendErr

	now := d.now().UTC()
	var toUserID *int64
	if job.UserID != nil {
		v := int64(*job.UserID)
		toUserID = &v
	}
	row := MailLogRow{
		Type:      job.Type,
		ToEmail:   &job.ToEmail,
		ToUserID:  toUserID,
		Subject:   &job.Subject,
		Scene:     job.Scene,
		BatchID:   job.BatchID,
		CreatedAt: &now,
	}
	if sendErr != nil {
		row.Status = mailLogStatusFailed
		em := sendErr.Error()
		row.Error = &em
		d.logger.Printf("admin mail deliver 发信失败: %v", sendErr)
	} else {
		row.Status = mailLogStatusSent
	}
	if err := d.store.CreateMailLog(ctx, row); err != nil {
		d.logger.Printf("admin 写发信日志失败: %v", err)
	}
	d.recordMailResult(sendErr == nil)
}

// recordMailResult 把结果记进队列水位（nil 队列或非 goroutine 实现时静默跳过）。
func (d *MailDomain) recordMailResult(ok bool) {
	if q, isQueue := d.queue.(*mailQueue); isQueue {
		q.record(ok)
	}
}
