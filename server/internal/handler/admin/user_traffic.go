package admin

// 用户×机器 流量只读接口（/admin-api/traffic/…）。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/UserTrafficController.php 与
// backend/app/Services/UserTrafficService.php。
//
// 窗口 24h|7d|30d，非法值回退 24h（同 MonitorController 的口径）。
// 数据源是同步事务内累加的 user_traffic_hourly / user_traffic_daily，本组不做任何写入。
// 没有数据的用户/节点不出现在结果里（缺数据 ≠ 0，同 MonitorService 的既有原则）。

import (
	"context"
	"sort"

	"github.com/gin-gonic/gin"
)

// windowLimit 对齐 UserTrafficService::limit：钳到 [1, 500]，空 → 50。
func windowLimit(n int64) int64 {
	if n == 0 {
		return 50
	}
	if n < 1 {
		return 1
	}
	if n > 500 {
		return 500
	}
	return n
}

// trafficWindowParam 取 ?window=（非法值由 ResolveWindow 回退 24h）。
func trafficWindowParam(c interface{ Query(string) string }) string {
	return c.Query("window")
}

// trafficUserRanking 对齐 UserTrafficController::userRanking。
func (d *UserDomain) trafficUserRanking(c *gin.Context) {
	w := d.traffic.ResolveWindow(trafficWindowParam(c), d.now())
	limit := windowLimit(queryInt(c, "limit", 50))

	rows, err := d.traffic.UserRanking(ctxOf(c), w, limit)
	if err != nil {
		d.failServer(c, "查询用户流量排行", err)
		return
	}
	dataStart, err := d.traffic.DataStart(ctxOf(c), w, d.now())
	if err != nil {
		d.failServer(c, "查询流量数据起始", err)
		return
	}
	// 占比分母：该窗口内**全部用户**的加权合计（不受 limit 影响，前端用它算「占全体」百分比）。
	totalWeighted, err := d.traffic.UserWeightedTotal(ctxOf(c), w)
	if err != nil {
		d.failServer(c, "查询用户流量合计", err)
		return
	}

	out := make(jsArr, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, jo(
			"user_id", r.UserID,
			"email", nullableStr(r.EmailPtr),
			"plan_type", nullableStr(r.PlanType),
			"plan_name", nullableStr(r.PlanName),
			"plan_months", nullableInt(r.PlanMonths),
			"weighted", r.Weighted,
			"up", r.Up,
			"down", r.Down,
		))
	}
	d.success(c, trafficEnvelope(w, dataStart, out, totalWeighted))
}

// trafficEnvelope 组装 {window, start, end, data_start, total_weighted, rows}。
//
// total_weighted 只对用户榜有意义（占比分母 = 全部用户加权合计）；机器榜传 0 占位。
func trafficEnvelope(w TrafficWindow, dataStart *string, rows jsArr, totalWeighted int64) *jsObj {
	ds := any(nil)
	if dataStart != nil {
		ds = *dataStart
	}
	return jo(
		"window", w.Key,
		"start", w.Start,
		"end", w.End,
		"data_start", ds,
		"total_weighted", totalWeighted,
		"rows", rows,
	)
}

// trafficNodeRanking 对齐 UserTrafficService::nodeRanking：
// 用户腿（原始/加权）与网卡口径并排，网卡合计降序排序。
func (d *UserDomain) trafficNodeRanking(c *gin.Context) {
	w := d.traffic.ResolveWindow(trafficWindowParam(c), d.now())
	rows, err := d.traffic.NodeRanking(ctxOf(c), w)
	if err != nil {
		d.failServer(c, "查询机器流量排行", err)
		return
	}
	dataStart, err := d.traffic.DataStart(ctxOf(c), w, d.now())
	if err != nil {
		d.failServer(c, "查询流量数据起始", err)
		return
	}
	if err := d.attachMachineColumns(ctxOf(c), &rows, w); err != nil {
		d.failServer(c, "查询网卡口径", err)
		return
	}

	// 网卡合计降序（null 垫底）→ 用户原始降序（null 垫底）→ 名称，保证稳定
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if keep := cmpNilLastDesc(a.NicTotal, b.NicTotal); keep != 0 {
			return keep > 0
		}
		if keep := cmpNilLastDesc(a.UserRaw, b.UserRaw); keep != 0 {
			return keep > 0
		}
		return a.Name < b.Name
	})

	out := make(jsArr, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, jo(
			"node_id", r.NodeID,
			"name", r.Name,
			"weighted", nullableInt(r.WeightedPtr),
			"up", nullableInt(r.UpPtr),
			"down", nullableInt(r.DownPtr),
			"user_raw", nullableInt(r.UserRaw),
			"multiplier", multiplierOut(r.Multiplier),
			"nic_up", nullableInt(r.NicUp),
			"nic_down", nullableInt(r.NicDown),
			"nic_total", nullableInt(r.NicTotal),
		))
	}
	d.success(c, trafficEnvelope(w, dataStart, out, 0))
}

