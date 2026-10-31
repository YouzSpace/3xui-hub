package task

import (
	"context"
	"fmt"
	"log"
	"strconv"
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

	// Deadline 是本单元**整条重试链**的截止时间（零值 = 首次执行时按 JobTimeout 设定）。
	//
	// 为什么必须随单元传递：退避等待不再占住 worker（见 scheduleRetry），重试由定时器
	// 跨 goroutine 重新入队 —— 截止时间不跟着单元走，「整条任务 5 分钟封顶」就无从执行。
	Deadline time.Time
}

// Handler 执行一条队列单元。返回错误 → 按重试策略重放或 failItem。
type Handler func(ctx context.Context, job JobPayload) error

// Dispatcher 是派发出口（AsyncTaskService 持有；测试可 fake 记录派发）。
type Dispatcher interface {
	Enqueue(job JobPayload)
}

// InflightReporter 由队列实现：报告某子项此刻是否还在队列手里。
//
// 单独定义而不并进 Dispatcher：Dispatcher 是派发口的窄接口，测试里的 fake 只需要
// Enqueue；超时清扫用它做「在途豁免」，取不到就退化成不做豁免（宁可不清也不误清）。
type InflightReporter interface {
	Inflight(taskID int64, itemKey string) bool
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

	// jobTimeout 是单个单元整条重试链的时限（默认 5 分钟）。
	//
	// 旧实现把退避写成 worker 内的 sleep，退避序列合计 60+300+900+3600=4860s（81 分钟）：
	// 4 个卡住的单元就能把默认的 4 个 worker 全部占满，其余任务永远排不上队。线上实测
	// 2026-10-31：任务 429 的 4 个裸内核子项占满 worker 后，用户手动同步任务（430）的
	// 7 个子项一次都没跑上，被 10 分钟清扫成「任务超时未完成」。
	jobTimeout time.Duration

	// inflight 记录「队列手里还没脱手的子项」：正在执行、或正在等退避。
	//
	// 用途有二：① 监控页的「运行中」不再把等退避的单元算进去；② 超时清扫据此区分
	// 「队列还在管」与「重启后丢了定时器、永远收不了尾的僵尸子项」—— 只有后者该被清扫。
	inflightMap map[string]int
	inflightMu  sync.Mutex

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
	// JobTimeout 是单个单元整条重试链的时限（默认 5 分钟，见 Queue.jobTimeout 的说明）。
	JobTimeout time.Duration
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
	jobTimeout := opts.JobTimeout
	if jobTimeout <= 0 {
		jobTimeout = defaultJobTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		svc:         svc,
		handlers:    map[JobKind]Handler{},
		jobs:        make(chan JobPayload, buf),
		workers:     workers,
		retries:     retries,
		backoff:     backoff,
		sleep:       sleep,
		logf:        logf,
		jobTimeout:  jobTimeout,
		inflightMap: map[string]int{},
		ctx:         ctx,
		cancel:      cancel,
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
//
// 入队即登记在途：子项可能真的在 channel 里排一会儿队才轮到（批量下发时会很长），
// 这段「还没跑」的时间同样属于「队列还在管」，超时清扫不能把它当僵尸清掉。
func (q *Queue) Enqueue(job JobPayload) {
	key := inflightKey(job.TaskID, job.ItemKey)
	select {
	case <-q.ctx.Done():
		q.logf("[queue] 队列已停止，丢弃 %s task=%d item=%s", job.Kind, job.TaskID, job.ItemKey)
		return
	default:
	}
	// 先记「排队中」再入 channel：worker 取走时立刻 -1 并 +1 到运行中，
	// 这样监控页的 queued+running 始终等于「还没跑完的单元数」，不会出现数字缺口。
	atomic.AddInt64(&q.queued, 1)
	q.markInflight(key, 1)
	select {
	case <-q.ctx.Done():
		atomic.AddInt64(&q.queued, -1)
		q.markInflight(key, -1)
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
			// 出队即交接：先放掉「排队中」那份在途计数，紧接着 runAttempt 会重新登记
			// 「执行中」那份 —— 中间不留空窗，超时清扫不会看到 0。
			q.markInflight(inflightKey(job.TaskID, job.ItemKey), -1)
			atomic.AddInt64(&q.running, 1)
			q.execute(q.ctx, job)
			atomic.AddInt64(&q.running, -1)
		}
	}
}

// defaultJobTimeout 是单个单元整条重试链的默认时限（用户定：超过 5 分钟没跑完就该放开）。
const defaultJobTimeout = 5 * time.Minute

// execute 跑一条单元：整链时限 + 逐次执行 + 异步退避 + 子项终态。
//
// 与旧实现的三处关键差异（改这里前务必读完）：
//  1. 退避等待**不再 sleep 在 worker 里**：旧写法会把 worker 按住最长 81 分钟，
//     4 个卡住的单元即可占满默认 4 个 worker，导致后续任务全部饿死；
//  2. 整条重试链受 jobTimeout（默认 5 分钟）约束，到点直接收终态，不再无限重试；
//  3. handler 在独立 goroutine 里跑并带 deadline：即使它不理会 ctx，worker 也能脱身。
func (q *Queue) execute(ctx context.Context, job JobPayload) {
	h := q.handlers[job.Kind]
	if h == nil {
		// PHP dispatch() 的 default => throw InvalidArgumentException
		q.failItem(ctx, job, "不支持的任务类型："+string(job.Kind))
		return
	}

	if job.Deadline.IsZero() {
		job.Deadline = time.Now().Add(q.jobTimeout)
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if !time.Now().Before(job.Deadline) {
			q.logf("[queue] %s task=%d item=%s 整链超时（>%s），放弃重试",
				job.Kind, job.TaskID, job.ItemKey, q.jobTimeout)
			summary := "任务执行超时"
			if lastErr != nil {
				summary = "任务执行超时：" + lastErr.Error()
			}
			q.failItem(ctx, job, summary)
			return
		}

		lastErr = q.runAttempt(ctx, job, h)
		if lastErr == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if attempt >= q.retries {
			break
		}

		delay := q.backoff[minInt(attempt, len(q.backoff)-1)]
		remain := time.Until(job.Deadline)
		if delay >= remain {
			// 剩余时间不够再等一轮：直接收终态，别空等到超时。
			q.logf("[queue] %s task=%d item=%s 剩余 %s 不足以退避 %s，直接判失败：%v",
				job.Kind, job.TaskID, job.ItemKey, remain.Round(time.Second), delay, lastErr)
			break
		}
		q.logf("[queue] %s task=%d item=%s 第 %d 次失败，%s 后重试：%v",
			job.Kind, job.TaskID, job.ItemKey, attempt+1, delay, lastErr)
		q.scheduleRetry(ctx, job, delay)
		return // worker 立即回池：退避期间不占着队列
	}
	q.logf("[queue] %s task=%d item=%s 重试耗尽：%v", job.Kind, job.TaskID, job.ItemKey, lastErr)
	q.failItem(ctx, job, lastErr.Error())
}

// runAttempt 跑一次 handler：登记在途、套单元 deadline、到点脱身。
//
// handler 放独立 goroutine 是为了「就算它不理会 ctx 也压不住 worker」。被放弃的
// goroutine 之后若再落终态，会被异步任务的终态粘性挡住（async.go 的终态守卫），
// 因此不会出现「已判失败的子项又被标成功」。
func (q *Queue) runAttempt(parent context.Context, job JobPayload, h Handler) error {
	key := inflightKey(job.TaskID, job.ItemKey)
	q.markInflight(key, 1)
	defer q.markInflight(key, -1)

	ctx, cancel := context.WithDeadline(parent, job.Deadline)
	defer cancel()

	done := make(chan error, 1) // 缓冲 1：handler 迟到返回也不会阻塞它自己
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("handler panic: %v", r)
			}
		}()
		done <- h(ctx, job)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if parent.Err() != nil {
			return parent.Err()
		}
		return fmt.Errorf("执行超时（单元时限 %s）", q.jobTimeout)
	}
}

