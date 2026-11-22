package admin

// 管理端地区域（/admin-api/regions…）：地区 CRUD + 节点归属/容量 + 总览 + 容量告警。
//
//	GET    /admin-api/regions             地区列表（含 node_count/user_count/capacity_*）
//	GET    /admin-api/regions/overview    看板首屏：全局合计 + 各地区 + 未归属节点 + 告警计数
//	GET    /admin-api/regions/alerts       容量告警（>=85% warn / >=95% critical，实时算）
//	POST   /admin-api/regions/backfill    老用户批量入组 {dry?, limit?}（实现见 region_backfill.go）
//	POST   /admin-api/regions             建地区 {name, sort}
//	PUT    /admin-api/regions/:region     改名 / 改排序（只改传入的字段）
//	DELETE /admin-api/regions/:region     删除（有节点时拒绝并报节点数）
//	GET    /admin-api/regions/:region/nodes  该地区的节点（含 bound_users / agree_ratio）
//	PUT    /admin-api/nodes/:node/region     设置节点归属 {region_id}（null=移出地区）
//	PUT    /admin-api/nodes/:node/capacity   设置容量 {capacity}（null=不限）
//	GET    /admin-api/nodes/:node/capacity-recommend  容量建议（按 node_metrics 最近 7 天样本推算）
//
// 路由参数名必须叫 :node（不能叫 :id）：NodeDomain 已经注册了 /nodes/:node，
// gin 的路由树同位置只允许一个通配名，换个名字直接 panic（实测
// "conflicts with existing wildcard ':node'"）。
//
// 迁移 / 均衡 / 自动分配都在别的域，本文件不碰 user_nodes。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode/utf8"

	"controlhub-server/internal/httpx"
	"controlhub-server/internal/metrics"

	"github.com/gin-gonic/gin"
)

// 地区名字段与 regions.name varchar(64) 对齐。
const regionNameMaxRunes = 64

// RegionDeps 是地区域的依赖清单。
type RegionDeps struct {
	Store RegionStore
	// Metrics 是容量建议的数据源（node_metrics 采样 + Recommend 推算）。
	// 接口化：单测注入固定结果，不必连库造 7 天样本；为 nil 时端点回「未启用」，不 panic。
	Metrics capacityRecommender
	// Backfill 是老用户批量入组的引擎（POST /regions/backfill）。
	// 为 nil 时该端点回 503「引擎未就绪」，其余地区接口不受影响。
	Backfill BackfillEngine
	Logger   loggerLike
}

// RegionDomain 是管理端地区域的 HTTP 入口。
type RegionDomain struct {
	store    RegionStore
	metrics  capacityRecommender
	backfill BackfillEngine
	logger   loggerLike
}

