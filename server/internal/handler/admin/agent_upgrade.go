package admin

// 批量升级节点 agent：给 `3hub update` 收尾用的一次性入口
// （`controlhub-server --upgrade-agents`），也可被后台按钮复用。
//
// 为什么需要它：`3hub update` 只换面板自己的二进制与前端，**从不碰节点上的 agent**。
// 于是「远程到底更新了没有」在命令行里完全不可见 —— 只能进面板逐台看。
//
// 四条硬边界（踩过的坑，改前必读）：
//   1. **不干等**：每台节点有独立的等待上限，整体还有一个总预算；超了就当失败收口，
//      绝不无限挂起（`3hub update` 卡死会把升级流程一起拖住）。
//   2. **离线直接跳过**：心跳不新鲜就不入队，避免给一台不在线的机器堆 pending 指令。
//      等它上线后再跑一次即可（或后端已有的在途判定会拦住重复下发）。
//   3. **命令回执不算结束**：agent 回了 success 只代表「文件替换完成、即将退出」，
//      真正结束要等它重启后心跳上报新版本。所以 success 之后继续轮询版本，
//      版本对上了才记「升级成功」。
//   4. **失败也算结束**：回执 failed、超时、回执成功但版本没变 —— 三种都收口成
//      明确结论并带上原因，不留在「不知道」状态。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/driver/xray"
)

const (
	// agentUpgradeWaitEnv / agentUpgradeWaitDefault 是**单台**节点的等待上限（秒）。
	// 默认 180s：agent 2 秒长轮询取指令 → 下载 ~6.6MB → sha256 校验 → 替换 →
	// 退出由 systemd 拉起 → 下一次心跳上报新版本，正常在 15s 内完成；
	// 180s 给慢网与重试留足余量，又不至于让一次 update 卡太久。
	agentUpgradeWaitEnv     = "AGENT_UPGRADE_WAIT_SECONDS"
	agentUpgradeWaitDefault = 180
	// agentUpgradeBudgetEnv / agentUpgradeBudgetDefault 是**整轮**总预算（秒）。
	// 防「几十台节点 × 每台 180s」把 `3hub update` 拖成几小时。
	agentUpgradeBudgetEnv     = "AGENT_UPGRADE_BUDGET_SECONDS"
	agentUpgradeBudgetDefault = 900
	// agentUpgradePollInterval 是轮询指令状态与版本的间隔（agent 心跳 5s，取 2s 足够）。
	// 实际用的是 d.pollInterval（由构造时注入，测试可压到 1ms）。
	agentUpgradePollInterval = 2 * time.Second
	// agentUpgradeMaxReason 是单条原因的最大长度（回执可能很长，日志里截断）。
	agentUpgradeMaxReason = 300
)

// AgentUpgradeOutcome 是单台节点的结论。
type AgentUpgradeOutcome struct {
	NodeID   uint64
	NodeName string
	// From / To 是升级前后的版本（From 为空 = 节点从未上报过版本）。
	From string
	To   string
	// Action 取 upgraded / failed / skipped。
	Action string
	// Reason 是给人看的一句话原因（failed / skipped 必填）。
	Reason string
}

// AgentUpgradeResult 是一轮批量升级的汇总。
type AgentUpgradeResult struct {
	// Latest 是面板 manifest 里的 agent 版本（所有节点应该达到的目标）。
	Latest   string
	Total    int
	Upgraded int
	Failed   int
	Skipped  int
	Outcomes []AgentUpgradeOutcome
}

// HasProblem 表示这一轮有节点没升上来（`3hub update` 据此决定是否提示）。
func (r AgentUpgradeResult) HasProblem() bool {
	return r.Failed > 0
}

// agentUpgradeWaitSeconds 读单台等待上限（秒），非法值回默认。
func agentUpgradeWaitSeconds() int {
	return envPositiveInt(agentUpgradeWaitEnv, agentUpgradeWaitDefault)
}

// agentUpgradeBudgetSeconds 读整轮总预算（秒），非法值回默认。
func agentUpgradeBudgetSeconds() int {
	return envPositiveInt(agentUpgradeBudgetEnv, agentUpgradeBudgetDefault)
}

