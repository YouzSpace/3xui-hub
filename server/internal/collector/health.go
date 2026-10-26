package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"controlhub-server/internal/tgbot"
)

// 健康检查 + 机器流量采样：逐行对齐 HealthCheckService（M9）与 MonitorService::record()。
//
// 语义：
//   - maintenance 节点跳过且**不写库**（管理员手工设的状态不被覆盖）；
//   - 探测异常 → offline / latency=0；
//   - 3x-ui 节点顺手把这次 /server/status 的 cpu/mem 落到 nodes，并更新 last_check_at=now；
//   - xray 节点的 cpu/mem 由 agent 心跳维护，健康检查只翻 status、**绝不覆盖 last_check_at**
//     （心跳新鲜度判定据此，覆盖了就会失真）；
//   - 每轮对失败的节点补测一次（panel.healthcheck_retry，默认 1）；
//   - 机器流量采样「没有数据就是 null，不能兜底成 0」，任何异常只 log、不打断健康检查。

// HealthResult 是单节点的健康检查结果（对齐 check() 的返回数组）。
type HealthResult struct {
	OK      bool
	Status  string // online | offline | maintenance
	Latency int64
	Skipped bool
	Error   string
}

// probeResult 是一次探测的原始产物（含只给监控采样用的机器累计流量）。
type probeResult struct {
	Online  bool
	Latency int64
	CPU     *float64
	Mem     *float64
	// NetSent / NetRecv 是 3x-ui /server/status 的机器网卡累计收发（缺字段为 nil）。
	NetSent *int64
	NetRecv *int64
	// Err 是探测层失败（网络/鉴权）；面板自己回 ok=false 时为空。
	Err error
}

// healthProbe 是「探测一个节点」的抽象（测试可注入假探测）。
type healthProbe func(ctx context.Context) probeResult

// CheckNode 复刻 HealthCheckService::check()：探测单节点并落库。
func (c *Collector) CheckNode(ctx context.Context, nodeID int64) (HealthResult, error) {
	node, err := c.loadNode(ctx, nodeID)
	if err != nil {
		return HealthResult{}, err
	}
	if node == nil {
		return HealthResult{}, fmt.Errorf("节点 #%d 不存在", nodeID)
	}
	return c.checkNode(ctx, *node)
}

// checkNode 是 check() 的实现：探测 → 写 status/latency → 采样。
func (c *Collector) checkNode(ctx context.Context, node nodeRow) (HealthResult, error) {
	if node.Status == "maintenance" {
		return HealthResult{Status: "maintenance", Latency: node.Latency, Skipped: true}, nil
	}

	probe, err := c.probeFor(node)
	if err != nil {
		// 驱动都建不出来（密文损坏 / 未知驱动类型）：与探测异常同处理 → offline。
		return c.markOffline(ctx, node, err)
	}

	res := probe(ctx)
	if res.Err != nil {
		return c.markOffline(ctx, node, res.Err)
	}

	status := "offline"
	if res.Online {
		status = "online"
	}
	if err := c.applyHealth(ctx, node, status, res.Latency, res.CPU, res.Mem); err != nil {
		return HealthResult{}, err
	}

	// 机器流量采样：把这一轮已经拿到的 netTraffic 留下来（不会额外发请求）。
	c.recordMonitor(ctx, node, &monitorSample{
		Sent: res.NetSent, Recv: res.NetRecv, CPU: res.CPU, Mem: res.Mem,
	})

	return HealthResult{OK: res.Online, Status: status, Latency: res.Latency}, nil
}

// markOffline 是「探测失败」分支：落 offline + 仍然尝试采样（xray 的累计值在库里，
// 不依赖这次探测；3x-ui 没有 netTraffic 可采，recordMonitor 内部按 nil 跳过）。
func (c *Collector) markOffline(ctx context.Context, node nodeRow, cause error) (HealthResult, error) {
	if err := c.applyHealth(ctx, node, "offline", 0, nil, nil); err != nil {
		return HealthResult{}, err
	}
	c.recordMonitor(ctx, node, nil)

	// TG 告警：只在「之前不在 offline」（本轮新掉线）时推，避免每 5 分钟刷屏。
	if c.tg != nil && node.Status != "offline" {
		text := fmt.Sprintf("🚨 节点离线：%s（%s）\n原因：%s", node.Name, node.Host, cause.Error())
		if err := c.tg.Notify(ctx, tgbot.KeyNotifyNodeOff, text); err != nil {
			c.logf_("TG 节点离线告警失败: %v", err)
		}
	}
	return HealthResult{Status: "offline", Error: cause.Error()}, nil
}

