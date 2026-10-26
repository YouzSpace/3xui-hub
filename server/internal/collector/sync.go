package collector

import (
	"context"
	"fmt"
	"sort"
)

// 入口编排：对齐 SyncTrafficCommand（traffic:sync，每 5 分钟）与
// SyncNodeTrafficJob（单节点/单用户手工同步）。
//
// 流程（与 PHP 同序）：
//  1. 遍历 enabled 节点，**xray 节点跳过**（流量由 agent 主动推送 /node-api/push）；
//  2. 每节点取数一次（listInbounds）→ 节点锁内算增量并落账；
//  3. 有增量的用户逐个做封禁检查（已确认关闭且未超时效 → 0 次 HTTP 早退）；
//  4. 单节点失败只跳过该节点，不影响其它节点。

// TrafficSyncResult 是一轮全量同步的统计（对齐命令输出的三行汇总）。
type TrafficSyncResult struct {
	// Nodes 是本轮实际参与的 3x-ui 节点数（xray 不计）。
	Nodes int
	// SyncedUsers 是「有增量」的 user×node 记录数（PHP 的 $totalSynced）。
	SyncedUsers int
	// SnapshotRows 是写入的快照行数总和。
	SnapshotRows int
	// Disabled 是本轮因封禁条件关掉流量的用户数。
	Disabled int
	// SkippedLock 是因节点锁被占用而跳过的节点数（acquired=false）。
	SkippedLock int
	// FailedNodes 是取数/落账失败的节点（失败不中断整轮）。
	FailedNodes []int64
	// DriftUsers 是本轮发现的「节点关了、面板放行」用户（去重）。
	DriftUsers []int64
}

// RunTrafficSync 复刻 SyncTrafficCommand::handle()。
//
// 取数与落库的顺序：先按节点顺序**取数**（每节点一次 listInbounds），拿到的样本立刻
// 在节点锁内落账 —— 与 PHP 的 prefetch 版语义一致（PHP 并发取数是为省墙钟，Go 侧取数
// 顺序化以免把「节点 A 的采样早于节点 B」这类顺序不确定性引入对账）。
func (c *Collector) RunTrafficSync(ctx context.Context) (TrafficSyncResult, error) {
	var out TrafficSyncResult

	nodes, err := c.enabledNodes(ctx)
	if err != nil {
		return out, err
	}

	deltaUsers := map[int64]bool{}
	driftSeen := map[int64]bool{}

	for _, node := range nodes {
		// xray 节点流量由 agent 主动推送（/node-api/push），不做面板侧轮询。
		if node.isXray() {
			continue
		}
		if node.DriverType != "" && node.DriverType != "3x-ui" {
			c.logf_("跳过未知驱动类型的节点（node=%d driver=%s）", node.ID, node.DriverType)
			continue
		}
		out.Nodes++

		res, err := c.SyncNode(ctx, node.ID, func(ctx context.Context) (map[string]ClientStat, error) {
			client, err := c.threeXUiClient(node)
			if err != nil {
				return nil, err
			}
			// 数据层已按 email 去重（同 email 跨入站只取首值，避免 N 倍计费）。
			stats, err := client.GetClientStatsByEmail(ctx)
			if err != nil {
				return nil, err
			}
			conv := make(map[string]ClientStat, len(stats))
			for email, s := range stats {
				conv[email] = ClientStat{Up: s.Up, Down: s.Down, Enable: s.Enable}
			}
			return conv, nil
		})
		if err != nil {
			out.FailedNodes = append(out.FailedNodes, node.ID)
			c.logf_("节点流量同步失败（node=%d %s）: %v", node.ID, node.Name, err)
			continue
		}
		if !res.Acquired {
			out.SkippedLock++
			continue
		}

		out.SyncedUsers += len(res.DeltaMap)
		out.SnapshotRows += res.SnapshotCount
		for uid := range res.DeltaMap {
			deltaUsers[uid] = true
		}
		for _, uid := range res.DriftUserIDs {
			if !driftSeen[uid] {
				driftSeen[uid] = true
				out.DriftUsers = append(out.DriftUsers, uid)
			}
		}
	}

	// 封禁检查（仅检查有流量变化的用户）—— 与 PHP 同序、同判据。
	ids := make([]int64, 0, len(deltaUsers))
	for id := range deltaUsers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, userID := range ids {
		user, err := c.loadUser(ctx, userID)
		if err != nil || user == nil {
			continue
		}
		if banReasonAt(*user, c.now()) == "" || !user.Enabled {
			continue
		}
		// 已确认关闭且未超时效 → 跳过（0 次 HTTP）。
		if isRecentlyDisabled(*user, c.now()) {
			continue
		}
		if _, err := c.toggleClient(ctx, userID, false, true); err != nil {
			c.logf_("关闭用户流量失败（user=%d）: %v", userID, err)
			continue
		}
		out.Disabled++
	}

	sort.Slice(out.DriftUsers, func(i, j int) bool { return out.DriftUsers[i] < out.DriftUsers[j] })
	return out, nil
}