// NewRegionDomain 组装地区域。
func NewRegionDomain(d RegionDeps) *RegionDomain {
	if d.Store == nil {
		panic("admin: RegionStore 不能为空")
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &RegionDomain{store: d.Store, metrics: d.Metrics, backfill: d.Backfill, logger: logger}
}

// Register 把地区域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
func (d *RegionDomain) Register(r gin.IRouter) {
	// 静态段必须排在 /regions/:region 之前（overview / alerts / backfill 不能被当成地区 id）。
	r.GET("/regions/overview", d.regionOverview)
	r.GET("/regions/alerts", d.regionAlerts)
	r.POST("/regions/backfill", d.regionBackfill)
	r.GET("/regions", d.regionIndex)
	r.POST("/regions", d.regionStore)
	r.PUT("/regions/:region", d.regionUpdate)
	r.DELETE("/regions/:region", d.regionDestroy)
	r.GET("/regions/:region/nodes", d.regionNodes)

	// 节点归属 / 容量：挂在现有 /nodes/:node 之下，只增两条子路径。
	r.PUT("/nodes/:node/region", d.nodeRegion)
	r.PUT("/nodes/:node/capacity", d.nodeCapacity)
	// 容量建议：只读，与上面的 PUT 同前缀不同后缀（静态段在 gin 里不会和 :node 冲突）。
	r.GET("/nodes/:node/capacity-recommend", d.nodeCapacityRecommend)
}

// ---- 响应封装（PHP ApiResponse 口径，与返佣/余额域一致）----

func (d *RegionDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *RegionDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *RegionDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// notFound 与其它域同文案（Laravel 隐式绑定失败 / 路由不存在都回这个）。
func (d *RegionDomain) notFound(c *gin.Context) { d.fail(c, "接口不存在", 404) }

// noRowsToNotFound 把「查无此行」映射成 404；其余错误返回 false。
func (d *RegionDomain) noRowsToNotFound(c *gin.Context, err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		d.notFound(c)
		return true
	}
	return false
}

// ---- 地区列表 / 总览 ----

// regionIndex 对齐 GET /admin-api/regions：全部地区 + 统计，一次请求给全。
func (d *RegionDomain) regionIndex(c *gin.Context) {
	rows, err := d.store.RegionList(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询地区列表", err)
		return
	}
	d.success(c, jo("regions", presentRegionSummaries(rows)))
}

// regionOverview 对齐 GET /admin-api/regions/overview：调度看板首屏。
//
// 除全局合计外还带 unassigned_nodes：前端「未归属节点（全量下发）」那一块
// 直接用这份数据，不必再拉一遍 /nodes 自己过滤（现有节点列表不含 region_id）。
// counts 是容量告警条数（warn/critical），首屏红点/角标用它，
// 明细仍走 /regions/alerts —— 首屏不该为了两个数字多传一整份告警列表。
func (d *RegionDomain) regionOverview(c *gin.Context) {
	regions, err := d.store.RegionList(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询地区总览", err)
		return
	}
	alerts, err := d.alertsOf(c)
	if err != nil {
		d.failServer(c, "计算容量告警", err)
		return
	}
	nodeTotal, capacityTotal, capacityUsed, boundUsers, unlimitedNodes, err := d.store.NodeTotals(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询节点合计", err)
		return
	}
	unassigned, err := d.store.UnassignedNodes(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询未归属节点", err)
		return
	}
	items := make(jsArr, 0, len(unassigned))
	for i := range unassigned {
		items = append(items, presentRegionNode(&unassigned[i]))
	}
	d.success(c, jo(
		"node_total", nodeTotal,
		"capacity_total", capacityTotal,
		"capacity_used", capacityUsed,
		"bound_users", boundUsers,
		"unlimited_nodes", unlimitedNodes,
		"regions", presentRegionSummaries(regions),
		"unassigned_nodes", items,
		"counts", presentAlertCounts(metrics.CountAlerts(alerts)),
	))
}

// ---- 容量告警 ----

// alertSource 是 store 上的可选能力：算得出当前容量告警。
//
// 故意不进 RegionStore 接口：告警不是地区 CRUD 的一部分，只有真正连库的实现才算得出来。
// fake store 不实现它时，handler 把告警当空、counts 全 0，总览照常返回 ——
// 「算不出告警」不该让整个看板 500。
type alertSource interface {
	Alerts(ctx context.Context) ([]metrics.Alert, error)
}

// alertsOf 取当前告警；store 没这能力时返回空（不是错误）。
func (d *RegionDomain) alertsOf(c *gin.Context) ([]metrics.Alert, error) {
	src, ok := d.store.(alertSource)
	if !ok {
		return nil, nil
	}
	return src.Alerts(ctxOf(c))
}

// regionAlerts 对齐 GET /admin-api/regions/alerts：容量告警明细。
//
// 实时算、不落库（见 internal/metrics/alert.go）：节点与地区各算一遍，
// critical 在前、同级按使用率降序。counts 与总览里的 counts 是同一份算法。
func (d *RegionDomain) regionAlerts(c *gin.Context) {
	alerts, err := d.alertsOf(c)
	if err != nil {
		d.failServer(c, "计算容量告警", err)
		return
	}
	items := make(jsArr, 0, len(alerts))
	for _, a := range alerts {
		items = append(items, presentAlert(a))
	}
	d.success(c, jo(
		"alerts", items,
		"counts", presentAlertCounts(metrics.CountAlerts(alerts)),
	))
}

// ---- 地区 CRUD ----

// regionStore 对齐 POST /admin-api/regions：{name, sort}。
func (d *RegionDomain) regionStore(c *gin.Context) {
	input := jsonInput(c)

	name, ok := regionNameInput(input["name"])
	if !ok {
		d.fail(c, "地区名称不能为空", 422)
		return
	}
	sort := int64(0)
	if v, exists := input["sort"]; exists && v != nil {
		sort = inputInt(v)
	}
	// 静态 IP 分组已随代理生产线下线移除。

	taken, err := d.store.RegionNameTaken(ctxOf(c), name, 0)
	if err != nil {
		d.failServer(c, "检查地区名称", err)
		return
	}
	if taken {
		d.fail(c, "地区名称已存在", 422)
		return
	}

	rec, err := d.store.RegionCreate(ctxOf(c), name, sort)
	if err != nil {
		if errors.Is(err, ErrRegionNameTaken) {
			// 并发下预检查没用，唯一键兜底（仍然是 422，不是 500）。
			d.fail(c, "地区名称已存在", 422)
			return
		}
		d.failServer(c, "创建地区", err)
		return
	}
	d.success(c, presentRegionSummary(&RegionSummary{RegionRecord: *rec}), "创建成功")
}

// regionUpdate 对齐 PUT /admin-api/regions/:region：只改传入的字段。
func (d *RegionDomain) regionUpdate(c *gin.Context) {
	id, ok := parseUint(c.Param("region"))
	if !ok {
		d.notFound(c)
		return
	}
	input := jsonInput(c)

	var name *string
	if v, exists := input["name"]; exists {
		trimmed, valid := regionNameInput(v)
		if !valid {
			d.fail(c, "地区名称不能为空", 422)
			return
		}
		name = &trimmed
	}
	var sort *int64
	if v, exists := input["sort"]; exists && v != nil {
		s := inputInt(v)
		sort = &s
	}

	// 静态 IP 分组已随代理生产线下线移除。

	if name != nil {
		taken, err := d.store.RegionNameTaken(ctxOf(c), *name, id)
		if err != nil {
			d.failServer(c, "检查地区名称", err)
			return
		}
		if taken {
			d.fail(c, "地区名称已存在", 422)
			return
		}
	}

	rec, err := d.store.RegionUpdate(ctxOf(c), id, name, sort)
	if err != nil {
		if errors.Is(err, ErrRegionNameTaken) {
			d.fail(c, "地区名称已存在", 422)
			return
		}
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "修改地区", err)
		return
	}
	d.success(c, presentRegionRecord(rec), "保存成功")
}

