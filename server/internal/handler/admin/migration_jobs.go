package admin

// 后台迁移任务（故障迁移 / 批量均衡）的进度登记表 —— 内存实现。
//
// 为什么不用 internal/task 队列（任务书要求交付里说明选型理由）：
//  1. admin 包不能 import internal/task。task 包要复用本包的取数/落账实现，
//     反向 import 立刻成环（见 node_domain.go 里 NodeTaskDispatcher 的头注释，同一原因）；
//  2. 进度语义对不上。这里要的是「一次搬移的总量 / 已搬 / 失败次数 + 起止时刻」四个计数，
//     而 async_tasks 的粒度是 item（一行一个子任务）。要表达同一件事，得把计数塞进
//     meta JSON 再自己解析回来，多一层状态机、还多一次库往返，纯属自找麻烦；
//  3. 迁移是人工触发的一次性动作，不需要跨进程（单实例面板）、不需要崩溃重放 ——
//     真搬移的结果已经落在 user_nodes / user_node_migrations 里（历史接口可查）。
//     进程重启丢掉的只是「进度百分比」，不是业务事实。
// 代价（明确写清，不藏着）：进程重启后 GET /migrations/jobs/:job_id 查不到旧任务，
// 所以已结束的任务只在内存保留 1 小时（migrationJobKeep），并且「任务不存在」的文案
// 直接告诉管理员这个保留期。

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// 任务状态。前端按这三个值分支。
const (
	migrationJobRunning  = "running"
	migrationJobFinished = "finished"
	migrationJobFailed   = "failed"
)

// 任务种类。
const (
	migrationJobKindMigrate = "migrate"
	migrationJobKindBalance = "balance"
)

const (
	// migrationJobKeep 是已结束任务在内存里的保留时长。
	migrationJobKeep = time.Hour
	// migrationJobTimeout 是单个后台任务的整条时限。一次搬几千个用户，
	// 引擎内部还要按 1000 条一批 + 批间 sleep，30 分钟足够；超时标 failed 而不是无限挂着。
	migrationJobTimeout = 30 * time.Minute
)

// migrationJob 是一个后台搬移任务的进度。
//
// 字段全部经 mu 读写：HTTP 侧轮询快照、后台 goroutine 写进度，两边并发。
type migrationJob struct {
	id      string
	batchID string
	kind    string

	// afterID 是本任务开始前 user_node_migrations 的 MAX(id)：收尾时用它把「本任务
	// 期间新写的行」圈出来打批次号（引擎不写 batch_id，见 stampBatch 注释）。
	afterID int64

	mu         sync.Mutex
	status     string
	total      int
	done       int
	failed     int
	startedAt  time.Time
	finishedAt *time.Time
	lastErr    string

	now func() time.Time
}

// migrationJobSnapshot 是给 HTTP 输出用的只读快照。
type migrationJobSnapshot struct {
	ID         string
	BatchID    string
	Kind       string
	Status     string
	Total      int
	Done       int
	Failed     int
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

func (j *migrationJob) snapshot() migrationJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return migrationJobSnapshot{
		ID:         j.id,
		BatchID:    j.batchID,
		Kind:       j.kind,
		Status:     j.status,
		Total:      j.total,
		Done:       j.done,
		Failed:     j.failed,
		StartedAt:  j.startedAt,
		FinishedAt: j.finishedAt,
		Error:      j.lastErr,
	}
}

func (j *migrationJob) setProgress(done int) {
	j.mu.Lock()
	j.done = done
	j.mu.Unlock()
}

// addFailed 记一次失败。引擎只回传 (moved, err)，拿不到单用户失败数，
// 所以 failed 的口径是「失败的批次次数」——这一点在接口返回里不猜、不换算。
func (j *migrationJob) addFailed(errMsg string) {
	j.mu.Lock()
	j.failed++
	if errMsg != "" {
		j.lastErr = errMsg
	}
	j.mu.Unlock()
}

// finish 正常收尾：只在还没被判失败时翻成 finished（失败在前则保留 failed，别覆盖）。
func (j *migrationJob) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status == migrationJobRunning {
		j.status = migrationJobFinished
	}
	if j.finishedAt == nil {
		t := j.now()
		j.finishedAt = &t
	}
}

// abort 把任务标成失败，保留已有进度（前端能看到「搬到一半」）。
func (j *migrationJob) abort(errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = migrationJobFailed
	if errMsg != "" {
		j.lastErr = errMsg
	}
	if j.finishedAt == nil {
		t := j.now()
		j.finishedAt = &t
	}
}

// migrationJobRegistry 是任务表（进程内）。
type migrationJobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*migrationJob
	now  func() time.Time
}

func newMigrationJobRegistry(now func() time.Time) *migrationJobRegistry {
	if now == nil {
		now = time.Now
	}
	return &migrationJobRegistry{jobs: make(map[string]*migrationJob), now: now}
}

// create 登记一个新任务（顺带清掉过期的已结束任务 —— 没有独立清理 goroutine，
// 靠「下一次创建」顺手扫，代价是空闲期内存不回收，但任务总数本来就少）。
func (r *migrationJobRegistry) create(kind string, total int, afterID int64) *migrationJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()

	now := r.now()
	job := &migrationJob{
		// id 24 个十六进制字符、batch_id 32 个（列宽 varchar(36)）：
		// newMigrationID(n) 返回 2n 个字符，写错字节数会撞「Data too long for column 'batch_id'」。
		id:        newMigrationID(12),
		batchID:   newMigrationID(16),
		kind:      kind,
		afterID:   afterID,
		status:    migrationJobRunning,
		total:     total,
		startedAt: now,
		now:       r.now,
	}
	r.jobs[job.id] = job
	return job
}

func (r *migrationJobRegistry) get(id string) (*migrationJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	return job, ok
}

// sweepLocked 清理「已结束且超过保留期」的任务。调用方必须持有 r.mu。
func (r *migrationJobRegistry) sweepLocked() {
	cutoff := r.now().Add(-migrationJobKeep)
	for id, job := range r.jobs {
		job.mu.Lock()
		expired := job.status != migrationJobRunning &&
			job.finishedAt != nil && job.finishedAt.Before(cutoff)
		job.mu.Unlock()
		if expired {
			delete(r.jobs, id)
		}
	}
}

// newMigrationID 生成 n 个字节的随机十六进制串（n*2 个字符）。
// job_id 用 16 字节、batch_id 用 32 字节（列宽 varchar(36)，留足余量）。
func newMigrationID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// 随机源不可用属于环境级故障，退回时间戳保证任务仍可登记/查询。
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(buf)
}
