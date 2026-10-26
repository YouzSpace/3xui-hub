package main

// 把 collector 的「节点用户全量比对」接到调度命令上。
//
// 为什么走适配器：internal/task 刻意不引用 internal/collector 的类型（只声明窄接口），
// 这里做一次字段映射，保持两边的边界干净。

import (
	"context"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/task"
)

// reconcileAdapter 返回 task.CommandDeps.ReconcileNodes 需要的函数。
func reconcileAdapter(col *collector.Collector) func(context.Context, bool, int) (task.ReconcileSummary, error) {
	return func(ctx context.Context, dry bool, maxWritesPerNode int) (task.ReconcileSummary, error) {
		rep, err := col.ReconcileNodes(ctx, dry, maxWritesPerNode)
		if err != nil {
			return task.ReconcileSummary{}, err
		}
		return task.ReconcileSummary{
			Nodes:        rep.Nodes,
			Users:        rep.Users,
			Probed:       rep.Probed,
			Registered:   rep.Registered,
			Fixed:        rep.Fixed,
			Drift:        rep.Drift,
			Opened:       rep.Opened,
			Closed:       rep.Closed,
			Unchanged:    rep.Unchanged,
			Unknown:      rep.Unknown,
			Deferred:     rep.Deferred,
			Foreign:      rep.Foreign,
			ForeignNames: rep.ForeignNames,
			OfflineNodes: rep.OfflineNodes,
			SnapshotBy:   rep.SnapshotBy,
			ElapsedMs:    rep.ElapsedMs,
		}, nil
	}
}