// SyncSingleUserNode 复刻 SyncNodeTrafficJob 的单用户分支（syncUserNodeFromSource）：
// 在节点锁内取该用户在本节点的流量并落账。
//
// 供 P8a/P8b 的「同步流量」按钮复用；定时任务不走这条路径（走 RunTrafficSync）。
func (c *Collector) SyncSingleUserNode(ctx context.Context, userID, nodeID int64) (SyncResult, error) {
	email := clientEmail(userID)

	// 取数回调：直接复用批量接口再挑出该 email —— 与 PHP 的 getClientStatsByEmail()[email]
	// 完全一致（3x-ui 没有「按 email 取累计值」的批量等价接口，客户端侧也只能这么取）。
	res, err := c.SyncNode(ctx, nodeID, func(ctx context.Context) (map[string]ClientStat, error) {
		node, err := c.loadNode(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		if node == nil {
			return nil, fmt.Errorf("节点 #%d 不存在", nodeID)
		}
		if node.isXray() {
			return nil, nil // xray 不走面板侧轮询
		}
		client, err := c.threeXUiClient(*node)
		if err != nil {
			return nil, err
		}
		stats, err := client.GetClientStatsByEmail(ctx)
		if err != nil {
			return nil, err
		}
		out := map[string]ClientStat{}
		if s, ok := stats[email]; ok {
			out[email] = ClientStat{Up: s.Up, Down: s.Down, Enable: s.Enable}
		}
		return out, nil
	})
	if err != nil {
		return SyncResult{DeltaMap: map[int64]Delta{}}, err
	}
	return res, nil
}

// RunAll 是本包的「一轮采集」入口：流量同步 → 健康检查 → 封禁扫描。
//
// 月重置是每日任务（PHP 00:00），由调度层单独调用 MonthlyReset —— 不放进 5 分钟轮，
// 否则「只清已用流量」的语义会被误用成高频操作。
//
// 任何一步失败都不阻断后续步骤（与 PHP 每个任务独立调度同义）。
type RoundResult struct {
	Traffic    TrafficSyncResult
	TrafficErr error
	Health     CheckManyResult
	HealthErr  error
	Bans       BanScanResult
	BansErr    error
}

// RunRound 串行跑一轮采集（P9 的调度器每个 tick 调它；也可被手工触发）。
func (c *Collector) RunRound(ctx context.Context) RoundResult {
	var out RoundResult

	out.Traffic, out.TrafficErr = c.RunTrafficSync(ctx)
	if out.TrafficErr != nil {
		c.logf_("流量同步失败: %v", out.TrafficErr)
	}

	out.Health, out.HealthErr = c.CheckAllEnabled(ctx)
	if out.HealthErr != nil {
		c.logf_("健康检查失败: %v", out.HealthErr)
	}

	out.Bans, out.BansErr = c.ScanBans(ctx)
	if out.BansErr != nil {
		c.logf_("封禁扫描失败: %v", out.BansErr)
	}
	return out
}