// probeFor 构造探测函数（3x-ui 走面板 API，xray 走心跳新鲜度）。
func (c *Collector) probeFor(node nodeRow) (healthProbe, error) {
	switch {
	case node.isXray():
		driver := c.xrayDriver(node)
		return func(ctx context.Context) probeResult {
			h, err := driver.HealthCheck(ctx)
			if err != nil {
				return probeResult{Err: err}
			}
			return probeResult{Online: h.OK, Latency: h.LatencyMs}
		}, nil
	case node.DriverType == "" || node.DriverType == "3x-ui":
		client, err := c.threeXUiClient(node)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) probeResult {
			h, err := client.HealthCheck(ctx)
			if err != nil {
				return probeResult{Err: err}
			}
			return probeResult{
				Online:  h.OK,
				Latency: h.LatencyMs,
				CPU:     numericOrNil(h.CPU),
				Mem:     h.Mem,
				NetSent: h.NetSent,
				NetRecv: h.NetRecv,
			}
		}, nil
	default:
		return nil, errUnsupportedDriver(node.DriverType)
	}
}

// numericOrNil 复刻 `is_numeric($v) ? (float) $v : null`（3x-ui 的 cpu 可能是数字/数字串）。
func numericOrNil(v any) *float64 {
	switch t := v.(type) {
	case nil:
		return nil
	case float64:
		return &t
	case float32:
		f := float64(t)
		return &f
	case int:
		f := float64(t)
		return &f
	case int64:
		f := float64(t)
		return &f
	case interface{ Float64() (float64, error) }:
		if f, err := t.Float64(); err == nil {
			return &f
		}
		return nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

// applyHealth 复刻 HealthCheckService::apply()：写 status/latency（3x-ui 另写 cpu/mem/最后检查）。
//
// 3x-ui 的 cpu/mem 是后来才加的列：升级窗口里库里可能还没有它们，那时去掉这两列再写一次 ——
// 附加指标不能把整轮健康检查（状态/延迟）拖挂（PHP 侧同一段 QueryException 兜底）。
func (c *Collector) applyHealth(ctx context.Context, node nodeRow, status string, latency int64, cpu, mem *float64) error {
	now := dbDateTime(c.now())

	if node.isXray() {
		// xray：只翻 status；cpu/mem/last_check_at 归 agent 心跳维护。
		if _, err := c.db.ExecContext(ctx,
			"UPDATE nodes SET status = ?, latency = ?, updated_at = ? WHERE id = ?",
			status, latency, now, node.ID); err != nil {
			return fmt.Errorf("更新节点 #%d 健康状态失败: %w", node.ID, err)
		}
		return nil
	}

	_, err := c.db.ExecContext(ctx,
		"UPDATE nodes SET status = ?, latency = ?, cpu = ?, mem = ?, last_check_at = ?, updated_at = ? WHERE id = ?",
		status, latency, nullableFloat(cpu), nullableFloat(mem), now, now, node.ID)
	if err == nil {
		return nil
	}
	if !isUnknownColumn(err) {
		return fmt.Errorf("更新节点 #%d 健康状态失败: %w", node.ID, err)
	}

	if _, retryErr := c.db.ExecContext(ctx,
		"UPDATE nodes SET status = ?, latency = ?, last_check_at = ?, updated_at = ? WHERE id = ?",
		status, latency, now, now, node.ID); retryErr != nil {
		return fmt.Errorf("更新节点 #%d 健康状态失败（cpu/mem 列缺失回退也失败）: %w", node.ID, retryErr)
	}
	return nil
}

// isUnknownColumn 判定 MySQL 1054（Unknown column）——对应 PHP 侧的 QueryException。
func isUnknownColumn(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "1054") || strings.Contains(msg, "Unknown column")
}

func nullableFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// CheckManyResult 是批量健康检查的汇总。
type CheckManyResult struct {
	// Results 是 nodeID → 结果。
	Results map[int64]HealthResult
	// Probed 是本轮实际探测次数（含补测）。
	Probed int
	// Retries 是实际补测轮数（0 表示一轮就全过）。
	Retries int
	// Errors 是建不出驱动 / 写库失败的节点数。
	Errors int
}

// healthcheckConcurrency 复刻 config('panel.healthcheck_concurrency')（默认 10，<=0 视为 1）。
func healthcheckConcurrency() int {
	n := envInt("PANEL_HEALTHCHECK_CONCURRENCY", 10)
	if n <= 0 {
		return 1
	}
	return n
}

// healthcheckRetries 复刻 config('panel.healthcheck_retry')（默认 1，<0 视为 0）。
func healthcheckRetries() int {
	n := envInt("PANEL_HEALTHCHECK_RETRY", 1)
	if n < 0 {
		return 0
	}
	return n
}

// CheckMany 复刻 HealthCheckService::checkMany()：批内并发探测 + 失败补测。
//
// 与 PHP 的差异（有意，唯一一处）：
//   - PHP 只对「3x-ui + api_key」节点并发，cookie 模式与 xray 串行；
//   - Go 侧**全部节点**都按批并发。PHP 不敢并发 cookie 模式的理由是「共享 cookie jar」，
//     而 Go 的客户端是 per-node 独立实例（独立 cookie jar / http.Client，无共享可变状态），
//     这个理由不成立。判定规则、超时、写库字段与 check() 完全一致。
//   - 单节点异常在内部消化（该节点记 offline），不牵连同批其它节点。
func (c *Collector) CheckMany(ctx context.Context, nodes []nodeRow) (CheckManyResult, error) {
	out := CheckManyResult{Results: map[int64]HealthResult{}}
	concurrency := healthcheckConcurrency()
	retries := healthcheckRetries()

	var pending []nodeRow
	for _, node := range nodes {
		if node.Status == "maintenance" {
			out.Results[node.ID] = HealthResult{Status: "maintenance", Latency: node.Latency, Skipped: true}
			continue
		}
		pending = append(pending, node)
	}

	for attempt := 0; ; attempt++ {
		var failed []nodeRow

		for start := 0; start < len(pending); start += concurrency {
			end := start + concurrency
			if end > len(pending) {
				end = len(pending)
			}
			batch := pending[start:end]

			type outcome struct {
				node   nodeRow
				result HealthResult
				err    error
			}
			results := make([]outcome, len(batch))
			var wg sync.WaitGroup
			for i, node := range batch {
				wg.Add(1)
				go func(i int, node nodeRow) {
					defer wg.Done()
					res, err := c.checkNode(ctx, node)
					results[i] = outcome{node: node, result: res, err: err}
				}(i, node)
			}
			wg.Wait()

			for _, r := range results {
				out.Probed++
				if r.err != nil {
					out.Errors++
					out.Results[r.node.ID] = HealthResult{Status: "offline", Error: r.err.Error()}
					failed = append(failed, r.node)
					continue
				}
				out.Results[r.node.ID] = r.result
				if !r.result.OK {
					failed = append(failed, r.node)
				}
			}
		}

		// 只补测失败的；一轮补测后仍失败就认了（真挂的节点不会靠重试复活，别把整轮拖长）。
		if attempt >= retries || len(failed) == 0 {
			out.Retries = attempt
			return out, nil
		}
		pending = failed
	}
}

// CheckAllEnabled 复刻 HealthCheckJob：遍历 enabled 节点刷新 status/latency。
func (c *Collector) CheckAllEnabled(ctx context.Context) (CheckManyResult, error) {
	nodes, err := c.enabledNodes(ctx)
	if err != nil {
		return CheckManyResult{}, err
	}
	return c.CheckMany(ctx, nodes)
}

// ── 机器流量采样（MonitorService::record 的数据侧） ────────────────────

// monitorThrottle 复刻 MonitorService::THROTTLE_SECONDS（距最近一条样本不足 240s 就跳过）。
const monitorThrottle = 240 * time.Second

// monitorSample 是一轮健康检查里可采到的机器流量/指标。
//
// sent/recv 是**累计值**（机器网卡从开机起的累计收发），不是区间增量 ——
// 差分口径统一在上层算（与 PHP MonitorService 同口径）。
type monitorSample struct {
	Sent *int64
	Recv *int64
	CPU  *float64
	Mem  *float64
}

// recordMonitor 复刻 MonitorService::record()：取累计值 → 节流 → 写 monitor_samples。
//
// 「没有数据就是 null，不能兜底成 0」：拿不到累计值就不写行（写 0 会被下游当成
// 「累计归零」，差分时凭空造出一段流量）。任何异常只 log 一次，绝不向外抛。
func (c *Collector) recordMonitor(ctx context.Context, node nodeRow, health *monitorSample) {
	sample, err := c.monitorValues(ctx, node, health)
	if err != nil {
		c.logf_("机器流量采样失败，已跳过（node=%d）: %v", node.ID, err)
		return
	}
	if sample == nil || sample.Sent == nil || sample.Recv == nil {
		return
	}

	// 节流：最近一条样本不足 240s → 跳过（正常 5 分钟轮不受影响；失败重试被挡住）。
	var last time.Time
	err = c.db.QueryRowContext(ctx,
		"SELECT sampled_at FROM monitor_samples WHERE node_id = ? ORDER BY sampled_at DESC LIMIT 1",
		node.ID).Scan(&last)
	switch {
	case err == nil:
		if c.now().Sub(last) < monitorThrottle {
			return
		}
	case errors.Is(err, sql.ErrNoRows):
		// 首次采样：继续写
	default:
		c.logf_("读取最近采样失败，已跳过（node=%d）: %v", node.ID, err)
		return
	}

	now := dbDateTime(c.now())
	if _, err := c.db.ExecContext(ctx,
		"INSERT INTO monitor_samples (node_id, sent, recv, cpu, mem, sampled_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		node.ID, *sample.Sent, *sample.Recv, nullableFloat(sample.CPU), nullableFloat(sample.Mem),
		now, now, now); err != nil {
		c.logf_("写入机器流量采样失败，已跳过（node=%d）: %v", node.ID, err)
	}
}

// monitorValues 复刻 MonitorService::extract()：取 (sent, recv, cpu, mem)，取不到一律 nil。
func (c *Collector) monitorValues(ctx context.Context, node nodeRow, health *monitorSample) (*monitorSample, error) {
	if node.isXray() {
		agent, err := c.xrayAgentSnapshot(ctx, node)
		if err != nil {
			return nil, err
		}
		if agent == nil || agent.Sent == nil || agent.Recv == nil {
			return nil, nil
		}
		return agent, nil
	}

	if health == nil {
		return nil, nil
	}
	// health.Sent/Recv 是驱动层已经归一化好的 *int64（panel.Health.NetSent 类型），
	// 不能再过一遍 nonNegativeInt —— 它只认基础数值与 Float64() 接口，收到 *int64 会返回 nil，
	// 于是 3x-ui 节点的机器流量采样被整段跳过（monitor_samples 永不写入、前端 24h 流量图无数据）。
	// 驱动层 netValue 已保证「非负数值」才转成指针，这里只需挡掉 nil 与负数。
	sent := nonNegativeIntPtr(health.Sent)
	recv := nonNegativeIntPtr(health.Recv)
	if sent == nil || recv == nil {
		return nil, nil
	}
	return &monitorSample{Sent: sent, Recv: recv, CPU: health.CPU, Mem: health.Mem}, nil
}

// nonNegativeIntPtr 校验驱动层已归一化的 *int64：nil 或负数 → nil，否则原样返回。
func nonNegativeIntPtr(p *int64) *int64 {
	if p == nil || *p < 0 {
		return nil
	}
	return p
}

// xrayAgentSnapshot 读 driver_config.agent 的 net_total_up/down + cpu/mem（同一份快照）。
//
// 跟随读取的意义：老 agent 只有 net_total 没有 cpu/mem（或反之）时，
// 不会出现「网卡值来自这一刻、cpu 来自上一次上报」的半新半旧。
func (c *Collector) xrayAgentSnapshot(ctx context.Context, node nodeRow) (*monitorSample, error) {
	var raw sql.NullString
	err := c.db.QueryRowContext(ctx, "SELECT driver_config FROM nodes WHERE id = ?", node.ID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 driver_config 失败: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}

	agent, ok := jsonObjectField([]byte(raw.String), "agent")
	if !ok {
		return nil, nil
	}
	return &monitorSample{
		Sent: nonNegativeInt(agent["net_total_up"]),
		Recv: nonNegativeInt(agent["net_total_down"]),
		CPU:  nonNegativeFloat(agent["cpu"]),
		Mem:  nonNegativeFloat(agent["mem"]),
	}, nil
}

// nonNegativeInt 复刻 MonitorService::nonNegativeInt()：非数值 → nil，负数 → nil。
func nonNegativeInt(v any) *int64 {
	n := numericOrNil(v)
	if n == nil || *n < 0 {
		return nil
	}
	out := int64(*n)
	return &out
}

// nonNegativeFloat 复刻 numericOrNull()（cpu/mem 只接受非负数值）。
func nonNegativeFloat(v any) *float64 {
	f := numericOrNil(v)
	if f == nil || *f < 0 || math.IsNaN(*f) || math.IsInf(*f, 0) {
		return nil
	}
	return f
}