// regionDestroy 对齐 DELETE /admin-api/regions/:region。
//
// 有节点属于它时**拒绝删除**并报节点数：节点归属是运营手工配的，
// 直接删地区会让那些节点的 region_id 悬空（订阅侧按地区过滤时行为不明）。
func (d *RegionDomain) regionDestroy(c *gin.Context) {
	id, ok := parseUint(c.Param("region"))
	if !ok {
		d.notFound(c)
		return
	}
	nodes, err := d.store.RegionNodeCount(ctxOf(c), id)
	if err != nil {
		d.failServer(c, "统计地区节点", err)
		return
	}
	if nodes > 0 {
		d.fail(c, fmt.Sprintf("该地区还有 %d 台节点，请先移出", nodes), 422)
		return
	}
	if err := d.store.RegionDelete(ctxOf(c), id); err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "删除地区", err)
		return
	}
	d.success(c, nil, "删除成功")
}

// regionNodes 对齐 GET /admin-api/regions/:region/nodes。
func (d *RegionDomain) regionNodes(c *gin.Context) {
	id, ok := parseUint(c.Param("region"))
	if !ok {
		d.notFound(c)
		return
	}
	region, err := d.store.RegionByID(ctxOf(c), id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "查询地区", err)
		return
	}
	rows, err := d.store.RegionNodes(ctxOf(c), id)
	if err != nil {
		d.failServer(c, "查询地区节点", err)
		return
	}
	items := make(jsArr, 0, len(rows))
	for i := range rows {
		items = append(items, presentRegionNode(&rows[i]))
	}
	d.success(c, jo(
		"region_id", region.ID,
		"region_name", region.Name,
		"nodes", items,
	))
}

// ---- 节点归属 / 容量 ----

// nodeRegion 对齐 PUT /admin-api/nodes/:node/region：{region_id}，null = 移出地区。
func (d *RegionDomain) nodeRegion(c *gin.Context) {
	nodeID, ok := parseUint(c.Param("node"))
	if !ok {
		d.notFound(c)
		return
	}
	input := jsonInput(c)

	var regionID *uint64
	if v, exists := input["region_id"]; exists && v != nil {
		n, valid := regionIntInput(v)
		if !valid || n < 0 {
			d.fail(c, "地区 id 不合法", 422)
			return
		}
		if n > 0 {
			exists, err := d.store.RegionExists(ctxOf(c), uint64(n))
			if err != nil {
				d.failServer(c, "查询地区", err)
				return
			}
			if !exists {
				d.fail(c, "地区不存在", 422)
				return
			}
			v := uint64(n)
			regionID = &v
		}
	}

	if err := d.store.NodeSetRegion(ctxOf(c), nodeID, regionID); err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "设置节点地区", err)
		return
	}
	out := any(nil)
	if regionID != nil {
		out = *regionID
	}
	d.success(c, jo("id", nodeID, "region_id", out), "保存成功")
}

