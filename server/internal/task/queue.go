package task

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// 队列：goroutine worker + default 单队列。
//
// 队列分离默认关闭（config queue.default 单队列）—— PHP 侧 Job 全进 default 队列，
// 没有多队列路由；这里保留 Queue 字段以便将来接 redis 多队列，当前实现就是
// 「一条 chan + N 个 worker」。
//
// 重试：Laravel Job 的 $tries=5 + $backoff=[60,300,900,3600] 由 Worker 在**同一 item**上
// 重放 handler；超过后 failItem。任务级 max_attempts 只在 pending→running 时 +1（PHP 同）。

// JobKind 是队列里执行单元的种类（对齐 PHP 的 Job 类）。
type JobKind string

const (
	// JobSyncNodeTraffic ← SyncNodeTrafficJob（traffic_sync 的 item）。
	JobSyncNodeTraffic JobKind = "SyncNodeTrafficJob"
	// JobNodeInboundSync ← NodeInboundSyncJob（sync_inbounds 批量）。
	JobNodeInboundSync JobKind = "NodeInboundSyncJob"
	// JobNodeInboundScan ← NodeInboundScanJob（入站同步重试走扫描）。
	JobNodeInboundScan JobKind = "NodeInboundScanJob"
	// JobNodeInitUser ← NodeInitUserJob（node_init 批量）。
	JobNodeInitUser JobKind = "NodeInitUserJob"
	// JobApplyDomain ← ApplyDomainJob（domain_action）。
	JobApplyDomain JobKind = "ApplyDomainJob"
	// JobNodeClientsCleanup ← NodeClientsCleanupJob（node_action.cleanup_clients）。
	JobNodeClientsCleanup JobKind = "NodeClientsCleanupJob"
	// JobProvisionClient ← ProvisionClientJob（建号后异步下发 client 到各节点）。
	JobProvisionClient JobKind = "ProvisionClientJob"
)

// JobPayload 是队列里的一条执行单元（对齐 PHP Job 构造参数）。
type JobPayload struct {
	Kind    JobKind
	TaskID  int64
	ItemKey string

	// NodeID 是 item 前缀解析出的节点 id（traffic_sync）或任务行 subject_id（node_*）。
	NodeID int64
	// UserID 是 meta.user_id（traffic_sync 单用户同步）。
	UserID *int64
	// UserIDs / ItemKeys 是批处理形态（NodeInboundSyncJob / NodeInitUserJob）。
	UserIDs  []int64
	ItemKeys []string

	// Action / Domain 是 domain_action 的 meta.action / meta.domain。
	Action string
	Domain *string
	// NodeSnapshot 是 cleanup_clients 的 meta.node_snapshot（行已删，靠 meta 快照）。
	NodeSnapshot any

	// ReleaseCount 是节点锁竞争时已重新入队的次数（PHP 的 $releaseCount）。
	ReleaseCount int
}

// Handler 执行一条队列单元。返回错误 → 按重试策略重放或 failItem。
type Handler func(ctx context.Context, job JobPayload) error

// Dispatcher 是派发出口（AsyncTaskService 持有；测试可 fake 记录派发）。
type Dispatcher interface {
	Enqueue(job JobPayload)
}

