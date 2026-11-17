package admin

// 管理端「迁移与调度」域（地区功能 #10）：故障迁移 / 批量均衡 / 迁移历史 / 撤销 / 后台任务进度。
//
// 挂载点（cmd/server/routes.go 的 adminGroup，即 /admin-api）：
//
//	POST /nodes/:node/migrate            故障迁移（后台任务，立即返回 job_id）
//	POST /regions/:region/balance        批量均衡（dry=true 同步出计划，dry=false 后台任务）
//	GET  /migrations                     迁移历史（node_id / user_id / reason / page / per_page）
//	POST /migrations/:batch_id/undo      撤销某批次（最近 24 小时内）
//	GET  /migrations/jobs/:job_id        后台任务进度
//
// 参数名用 :node / :region 而不是任务书里写的 :id：Gin 的同一层级只允许一个通配名，
// 节点域（/nodes/:node/...）与地区域（/regions/:region/...）已经占用了这两个名字，
// 写成 :id 会在注册路由时 panic（"conflicts with existing wildcard"）。URL 形状不变。
//
// 引擎依赖 internal/scheduler（region-08 创建，本任务进行中已合入）：handler 只依赖窄接口
// MigrationEngine，真实实现由 migration_engine.go 的 SchedulerEngine 适配；
// 测试用 fake 引擎（进度类用例需要可控的阻塞/放行）。Engine 传 nil 时两个写接口返回
// 明确文案「迁移引擎未就绪」，历史/进度查询照常可用。

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 历史列表的分页口径。
const (
	migrationDefaultPerPage = 20
	migrationMaxPerPage     = 200
	// migrationChunk 是后台任务每次驱动引擎搬运的用户数。
	// 不一次搬完是为了让 done 变成「看得见的进度」，并在中断时少丢已完成的批次
	// （引擎自己按 1000 条一批，这里再切一层是接口侧的进度粒度）。
	migrationChunk = 500
)

// MigrationMove 是一次「把某用户从 A 节点搬到 B 节点」的计划项。
//
// 与 scheduler.Move 字段一一对应；不复用对方的类型是因为它此刻还不存在，
// 而 admin 包不能为了将来的包先写一个编译不过的 import。
type MigrationMove struct {
	UserID     int64
	FromNodeID int64
	ToNodeID   int64
}

// MigrationEngine 是调度引擎的窄接口（internal/scheduler 的门面）。
//
// 签名对齐 region-08 任务书里的 Scheduler.Migrate / Scheduler.Balance；
// 真实接线时用一个匿名 adapter 适配（scheduler 的 Balance 返回 []scheduler.Move）。
type MigrationEngine interface {
	// Migrate 把 fromNodeID 上的一批用户迁到 toNodeID，count<=0 表示全部，返回实际迁移数。
	Migrate(ctx context.Context, fromNodeID, toNodeID int64, count int) (moved int, err error)
	// Balance 做地区内均衡；dry=true 只返回计划不写库，limit>0 时最多迁移 limit 个。
	Balance(ctx context.Context, regionID int64, dry bool, limit int) (moved int, plan []MigrationMove, err error)
}

// MigrationDeps 是迁移域的依赖清单（显式传参，避免测试构造半吊子实例）。
type MigrationDeps struct {
	DB *sql.DB
	// Engine 为空时写接口返回「引擎未就绪」（见 errNoEngine），只读接口不受影响。
	Engine MigrationEngine
	// Now 供测试注入固定时钟（任务的 started_at/finished_at 用它；
	// 撤销的 24 小时窗口由 MySQL 自己判定，见 migration_store.go 的 batchAge）。
	Now    func() time.Time
	Logger *log.Logger
}

// MigrationDomain 是迁移域的 HTTP 入口。
type MigrationDomain struct {
	db     *sql.DB
	engine MigrationEngine
	jobs   *migrationJobRegistry
	logger *log.Logger
}

// NewMigrationDomain 组装迁移域。
// 注意：db 允许为 nil（cmd/server 的 registerRoutes 单元测试会传 nil db 只验路由/IP 处理），
// 所以不在构造期 panic —— 真正的查询入口由 requireDB 兜底成 500，进程不会崩。
func NewMigrationDomain(d MigrationDeps) *MigrationDomain {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &MigrationDomain{
		db:     d.DB,
		engine: d.Engine,
		jobs:   newMigrationJobRegistry(now),
		logger: logger,
	}
}