// nodeCapacity 对齐 PUT /admin-api/nodes/:node/capacity：{capacity}，null = 不限。
func (d *RegionDomain) nodeCapacity(c *gin.Context) {
	nodeID, ok := parseUint(c.Param("node"))
	if !ok {
		d.notFound(c)
		return
	}
	input := jsonInput(c)

	var capacity *int64
	if v, exists := input["capacity"]; exists && v != nil {
		n, valid := regionIntInput(v)
		if !valid {
			d.fail(c, "容量必须是整数", 422)
			return
		}
		if n < 0 {
			d.fail(c, "容量不能为负数", 422)
			return
		}
		capacity = &n
	}

	if err := d.store.NodeSetCapacity(ctxOf(c), nodeID, capacity); err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "设置节点容量", err)
		return
	}
	d.success(c, jo("id", nodeID, "capacity", nullableInt(capacity)), "保存成功")
}

// capacityRecommender 是容量建议需要的最小能力（*metrics.Store 直接满足）。
//
// 独立成接口：单测注入固定推荐结果，不必连库造 7 天样本。与 alertSource 一样
// 「能力缺失不影响其余接口」—— 字段为 nil 时端点回「未启用」，不是 500。
type capacityRecommender interface {
	Recommend(ctx context.Context, nodeID int64) (metrics.Recommendation, error)
}

// nodeCapacityRecommend 对齐 GET /admin-api/nodes/:node/capacity-recommend。
//
// 数据来自 node_metrics 最近 7 天的采样（internal/metrics.Recommend 推算），
// 只读、不改任何配置：填不填看管理员自己决定（前端「一键填入」只是把数字
// 写进容量输入框，仍需再点保存才会落库）。
//
// 三种情况：
//   - 节点不存在 → 404（与 PUT /nodes/:node/capacity 同口径）。必须先查节点：
//     Recommend 对不存在的 id 只会回「最近 7 天没有采样数据」，前端会把打错的
//     id 当成「节点还没上报」；
//   - 推算不出数（Suggest = 0：样本不足或节点没上报内存）→ suggest 回 null，
//     不下发建议值，reason 说明原因；
//   - 正常 → suggest + confidence + basis + per_user_kb + samples + reason。
func (d *RegionDomain) nodeCapacityRecommend(c *gin.Context) {
	nodeID, ok := parseUint(c.Param("node"))
	if !ok {
		d.notFound(c)
		return
	}
	if _, _, err := d.store.NodeBinding(ctxOf(c), nodeID); err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "查询节点", err)
		return
	}
	if d.metrics == nil {
		// 没接入指标存储（单测 / 裁剪部署）：明说「未启用」，别伪装成「样本不足」。
		d.success(c, presentCapacityRecommendation(metrics.Recommendation{
			NodeID: int64(nodeID),
			Reason: "未启用容量建议（没有接入节点指标存储）",
		}))
		return
	}
	rec, err := d.metrics.Recommend(ctxOf(c), int64(nodeID))
	if err != nil {
		d.failServer(c, "推算建议容量", err)
		return
	}
	d.success(c, presentCapacityRecommendation(rec))
}

// ---- 展示层 ----

// presentRegionSummaries 输出地区统计数组（字段顺序固定，前端契约）。
func presentRegionSummaries(rows []RegionSummary) jsArr {
	items := make(jsArr, 0, len(rows))
	for i := range rows {
		items = append(items, presentRegionSummary(&rows[i]))
	}
	return items
}

// presentRegionRecord 输出地区自身字段。
//
// 更新接口只回这三项：统计值（node_count 等）必须从列表/总览重新聚合，
// 不能在更新响应里凭空写 0（该地区可能还有节点）。
func presentRegionRecord(r *RegionRecord) *jsObj {
	return jo(
		"id", r.ID,
		"name", r.Name,
		"sort", r.Sort,
	)
}

// presentRegionSummary 输出地区统计一行。
func presentRegionSummary(r *RegionSummary) *jsObj {
	return jo(
		"id", r.ID,
		"name", r.Name,
		"sort", r.Sort,
		"node_count", r.NodeCount,
		"user_count", r.UserCount,
		"capacity_total", r.CapacityTotal,
		"capacity_used", r.CapacityUsed,
	)
}

