package task

// 用户启停对账（users:reconcile-enable）：节点快照比对与 client 开合。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"strings"
)

// runUsersReconcileEnable 是「节点用户全量比对 + 兜底修复」的调度入口。
//
// 实现委托 collector.ReconcileNodes（cmd/server 注入）：
//   - 读法：每台 3x-ui 节点一次 clients/list 拿全量快照（旧实现是每用户一次 getClient，
//     94 人 × 4 台 ≈ 376 次往返，还用了恒失败的 GetClientTraffic → 12 小时 143 轮 checked=0）；
//   - 修法：缺人→补注册、enable 该开该关→兜底执行、totalGB/expiry 对不上（含残留旧记录）
//     →删除重建（keepTraffic=1 保住流量统计基线）；超时/未知→本轮不动；
//   - 范围：只处理我们自己建的 ch_user_{id}，面板上自建的远程用户只报告、绝不动手。
//
// 旧版在这里另写了一份「只会把 enable=false 重开」的实现，候选 SQL 还带
// `traffic_disabled_at IS NULL`，把真正卡住的人全筛掉 —— 线上 84 就是这么被漏掉的。
func (r *commandRunner) runUsersReconcileEnable(ctx context.Context, args map[string]string) error {
	dry := args["--dry-run"] == "1" || args["--dry-run"] == "true"
	if r.deps.ReconcileNodes == nil {
		r.logf("ReconcileNodes 未配置，跳过对账")
		return nil
	}
	sum, err := r.deps.ReconcileNodes(ctx, dry, 50)
	if err != nil {
		return err
	}
	prefix := ""
	if dry {
		prefix = "[dry-run] "
	}
	// 结构化计数（一行、固定键名，便于日志检索与面板解析）：
	//   nodes/users 规模 · probed 有结论数 · probe_failed 探测失败 · fixed/skipped(deferred) 动作
	//   unknown 未知 · drift 三档（safe/early_cut/loose）· ms 墙钟。
	r.logf("%snodes=%d users=%d probed=%d probe_failed=%d registered=%d fixed=%d attached=%d skipped=%d "+
		"unknown=%d drift=%d drift_fixed=%d drift_safe=%d drift_early_cut=%d drift_loose=%d "+
		"opened=%d closed=%d unchanged=%d foreign=%d snap=%s ms=%d "+
		"bare_nodes=%d bare_mismatch=%d bare_bumped=%d bare_drift=%d bare_stale=%d",
		prefix, sum.Nodes, sum.Users, sum.Probed, sum.ProbeFailed, sum.Registered, sum.Fixed, sum.Attached,
		sum.Deferred, sum.Unknown, sum.Drift, sum.DriftFixed, sum.DriftSafe, sum.DriftEarlyCut, sum.DriftLoose,
		sum.Opened, sum.Closed, sum.Unchanged, sum.Foreign, sum.SnapshotBy, sum.ElapsedMs,
		sum.BareNodes, sum.BareMismatch, sum.BareBumped, sum.BareDrift, sum.BareStale)
	if sum.DriftFixed > 0 {
		r.logf("对账漂移已自动修复：%d 条节点的 total/expiry 按面板值写回并复核一致", sum.DriftFixed)
	}
	if sum.BareBumped > 0 {
		// 裸内核的唯一修复手段是重拉配置（agent 随后会重启内核），这条要留痕好对时间线。
		r.logf("裸内核重拉：%d 台节点因用户集合/开关与面板不一致而 bump config_version"+
			"（缺人或该开被关 %d 条）", sum.BareBumped, sum.BareMismatch)
	}
	if sum.DriftEarlyCutWarning() {
		r.logf("[WARN] 对账：%d 条漂移会让用户被节点提前掐（节点限额/期限比面板更紧），需人工确认",
			sum.DriftEarlyCut)
	}
	if sum.ProbeStalled() {
		r.logf("[WARN] 对账空转：本轮 probed=0（probe_failed=%d users=%d），节点侧一条都没读到",
			sum.ProbeFailed, sum.Users)
	}
	if len(sum.ForeignNames) > 0 {
		r.logf("  非本站 client（只报告，未动手）: %s", strings.Join(sum.ForeignNames, ", "))
	}
	if len(sum.OfflineNodes) > 0 {
		r.logf("  本轮跳过（读不到快照，不判失败）: %s", strings.Join(sum.OfflineNodes, ", "))
	}
	return nil
}

// ── 13. xray:reality-audit ─────────────────────────────────────────
