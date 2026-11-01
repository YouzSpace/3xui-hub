package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 异步任务体系：逐条对齐 backend/app/Services/AsyncTaskService.php
// （async_tasks + async_task_items，日志页可重试）。
//
// 状态机铁律（PHP P2-3 终态粘性，抄错会在「超时兜底 vs 迟到回执」上翻转成败）：
//   - 任务终态 = succeeded / failed；一旦判终态，迟到的回执只刷新计数，不翻转 status；
//   - claimItem 对终态任务拒绝认领（避免复活并翻转 失败→成功）；
//   - 合法重试走 retry()（先把任务重置为 pending 再派发），不受终态粘性影响。
//
// 状态常量（与 AsyncTask / AsyncTaskItem 模型逐字一致）。

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusPartial   = "partial"
	StatusFailed    = "failed"
)

// 任务类型（AsyncTaskService::dispatch 的分支键）。
const (
	TypeTrafficSync  = "traffic_sync"
	TypeNodeInit     = "node_init"
	TypeNodeAction   = "node_action"
	TypeDomainAction = "domain_action"
)

// meta.action 取值（node_action / domain_action）。
const (
	ActionSyncInbounds   = "sync_inbounds"
	ActionCleanupClients = "cleanup_clients"
)

// config/tasks.php 的缺省值（env 可覆盖）。
const (
	defaultStaleAfterMinutes = 10
	defaultNodeSyncBatch     = 2
	defaultMaxLockReleases   = 5

	// staleItemMinutes 是「未终态子项多久没动静就判定僵尸」的阈值。
	//
	// 取 6 分钟 = 队列单元时限（5 分钟）+ 1 分钟余量：正常单元最迟 5 分钟就会有终态，
	// 超过 6 分钟还没动静、且队列也不认为在途（见 Queue.Inflight），只可能是退避定时器
	// 随进程重启丢了，必须收尾，否则任务永远「执行中」。
	staleItemMinutes = 6
)

