package admin

// 节点 agent 版本下发（`controlhub-server --upgrade-agents`，`3hub update` 收尾自动调用）
// 与「上线后自动补跑」的周期对账。
//
// 口径（2026-11 用户定稿，改前必读）：
//   1. **只下发，不等回执**：`3hub update` 不该为了等节点升级完而挂住 —— 指令入队即返回，
//      节点取走后自己下载 → sha256 校验 → 替换 → 重启（见 agent/upgrade.go）。
//      这一点与面板上「单台升级」按钮（node_crud.upgradeAgent）完全同口径。
//   2. **离线不入队**：心跳不新鲜就不下发，避免给不在线的机器堆 pending 指令；
//      它上线后由周期补跑自动补上（上线后照常享有完整的机会数）。
//   3. **总共 3 次机会**：自动补跑按「已下发的指令条数」计数，同一目标版本累计 3 次就停手
//      （间隔默认 600s）。**离线恢复后下发的同样算一次，离线恢复后的更新失败也不额外加次数**
//      —— 机会数就是 3，不会因为中间断过线而变多。
//   4. **错误三处可见**：面板日志（log）、系统审计（operation_logs，前端「操作审计」页）、
//      节点行/详情的 agent 提示（agent_upgrade_last_error）。
//
// 为什么尝试次数不额外建表：`node_api_commands` 里每次都留了 payload(version)/status/result，
// 按「节点 + 目标版本」现算即可（见 UpgradeAttemptStats），少一张表少一处漂移。

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
	// agentUpgradeMaxAttemptsEnv / Default 是**同一目标版本**下自动补跑的最大尝试次数。
	agentUpgradeMaxAttemptsEnv     = "AGENT_UPGRADE_MAX_ATTEMPTS"
	agentUpgradeMaxAttemptsDefault = 3
	// agentUpgradeRetryIntervalEnv / Default 是两次自动尝试之间的最小间隔（秒）。
	// 默认 600s（10 分钟）：给节点重启 + 心跳上报留足时间，也不至于让失败节点刷屏。
	agentUpgradeRetryIntervalEnv     = "AGENT_UPGRADE_RETRY_INTERVAL_SECONDS"
	agentUpgradeRetryIntervalDefault = 600
	// agentUpgradeMaxReason 是单条原因的最大长度（回执可能很长，日志/审计里截断）。
	agentUpgradeMaxReason = 300

	// 系统审计动作名（operation_logs.type = 'system'）；前端「操作审计」页按类型/操作展示。
	agentUpgradeAuditAction       = "节点 agent 升级失败"
	agentUpgradeAuditGiveUpAction = "节点 agent 自动升级已放弃"
	// agentUpgradeAuditActor 是系统审计里的操作者名（与 traffic-sync 那类后台任务同风格）。
	agentUpgradeAuditActor = "agent-upgrade"
)

// AgentUpgradeOutcome 是单台节点的结论。
type AgentUpgradeOutcome struct {
	NodeID   uint64
	NodeName string
	// From / To 是升级前后的版本（From 为空 = 节点从未上报过版本）。
	From string
	To   string
	// Action 取 dispatched / failed / skipped。
	Action string
	// Reason 是给人看的一句话原因（failed / skipped 必填）。
	Reason string
}

// AgentUpgradeResult 是一轮下发的汇总。
type AgentUpgradeResult struct {
	// Latest 是面板 manifest 里的 agent 版本（所有节点应该达到的目标）。
	Latest     string
	Total      int
	Dispatched int
	Failed     int
	Skipped    int
	Outcomes   []AgentUpgradeOutcome
}

// HasProblem 表示这一轮有节点连指令都没下下去（`3hub update` 据此提示）。
func (r AgentUpgradeResult) HasProblem() bool {
	return r.Failed > 0
}

// UpgradeAttemptStats 是「节点 + 目标版本」维度下的历史尝试统计（从 node_api_commands 现算）。
type UpgradeAttemptStats struct {
	// Attempts 是针对该目标版本已经下发过的指令条数（含成功与失败）。
	Attempts int
	// LastAt / LastStatus / LastCommandID 是最近一条该版本指令的时间、状态与 id。
	LastAt        *time.Time
	LastStatus    string
	LastCommandID uint64
	// LastError 是最近一条失败指令给的原因（没有失败则为空）。
	LastError string
}

// agentUpgradeMaxAttempts 读自动补跑的最大尝试次数，非法值回默认。
func agentUpgradeMaxAttempts() int {
	return envPositiveInt(agentUpgradeMaxAttemptsEnv, agentUpgradeMaxAttemptsDefault)
}