// Queue 是 default 队列的 goroutine 实现。
type Queue struct {
	svc      *Service
	handlers map[JobKind]Handler
	jobs     chan JobPayload
	workers  int

	retries int             // handler 失败重放次数（Laravel $tries-1）
	backoff []time.Duration // Laravel $backoff = [60,300,900,3600]
	sleep   func(time.Duration)
	logf    func(format string, args ...any)

	// queued / running / failed 是**队列实时水位**，供系统监控页显示。
	//
	// 为什么必须自己数：Go 侧队列走进程内 goroutine + channel，Laravel 的
	// jobs/failed_jobs 表在纯 Go 部署下恒为 0 行，监控页会一直显示「排队中 0 /
	// 运行中 0 / 失败 0」，用户以为任务根本没在跑（线上就踩过这个）。
	queued  int64 // 已入队未取走（含 channel 缓冲 + 已入队未消费）
	running int64 // 已取走正在执行
	failed  int64 // 累计失败（handler 重试耗尽的单元数，进程生命周期内只增）

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// QueueStats 是队列实时水位的快照。
type QueueStats struct {
	Pending int64
	Running int64
	Failed  int64
}

// Stats 返回队列实时水位（实现 admin.QueueCounter）。
func (q *Queue) Stats() QueueStats {
	return QueueStats{
		Pending: atomic.LoadInt64(&q.queued),
		Running: atomic.LoadInt64(&q.running),
		Failed:  atomic.LoadInt64(&q.failed),
	}
}

// QueueOptions 是队列构造参数。
type QueueOptions struct {
	// Workers 是并发 worker 数（默认 4）。
	Workers int
	// Retries 是 handler 失败重放次数（默认 4 → 合计 tries=5，对齐 SyncNodeTrafficJob::$tries）。
	Retries int
	// Backoff 是重放退避序列（默认 60s/300s/900s/3600s）。
	Backoff []time.Duration
	// Sleep 可注入（测试跳过等待）；nil → time.Sleep。
	Sleep func(time.Duration)
	// Logf 是日志出口。
	Logf func(format string, args ...any)
	// Buffer 是队列缓冲（默认 256）。
	Buffer int
}

// NewQueue 构造 default 队列。Service.Queue 记为本队列后，dispatch 即入队。
func NewQueue(svc *Service, opts QueueOptions) *Queue {
	workers := opts.Workers
	if workers <= 0 {
		workers = 4
	}
	buf := opts.Buffer
	if buf <= 0 {
		buf = 256
	}
	retries := opts.Retries
	if retries < 0 {
		retries = 0
	}
	if opts.Retries == 0 {
		retries = 4 // 默认 tries=5
	}
	backoff := opts.Backoff
	if len(backoff) == 0 {
		backoff = []time.Duration{60 * time.Second, 300 * time.Second, 900 * time.Second, 3600 * time.Second}
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		svc:      svc,
		handlers: map[JobKind]Handler{},
		jobs:     make(chan JobPayload, buf),
		workers:  workers,
		retries:  retries,
		backoff:  backoff,
		sleep:    sleep,
		logf:     logf,
		ctx:      ctx,
		cancel:   cancel,
	}
	if svc != nil {
		svc.Queue = q
	}
	return q
}

// Register 登记某种 Job 的执行体（重复登记覆盖）。
func (q *Queue) Register(kind JobKind, h Handler) {
	q.handlers[kind] = h
}

// Enqueue 实现 Dispatcher。队列已停时丢弃（PHP 进程退出同向）。
func (q *Queue) Enqueue(job JobPayload) {
	select {
	case <-q.ctx.Done():
		q.logf("[queue] 队列已停止，丢弃 %s task=%d item=%s", job.Kind, job.TaskID, job.ItemKey)
		return
	default:
	}
	// 先记「排队中」再入 channel：worker 取走时立刻 -1 并 +1 到运行中，
	// 这样监控页的 queued+running 始终等于「还没跑完的单元数」，不会出现数字缺口。
	atomic.AddInt64(&q.queued, 1)
	select {
	case <-q.ctx.Done():
		atomic.AddInt64(&q.queued, -1)
		q.logf("[queue] 队列已停止，丢弃 %s task=%d item=%s", job.Kind, job.TaskID, job.ItemKey)
		return
	case q.jobs <- job:
	}
}

// Start 启动 worker。
func (q *Queue) Start() {
	q.once.Do(func() {
		for i := 0; i < q.workers; i++ {
			q.wg.Add(1)
			go q.worker(i)
		}
		q.logf("[queue] default 队列启动 workers=%d", q.workers)
	})
}

// Stop 停止接收并等待在跑的单元结束。
func (q *Queue) Stop() {
	q.cancel()
	q.wg.Wait()
}

func (q *Queue) worker(_ int) {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case job := <-q.jobs:
			atomic.AddInt64(&q.queued, -1)
			atomic.AddInt64(&q.running, 1)
			q.execute(q.ctx, job)
			atomic.AddInt64(&q.running, -1)
		}
	}
}

// execute 跑一条单元：handler + 重放退避 + 子项终态。
func (q *Queue) execute(ctx context.Context, job JobPayload) {
	h := q.handlers[job.Kind]
	if h == nil {
		// PHP dispatch() 的 default => throw InvalidArgumentException
		q.failItem(ctx, job, "不支持的任务类型："+string(job.Kind))
		return
	}

	var err error
	for attempt := 0; ; attempt++ {
		err = h(ctx, job)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if attempt >= q.retries {
			break
		}
		delay := q.backoff[minInt(attempt, len(q.backoff)-1)]
		q.logf("[queue] %s task=%d item=%s 第 %d 次失败，%s 后重试：%v",
			job.Kind, job.TaskID, job.ItemKey, attempt+1, delay, err)
		q.sleep(delay)
	}
	q.logf("[queue] %s task=%d item=%s 重试耗尽：%v", job.Kind, job.TaskID, job.ItemKey, err)
	q.failItem(ctx, job, err.Error())
}

// failItem 把子项标失败（taskID/itemKey 为空的单元只记日志）。
func (q *Queue) failItem(ctx context.Context, job JobPayload, summary string) {
	// 监控页的「失败累计」是队列级计数：不论有没有子项，重试耗尽的单元都算一次。
	atomic.AddInt64(&q.failed, 1)
	if q.svc == nil || job.TaskID == 0 || job.ItemKey == "" {
		return
	}
	if _, err := q.svc.FailItem(ctx, job.TaskID, job.ItemKey, summary); err != nil {
		q.logf("[queue] 标记失败项出错 task=%d item=%s: %v", job.TaskID, job.ItemKey, err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