// cmpNilLastDesc 比较两个可空数值：双 nil → 0；nil 垫底（返回 -1/1）；否则降序。
// 返回 1 表示 a 应排在 b 前。
func cmpNilLastDesc(a, b *int64) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	if *a == *b {
		return 0
	}
	if *a > *b {
		return 1
	}
	return -1
}

// trafficUserNodes 对齐 UserTrafficService::userNodeBreakdown：
// 单用户按机器明细 + 倍率 + 占本用户窗口内加权合计的百分比。
func (d *UserDomain) trafficUserNodes(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	w := d.traffic.ResolveWindow(trafficWindowParam(c), d.now())
	rows, err := d.traffic.UserNodeBreakdown(ctxOf(c), w, u.ID)
	if err != nil {
		d.failServer(c, "查询用户机器流量", err)
		return
	}
	dataStart, err := d.traffic.DataStart(ctxOf(c), w, d.now())
	if err != nil {
		d.failServer(c, "查询流量数据起始", err)
		return
	}

	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].NodeID)
	}
	nodes, err := d.traffic.NodesByID(ctxOf(c), ids)
	if err != nil {
		d.failServer(c, "查询节点倍率", err)
		return
	}
	defMult, err := d.traffic.DefaultNodeMultiplier(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询默认倍率", err)
		return
	}

	total := int64(0)
	for i := range rows {
		total += rows[i].Weighted
	}
	out := make(jsArr, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		meta := nodes[r.NodeID]
		mult := MultiplierFor(meta, defMult)
		// 合计为 0 → 占比无意义，给 null 而不是 0%
		var share any
		if total > 0 {
			share = phpFloat(roundShare(float64(r.Weighted) / float64(total) * 100))
		}
		out = append(out, jo(
			"node_id", r.NodeID,
			"name", r.Name,
			"weighted", r.Weighted,
			"up", r.Up,
			"down", r.Down,
			"multiplier", phpFloat(mult),
			"share", share,
		))
	}
	d.success(c, trafficEnvelope(w, dataStart, out, 0))
}

// attachMachineColumns 给 node-ranking 补网卡口径三列 + 倍率，并按并集扩展节点。
//
// 只有系统流量、没有用户代理流量的机器：用户腿三列是 null（不是 0）。
func (d *UserDomain) attachMachineColumns(ctx context.Context, rows *[]UserTrafficRow, w TrafficWindow) error {
	nic, err := d.traffic.NicSummary(ctx, w)
	if err != nil {
		return err
	}

	// 并集：用户腿里出现过的节点 + 网卡口径里的全部启用节点（保持用户腿原顺序，网卡追加）。
	out := make([]UserTrafficRow, 0, len(*rows)+len(nic))
	seen := map[uint64]bool{}
	for _, r := range *rows {
		out = append(out, r)
		seen[r.NodeID] = true
	}
	ids := make([]uint64, 0, len(out)+len(nic))
	for i := range out {
		ids = append(ids, out[i].NodeID)
	}
	for id := range nic {
		if !seen[id] {
			out = append(out, UserTrafficRow{NodeID: id})
			seen[id] = true
			ids = append(ids, id)
		}
	}

	nodes, err := d.traffic.NodesByID(ctx, ids)
	if err != nil {
		return err
	}
	defMult, err := d.traffic.DefaultNodeMultiplier(ctx)
	if err != nil {
		return err
	}

	for i := range out {
		r := &out[i]
		meta, hasNode := nodes[r.NodeID]
		item, hasNic := nic[r.NodeID]

		if r.Name == "" {
			if hasNode && meta.Name != "" {
				r.Name = meta.Name
			} else {
				r.Name = "#" + itoaUint(r.NodeID)
			}
		}

		// 用户腿原始字节（上下行合计，未乘倍率）：有腿才算，没腿是 null。
		if r.UserRaw == nil && r.WeightedPtr != nil {
			raw := derefInt64(r.UpPtr) + derefInt64(r.DownPtr)
			r.UserRaw = &raw
		}

		if hasNode {
			m := MultiplierFor(meta, defMult)
			r.Multiplier = &m
		} else {
			r.Multiplier = nil
		}
		if hasNic {
			up, down, total := item.Up, item.Down, item.Total
			r.NicUp, r.NicDown, r.NicTotal = &up, &down, &total
		} else {
			r.NicUp, r.NicDown, r.NicTotal = nil, nil, nil
		}
	}
	*rows = out
	return nil
}

// multiplierOut 输出 PHP float 或 null。
func multiplierOut(v *float64) any {
	if v == nil {
		return nil
	}
	return phpFloat(*v)
}

// roundShare 对齐 round($x, 1)（四舍五入远离零）。
func roundShare(f float64) float64 {
	neg := f < 0
	if neg {
		f = -f
	}
	v := float64(int64(f*10+0.5)) / 10
	if neg {
		v = -v
	}
	return v
}
