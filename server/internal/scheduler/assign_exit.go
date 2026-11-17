package scheduler

// 静态 IP 出口分类的前置节点分配（#静态IP-02）。
//
// 与地区分配的区别（刻意分开写，不硬塞进 AssignRegion）：
//   - 候选来源是 exit_profile_nodes（分类 ↔ 前置节点），不是 nodes.region_id；
//   - 不要求套餐地区、不参与 regions.sort；
//   - 同一用户在同一分类里只绑一个前置节点（已经绑了就直接沿用，续费不换）。
//
// 排序口径与地区分配完全一致（pickNode）：排除满载，按「已绑 / capacity」升序，
// capacity 为 NULL 排最后，平局取 id 最小 —— 同一份数据永远选出同一台。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoExitNode 表示该出口分类当前没有可分配的前置节点
// （分类没绑节点 / 节点全部离线、禁用或满容量）。
var ErrNoExitNode = errors.New("scheduler: 该出口分类没有可用的前置节点")

// ExitProfileLockKeyFmt 是分类分配的锁 key 格式（与地区锁同一把 Redis 锁实现）。
const exitProfileLockKeyFmt = "ch:lock:assign:exit-profile:%d"

// AssignProfileNode 为该用户在某个出口分类里分配一个前置节点。
//
// 语义与 AssignRegion 一致：先看用户在这个分类里是否已有绑定（有就返回，续费不换节点）→
// 抢分类锁 → 锁内重新算候选 → INSERT IGNORE 写入。
func (s *Scheduler) AssignProfileNode(ctx context.Context, userID uint64, profileID uint64) (int64, error) {
	if nodeID, ok, err := s.boundNodeInProfile(ctx, userID, profileID); err != nil {
		return 0, err
	} else if ok {
		return nodeID, nil
	}

	unlock, err := s.lockKey(ctx, fmt.Sprintf(exitProfileLockKeyFmt, profileID))
	if err != nil {
		return 0, err
	}
	defer unlock()

	candidates, err := s.profileCandidates(ctx, profileID)
	if err != nil {
		return 0, err
	}
	nodeID, err := pickNode(candidates)
	if err != nil {
		if errors.Is(err, ErrNoCapacity) {
			return 0, ErrNoExitNode
		}
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT IGNORE INTO user_nodes (user_id, node_id) VALUES (?, ?)", userID, nodeID); err != nil {
		return 0, fmt.Errorf("写入用户节点绑定失败: %w", err)
	}
	return nodeID, nil
}

// boundNodeInProfile 查该用户在这个分类的前置节点里已经绑定的那一台（没有则 ok=false）。
func (s *Scheduler) boundNodeInProfile(ctx context.Context, userID uint64, profileID uint64) (int64, bool, error) {
	var nodeID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT un.node_id
		FROM user_nodes un
		JOIN exit_profile_nodes epn ON epn.node_id = un.node_id
		WHERE un.user_id = ? AND epn.profile_id = ?
		ORDER BY un.node_id
		LIMIT 1`, userID, profileID).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询用户出口分类绑定失败: %w", err)
	}
	return nodeID, true, nil
}

// profileCandidates 取该分类的前置节点候选：enabled=1 且 status='online'。
// 已绑用户数同样按 user_nodes 行数统计（与地区分配同一口径，便于对比排查）。
func (s *Scheduler) profileCandidates(ctx context.Context, profileID uint64) ([]candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.capacity, COUNT(un.user_id)
		FROM exit_profile_nodes epn
		JOIN nodes n ON n.id = epn.node_id
		LEFT JOIN user_nodes un ON un.node_id = n.id
		WHERE epn.profile_id = ? AND n.enabled = 1 AND n.status = 'online'
		GROUP BY n.id, n.capacity
		ORDER BY n.id`, profileID)
	if err != nil {
		return nil, fmt.Errorf("查询出口分类候选节点失败: %w", err)
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

// PruneNonProfileNodes 删除该用户「不属于这个分类」的节点绑定，返回删除行数。
//
// 购买 static_chain 套餐后调用：链式出口的入口只能是分类里的前置节点，
// 用户从直连套餐换过来时，旧的地区绑定必须清掉，否则订阅里仍会下发直连节点
// （nodeapi 的 eligibleUsers 按 user_nodes 过滤，旧绑定的节点会把该用户带上）。
//
// 调用时机必须排在「分类节点分配成功」之后：分类没有可用节点时先删光绑定，
// 用户就彻底连不上了。
func (s *Scheduler) PruneNonProfileNodes(ctx context.Context, userID uint64, profileID uint64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE un FROM user_nodes un
		WHERE un.user_id = ?
		  AND NOT EXISTS (
			SELECT 1 FROM exit_profile_nodes epn
			WHERE epn.profile_id = ? AND epn.node_id = un.node_id
		  )`, userID, profileID)
	if err != nil {
		return 0, fmt.Errorf("解除非分类前置节点绑定失败: %w", err)
	}
	return res.RowsAffected()
}
