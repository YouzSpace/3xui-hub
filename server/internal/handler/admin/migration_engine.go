package admin

// 把 internal/scheduler（region-08 交付的调度引擎）适配成 MigrationEngine。
//
// 为什么单独成文件、而不是让 handler 直接 import scheduler：
//   - handler 与测试只需要 MigrationEngine 这两个方法，直接依赖具体包会把「接口层」
//     和「引擎实现」焊死，测试就得连库跑真引擎（进度类用例需要可控的假引擎）；
//   - scheduler 的 Balance 回传 []scheduler.Move，与对外输出的 MigrationMove 不是同一
//     类型，转换点集中在这一个文件里，改动面最小。
//
// 接线（cmd/server/routes.go 那一行）就是：Engine: admin.NewSchedulerEngine(db, nil)。
// locker 只有 Assign / AssignRegion 用得到（scheduler.New 的注释明确写了 Migrate/Balance
// 不抢分配锁），管理端这两个接口不涉及分配，先传 nil；等 cache.Client 接进主进程后，
// 从这里把它透传进去即可（*cache.Client 的 Lock 方法签名与 scheduler.Locker 完全一致）。

import (
	"context"
	"database/sql"

	"controlhub-server/internal/scheduler"
)

// SchedulerEngine 是 MigrationEngine 的 scheduler 实现。
type SchedulerEngine struct {
	s *scheduler.Scheduler
}

// NewSchedulerEngine 用 internal/scheduler 构造迁移引擎。
func NewSchedulerEngine(db *sql.DB, locker scheduler.Locker) *SchedulerEngine {
	return &SchedulerEngine{s: scheduler.New(db, locker)}
}

// Migrate 直接转发给 scheduler。
func (e *SchedulerEngine) Migrate(ctx context.Context, fromNodeID, toNodeID int64, count int) (int, error) {
	return e.s.Migrate(ctx, fromNodeID, toNodeID, count)
}

// Balance 转发并做计划项类型转换。
func (e *SchedulerEngine) Balance(ctx context.Context, regionID int64, dry bool, limit int) (int, []MigrationMove, error) {
	moved, plan, err := e.s.Balance(ctx, regionID, dry, limit)
	if err != nil {
		return moved, nil, err
	}
	out := make([]MigrationMove, 0, len(plan))
	for _, mv := range plan {
		out = append(out, MigrationMove{
			UserID:     int64(mv.UserID),
			FromNodeID: mv.FromNodeID,
			ToNodeID:   mv.ToNodeID,
		})
	}
	return moved, out, nil
}

// 编译期断言：SchedulerEngine 必须满足 MigrationEngine。
var _ MigrationEngine = (*SchedulerEngine)(nil)