// presentRegionNode 输出节点一行：bound_users 已绑人数，agree_ratio 是已绑/容量。
//
// agree_ratio 在 capacity 为 NULL（不限）或 <=0 时输出 null：
// 有限容量才有「水位」，前端据此显示进度条还是「不限」。
func presentRegionNode(n *RegionNode) *jsObj {
	inbounds := make(jsArr, 0, len(n.Inbounds))
	for i := range n.Inbounds {
		inbounds = append(inbounds, presentRegionInbound(&n.Inbounds[i]))
	}
	return jo(
		"id", n.ID,
		"name", n.Name,
		"host", n.Host,
		"driver_type", n.DriverType,
		"status", n.Status,
		"capacity", nullableInt(n.Capacity),
		"region_id", nullableUint(n.RegionID),
		"bound_users", n.BoundUsers,
		"agree_ratio", regionAgreeRatio(n.BoundUsers, n.Capacity),
		"inbounds", inbounds,
	)
}

// presentRegionInbound 输出入站归属一行（#入站级分组，只读展示）。
//
// region_id/region_name 是**有效归属**：入站自己归了地区就它，否则是节点地区；
// 都没有则为 null（前端显示「跟随节点」）。
func presentRegionInbound(in *RegionInbound) *jsObj {
	return jo(
		"id", in.ID,
		"tag", in.Tag,
		"protocol", in.Protocol,
		"port", in.Port,
		"enabled", in.Enabled,
		"region_id", nullableUint(in.RegionID),
		"region_name", nullableStr(in.RegionName),
	)
}

// presentAlert 输出一条容量告警（字段顺序固定，前端契约）。
//
// ratio 走 phpFloat：PHP 的 (float) 口径是 0.9 而不是 0.900000，
// 前端拿它算进度条/判色都直接可用。
func presentAlert(a metrics.Alert) *jsObj {
	return jo(
		"level", a.Level,
		"scope", a.Scope,
		"id", a.ID,
		"name", a.Name,
		"used", a.Used,
		"capacity", a.Capacity,
		"ratio", phpFloat(a.Ratio),
		"message", a.Message,
	)
}

// presentAlertCounts 输出告警计数 {warn, critical}：总览与告警列表共用。
func presentAlertCounts(c metrics.AlertCounts) *jsObj {
	return jo("warn", c.Warn, "critical", c.Critical)
}

// presentCapacityRecommendation 输出容量建议（字段顺序固定，前端契约）。
//
// suggest 在 Recommend 给不出数（Suggest = 0）时回 null，不是 0：容量列里 0 与
// NULL 对管理员来说是「不限」的意思，下发 0 前端就会显示「建议 0 人」。
// per_user_kb 走 phpFloat（0 / 5 / 12.5 这种短表示，不带 12.500000）。
func presentCapacityRecommendation(r metrics.Recommendation) *jsObj {
	var suggest any
	if r.Suggest > 0 {
		suggest = r.Suggest
	}
	return jo(
		"suggest", suggest,
		"confidence", r.Confidence,
		"basis", r.Basis,
		"per_user_kb", phpFloat(r.PerUserKB),
		"samples", r.Samples,
		"reason", r.Reason,
	)
}

// regionAgreeRatio = bound_users / capacity；容量无效时 null。
func regionAgreeRatio(bound int64, capacity *int64) any {
	if capacity == nil || *capacity <= 0 {
		return nil
	}
	return phpFloat(float64(bound) / float64(*capacity))
}

// ---- 输入整理 ----

// regionNameInput 取地区名并校验（去空白后非空、不超长）。
// 返回 (name, false) 表示校验失败。
func regionNameInput(v any) (string, bool) {
	raw, _ := inputString(v)
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > regionNameMaxRunes {
		return "", false
	}
	return name, true
}

// regionIntInput 严格解析整数输入：非整数（"1.5" / "abc"）返回 false。
//
// 不复用 inputInt：它把非法值悄悄当 0，capacity/region_id 这类字段
// 静默变 0 等于「不限容量 / 移出地区」，属于会改数据的误判。
func regionIntInput(v any) (int64, bool) {
	switch t := v.(type) {
	case jsonNumber:
		if !t.isIntLiteral() {
			return 0, false
		}
		n, err := strconv.ParseInt(string(t), 10, 64)
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	case int:
		return int64(t), true
	case int64:
		return t, true
	case uint64:
		return int64(t), true
	case float64:
		if t != float64(int64(t)) {
			return 0, false
		}
		return int64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