// Register 把迁移域挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
//
// 路由顺序有讲究：/migrations/jobs/:job_id 是静态段 + 参数，/migrations/:batch_id/undo
// 是参数 + 静态段，两者层级不同不会互相吞；但 /migrations/jobs/... 仍写在前面，
// 与 /nodes/third-party 同理 —— 同级静态段优先声明，后面加 /migrations/:id 也不会踩到。
func (d *MigrationDomain) Register(r gin.IRouter) {
	r.GET("/migrations/jobs/:job_id", d.migrationJobShow)
	r.GET("/migrations", d.migrationIndex)
	r.POST("/migrations/:batch_id/undo", d.migrationUndo)

	// 通配名必须与已有域一致：节点域用 :node、地区域用 :region（见文件头注释）。
	r.POST("/nodes/:node/migrate", d.nodeMigrate)
	r.POST("/regions/:region/balance", d.regionBalance)
}

// ---- 响应封装（与 NodeDomain 同口径：HTTP 200 + PHP json_encode 字节）----

func (d *MigrationDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *MigrationDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// requireDB 在数据库未注入时统一回 500 并返回 false。
func (d *MigrationDomain) requireDB(c *gin.Context) bool {
	if d.db == nil {
		d.fail(c, "数据库未初始化", 500)
		return false
	}
	return true
}

func (d *MigrationDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	d.fail(c, "服务器错误", 500)
}

// ---- POST /nodes/:id/migrate ----

func (d *MigrationDomain) nodeMigrate(c *gin.Context) {
	if !d.requireDB(c) {
		return
	}
	fromID, ok := parseUint(c.Param("node"))
	if !ok {
		d.fail(c, "节点不存在", 404)
		return
	}
	if d.engine == nil {
		d.fail(c, errNoEngine, 503)
		return
	}

	in := jsonInput(c)
	toID := inputInt(in["to_node_id"])
	count := int(inputInt(in["count"]))
	if toID <= 0 {
		d.fail(c, "to_node_id 必须填写目标节点 id", 422)
		return
	}
	if toID == int64(fromID) {
		d.fail(c, "源节点与目标节点相同，无需迁移", 422)
		return
	}

	ctx := c.Request.Context()
	if exists, err := d.nodeExists(ctx, int64(fromID)); err != nil {
		d.failServer(c, "查询源节点", err)
		return
	} else if !exists {
		d.fail(c, "源节点不存在", 404)
		return
	}
	if exists, err := d.nodeExists(ctx, toID); err != nil {
		d.failServer(c, "查询目标节点", err)
		return
	} else if !exists {
		d.fail(c, "目标节点不存在", 404)
		return
	}

	job, err := d.newJob(migrationJobKindMigrate, count)
	if err != nil {
		d.failServer(c, "登记迁移任务", err)
		return
	}
	go d.runMigrateJob(job, int64(fromID), toID, count)

	d.success(c, jo(
		"job_id", job.id,
		"batch_id", job.batchID,
		"status", migrationJobRunning,
	))
}

// newJob 登记一个后台任务，并记下 user_node_migrations 的 id 水位线（收尾打批次号用）。
func (d *MigrationDomain) newJob(kind string, total int) (*migrationJob, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	afterID, err := d.maxMigrationID(ctx)
	if err != nil {
		return nil, err
	}
	if kind == migrationJobKindBalance && total < 0 {
		total = 0
	}
	return d.jobs.create(kind, total, afterID), nil
}

// ---- POST /regions/:id/balance ----

func (d *MigrationDomain) regionBalance(c *gin.Context) {
	if !d.requireDB(c) {
		return
	}
	regionID, ok := parseUint(c.Param("region"))
	if !ok {
		d.fail(c, "地区不存在", 404)
		return
	}
	if d.engine == nil {
		d.fail(c, errNoEngine, 503)
		return
	}

	in := jsonInput(c)
	dry := inputBool(in["dry"])
	limit := int(inputInt(in["limit"]))
	if limit < 0 {
		limit = 0
	}

	ctx := c.Request.Context()
	if exists, err := d.regionExists(ctx, int64(regionID)); err != nil {
		d.failServer(c, "查询地区", err)
		return
	} else if !exists {
		d.fail(c, "地区不存在", 404)
		return
	}

	if dry {
		// dry=true 同步执行：只出计划，不写库（引擎保证），管理员先看一眼再决定。
		moved, plan, err := d.engine.Balance(ctx, int64(regionID), true, limit)
		if err != nil {
			d.fail(c, "生成均衡计划失败: "+err.Error(), 500)
			return
		}
		d.success(c, jo(
			"dry", true,
			"moved", moved,
			"plan", migrationPlanJSON(plan),
		))
		return
	}

	job, err := d.newJob(migrationJobKindBalance, limit)
	if err != nil {
		d.failServer(c, "登记均衡任务", err)
		return
	}
	go d.runBalanceJob(job, int64(regionID), limit)

	d.success(c, jo(
		"job_id", job.id,
		"batch_id", job.batchID,
		"status", migrationJobRunning,
	))
}

// migrationPlanJSON 把计划项编码成 [{user_id,from_node_id,to_node_id}]。
func migrationPlanJSON(plan []MigrationMove) any {
	items := make(jsArr, 0, len(plan))
	for _, mv := range plan {
		items = append(items, jo(
			"user_id", mv.UserID,
			"from_node_id", mv.FromNodeID,
			"to_node_id", mv.ToNodeID,
		))
	}
	return items
}

// ---- GET /migrations ----

func (d *MigrationDomain) migrationIndex(c *gin.Context) {
	if !d.requireDB(c) {
		return
	}
	f := migrationFilter{
		nodeID:  queryInt(c, "node_id", 0),
		userID:  queryInt(c, "user_id", 0),
		reason:  strings.TrimSpace(c.Query("reason")),
		page:    int(queryInt(c, "page", 1)),
		perPage: int(queryInt(c, "per_page", migrationDefaultPerPage)),
	}
	if f.page < 1 {
		f.page = 1
	}
	if f.perPage <= 0 {
		f.perPage = migrationDefaultPerPage
	}
	if f.perPage > migrationMaxPerPage {
		f.perPage = migrationMaxPerPage
	}

	items, total, err := d.listMigrations(c.Request.Context(), f)
	if err != nil {
		d.failServer(c, "查询迁移历史", err)
		return
	}

	// 越界夹回最后一页（与 PHP paginate 一致，不返回空页）。
	lastPage := (total + f.perPage - 1) / f.perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if f.page > lastPage {
		f.page = lastPage
		items, total, err = d.listMigrations(c.Request.Context(), f)
		if err != nil {
			d.failServer(c, "查询迁移历史", err)
			return
		}
	}

	out := make(jsArr, 0, len(items))
	for _, rec := range items {
		out = append(out, jo(
			"id", rec.ID,
			"user_id", rec.UserID,
			"from_node_id", nullableI64(rec.FromNodeID),
			"to_node_id", rec.ToNodeID,
			"reason", rec.Reason,
			"operator", nullableStr(rec.Operator),
			"batch_id", nullableStr(rec.BatchID),
			"created_at", nullableDateTime(rec.CreatedAt),
		))
	}
	d.success(c, jo(
		"items", out,
		"total", total,
		"page", f.page,
		"per_page", f.perPage,
	))
}

// nullableI64 输出可空整数（PHP 里 null 原样输出）。
func nullableI64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// ---- POST /migrations/:batch_id/undo ----

func (d *MigrationDomain) migrationUndo(c *gin.Context) {
	if !d.requireDB(c) {
		return
	}
	batchID := strings.TrimSpace(c.Param("batch_id"))
	if batchID == "" || len(batchID) > 64 {
		d.fail(c, "批次号不合法", 422)
		return
	}

	res, err := d.undoBatch(c.Request.Context(), batchID)
	if err != nil {
		var rej migrationReject
		if errors.As(err, &rej) {
			d.fail(c, rej.msg, 422)
			return
		}
		d.failServer(c, "撤销迁移批次", err)
		return
	}
	d.success(c, jo(
		"batch_id", res.BatchID,
		"undone", res.Undone,
		"rebound", res.Rebound,
		"unbound", res.Unbound,
	))
}

// ---- GET /migrations/jobs/:job_id ----

func (d *MigrationDomain) migrationJobShow(c *gin.Context) {
	id := strings.TrimSpace(c.Param("job_id"))
	job, ok := d.jobs.get(id)
	if !ok {
		d.fail(c, "任务不存在或进度已过期（后台任务进度只在内存保留 1 小时）", 404)
		return
	}
	s := job.snapshot()
	d.success(c, jo(
		"job_id", s.ID,
		"kind", s.Kind,
		"batch_id", s.BatchID,
		"status", s.Status,
		"total", s.Total,
		"done", s.Done,
		"failed", s.Failed,
		"started_at", dateTimeStr(s.StartedAt),
		"finished_at", nullableDateTime(s.FinishedAt),
		"error", nullableStr(strPtrIfNotEmpty(s.Error)),
	))
}

// strPtrIfNotEmpty 把空串当成 null 输出（前端「error」列只在有值时展示）。
func strPtrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
