package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"

	"controlhub-server/internal/planregion"
)

// Assign 为该用户在「还没有绑定记录的每一个地区」里各分配一个节点。
//
// 单个地区失败（该地区没有可售节点等）不打断其他地区：继续处理后面的地区，
// 最后把所有错误汇总（errors.Join）返回 —— 调用方用 errors.Is(err, ErrNoCapacity)
// 判断是否只是「该地区暂无可售」。返回值只包含真正写入的绑定。
func (s *Scheduler) Assign(ctx context.Context, userID uint64) ([]Assignment, error) {
	regionIDs, err := s.unboundRegions(ctx, userID)
	if err != nil {
		return nil, err
	}
	assigned := make([]Assignment, 0, len(regionIDs))
	var errs []error
	for _, regionID := range regionIDs {
		nodeID, err := s.AssignRegion(ctx, userID, regionID)
		if err != nil {
			errs = append(errs, fmt.Errorf("地区 %d: %w", regionID, err))
			continue
		}
		assigned = append(assigned, Assignment{RegionID: regionID, NodeID: nodeID})
	}
	return assigned, errors.Join(errs...)
}

// AssignRegion 为单个地区分配一个节点（Assign 的内部实现，测试也直接调）。
//
// 顺序：先看该用户在这个地区是否已有绑定（有就直接返回，不重复分）→ 抢地区锁 →
// 在锁内重新算一遍候选（避免并发下两个请求挑到同一台已满节点）→ INSERT IGNORE 写入。
func (s *Scheduler) AssignRegion(ctx context.Context, userID uint64, regionID int64) (int64, error) {
	if nodeID, ok, err := s.boundNodeInRegion(ctx, userID, regionID); err != nil {
		return 0, err
	} else if ok {
		return nodeID, nil
	}

	unlock, err := s.lockRegion(ctx, regionID)
	if err != nil {
		return 0, err
	}
	defer unlock()

	candidates, err := s.candidates(ctx, regionID)
	if err != nil {
		return 0, err
	}
	nodeID, err := pickNode(candidates)
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT IGNORE INTO user_nodes (user_id, node_id) VALUES (?, ?)", userID, nodeID); err != nil {
		return 0, fmt.Errorf("写入用户节点绑定失败: %w", err)
	}
	return nodeID, nil
}

// boundNodeInRegion 查该用户在这个地区已经绑定的节点（没有则 ok=false）。
func (s *Scheduler) boundNodeInRegion(ctx context.Context, userID uint64, regionID int64) (int64, bool, error) {
	var nodeID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT un.node_id
		FROM user_nodes un
		JOIN nodes n ON n.id = un.node_id
		WHERE un.user_id = ? AND n.region_id = ?
		ORDER BY un.node_id
		LIMIT 1`, userID, regionID).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询用户地区绑定失败: %w", err)
	}
	return nodeID, true, nil
}

// PrunePlanRegions 删除该用户在「当前套餐不允许地区」里的节点绑定，返回删除行数（#地区功能-15）。
//
// 换套餐用：只删新套餐不允许地区的绑定，其余绑定原样保留（不做全删，避免无谓断线），
// 之后调 Assign 就会把「一条绑定都没有的地区」补齐。
//
// 口径与订阅过滤完全一致：套餐不限地区（region_ids 为 NULL）→ 一条都不删；
// n.region_id IS NULL 的老节点不受套餐地区限制，同样不删。
func (s *Scheduler) PrunePlanRegions(ctx context.Context, userID uint64) (int64, error) {
	allowed, restricted, err := planregion.Allowed(ctx, s.db, userID)
	if err != nil {
		return 0, err
	}
	if !restricted {
		return 0, nil
	}
	list, listArgs := planregion.In(allowed)
	args := append([]any{userID}, listArgs...)
	res, err := s.db.ExecContext(ctx, `
		DELETE un
		FROM user_nodes un
		JOIN nodes n ON n.id = un.node_id
		WHERE un.user_id = ? AND n.region_id IS NOT NULL AND n.region_id NOT IN `+list, args...)
	if err != nil {
		return 0, fmt.Errorf("解除套餐不允许地区的绑定失败: %w", err)
	}
	return res.RowsAffected()
}

// unboundRegions 返回该用户「一条绑定记录都没有」的地区，按 regions.sort、id 排序。
//
// 套餐地区过滤（#地区功能-15）：套餐绑了地区就只在这些地区里分配；
// 套餐不限地区（region_ids 为 NULL，老套餐）时 SQL 与升级前完全一致。
func (s *Scheduler) unboundRegions(ctx context.Context, userID uint64) ([]int64, error) {
	allowed, restricted, err := planregion.Allowed(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}

	query := `
		SELECT r.id
		FROM regions r
		WHERE r.id NOT IN (
			SELECT n.region_id
			FROM user_nodes un
			JOIN nodes n ON n.id = un.node_id
			WHERE un.user_id = ? AND n.region_id IS NOT NULL
		)`
	args := []any{userID}
	if restricted {
		clause, extra := planregion.RegionFilter(allowed)
		query += clause
		args = append(args, extra...)
	}
	query += `
		ORDER BY r.sort, r.id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询未绑定的地区失败: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取未绑定地区失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历未绑定地区失败: %w", err)
	}
	return ids, nil
}

// candidate 是一个候选节点及其当前负载。
type candidate struct {
	id       int64
	capacity int64 // -1 表示 NULL（不限）
	bound    int64 // 已绑用户数（user_nodes 行数）
}

// full 判断节点是否已经满容量（capacity 为 NULL 时永不满足）。
func (c candidate) full() bool {
	return c.capacity >= 0 && c.bound >= c.capacity
}

// ratio 是排序用的负载率「已绑 / capacity」；capacity 为 NULL 视为无限，排最后。
func (c candidate) ratio() float64 {
	if c.capacity <= 0 {
		return math.Inf(1)
	}
	return float64(c.bound) / float64(c.capacity)
}

// candidates 取该地区的候选节点：enabled=1 且 status='online'（离线节点不参与分配），
// 已绑用户数用 user_nodes 行数统计。
func (s *Scheduler) candidates(ctx context.Context, regionID int64) ([]candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.capacity, COUNT(un.user_id)
		FROM nodes n
		LEFT JOIN user_nodes un ON un.node_id = n.id
		WHERE n.enabled = 1 AND n.status = 'online' AND n.region_id = ?
		GROUP BY n.id, n.capacity
		ORDER BY n.id`, regionID)
	if err != nil {
		return nil, fmt.Errorf("查询地区候选节点失败: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var (
			c        candidate
			capacity sql.NullInt64
		)
		if err := rows.Scan(&c.id, &capacity, &c.bound); err != nil {
			return nil, fmt.Errorf("读取候选节点失败: %w", err)
		}
		c.capacity = -1
		if capacity.Valid {
			c.capacity = capacity.Int64
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历候选节点失败: %w", err)
	}
	return out, nil
}

// pickNode 选节点：满载的排除；剩下按「已绑 / capacity」升序（capacity 为 NULL 最后），
// 平局取 id 最小的，保证同一份数据永远选出同一台，方便测试与排查。
func pickNode(cands []candidate) (int64, error) {
	usable := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if c.full() {
			continue
		}
		usable = append(usable, c)
	}
	if len(usable) == 0 {
		return 0, ErrNoCapacity
	}
	sort.Slice(usable, func(i, j int) bool {
		ri, rj := usable[i].ratio(), usable[j].ratio()
		if ri != rj {
			return ri < rj
		}
		return usable[i].id < usable[j].id
	})
	return usable[0].id, nil
}