// scheduleRetry 到点把同一单元重新入队；等待期间**不占 worker**。
//
// 等待期间在途标记保持置位：超时清扫据此知道「这个子项的定时器还在，别当僵尸清掉」。
// 进程重启则标记随之消失（表在内存里），清扫器就能把「定时器已随重启丢失」的子项
// 正常收尾 —— 这正是 2026-10-31 任务 429 卡死 8 小时的那类情形。
func (q *Queue) scheduleRetry(parent context.Context, job JobPayload, delay time.Duration) {
	key := inflightKey(job.TaskID, job.ItemKey)
	q.markInflight(key, 1)
	go func() {
		defer func() { _ = recover() }()
		q.sleep(delay)
		q.markInflight(key, -1)
		if parent.Err() != nil {
			return // 队列已停：丢弃，不再入队
		}
		q.Enqueue(job)
	}()
}

// inflightKey 生成在途键；批量单元（无 task/item）不登记，返回空串。
func inflightKey(taskID int64, itemKey string) string {
	if taskID == 0 || itemKey == "" {
		return ""
	}
	return strconv.FormatInt(taskID, 10) + "|" + itemKey
}

// markInflight 增减在途计数（键为空则忽略）。
func (q *Queue) markInflight(key string, delta int) {
	if key == "" {
		return
	}
	q.inflightMu.Lock()
	defer q.inflightMu.Unlock()
	next := q.inflightMap[key] + delta
	if next <= 0 {
		delete(q.inflightMap, key)
		return
	}
	q.inflightMap[key] = next
}

// Inflight 报告某子项此刻是否还在队列手里（正在执行、或正在等退避）。
//
// 供超时清扫区分「队列还在管」与「重启后丢了定时器、永远收不了尾的僵尸子项」。
func (q *Queue) Inflight(taskID int64, itemKey string) bool {
	if q == nil {
		return false
	}
	key := inflightKey(taskID, itemKey)
	if key == "" {
		return false
	}
	q.inflightMu.Lock()
	defer q.inflightMu.Unlock()
	return q.inflightMap[key] > 0
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