// Task 是 async_tasks 一行。
type Task struct {
	ID          int64
	Type        string
	Status      string
	SubjectType *string
	SubjectID   *int64
	Total       int
	Completed   int
	Failed      int
	Attempts    int
	MaxAttempts int
	Error       *string
	Meta        json.RawMessage
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

// MetaMap 是 meta JSON 的 map 形态（找不到/解析失败 → 空 map）。
func (t Task) MetaMap() map[string]any {
	if len(t.Meta) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(t.Meta, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// MetaInt 读 meta 里的整数（PHP isset($meta['k']) ? (int)$meta['k'] : null）。
func (t Task) MetaInt(key string) *int64 {
	v, ok := t.MetaMap()[key]
	if !ok || v == nil {
		return nil
	}
	n := toInt64(v)
	return &n
}

// MetaStr 读 meta 里的字符串。
func (t Task) MetaStr(key string) *string {
	v, ok := t.MetaMap()[key]
	if !ok || v == nil {
		return nil
	}
	s := fmt.Sprintf("%v", v)
	return &s
}

// Item 是 async_task_items 一行。
type Item struct {
	ID         int64
	TaskID     int64
	ItemKey    string
	Status     string
	Attempts   int
	Error      *string
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// Service 是异步任务体系入口（*sql.DB 并发安全）。
type Service struct {
	DB  *sql.DB
	Now func() time.Time // nil → time.Now().UTC()

	// Queue 是派发出口（queue.go）；nil 时 dispatch 只落库不执行。
	Queue Dispatcher
	// NodeSyncBatch 是 node_init / sync_inbounds 的批大小（config tasks.node_sync_batch）。
	NodeSyncBatch int
	// StaleAfterMinutes 是超时清扫阈值（config tasks.stale_after_minutes）。
	StaleAfterMinutes int
	// MaxLockReleases 是节点锁竞争时的重入队上限（config tasks.max_lock_releases）。
	MaxLockReleases int
}

// NewService 构造异步任务服务。
func NewService(db *sql.DB, queue Dispatcher) *Service {
	return &Service{
		DB:                db,
		Queue:             queue,
		NodeSyncBatch:     envInt("TASKS_NODE_SYNC_BATCH", defaultNodeSyncBatch),
		StaleAfterMinutes: envInt("TASKS_STALE_AFTER_MINUTES", defaultStaleAfterMinutes),
		MaxLockReleases:   envInt("TASKS_MAX_LOCK_RETRIES", defaultMaxLockReleases),
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// dbDateTime 是 MySQL datetime 列的写入格式（Laravel toDateTimeString，UTC 字面量）。
func dbDateTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

// SummaryFor 对齐 AsyncTaskService::summaryFor()。
func SummaryFor(taskType string) string {
	switch taskType {
	case TypeTrafficSync:
		return "节点同步失败"
	case TypeNodeInit:
		return "节点接入初始化失败"
	case TypeNodeAction:
		return "节点操作失败"
	case TypeDomainAction:
		return "域名操作失败"
	default:
		return "任务执行失败"
	}
}

// PublicError 对齐 AsyncTaskService::publicError()：对外只暴露按类型归一的文案。
func PublicError(t Task) *string {
	if t.Error == nil {
		return nil
	}
	s := SummaryFor(t.Type)
	return &s
}

// isTerminal 是任务终态判定（PHP in_array(status, [succeeded, failed])）。
func isTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed
}

// ── 建任务 ─────────────────────────────────────────────────

// CreateInput 是 create() 的入参。
type CreateInput struct {
	Type        string
	SubjectType *string
	SubjectID   *int64
	Total       int
	Meta        json.RawMessage
	MaxAttempts int
	ItemKeys    []string
}

// Create 对齐 AsyncTaskService::create()：事务内建任务 + 去重后的 item。
func (s *Service) Create(ctx context.Context, in CreateInput) (*Task, error) {
	if in.Total < 0 || in.MaxAttempts < 1 {
		return nil, fmt.Errorf("任务参数无效")
	}
	keys := normalizeItemKeys(in.ItemKeys, in.Total)

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	var metaVal any
	if len(in.Meta) > 0 {
		metaVal = string(in.Meta)
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO async_tasks (type, status, subject_type, subject_id, total, completed, failed, "+
			"attempts, max_attempts, meta, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 0, 0, 0, 0, ?, ?, ?, ?)",
		in.Type, StatusPending, in.SubjectType, in.SubjectID, in.MaxAttempts, metaVal, now, now)
	if err != nil {
		return nil, fmt.Errorf("创建异步任务失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := insertItems(ctx, tx, id, keys, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Task{
		ID: id, Type: in.Type, Status: StatusPending,
		SubjectType: in.SubjectType, SubjectID: in.SubjectID,
		Total: len(keys), MaxAttempts: in.MaxAttempts, Meta: in.Meta,
	}, nil
}

// AddItems 对齐 AsyncTaskService::addItems()：insertOrIgnore 去重后回填 total。
func (s *Service) AddItems(ctx context.Context, taskID int64, itemKeys []string) error {
	keys := normalizeItemKeys(itemKeys, 0)
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertItems(ctx, tx, taskID, keys, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE async_tasks SET total = (SELECT COUNT(*) FROM async_task_items WHERE task_id = ?), updated_at = ? WHERE id = ?",
		taskID, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// insertItems 复刻 insertOrIgnore（唯一键 (task_id,item_key)）。
func insertItems(ctx context.Context, tx *sql.Tx, taskID int64, keys []string, now time.Time) error {
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx,
			"INSERT IGNORE INTO async_task_items (task_id, item_key, status, attempts, created_at, updated_at) "+
				"VALUES (?, ?, ?, 0, ?, ?)",
			taskID, key, StatusPending, now, now); err != nil {
			return fmt.Errorf("写入任务项 %s 失败: %w", key, err)
		}
	}
	// PHP create() 里 total = count($itemKeys)（去重后），addItems 后再数一次。
	if _, err := tx.ExecContext(ctx,
		"UPDATE async_tasks SET total = (SELECT COUNT(*) FROM async_task_items WHERE task_id = ?) WHERE id = ?",
		taskID, taskID); err != nil {
		return err
	}
	return nil
}

// normalizeItemKeys 复刻 create() 的 item_key 归一：
// 未给 key 且 total>0 → item:1..N；去重保序。
func normalizeItemKeys(keys []string, total int) []string {
	if len(keys) == 0 && total > 0 {
		keys = make([]string, 0, total)
		for i := 1; i <= total; i++ {
			keys = append(keys, "item:"+strconv.Itoa(i))
		}
	}
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// FindOrCreateTrafficSync 对齐 findOrCreateTrafficSync()：
// 同 subject 的在途 traffic_sync 任务去重（cache 锁防并发重复建）。
func (s *Service) FindOrCreateTrafficSync(ctx context.Context, nodeIDs []int64,
	subjectType *string, subjectID *int64, userID *int64) (task *Task, created bool, err error) {

	scope := "all"
	if subjectType != nil {
		scope = *subjectType + ":" + fmt.Sprintf("%v", subjectID)
	}
	lockOwner, ok, err := acquireCacheLock(ctx, s.DB, "traffic-sync-submit:"+scope, 10, s.now())
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, fmt.Errorf("获取 traffic-sync-submit 锁失败（scope=%s）", scope)
	}
	defer releaseCacheLock(context.WithoutCancel(ctx), s.DB, "traffic-sync-submit:"+scope, lockOwner)

	active, err := s.activeTrafficSync(ctx, subjectType, subjectID)
	if err != nil {
		return nil, false, err
	}
	if active != nil {
		return active, false, nil
	}

	ids := uniqueInt64s(nodeIDs)
	meta := map[string]any{"node_ids": ids}
	if userID != nil {
		meta["user_id"] = *userID
	}
	metaJSON, _ := json.Marshal(meta)

	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, "node:"+strconv.FormatInt(id, 10))
	}
	task, err = s.Create(ctx, CreateInput{
		Type:        TypeTrafficSync,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		Total:       len(ids),
		Meta:        metaJSON,
		MaxAttempts: 5,
		ItemKeys:    keys,
	})
	if err != nil {
		return nil, false, err
	}
	if len(ids) == 0 {
		task, err = s.CompleteEmpty(ctx, task.ID)
		if err != nil {
			return nil, false, err
		}
		return task, true, nil
	}
	if err := s.Dispatch(ctx, task, nil); err != nil {
		return nil, false, err
	}
	return task, true, nil
}

// activeTrafficSync 查同 subject 的在途任务（latest id）。
func (s *Service) activeTrafficSync(ctx context.Context, subjectType *string, subjectID *int64) (*Task, error) {
	row := s.DB.QueryRowContext(ctx,
		"SELECT id, type, status, subject_type, subject_id, total, completed, failed, attempts, "+
			"max_attempts, error, meta, started_at, finished_at FROM async_tasks "+
			"WHERE type = ? AND subject_type <=> ? AND subject_id <=> ? "+
			"AND status IN (?, ?) ORDER BY id DESC LIMIT 1",
		TypeTrafficSync, subjectType, subjectID, StatusPending, StatusRunning)
	return scanTask(row)
}

// ── 子项状态机 ─────────────────────────────────────────────

// ClaimItem 对齐 claimItem()：认领子项（事务 + 行锁）。
//
// 返回 false 的三种情况（PHP 同）：子项已成功、任务已终态、子项不存在。
// 任务 attempts 已到上限且仍 pending → 报错（PHP InvalidArgumentException）。
func (s *Service) ClaimItem(ctx context.Context, taskID int64, itemKey string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := lockTask(ctx, tx, taskID)
	if err != nil {
		return false, err
	}
	item, err := lockItem(ctx, tx, taskID, itemKey)
	if err != nil {
		return false, err
	}
	claim, err := ClaimDecision(task.Status, item.Status, task.Attempts, task.MaxAttempts)
	if err != nil || !claim {
		return claim, err
	}

	now := s.now()
	if task.Status == StatusPending {
		started := now
		if task.StartedAt != nil {
			started = *task.StartedAt
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE async_tasks SET status = ?, attempts = ?, error = NULL, started_at = ?, "+
				"finished_at = NULL, updated_at = ? WHERE id = ?",
			StatusRunning, task.Attempts+1, started, now, taskID); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE async_task_items SET status = ?, attempts = ?, error = NULL, started_at = ?, "+
			"finished_at = NULL, updated_at = ? WHERE id = ?",
		StatusRunning, item.Attempts+1, now, now, item.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ClaimDecision 是 ClaimItem 的纯判定（行为契约：P2-3 终态粘性 + 上限拒绝）。
//
//   - item 已 succeeded → 不认领（false, nil）
//   - 任务已终态（succeeded/failed）→ 不认领（false, nil）——迟到 Job 不许复活任务
//   - 任务 pending 且 attempts >= max_attempts → 错误（PHP InvalidArgumentException）
//   - 其余 → 认领
func ClaimDecision(taskStatus, itemStatus string, attempts, maxAttempts int) (bool, error) {
	if itemStatus == StatusSucceeded {
		return false, nil
	}
	if isTerminal(taskStatus) {
		return false, nil
	}
	if attempts >= maxAttempts && taskStatus == StatusPending {
		return false, fmt.Errorf("任务已达到最大尝试次数")
	}
	return true, nil
}

// CompleteItem 对齐 completeItem()。
func (s *Service) CompleteItem(ctx context.Context, taskID int64, itemKey string) (*Task, error) {
	return s.finishItem(ctx, taskID, itemKey, StatusSucceeded, nil)
}

// FailItem 对齐 failItem()。
func (s *Service) FailItem(ctx context.Context, taskID int64, itemKey, summary string) (*Task, error) {
	return s.finishItem(ctx, taskID, itemKey, StatusFailed, &summary)
}

// finishItem 对齐 finishItem()：写子项终态 → aggregateLocked（任务终态粘性）。
func (s *Service) finishItem(ctx context.Context, taskID int64, itemKey, status string, summary *string) (*Task, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := lockTask(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	item, err := lockItem(ctx, tx, taskID, itemKey)
	if err != nil {
		return nil, err
	}
	// 任务已终态 / 子项已成功 → 只刷新计数不翻转（P2-3）。
	if isTerminal(task.Status) || item.Status == StatusSucceeded {
		return task, nil
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx,
		"UPDATE async_task_items SET status = ?, error = ?, finished_at = ?, updated_at = ? WHERE id = ?",
		status, summary, now, now, item.ID); err != nil {
		return nil, err
	}
	if err := aggregateLocked(ctx, tx, task, s.now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, taskID)
}

// aggregateLocked 对齐 aggregateLocked()：按子项计数回填任务计数/状态。
//
// active == 0 时才判终态；任务已终态则**只刷计数**（终态粘性，失败→成功不许跳变）。
func aggregateLocked(ctx context.Context, tx *sql.Tx, task *Task, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		"SELECT status, COUNT(*) FROM async_task_items WHERE task_id = ? GROUP BY status", task.ID)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return err
		}
		counts[st] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	total := 0
	for _, n := range counts {
		total += n
	}
	completed := counts[StatusSucceeded]
	failed := counts[StatusFailed]
	active := counts[StatusPending] + counts[StatusRunning]

	fields := []any{total, completed, failed, now}
	q := "UPDATE async_tasks SET total = ?, completed = ?, failed = ?, updated_at = ?"
	if status, finish := AggregateDecision(task.Status, completed, failed, active); finish {
		// 判终态：有失败 → failed（error 用类型文案），全成 → succeeded。
		var summary any
		if status == StatusFailed {
			summary = SummaryFor(task.Type)
		}
		q += ", status = ?, error = ?, finished_at = ?"
		fields = append(fields, status, summary, now)
	}
	q += " WHERE id = ?"
	fields = append(fields, task.ID)
	_, err = tx.ExecContext(ctx, q, fields...)
	return err
}

// AggregateDecision 是 aggregateLocked 的纯判定（P2-3 终态粘性）。
//
// active == 0 才判终态；任务已终态则不 finish（只刷计数，失败→成功不许跳变）。
// 返回 (newStatus, finish)：newStatus 仅在 finish 时有意义。
func AggregateDecision(taskStatus string, completed, failed, active int) (string, bool) {
	_ = completed // 计数只用于回填列，不参与状态判定（与 PHP 一致）
	if active != 0 || isTerminal(taskStatus) {
		return "", false
	}
	if failed > 0 {
		return StatusFailed, true
	}
	return StatusSucceeded, true
}

// CompleteEmpty 对齐 completeEmpty()：0 项任务直接判成功。
func (s *Service) CompleteEmpty(ctx context.Context, taskID int64) (*Task, error) {
	now := s.now()
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE async_tasks SET status = ?, total = 0, completed = 0, failed = 0, error = NULL, "+
			"finished_at = ?, updated_at = ? WHERE id = ?",
		StatusSucceeded, now, now, taskID); err != nil {
		return nil, err
	}
	return s.Get(ctx, taskID)
}

// TimeoutStale 对齐 timeoutStale()：把超阈值未终态的任务标失败，防止永久 running。
// 返回被清扫的任务数。
//
// 两条判据取并集：
//   - 任务行本身太久没动（原判据）；
//   - **未终态子项**太久没动 —— 任务行的 updated_at 会被子项聚合/认领刷新，只看任务行
//     会漏掉「重试定时器随进程重启丢失」这种情形：子项永远停在 running、任务永远显示
//     「执行中」、队列监控永远显示「运行中 N」（2026-10-31 实测：任务 429 的 4 台裸内核
//     子项从 13:37 卡到 22:00 无人收尾，正是这个盲点）。
func (s *Service) TimeoutStale(ctx context.Context, minutes int) (int, error) {
	if minutes <= 0 {
		minutes = s.StaleAfterMinutes
	}
	if minutes <= 0 {
		minutes = defaultStaleAfterMinutes
	}
	cutoff := s.now().Add(-time.Duration(minutes) * time.Minute)

	staleIDs := map[int64]bool{}
	rows, err := s.DB.QueryContext(ctx,
		"SELECT id FROM async_tasks WHERE status IN (?, ?) AND updated_at < ?",
		StatusPending, StatusRunning, cutoff)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		staleIDs[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// 子项级判据：静默超过 staleItemMinutes 的未终态子项。
	//
	// 为什么要这一条：任务行的 updated_at 会被子项聚合/认领刷新，只看任务行会漏掉
	// 「退避定时器随进程重启丢失」这种情形 —— 子项永远停在 running、任务永远显示
	// 「执行中」、队列监控永远显示「运行中 N」（2026-10-31 任务 429 的 4 个裸内核
	// 子项从 13:37 卡到 22:00 无人收尾，正是这个盲点）。
	//
	// 在途豁免：队列还在管的子项（正在执行 / 正在等退避）一律不动，避免把正常的重试
	// 半路清掉；进程重启后队列的内存表是空的，僵尸子项才会落进来被收尾。
	itemCutoff := s.now().Add(-staleItemMinutes * time.Minute)
	irows, err := s.DB.QueryContext(ctx,
		"SELECT DISTINCT task_id, item_key FROM async_task_items "+
			"WHERE status IN (?, ?) AND updated_at < ?",
		StatusPending, StatusRunning, itemCutoff)
	if err != nil {
		return len(staleIDs), err
	}
	type itemRef struct {
		taskID int64
		key    string
	}
	var itemRefs []itemRef
	for irows.Next() {
		var ref itemRef
		if err := irows.Scan(&ref.taskID, &ref.key); err != nil {
			irows.Close()
			return len(staleIDs), err
		}
		itemRefs = append(itemRefs, ref)
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return len(staleIDs), err
	}
	for _, ref := range itemRefs {
		// 队列还在管（执行中 / 等退避 / 已入队排队）就不清；取不到在途信息时保守跳过，
		// 宁可少清一次也不误杀正常单元。
		if reporter, ok := s.Queue.(InflightReporter); ok && reporter.Inflight(ref.taskID, ref.key) {
			continue
		}
		staleIDs[ref.taskID] = true
	}

	ids := make([]int64, 0, len(staleIDs))
	for id := range staleIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if _, err := s.FailPendingItems(ctx, id, "任务超时未完成"); err != nil {
			return len(ids), err
		}
	}
	return len(ids), nil
}

// FailPendingItems 对齐 failPendingItems()：未终态子项批量标失败后聚合。
func (s *Service) FailPendingItems(ctx context.Context, taskID int64, summary string) (*Task, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := lockTask(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx,
		"UPDATE async_task_items SET status = ?, error = ?, finished_at = ?, updated_at = ? "+
			"WHERE task_id = ? AND status IN (?, ?)",
		StatusFailed, summary, now, now, taskID, StatusPending, StatusRunning); err != nil {
		return nil, err
	}
	if err := aggregateLocked(ctx, tx, task, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, taskID)
}

// Retry 对齐 retry()：仅失败任务可重试；失败项重置为 pending 后重新派发。
//
// 入站同步（node_action + meta.action=sync_inbounds）的重试重新走一遍【扫描 + 差异】，
// 而不是重放逐用户 Job —— 与 PHP 同分支。
func (s *Service) Retry(ctx context.Context, taskID int64) (*Task, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := lockTask(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != StatusFailed {
		return nil, fmt.Errorf("仅失败任务可以重试")
	}
	if task.Attempts >= task.MaxAttempts {
		return nil, fmt.Errorf("任务已达到最大尝试次数")
	}

	now := s.now()
	rows, err := tx.QueryContext(ctx,
		"SELECT item_key FROM async_task_items WHERE task_id = ? AND status = ?", taskID, StatusFailed)
	if err != nil {
		return nil, err
	}
	var failedKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		failedKeys = append(failedKeys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(failedKeys) == 0 {
		return nil, fmt.Errorf("没有可重试的失败项")
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE async_task_items SET status = ?, error = NULL, started_at = NULL, finished_at = NULL, "+
			"updated_at = ? WHERE task_id = ? AND status = ?",
		StatusPending, now, taskID, StatusFailed); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE async_tasks SET status = ?, failed = 0, error = NULL, finished_at = NULL, updated_at = ? WHERE id = ?",
		StatusPending, now, taskID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	fresh, err := s.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// 入站同步重试走扫描，不重放逐用户 Job（PHP afterCommit 分支）。
	if IsInboundSync(fresh) {
		if s.Queue != nil {
			s.Queue.Enqueue(JobPayload{
				Kind:   JobNodeInboundScan,
				TaskID: taskID,
				NodeID: subjectIDOf(fresh),
			})
		}
		return fresh, nil
	}
	if err := s.Dispatch(ctx, fresh, failedKeys); err != nil {
		return nil, err
	}
	return fresh, nil
}

// Get 读任务行。
func (s *Service) Get(ctx context.Context, taskID int64) (*Task, error) {
	row := s.DB.QueryRowContext(ctx,
		"SELECT id, type, status, subject_type, subject_id, total, completed, failed, attempts, "+
			"max_attempts, error, meta, started_at, finished_at FROM async_tasks WHERE id = ?", taskID)
	return scanTask(row)
}

// ── 派发（对齐 dispatch()） ─────────────────────────────────

// Dispatch 对齐 dispatch()：按类型把 pending 子项派进队列。
// onlyKeys 非 nil 时只派这些 key（retry 路径）。
func (s *Service) Dispatch(ctx context.Context, task *Task, onlyKeys []string) error {
	keys := onlyKeys
	if keys == nil {
		var err error
		keys, err = s.pendingItemKeys(ctx, task.ID)
		if err != nil {
			return err
		}
	}
	if len(keys) == 0 {
		return nil
	}

	// sync_inbounds / node_init 按批派发（批大小 config tasks.node_sync_batch）。
	if IsInboundSync(task) {
		for _, batch := range UserItemBatches(keys, s.batchSize()) {
			s.enqueue(JobPayload{
				Kind:     JobNodeInboundSync,
				TaskID:   task.ID,
				NodeID:   subjectIDOf(task),
				UserIDs:  batch.UserIDs,
				ItemKeys: batch.ItemKeys,
			})
		}
		return nil
	}
	if task.Type == TypeNodeInit {
		for _, batch := range UserItemBatches(keys, s.batchSize()) {
			s.enqueue(JobPayload{
				Kind:     JobNodeInitUser,
				TaskID:   task.ID,
				NodeID:   subjectIDOf(task),
				UserIDs:  batch.UserIDs,
				ItemKeys: batch.ItemKeys,
			})
		}
		return nil
	}

	for _, key := range keys {
		switch task.Type {
		case TypeTrafficSync:
			nodeID := parseIDPrefix(key, "node:")
			s.enqueue(JobPayload{
				Kind:    JobSyncNodeTraffic,
				TaskID:  task.ID,
				ItemKey: key,
				NodeID:  nodeID,
				UserID:  task.MetaInt("user_id"),
			})
		case TypeDomainAction:
			action := "apply"
			if v := task.MetaStr("action"); v != nil {
				action = *v
			}
			s.enqueue(JobPayload{
				Kind:    JobApplyDomain,
				TaskID:  task.ID,
				ItemKey: key,
				NodeID:  subjectIDOf(task),
				Action:  action,
				Domain:  task.MetaStr("domain"),
			})
		case TypeNodeAction:
			action := ""
			if v := task.MetaStr("action"); v != nil {
				action = *v
			}
			switch action {
			case ActionCleanupClients:
				s.enqueue(JobPayload{
					Kind:         JobNodeClientsCleanup,
					TaskID:       task.ID,
					ItemKey:      key,
					NodeID:       subjectIDOf(task),
					Action:       action,
					NodeSnapshot: task.MetaMap()["node_snapshot"],
				})
			default:
				return fmt.Errorf("不支持的节点操作：%s", action)
			}
		default:
			return fmt.Errorf("不支持任务类型：%s", task.Type)
		}
	}
	return nil
}

// DispatchAfterCommit 对齐 dispatchAfterCommit()（Go 侧无事务外提，
// Create/Retry 的提交点即派发点，语义等价）。
func (s *Service) DispatchAfterCommit(ctx context.Context, task *Task) error {
	return s.Dispatch(ctx, task, nil)
}

// IsInboundSync 对齐 isInboundSync()：type=node_action 且 meta.action=sync_inbounds。
func IsInboundSync(t *Task) bool {
	return t.Type == TypeNodeAction && t.MetaStr("action") != nil && *t.MetaStr("action") == ActionSyncInbounds
}

// UserBatch 是 userItemBatches() 的一批产物。
type UserBatch struct {
	UserIDs  []int64
	ItemKeys []string
}

// UserItemBatches 对齐 userItemBatches()：把「user:{id}」形状的 item 按批分组。
//
// 批大小统一读 config('tasks.node_sync_batch')：node_init 与 sync_inbounds 的 item 都是
// 「1 个用户 × 1 台节点」，单个 Job 的成本结构完全一样，没有理由各配一套。
func UserItemBatches(itemKeys []string, batchSize int) []UserBatch {
	if batchSize < 1 {
		batchSize = defaultNodeSyncBatch
	}
	var out []UserBatch
	for i := 0; i < len(itemKeys); i += batchSize {
		end := i + batchSize
		if end > len(itemKeys) {
			end = len(itemKeys)
		}
		chunk := itemKeys[i:end]
		b := UserBatch{ItemKeys: append([]string(nil), chunk...)}
		for _, k := range chunk {
			b.UserIDs = append(b.UserIDs, parseIDPrefix(k, "user:"))
		}
		out = append(out, b)
	}
	return out
}

func (s *Service) batchSize() int {
	if s.NodeSyncBatch > 0 {
		return s.NodeSyncBatch
	}
	return defaultNodeSyncBatch
}

// PendingItemKeys 是 pendingItemKeys 的导出形态（节点类 Job 需要用它在扫描时做 diff）。
func (s *Service) PendingItemKeys(ctx context.Context, taskID int64) ([]string, error) {
	return s.pendingItemKeys(ctx, taskID)
}

func (s *Service) pendingItemKeys(ctx context.Context, taskID int64) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT item_key FROM async_task_items WHERE task_id = ? AND status = ? ORDER BY id",
		taskID, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Service) enqueue(p JobPayload) {
	if s.Queue == nil {
		return
	}
	s.Queue.Enqueue(p)
}

// ── SQL 扫描辅助 ───────────────────────────────────────────

type scanner interface{ Scan(...any) error }

const taskColumns = "id, type, status, subject_type, subject_id, total, completed, failed, " +
	"attempts, max_attempts, error, meta, started_at, finished_at"

func scanTask(sc scanner) (*Task, error) {
	var (
		t       Task
		subjTyp sql.NullString
		subjID  sql.NullInt64
		errMsg  sql.NullString
		meta    sql.NullString
		started sql.NullTime
		done    sql.NullTime
	)
	err := sc.Scan(&t.ID, &t.Type, &t.Status, &subjTyp, &subjID, &t.Total, &t.Completed, &t.Failed,
		&t.Attempts, &t.MaxAttempts, &errMsg, &meta, &started, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if subjTyp.Valid {
		s := subjTyp.String
		t.SubjectType = &s
	}
	if subjID.Valid {
		v := subjID.Int64
		t.SubjectID = &v
	}
	if errMsg.Valid {
		s := errMsg.String
		t.Error = &s
	}
	if meta.Valid {
		t.Meta = json.RawMessage(meta.String)
	}
	if started.Valid {
		v := started.Time
		t.StartedAt = &v
	}
	if done.Valid {
		v := done.Time
		t.FinishedAt = &v
	}
	return &t, nil
}

func lockTask(ctx context.Context, tx *sql.Tx, taskID int64) (*Task, error) {
	row := tx.QueryRowContext(ctx,
		"SELECT "+taskColumns+" FROM async_tasks WHERE id = ? FOR UPDATE", taskID)
	t, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("异步任务 #%d 不存在", taskID)
	}
	return t, nil
}

func lockItem(ctx context.Context, tx *sql.Tx, taskID int64, itemKey string) (*Item, error) {
	var (
		it      Item
		errMsg  sql.NullString
		started sql.NullTime
		done    sql.NullTime
	)
	err := tx.QueryRowContext(ctx,
		"SELECT id, task_id, item_key, status, attempts, error, started_at, finished_at "+
			"FROM async_task_items WHERE task_id = ? AND item_key = ? FOR UPDATE",
		taskID, itemKey).Scan(&it.ID, &it.TaskID, &it.ItemKey, &it.Status, &it.Attempts,
		&errMsg, &started, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("任务项 %s 不存在", itemKey)
	}
	if err != nil {
		return nil, err
	}
	if errMsg.Valid {
		s := errMsg.String
		it.Error = &s
	}
	if started.Valid {
		v := started.Time
		it.StartedAt = &v
	}
	if done.Valid {
		v := done.Time
		it.FinishedAt = &v
	}
	return &it, nil
}

// ── cache_locks（Laravel DatabaseLock 同口径，findOrCreateTrafficSync 防并发） ──

// acquireCacheLock 复刻 Cache::lock(name, ttl)->block(...) 的底层 DatabaseLock::acquire：
// INSERT IGNORE 抢锁；被占用时只在「已过期」时接管。
func acquireCacheLock(ctx context.Context, db *sql.DB, key string, ttlSec int, now time.Time) (string, bool, error) {
	owner := lockOwnerToken()
	expires := now.Unix() + int64(ttlSec)
	res, err := db.ExecContext(ctx,
		"INSERT IGNORE INTO cache_locks (`key`, owner, expiration) VALUES (?, ?, ?)",
		key, owner, expires)
	if err != nil {
		return "", false, fmt.Errorf("获取缓存锁失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return owner, true, nil
	}
	res, err = db.ExecContext(ctx,
		"UPDATE cache_locks SET owner = ?, expiration = ? WHERE `key` = ? AND (owner = ? OR expiration <= ?)",
		owner, expires, key, owner, now.Unix())
	if err != nil {
		return "", false, fmt.Errorf("接管缓存锁失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return owner, n >= 1, nil
}

func releaseCacheLock(ctx context.Context, db *sql.DB, key, owner string) {
	_, _ = db.ExecContext(ctx, "DELETE FROM cache_locks WHERE `key` = ? AND owner = ?", key, owner)
}

func lockOwnerToken() string {
	return "task-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// ── 小工具 ─────────────────────────────────────────────────

// parseIDPrefix 解析 "user:12" / "node:3" 这类 item_key（PHP explode(':', $key, 2)）。
func parseIDPrefix(key, prefix string) int64 {
	if !strings.HasPrefix(key, prefix) {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(key, prefix), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func subjectIDOf(t *Task) int64 {
	if t.SubjectID == nil {
		return 0
	}
	return *t.SubjectID
}

// uniqueInt64s 复刻 array_values(array_unique(array_map('intval', $nodeIds)))：
// 去重保序（PHP array_unique 保留首次出现，不排序）。
func uniqueInt64s(in []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// toInt64 是 PHP (int)$v 的宽松形态。
func toInt64(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// envInt 读整数 env（非法/缺失回落 def，与 config/tasks.php 的 env() 同向）。
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
