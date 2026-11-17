package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// Balance 在一个地区内做均衡：把「已绑用户数超过 capacity」的节点上的用户，
// 搬到同地区还有余量的节点上。
//
//   - dry=true 只产出计划、不写库；此时返回的 moved 是「计划条数」（= len(plan)），
//     不是实际写入数，方便管理端预览时直接展示。
//   - limit>0 时最多规划 / 执行 limit 个用户。
//   - 只在被调用时执行，不做任何自动触发。
//
// 目标节点按「剩余容量降序（平局 id 升序）」排定先后，依次填满；
// 源节点内的用户按 user_id 升序取，保证同一份数据产出的计划稳定可复现。
func (s *Scheduler) Balance(ctx context.Context, regionID int64, dry bool, limit int) (int, []Move, error) {
	stats, err := s.regionNodeStats(ctx, regionID)
	if err != nil {
		return 0, nil, err
	}
	plan, err := s.planBalance(ctx, stats, limit)
	if err != nil {
		return 0, nil, err
	}
	if dry {
		return len(plan), plan, nil
	}

	moved := 0
	for _, mv := range plan {
		ok, err := moveUser(ctx, s.db, mv.UserID, mv.FromNodeID, mv.ToNodeID)
		if err != nil {
			return moved, plan, err
		}
		if !ok {
			// 计划生成之后这条绑定已经被别人搬走 / 用户被删：跳过，不计入实际迁移数。
			continue
		}
		if err := insertMigrations(ctx, s.db, reasonBalance, []migrationRow{{
			userID:     mv.UserID,
			fromNodeID: mv.FromNodeID,
			toNodeID:   mv.ToNodeID,
		}}); err != nil {
			return moved, plan, err
		}
		moved++
	}
	return moved, plan, nil
}

// nodeStat 是均衡用到的单节点状态。
type nodeStat struct {
	id       int64
	capacity int64 // -1 表示 NULL（不限）
	bound    int64
	enabled  bool
	online   bool
}

// regionNodeStats 取该地区每个节点的容量与已绑用户数。
//
// 只取 capacity 非 NULL 的节点：capacity 为 NULL 表示不限，永远不会「超容量」，
// 既不需要往外搬，也不该被当成均衡目标（它不参与容量约束）。
func (s *Scheduler) regionNodeStats(ctx context.Context, regionID int64) ([]nodeStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.capacity, n.enabled, n.status, COUNT(un.user_id)
		FROM nodes n
		LEFT JOIN user_nodes un ON un.node_id = n.id
		WHERE n.region_id = ? AND n.capacity IS NOT NULL
		GROUP BY n.id, n.capacity, n.enabled, n.status
		ORDER BY n.id`, regionID)
	if err != nil {
		return nil, fmt.Errorf("查询地区节点负载失败: %w", err)
	}
	defer rows.Close()

	var out []nodeStat
	for rows.Next() {
		var (
			st       nodeStat
			capacity sql.NullInt64
			enabled  bool
			status   string
		)
		if err := rows.Scan(&st.id, &capacity, &enabled, &status, &st.bound); err != nil {
			return nil, fmt.Errorf("读取地区节点负载失败: %w", err)
		}
		st.capacity = -1
		if capacity.Valid {
			st.capacity = capacity.Int64
		}
		st.enabled = enabled
		st.online = status == "online"
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历地区节点负载失败: %w", err)
	}
	return out, nil
}

// planBalance 产出搬运计划（不写库）。limit>0 时计划最多 limit 条。
func (s *Scheduler) planBalance(ctx context.Context, stats []nodeStat, limit int) ([]Move, error) {
	// 目标：启用且在线、capacity 有值且未满的节点，按剩余容量降序（平局 id 升序）。
	targets := make([]nodeStat, 0, len(stats))
	for _, st := range stats {
		if !st.enabled || !st.online || st.capacity <= 0 || st.bound >= st.capacity {
			continue
		}
		targets = append(targets, st)
	}
	sort.Slice(targets, func(i, j int) bool {
		ri := targets[i].capacity - targets[i].bound
		rj := targets[j].capacity - targets[j].bound
		if ri != rj {
			return ri > rj
		}
		return targets[i].id < targets[j].id
	})

	// 源：真正超容量的节点，按 id 升序，保证计划顺序稳定。
	sources := make([]nodeStat, 0, len(stats))
	for _, st := range stats {
		if st.capacity < 0 || st.bound <= st.capacity {
			continue
		}
		sources = append(sources, st)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].id < sources[j].id })

	remaining := make([]int64, len(targets))
	for i, t := range targets {
		remaining[i] = t.capacity - t.bound
	}

	plan := make([]Move, 0, 16)
	ti := 0
	for _, src := range sources {
		users, err := s.nodeUserIDs(ctx, src.id, int(src.bound-src.capacity))
		if err != nil {
			return nil, err
		}
		for _, uid := range users {
			if ti >= len(targets) {
				return plan, nil // 目标容量已经排满，剩下的搬不动
			}
			plan = append(plan, Move{UserID: uid, FromNodeID: src.id, ToNodeID: targets[ti].id})
			remaining[ti]--
			if remaining[ti] <= 0 {
				ti++
			}
			if limit > 0 && len(plan) >= limit {
				return plan, nil
			}
		}
	}
	return plan, nil
}

// nodeUserIDs 取某节点上的前 n 个用户（按 user_id 升序，保证确定性）。
func (s *Scheduler) nodeUserIDs(ctx context.Context, nodeID int64, n int) ([]uint64, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT user_id FROM user_nodes WHERE node_id = ? ORDER BY user_id LIMIT ?", nodeID, n)
	if err != nil {
		return nil, fmt.Errorf("查询节点 %d 上的用户失败: %w", nodeID, err)
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取节点 %d 上的用户失败: %w", nodeID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历节点 %d 上的用户失败: %w", nodeID, err)
	}
	return ids, nil
}
