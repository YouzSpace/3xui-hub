package scheduler

import (
	"context"
	"fmt"
	"strings"
)

// user_node_migrations.reason 的两个取值（手动的 'manual' 由管理端接口自己写）。
const (
	reasonBalance  = "balance"
	reasonFailover = "failover"
)

// migrationRow 是一条待写入的迁移历史。
type migrationRow struct {
	userID     uint64
	fromNodeID int64
	toNodeID   int64
}

// moveUser 把一个用户的一条绑定从 fromNodeID 搬到 toNodeID，并同步 users.updated_at。
//
// 返回 false 表示这条绑定已经不在 fromNodeID 上（被并发搬走 / 用户被删），
// 调用方按「没有迁移」处理，不当成错误。
//
// 为什么要一起改 users.updated_at：节点侧靠「Max(updated_at) 指纹」判断自己
// 这份配置有没有变化，只改 user_nodes 不改 users 的话，迁移完节点不会重新拉配置，
// 用户在新节点上一直没被创建。
func moveUser(ctx context.Context, ex execer, userID uint64, fromNodeID, toNodeID int64) (bool, error) {
	res, err := ex.ExecContext(ctx,
		"UPDATE user_nodes SET node_id = ? WHERE user_id = ? AND node_id = ?",
		toNodeID, userID, fromNodeID)
	if err != nil {
		return false, fmt.Errorf("迁移用户 %d 的节点绑定失败: %w", userID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取迁移影响行数失败: %w", err)
	}
	if affected == 0 {
		return false, nil
	}
	if _, err := ex.ExecContext(ctx,
		"UPDATE users SET updated_at = NOW() WHERE id = ?", userID); err != nil {
		return false, fmt.Errorf("更新用户 %d 的 updated_at 失败: %w", userID, err)
	}
	return true, nil
}

// insertMigrations 一次多行写入迁移历史，避免几千次往返。
//
// 说明：user_node_migrations 没有「数量」列，所以「每批一条汇总」在这里实现为
// 「每批一条多行 INSERT，每个用户一行」—— 管理端的撤销要按用户反向 UPDATE，
// 只有按用户留痕才撤得回去。
func insertMigrations(ctx context.Context, ex execer, reason string, rows []migrationRow) error {
	if len(rows) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, operator, created_at) VALUES ")
	args := make([]any, 0, len(rows)*4)
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,NULL,NOW())")
		args = append(args, r.userID, r.fromNodeID, r.toNodeID, reason)
	}
	if _, err := ex.ExecContext(ctx, b.String(), args...); err != nil {
		return fmt.Errorf("写入迁移历史失败: %w", err)
	}
	return nil
}
