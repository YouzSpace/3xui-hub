package collector

// 节点指标采样（#地区功能-14）：每个启用节点采一条快照写进 node_metrics，
// 供 internal/metrics 的容量推荐（Recommend）用真实历史样本拟合。
//
// 为什么放在 collector 而不是 cmd/server：这里已经有「读启用节点（含解密后的
// 凭据）/ 区分 3x-ui 与原生 xray / 统一日志」的全套私有能力（nodes.go 的
// enabledNodes / threeXuiClient / nodeRow.isXray），在 main 里重写一遍等于把
// nodes 表的列清单和解密入口复制成第二份 —— 以后加列就得改两处。
//
// 本文件只做「取数 + 组装 Snapshot」，写库交给调用方注入的 MetricsRecorder
// （生产是 *metrics.Store），所以 collector 不依赖调度/分配逻辑，也不会把
// 采样的失败传导成业务失败：单节点失败只记日志、计入 Failed，继续下一个。

import (
	"context"
	"math"
	"time"

	"controlhub-server/internal/metrics"
)

// MetricsRecorder 是采样结果的落地口（生产实现：internal/metrics.Store）。
type MetricsRecorder interface {
	Record(ctx context.Context, snap metrics.Snapshot) error
}

// MetricsSampleResult 是一轮采样的结果统计（给调度循环打日志用）。
type MetricsSampleResult struct {
	Sampled int // 成功落库的节点数
	Failed  int // 取数或落库失败的节点数
}

// metricsMemNoAbsolute 是「只有百分比、拿不到绝对值」时写进 mem_total 的标记值。
//
// 两个驱动都只给**内存百分比**：3x-ui 的 /server/status 里 mem.current/mem.total 在
// panel.Health 里已经归一化成百分比（见 threexui 的 memPercent），原生 xray 节点由
// agent 心跳上报百分比（nodes.mem）。真实字节数在现有驱动契约里拿不到。
//
// node_metrics 的 mem_used/mem_total 是「MB」语义，所以这里不能把百分比当 MB 塞进去
// （旧的「按总量 100 归一化」写法会让 Recommend 报出 0.17 KB/用户 这种假数）：
// mem_total 写 0 明确标记「无绝对值」，mem_used 写实测百分比（1-100），
// internal/metrics.Recommend 见到 mem_total <= 100 就走百分比口径（BasisPercent）。
// 等驱动把原始字节数暴露出来后，这里改成真实 MB 即可（表结构不用动）。
const metricsMemNoAbsolute = 0

// clampPercent 把百分比收敛到 0-100：上游偶发给 0.56（比例而非百分比）或 >100 的脏值时，
// 写进去的样本仍是「百分比」口径，不会把 Recommend 的斜率带偏。
func clampPercent(p float64) int {
	switch {
	case p <= 0 || math.IsNaN(p):
		return 0
	case p >= 100:
		return 100
	default:
		return int(math.Round(p))
	}
}

// metricsNodeTimeout 是单节点的取数上限：一台失联节点不该把一轮采样拖到超时，
// 也不该让后面的节点排队等待。
const metricsNodeTimeout = 30 * time.Second

// SampleMetrics 对每个 enabled 节点采一次快照并写入 rec。
//
// 数据来源（任务书 #地区功能-14 定的口径）：
//   - 3x-ui 节点：driver.HealthCheck（CPU/Mem 百分比）+ GetOnlineClients（在线连接数）；
//   - 原生 xray 节点：nodes.cpu / nodes.mem（agent 心跳维护的百分比），在线数未知记 0；
//   - 两者共同：users = user_nodes 里绑定到该节点的行数。
//
// 失败语义：任何单节点的失败（凭据解密、健康检查、在线列表、写库）只记日志并计入
// Failed，绝不返回错误、绝不 panic —— 调用方是常驻定时循环，一次失败不能让它退出。
func (c *Collector) SampleMetrics(ctx context.Context, rec MetricsRecorder) MetricsSampleResult {
	var res MetricsSampleResult
	if rec == nil {
		return res
	}

	nodes, err := c.enabledNodes(ctx)
	if err != nil {
		c.logf_("[metrics] 读取启用节点失败: %v", err)
		return res
	}
	if len(nodes) == 0 {
		return res
	}

	users, err := c.nodeUserCounts(ctx)
	if err != nil {
		// 拿不到绑定数不影响「内存/CPU 趋势」这个主信号，按 0 记，只提示一句。
		c.logf_("[metrics] 统计节点绑定用户数失败（本次按 0 记）: %v", err)
		users = map[int64]int{}
	}

	for _, n := range nodes {
		snap, err := c.sampleNode(ctx, n)
		if err != nil {
			c.logf_("[metrics] 节点 #%d(%s) 采样失败: %v", n.ID, n.Name, err)
			res.Failed++
			continue
		}
		snap.NodeID = n.ID
		snap.Users = users[n.ID]
		if err := rec.Record(ctx, snap); err != nil {
			c.logf_("[metrics] 节点 #%d(%s) 指标落库失败: %v", n.ID, n.Name, err)
			res.Failed++
			continue
		}
		res.Sampled++
	}
	return res
}

// nodeUserCounts 统计每个节点绑定的用户数（user_nodes 行数，与 scheduler 的负载口径一致）。
func (c *Collector) nodeUserCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT node_id, COUNT(*) FROM user_nodes GROUP BY node_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]int, 16)
	for rows.Next() {
		var (
			nodeID int64
			count  int
		)
		if err := rows.Scan(&nodeID, &count); err != nil {
			return nil, err
		}
		out[nodeID] = count
	}
	return out, rows.Err()
}

// sampleNode 取单个节点的 CPU / 内存 / 在线连接数。
func (c *Collector) sampleNode(ctx context.Context, n nodeRow) (metrics.Snapshot, error) {
	var snap metrics.Snapshot

	if n.isXray() {
		// 原生 xray：cpu/mem 由 agent 心跳维护（列可能为 NULL = 从未上报），mem 是百分比。
		if n.CPU.Valid {
			snap.CPU = n.CPU.Float64
		}
		if n.Mem.Valid {
			snap.MemUsed = clampPercent(n.Mem.Float64) // 实测百分比
			snap.MemTotal = metricsMemNoAbsolute       // 无绝对值
		}
		return snap, nil
	}

	client, err := c.threeXUiClient(n)
	if err != nil {
		return snap, err
	}

	nodeCtx, cancel := context.WithTimeout(ctx, metricsNodeTimeout)
	defer cancel()

	health, err := client.HealthCheck(nodeCtx)
	if err != nil {
		return snap, err
	}
	if health.Mem != nil {
		snap.MemUsed = clampPercent(*health.Mem) // 实测百分比
		snap.MemTotal = metricsMemNoAbsolute    // 无绝对值
	}
	if cpu := numericOrNil(health.CPU); cpu != nil {
		snap.CPU = *cpu
	}

	online, err := client.GetOnlineClients(nodeCtx)
	if err != nil {
		// 在线数拿不到不算整条失败：CPU/内存已经采到了，记录 0 并提示一句。
		c.logf_("[metrics] 节点 #%d(%s) 在线客户端读取失败（本次 conns=0）: %v", n.ID, n.Name, err)
	} else {
		snap.Conns = len(online)
	}
	return snap, nil
}
