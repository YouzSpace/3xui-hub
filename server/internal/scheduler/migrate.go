package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 迁移分批：每批 1000 条、批间 sleep 10ms，避免一次搬几万人把库压住。
const (
	migrateBatchSize     = 1000
	migrateBatchInterval = 10 * time.Millisecond
)

// Migrate 把 fromNodeID 上的一批用户迁到 toNodeID（节点故障时的迁移用）。
//
// count<=0 表示全部迁移。分批执行：每批 1000 条、一个事务，批间 sleep 10ms；
// 每批结束写迁移历史（reason='failover'）。每条迁移都同步
// UPDATE users.updated_at —— 节点侧靠 MAX(updated_at) 判断配置是否变化，
// 不同步更新的话节点不会重拉配置，用户在新节点上永远不会被创建。
//
// 返回实际迁移的用户数（目标绑定已经存在、绑定被并发搬走等情况不计入）。
func (s *Scheduler) Migrate(ctx context.Context, fromNodeID, toNodeID int64, count int) (int, error) {
	if fromNodeID == toNodeID {
		return 0, fmt.Errorf("scheduler: 源节点与目标节点相同（%d）", fromNodeID)
	}
	if err := s.ensureNodesExist(ctx, fromNodeID, toNodeID); err != nil {
		return 0, err
	}

	moved := 0
	var lastUserID uint64
	for {
		want := migrateBatchSize
		if count > 0 && count-moved < want {
			want = count - moved
		}
		if want <= 0 {
			break
		}
		userIDs, err := s.nodeUserIDsAfter(ctx, fromNodeID, lastUserID, want)
		if err != nil {
			return moved, err
		}
		if len(userIDs) == 0 {
			break // 源节点已经搬空
		}
		n, err := s.migrateBatch(ctx, userIDs, fromNodeID, toNodeID)
		if err != nil {
			return moved, err
		}
		moved += n
		lastUserID = userIDs[len(userIDs)-1]
		if s.onMigrateBatch != nil {
			s.onMigrateBatch(n)
		}
		if len(userIDs) < want {
			break // 这一批没取满，说明后面没有用户了
		}
		if count > 0 && moved >= count {
			break
		}
		select {
		case <-ctx.Done():
			return moved, ctx.Err()
		case <-time.After(migrateBatchInterval):
		}
	}
	return moved, nil
}

// migrateBatch 用一个事务搬一批用户（并写入这一批的迁移历史），整批成功或整批回滚。
func (s *Scheduler) migrateBatch(ctx context.Context, userIDs []uint64, fromNodeID, toNodeID int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows := make([]migrationRow, 0, len(userIDs))
	for _, uid := range userIDs {
		ok, err := moveUser(ctx, tx, uid, fromNodeID, toNodeID)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		rows = append(rows, migrationRow{userID: uid, fromNodeID: fromNodeID, toNodeID: toNodeID})
	}
	if err := insertMigrations(ctx, tx, reasonFailover, rows); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return len(rows), nil
}

// nodeUserIDsAfter 按 user_id 游标往后取某节点上的用户（keyset 分页）：
// 边搬边取不会漏也不会重复，因为游标只往前走。
func (s *Scheduler) nodeUserIDsAfter(ctx context.Context, nodeID int64, afterUserID uint64, n int) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id
		FROM user_nodes
		WHERE node_id = ? AND user_id > ?
		ORDER BY user_id
		LIMIT ?`, nodeID, afterUserID, n)
	if err != nil {
		return nil, fmt.Errorf("查询节点 %d 上的用户失败: %w", nodeID, err)
	}
	defer rows.Close()

	ids := make([]uint64, 0, n)
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

// ensureNodesExist 确认源 / 目标节点都存在（user_nodes 没有外键，写错 id 会留下脏绑定）。
func (s *Scheduler) ensureNodesExist(ctx context.Context, nodeIDs ...int64) error {
	for _, id := range nodeIDs {
		var got int64
		err := s.db.QueryRowContext(ctx, "SELECT id FROM nodes WHERE id = ?", id).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("scheduler: 节点 %d 不存在", id)
		}
		if err != nil {
			return fmt.Errorf("查询节点 %d 失败: %w", id, err)
		}
	}
	return nil
}