// envPositiveInt 读一个正整数环境变量；缺失/非法/非正数一律回默认。
func envPositiveInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// UpgradeAllAgents 逐台检查并升级 agent，返回可打印的结论清单。
//
// 顺序处理而非并发：节点数是个位数到几十，串行能让输出顺序稳定、也不会同时
// 让多台机器下载同一个二进制把面板出口打满。
func (d *NodeDomain) UpgradeAllAgents(ctx context.Context) (AgentUpgradeResult, error) {
	res := AgentUpgradeResult{}

	nodes, _, err := d.store.NodesPage(ctx, true, 1, 50)
	if err != nil {
		return res, fmt.Errorf("查询节点列表失败: %w", err)
	}

	manifest, err := d.ops.Manifest(ctx)
	if err != nil {
		return res, fmt.Errorf("读取 agent manifest 失败: %w", err)
	}
	if latest := manifestLatestVersion(manifest); latest != nil {
		res.Latest = *latest
	}
	if res.Latest == "" {
		return res, fmt.Errorf("面板 manifest 里没有 agent 版本号，无法判断目标版本")
	}

	perNodeWait := time.Duration(agentUpgradeWaitSeconds()) * time.Second
	deadline := d.now().Add(time.Duration(agentUpgradeBudgetSeconds()) * time.Second)

	for i := range nodes {
		n := &nodes[i]
		res.Total++

		from := ""
		if v := d.reportedAgentVersion(n); v != nil {
			from = *v
		}
		out := AgentUpgradeOutcome{NodeID: n.ID, NodeName: n.Name, From: from, To: res.Latest}

		// 预算用尽：后面的节点本轮不碰，如实记账（下次 update 再来）。
		if d.now().After(deadline) {
			out.Action = "skipped"
			out.Reason = "本轮总预算已用尽，未处理"
			res.Skipped++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}

		// ── 一道道判定，任何一条不满足就跳过并且说清楚为什么 ──
		if reason, skip := d.agentUpgradeSkipReason(ctx, n, from, res.Latest); skip {
			out.Action = "skipped"
			out.Reason = reason
			res.Skipped++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}

		outcome := d.upgradeOneAgent(ctx, n, res.Latest, perNodeWait, deadline)
		out.Action = outcome.Action
		out.Reason = outcome.Reason
		switch outcome.Action {
		case "upgraded":
			res.Upgraded++
		default:
			res.Failed++
		}
		res.Outcomes = append(res.Outcomes, out)
	}

	return res, nil
}

// agentUpgradeSkipReason 判断这台节点本轮是否应当跳过；返回原因与是否跳过。
func (d *NodeDomain) agentUpgradeSkipReason(ctx context.Context, n *NodeRecord, from, latest string) (string, bool) {
	if !n.IsXray() {
		return "不是 agent 节点（3x-ui 面板节点不受管）", true
	}
	if !n.Enabled {
		return "节点已禁用", true
	}
	if !d.agentNodeOnline(n) {
		// 不入队：避免给一台不在线的机器堆 pending 指令；它上线后再跑一次即可。
		return "节点离线（心跳不新鲜），本轮跳过；上线后再执行一次即可", true
	}
	if from != "" && versionAtLeast(from, latest) {
		return "已是最新 " + from, true
	}
	inFlight, err := d.store.HasInFlightUpgrade(ctx, n.ID)
	if err != nil {
		return "查询在途升级指令失败: " + err.Error(), true
	}
	if inFlight {
		return "已有升级指令在途（等它执行完再检查）", true
	}
	return "", false
}

// agentNodeOnline 判断节点心跳是否新鲜（与 xray 驱动 HealthCheck 同一阈值口径）。
//
// 只认心跳，不看 nodes.status：status 是健康检查写进去的，健康检查每 5 分钟才跑一次，
// 用它判「现在能不能收指令」会滞后；心跳是 agent 自己上报的，5 秒一次，更接近事实。
func (d *NodeDomain) agentNodeOnline(n *NodeRecord) bool {
	if n.LastCheckAt == nil {
		return false
	}
	age := d.now().Sub(*n.LastCheckAt)
	// 负值（时钟漂移导致的「未来心跳」）按在线处理，不因此判定离线。
	if age < 0 {
		return true
	}
	return age <= time.Duration(xray.HeartbeatFreshSeconds)*time.Second
}

