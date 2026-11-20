package admin

// 管理端「老用户批量入组」（POST /admin-api/regions/backfill）。
//
// 为什么单独成文件：地区域（region_domain.go）只做 CRUD 与查询，语义上「改 user_nodes」
// 属于调度侧；这里只放一个补数据入口 + 与 scheduler 之间的窄接口适配，改起来不牵动地区 CRUD。
//
// 入参（全部可选）：
//
//	dry    默认 true，只预览不写库；只有显式传 false 才真写
//	limit  本批候选用户数，默认 500，上限 5000（超出按 5000 夹住）
//
// 出参（前端契约，字段顺序固定）：
//
//	dry, candidates, assigned, users_done, users_skipped, remaining,
//	by_node: [{node_id, count}], skipped: [{region_id, region_name, reason, count}]
//
// candidates 是「本批开始时数据库里符合条件的用户总数」，remaining 是本批之后还没轮到的数；
// 管理端就是靠「反复调用直到 remaining = 0」把存量用户补完（前端每批都重新预览）。
//
// 引擎未接线（BackfillEngine 为 nil）时回 503 + 与迁移域同一句 errNoEngine，不伪装成「补了 0 个人」。

import (
	"context"

	"github.com/gin-gonic/gin"
)

// BackfillEngine 是「批量入组」的窄接口（internal/scheduler 的门面）。
//
// 只声明一个方法：handler 不该知道调度器还有分配 / 均衡 / 迁移。测试注入 fake，
// 可以断言 handler 默认 dry、limit 夹取与响应字段，而不必连库。
type BackfillEngine interface {
	Backfill(ctx context.Context, dry bool, limit int) (BackfillReport, error)
}

// BackfillReport 是引擎返回的原始结果（字段语义见 scheduler.BackfillResult）。
type BackfillReport struct {
	Dry          bool
	Candidates   int
	Assigned     int
	UsersDone    int
	UsersSkipped int
	Remaining    int
	ByNode       []BackfillNodeCount
	Skipped      []BackfillSkipRow
}

// BackfillNodeCount 是引擎结果里的节点分布一项。
type BackfillNodeCount struct {
	NodeID int64
	Count  int
}

// BackfillSkipRow 是引擎结果里的跳过统计一项。
type BackfillSkipRow struct {
	RegionID   int64
	RegionName string
	Reason     string
	Count      int
}

// 批量入组的 limit 口径（与 scheduler.BackfillDefaultLimit / BackfillMaxLimit 一致）。
const (
	backfillDefaultLimit = 500
	backfillMaxLimit     = 5000
)

const errNoBackfillEngine = "批量入组引擎未就绪：服务端没有接入调度器，请检查部署版本"

// backfillEngineSource 是 Store 上的可选能力：能从连接池直接造出批量入组引擎。
//
// 为什么不要求 routes.go 显式传引擎：地区域已有的接线只有 Store（NewRegionSQLStore(db)），
// 批量入组不抢分配锁、也不需要别的依赖，由 Store 自己给出引擎就够了 ——
// 这样接线处一行不用改，fake Store 不实现本接口时端点回 503（测试可以显式注入假引擎）。
type backfillEngineSource interface {
	backfillEngine() BackfillEngine
}

// backfillEngine 让 *regionSQLStore（定义在 region_sqlstore.go）满足上面的可选能力。
//
// locker 传 nil：Backfill 只做「按地区挑节点 + INSERT IGNORE」，不走 Assign/AssignRegion
// 的加锁路径（并发下重复绑定由主键兜底），与其说「忘了锁」，不如说这条路径本来就不需要
// 跨实例互斥 —— 这一点写进代码，免得后来人以为是遗漏。
func (s *regionSQLStore) backfillEngine() BackfillEngine {
	if s.db == nil {
		// 路由注册类测试会传 nil db（只验路径/方法）；此时不给引擎，端点回 503 而不是空指针崩溃。
		return nil
	}
	return NewSchedulerEngine(s.db, nil)
}

// regionBackfill 对齐 POST /admin-api/regions/backfill：{dry?, limit?}。
func (d *RegionDomain) regionBackfill(c *gin.Context) {
	engine := d.backfill
	if engine == nil {
		if src, ok := d.store.(backfillEngineSource); ok {
			engine = src.backfillEngine()
		}
	}
	if engine == nil {
		d.fail(c, errNoBackfillEngine, 503)
		return
	}

	in := jsonInput(c)
	// dry 缺省 true：这个接口会写 user_nodes，默认必须是最安全的预览态。
	dry := true
	if v, exists := in["dry"]; exists && v != nil {
		dry = inputBool(v)
	}
	limit := int(inputInt(in["limit"]))
	if limit <= 0 {
		limit = backfillDefaultLimit
	}
	if limit > backfillMaxLimit {
		limit = backfillMaxLimit
	}

	report, err := engine.Backfill(ctxOf(c), dry, limit)
	if err != nil {
		d.failServer(c, "批量入组", err)
		return
	}
	d.success(c, presentBackfill(report))
}

// presentBackfill 把引擎结果摆成前端契约的 JSON（字段顺序固定）。
func presentBackfill(r BackfillReport) *jsObj {
	byNode := make(jsArr, 0, len(r.ByNode))
	for _, n := range r.ByNode {
		byNode = append(byNode, jo("node_id", n.NodeID, "count", n.Count))
	}
	skipped := make(jsArr, 0, len(r.Skipped))
	for _, s := range r.Skipped {
		skipped = append(skipped, jo(
			"region_id", s.RegionID,
			"region_name", s.RegionName,
			"reason", s.Reason,
			"count", s.Count,
		))
	}
	return jo(
		"dry", r.Dry,
		"candidates", r.Candidates,
		"assigned", r.Assigned,
		"users_done", r.UsersDone,
		"users_skipped", r.UsersSkipped,
		"remaining", r.Remaining,
		"by_node", byNode,
		"skipped", skipped,
	)
}
