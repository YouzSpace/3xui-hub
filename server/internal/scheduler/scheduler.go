// Package scheduler 负责「用户 → 节点」的分配、地区内均衡与故障迁移。
//
// 边界（#地区功能）：只做决策与数据库写入 —— 不碰 HTTP、不碰订阅渲染、
// 不碰 3x-ui 下发；接线（购买流程、管理端接口）由后续任务做。
//
// 依赖注入：
//   - 数据库是裸 *sql.DB（与仓库其他包一致，手写 SQL）；
//   - 分布式锁抽象成 Locker 接口，签名与 internal/cache.Client.Lock 完全一致：
//     生产接线直接把 *cache.Client 传进来即可，测试传 fake —— 本包不直接依赖 Redis。
//
// 时间列一律交给 MySQL（NOW()）写入，不在这里做时区换算。
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoCapacity 表示该地区当前没有可分配的节点（没有候选节点，或候选全部满容量）。
// 调用方据此给用户提示「该地区暂无可售」。
var ErrNoCapacity = errors.New("scheduler: 该地区暂无可分配节点")

// Assignment 是一次分配的结果：地区 → 节点。
type Assignment struct {
	RegionID int64
	NodeID   int64
}

// Move 是一次节点间搬运的计划项。
type Move struct {
	UserID     uint64
	FromNodeID int64
	ToNodeID   int64
}

// Locker 是分配时需要的分布式锁能力，签名与 internal/cache.Client.Lock 一致：
// 拿到锁 -> (unlock, true, nil)，unlock 必须调用；拿不到锁 -> (nil, false, nil)，
// 这不算错误，由调用方决定是否重试。
type Locker interface {
	Lock(ctx context.Context, key string, ttl time.Duration) (unlock func(), ok bool, err error)
}

// 分配锁：按地区分片，TTL 3 秒足够一次「选节点 + 写绑定」。
//
// 首次抢锁失败算 1 次尝试，之后最多再重试 3 次、每次间隔 50ms。
const (
	assignLockTTL        = 3 * time.Second
	assignLockKeyFmt     = "ch:lock:assign:region:%d"
	assignLockTries      = 4
	assignLockRetryDelay = 50 * time.Millisecond
)

// Scheduler 是分配器。db / locker 都并发安全，进程内可全局共享一个实例。
type Scheduler struct {
	db     *sql.DB
	locker Locker

	// onMigrateBatch 仅测试用：每批迁移结束回调一次，参数为本批实际迁移数。
	// 生产路径为 nil，不做任何事。
	onMigrateBatch func(moved int)
}

// New 构造 Scheduler。locker 只有 Assign/AssignRegion 用得到，
// Balance / Migrate 不抢分配锁，传 nil 也能跑。
func New(db *sql.DB, locker Locker) *Scheduler {
	return &Scheduler{db: db, locker: locker}
}

// execer 抹掉 *sql.DB 与 *sql.Tx 的差异，让「搬一条绑定」在批量事务内外都能复用。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// lockRegion 抢某个地区的分配锁；ctx 取消时立即放弃。
func (s *Scheduler) lockRegion(ctx context.Context, regionID int64) (func(), error) {
	return s.lockKey(ctx, fmt.Sprintf(assignLockKeyFmt, regionID))
}

// lockKey 抢一把分配锁（地区分配与出口分类分配共用同一套重试口径）。
func (s *Scheduler) lockKey(ctx context.Context, key string) (func(), error) {
	if s.locker == nil {
		return nil, errors.New("scheduler: 未注入分布式锁实现，无法分配节点")
	}
	for attempt := 1; attempt <= assignLockTries; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(assignLockRetryDelay):
			}
		}
		unlock, ok, err := s.locker.Lock(ctx, key, assignLockTTL)
		if err != nil {
			return nil, fmt.Errorf("获取分配锁失败(key=%s): %w", key, err)
		}
		if ok {
			if unlock == nil {
				return func() {}, nil
			}
			return unlock, nil
		}
	}
	return nil, fmt.Errorf("获取分配锁失败(key=%s): 重试 %d 次仍被占用", key, assignLockTries-1)
}