// agentUpgradeAttempt 是单台节点的升级过程结果。
type agentUpgradeAttempt struct {
	Action string
	Reason string
}

// upgradeOneAgent 入队并**等到真正结束**（版本变化 / 失败 / 超时），返回结论。
func (d *NodeDomain) upgradeOneAgent(ctx context.Context, n *NodeRecord, latest string, perNodeWait time.Duration, budgetDeadline time.Time) agentUpgradeAttempt {
	manifest, err := d.ops.Manifest(ctx)
	if err != nil {
		return agentUpgradeAttempt{Action: "failed", Reason: "读取 agent manifest 失败: " + err.Error()}
	}
	target := nodeUpgradeTarget(n, manifest)
	if target == nil {
		return agentUpgradeAttempt{Action: "failed", Reason: "面板 manifest 里没有该架构的 agent 资产"}
	}

	payload := jo(
		"file", target.File,
		"sha256", target.SHA256,
		"version", target.Version,
		"url", "/node-bin/agent/"+target.Arch,
	)
	cmd, err := d.store.CreateCommand(ctx, n.ID, "upgrade", payload)
	if err != nil {
		return agentUpgradeAttempt{Action: "failed", Reason: "入队升级指令失败: " + err.Error()}
	}

	// 单台上限与整轮预算取更紧的那个：不能因为一台慢节点把整轮拖超预算。
	deadline := d.now().Add(perNodeWait)
	if budgetDeadline.Before(deadline) {
		deadline = budgetDeadline
	}

	ackSeen := false
	for d.now().Before(deadline) {
		fresh, err := d.store.CommandByID(ctx, cmd.ID)
		if err != nil {
			return agentUpgradeAttempt{Action: "failed", Reason: "读取指令状态失败: " + err.Error()}
		}
		switch fresh.Status {
		case "failed":
			// agent 明确说失败：这是终态，直接收口。
			return agentUpgradeAttempt{Action: "failed", Reason: "节点执行失败: " + trimReason(resultText(fresh))}
		case "success":
			// 回执成功 ≠ 真正结束：agent 只是「文件已替换、马上退出重启」。
			// 继续等新版本心跳，版本对上才记成功。
			ackSeen = true
		case "acknowledged":
			ackSeen = true
		}

		// 每轮都读一次节点：版本可能已经变了（agent 重启后心跳会上报）。
		if cur, err := d.store.NodeByID(ctx, n.ID); err == nil {
			if v := d.reportedAgentVersion(cur); v != nil && versionAtLeast(*v, latest) {
				return agentUpgradeAttempt{Action: "upgraded"}
			}
		}

		// 用 d.pollInterval 而不是包级常量：测试注入 1ms，用例不必真睡。
		d.sleep(d.pollInterval)
	}

	// 到点了还没确认：按「没升上来」收口，但要说清到底卡在哪一步。
	if ackSeen {
		return agentUpgradeAttempt{Action: "failed", Reason: "回执已收到但版本未更新（可能正在重启，稍后重跑本检查即可）"}
	}
	return agentUpgradeAttempt{Action: "failed", Reason: "等待超时：节点未取走指令（可能刚掉线，上线后会自动执行）"}
}

// resultText 取指令回执文本（可能为空）。
func resultText(c *CommandRecord) string {
	if c == nil || c.Result == nil {
		return ""
	}
	return *c.Result
}

// trimReason 截断过长的回执，避免把一整段 JSON 打进 update 输出。
func trimReason(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "（节点未给出原因）"
	}
	// 回执常是一行 JSON，压掉换行更利于终端阅读。
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > agentUpgradeMaxReason {
		r := []rune(s)
		return string(r[:agentUpgradeMaxReason]) + "…"
	}
	return s
}