// agentUpgradeRetryInterval 读两次自动尝试之间的最小间隔。
func agentUpgradeRetryInterval() time.Duration {
	return time.Duration(envPositiveInt(agentUpgradeRetryIntervalEnv, agentUpgradeRetryIntervalDefault)) * time.Second
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

// UpgradeAllAgents 逐台比对版本并**只下发**升级指令（`3hub update` 用，手动触发）。
//
// 手动路径不受「最多 N 次 / 最小间隔」限制（那是自动补跑的节流），但仍会跳过
// 离线、已是最新、已有在途指令的节点。
func (d *NodeDomain) UpgradeAllAgents(ctx context.Context) (AgentUpgradeResult, error) {
	return d.runAgentUpgrade(ctx, true)
}

// ReconcileAgentUpgrades 是周期补跑：离线/失败节点上线后自动再试，带次数与间隔上限。
//
// 与 UpgradeAllAgents 的唯一差别是节流：同一目标版本尝试满 N 次后停手（并写一条系统审计），
// 两次尝试之间至少间隔 M 秒。全程只下发，不等待。
func (d *NodeDomain) ReconcileAgentUpgrades(ctx context.Context) (AgentUpgradeResult, error) {
	return d.runAgentUpgrade(ctx, false)
}

// runAgentUpgrade 是两条入口的公共实现；manual=true 表示手动触发（不受节流限制）。
func (d *NodeDomain) runAgentUpgrade(ctx context.Context, manual bool) (AgentUpgradeResult, error) {
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

	for i := range nodes {
		n := &nodes[i]
		res.Total++

		from := ""
		if v := d.reportedAgentVersion(n); v != nil {
			from = *v
		}
		out := AgentUpgradeOutcome{NodeID: n.ID, NodeName: n.Name, From: from, To: res.Latest}

		// ── 一道道判定，任何一条不满足就跳过并且说清楚为什么 ──
		if reason, skip := d.agentUpgradeSkipReason(ctx, n, from, res.Latest); skip {
			out.Action = "skipped"
			out.Reason = reason
			res.Skipped++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}

		// 上一轮失败过的，先把错误补记进系统审计（按指令 id 幂等，不会重复写）。
		stats, err := d.store.UpgradeAttemptStats(ctx, n.ID, res.Latest)
		if err != nil {
			out.Action = "failed"
			out.Reason = "查询历史升级尝试失败: " + err.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if stats.LastStatus == "failed" && stats.LastCommandID > 0 {
			d.auditAgentUpgradeFailure(ctx, n, from, res.Latest, stats)
		}

		// 自动补跑的节流：尝试次数上限 + 最小间隔。
		if !manual {
			if stats.Attempts >= agentUpgradeMaxAttempts() {
				d.auditAgentUpgradeGiveUp(ctx, n, from, res.Latest, stats)
				out.Action = "skipped"
				out.Reason = fmt.Sprintf("已用完 %d 次自动升级机会仍未成功，停止自动补跑（最近错误：%s）",
					stats.Attempts, orNone(stats.LastError))
				res.Skipped++
				res.Outcomes = append(res.Outcomes, out)
				continue
			}
			if stats.LastAt != nil {
				if remaining := agentUpgradeRetryInterval() - d.now().Sub(*stats.LastAt); remaining > 0 {
					out.Action = "skipped"
					out.Reason = fmt.Sprintf("距上次尝试不足 %s，等下一轮补跑", humanDuration(agentUpgradeRetryInterval()))
					res.Skipped++
					res.Outcomes = append(res.Outcomes, out)
					continue
				}
			}
		}

		// ── 下发（入队即返回，不等节点执行完）──
		if err := d.dispatchAgentUpgrade(ctx, n, manifest); err != nil {
			out.Action = "failed"
			out.Reason = err.Error()
			res.Failed++
			d.auditAgentUpgradeDispatchFailed(ctx, n, from, res.Latest, err)
			d.logf("[agent] 下发升级指令失败：节点 %s(#%d) %s → %s：%v", n.Name, n.ID, orUnknown(from), res.Latest, err)
		} else {
			out.Action = "dispatched"
			res.Dispatched++
			d.logf("[agent] 已下发升级指令：节点 %s(#%d) %s → %s", n.Name, n.ID, orUnknown(from), res.Latest)
		}
		res.Outcomes = append(res.Outcomes, out)
	}

	return res, nil
}

// dispatchAgentUpgrade 把升级指令入队（只入队，不等回执）。
func (d *NodeDomain) dispatchAgentUpgrade(ctx context.Context, n *NodeRecord, manifest *ManifestAgent) error {
	target := nodeUpgradeTarget(n, manifest)
	if target == nil {
		return fmt.Errorf("面板 manifest 里没有该架构的 agent 资产")
	}
	payload := jo(
		"file", target.File,
		"sha256", target.SHA256,
		"version", target.Version,
		"url", "/node-bin/agent/"+target.Arch,
	)
	if _, err := d.store.CreateCommand(ctx, n.ID, "upgrade", payload); err != nil {
		return fmt.Errorf("入队升级指令失败: %w", err)
	}
	return nil
}

// ── 系统审计（operation_logs.type = 'system'）──

// auditAgentUpgradeFailure 把「某条升级指令失败」记进系统审计。
//
// path 里带指令 id → 同一条失败指令只会写一条，重复跑对账不会刷屏。
func (d *NodeDomain) auditAgentUpgradeFailure(ctx context.Context, n *NodeRecord, from, latest string, stats UpgradeAttemptStats) {
	errMsg := fmt.Sprintf("节点 %s(#%d) agent %s → %s 升级失败：%s",
		n.Name, n.ID, orUnknown(from), latest, orNone(stats.LastError))
	path := fmt.Sprintf("node#%d/cmd#%d", n.ID, stats.LastCommandID)
	d.auditAgentUpgradeOnce(ctx, agentUpgradeAuditAction, path, errMsg)
}

// auditAgentUpgradeGiveUp 把「自动补跑已放弃」记进系统审计（同一节点同一版本只记一条）。
func (d *NodeDomain) auditAgentUpgradeGiveUp(ctx context.Context, n *NodeRecord, from, latest string, stats UpgradeAttemptStats) {
	errMsg := fmt.Sprintf("节点 %s(#%d) agent %s → %s 已用完 %d 次自动升级机会仍未成功，已停止自动补跑（最近错误：%s）",
		n.Name, n.ID, orUnknown(from), latest, stats.Attempts, orNone(stats.LastError))
	path := fmt.Sprintf("node#%d/ver#%s", n.ID, latest)
	d.auditAgentUpgradeOnce(ctx, agentUpgradeAuditGiveUpAction, path, errMsg)
}

// auditAgentUpgradeDispatchFailed 记「连指令都没下下去」（按小时去重，避免每轮对账都写）。
func (d *NodeDomain) auditAgentUpgradeDispatchFailed(ctx context.Context, n *NodeRecord, from, latest string, cause error) {
	errMsg := fmt.Sprintf("节点 %s(#%d) agent %s → %s 下发失败：%v", n.Name, n.ID, orUnknown(from), latest, cause)
	path := fmt.Sprintf("node#%d/dispatch#%s", n.ID, d.now().UTC().Format("2006010215"))
	d.auditAgentUpgradeOnce(ctx, agentUpgradeAuditAction, path, errMsg)
}

// auditAgentUpgradeOnce 幂等写系统审计：写成功/失败都只记日志，绝不因为审计失败影响升级流程。
func (d *NodeDomain) auditAgentUpgradeOnce(ctx context.Context, action, path, errMsg string) {
	msg := auditText(errMsg)
	written, err := d.store.RecordSystemAuditOnce(ctx, action, agentUpgradeAuditActor, path, msg)
	if err != nil {
		d.logf("[agent] 写系统审计失败（%s %s）: %v", action, path, err)
		return
	}
	if written {
		d.logf("[agent] 已记系统审计：%s", msg)
	}
}

// auditText 把审计文案压成单行（审计页「报错」列是窄列，换行/缩进没必要；
// 原因本身已在 trimReason 里限长，这里不再二次截断 —— 审计里要能看见完整原因）。
func auditText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ── 判定 ──

// agentUpgradeSkipReason 判断这台节点本轮是否应当跳过下发；返回原因与是否跳过。
func (d *NodeDomain) agentUpgradeSkipReason(ctx context.Context, n *NodeRecord, from, latest string) (string, bool) {
	if !n.IsXray() {
		return "不是 agent 节点（3x-ui 面板节点不受管）", true
	}
	if !n.Enabled {
		return "节点已禁用", true
	}
	if !d.agentNodeOnline(n) {
		// 不入队：避免给一台不在线的机器堆 pending 指令；上线后由周期补跑自动补上。
		return "节点离线（心跳不新鲜），跳过；上线后会自动补跑", true
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

// ── 小工具 ──

// trimReason 截断过长的回执，避免把一整段 JSON 打进日志/审计。
func trimReason(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 回执常是一行 JSON，压掉换行更利于终端阅读。
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > agentUpgradeMaxReason {
		r := []rune(s)
		return string(r[:agentUpgradeMaxReason]) + "…"
	}
	return s
}

func orUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return "未上报"
	}
	return v
}

func orNone(v string) string {
	if strings.TrimSpace(v) == "" {
		return "（节点未给出原因）"
	}
	return v
}

// humanDuration 把间隔说成人话（600s → 10 分钟），只用于提示文案。
func humanDuration(d time.Duration) string {
	if d >= time.Minute {
		if d%time.Minute == 0 {
			return fmt.Sprintf("%d 分钟", int(d/time.Minute))
		}
		return fmt.Sprintf("%.1f 分钟", d.Minutes())
	}
	return fmt.Sprintf("%d 秒", int(d/time.Second))
}

// d.logf 带域前缀的日志（Logger 可能为 nil，测试里不注入）。
func (d *NodeDomain) logf(format string, args ...any) {
	if d.logger == nil {
		return
	}
	d.logger.Printf(format, args...)
}
