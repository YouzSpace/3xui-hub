// Package sockpool 是「独占 socks 池」的公共口径（#地区功能-17）。
//
// 契约：
//   - proxy_socks.allocation='exclusive' 的一条线路同一时刻只能挂给一个用户：
//     分配时写 sold_to（用户 id）+ sold_at（售出时间），sold_to IS NULL 才在池子里；
//   - 分配必须在一个事务里 SELECT ... FOR UPDATE + UPDATE —— 否则两笔并发订单
//     各自查到同一条未售出的线路，会卖给两个人；
//   - 续费沿用原来那条（用户不该因为续费被换线路），但沿用前要确认那条还归他：
//     权益到期后线路会被释放回池子，可能已经被别人买走了；
//   - 释放（权益到期 / 作废）只清 sold_to / sold_at，不动 is_active：
//     管理员停用一条线路是「这条先别卖」，不等于把已售记录抹掉；
//   - 'shared' 的线路（多人共用）完全不经过这里，老行为不变。
//
// 之所以单独成包：分配发生在两处 —— 支付回调开通（internal/handler/api）与
// 管理端补开（internal/handler/admin）。SQL 各写一遍迟早走样，尤其是
// 「sold_to IS NULL 才算在池子里」和「指定了 source_id 就只在那一条里挑」这两条。
package sockpool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 两种分配方式（proxy_socks.allocation / proxy_skus.allocation 同口径）。
const (
	Shared    = "shared"
	Exclusive = "exclusive"
)

// ErrPoolEmpty 是独占池里没有可售线路时的错误（下单侧据此报「无货」）。
var ErrPoolEmpty = errors.New("独占 socks 池已空")

// IsExclusive 判断规格/线路是不是独占；NULL（老数据）按共享处理。
func IsExclusive(allocation *string) bool {
	return allocation != nil && *allocation == Exclusive
}

// Acquire 给用户分配一条独占线路，返回线路 id。
//
//   - renewSockID 非空且那条仍归该用户 → 直接沿用（续费不重新分配）；
//   - 否则从池子里挑一条 sold_to IS NULL 的：sourceID 非空时只在那一条里挑
//     （等价于池子里只有一条），为空则按 id 升序取第一条；
//   - 池子空 → ErrPoolEmpty。
//
// 挑 + 标记在一个事务里完成：SELECT ... FOR UPDATE 锁住候选行，UPDATE 之后再提交 ——
// 并发订单排队拿不同的行，而不是拿同一条。
func Acquire(ctx context.Context, db *sql.DB, renewSockID, sourceID *uint64, userID uint64, now time.Time) (uint64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("独占 socks 分配开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	reused, err := reuseOwned(ctx, tx, renewSockID, userID)
	if err != nil {
		return 0, err
	}
	if reused {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("独占 socks 续用提交失败: %w", err)
		}
		return *renewSockID, nil
	}

	query := `SELECT id FROM proxy_socks
		WHERE allocation = 'exclusive' AND is_active = 1 AND sold_to IS NULL`
	args := []any{}
	if sourceID != nil {
		query += " AND id = ?"
		args = append(args, *sourceID)
	}
	// FOR UPDATE 是正确性的关键：不加就是把同一条线路卖给两个并发订单
	// （两边都读到 sold_to IS NULL，各自 UPDATE 覆盖对方的 sold_to）。
	query += " ORDER BY id LIMIT 1 FOR UPDATE"

	var id uint64
	switch err := tx.QueryRowContext(ctx, query, args...).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrPoolEmpty
	case err != nil:
		return 0, fmt.Errorf("挑选独占 socks 失败: %w", err)
	}

	now = now.UTC().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx,
		`UPDATE proxy_socks SET sold_to = ?, sold_at = ?, updated_at = ? WHERE id = ?`,
		userID, now, now, id); err != nil {
		return 0, fmt.Errorf("标记独占 socks 已售失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("独占 socks 分配提交失败: %w", err)
	}
	return id, nil
}

// reuseOwned 判断 renewSockID 那条线路现在是否还归该用户（续费沿用）。
//
// 必须真查库：权益到期后 internal/task 会清掉 sold_to，这条可能已经被下一个人
// 买走了，此时不能沿用，得重新分配 —— 否则一条线路会挂在两个用户身上。
func reuseOwned(ctx context.Context, tx *sql.Tx, renewSockID *uint64, userID uint64) (bool, error) {
	if renewSockID == nil {
		return false, nil
	}
	var owner sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT sold_to FROM proxy_socks WHERE id = ? AND allocation = 'exclusive' FOR UPDATE`,
		*renewSockID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("查询续费线路归属失败: %w", err)
	}
	return owner.Valid && uint64(owner.Int64) == userID, nil
}

// Release 把一条独占线路放回池子（权益到期 / 作废时调用）。
//
// 只对 allocation='exclusive' 且确实售出过的行生效：共享线路、池中未售出的线路
// 传进来都是空操作，调用方不用先判断类型。is_active 一律不动 —— 停用是管理员
// 的选择，与「卖没卖出去」是两件事。
func Release(ctx context.Context, db *sql.DB, socksID uint64, now time.Time) error {
	_, err := db.ExecContext(ctx,
		`UPDATE proxy_socks SET sold_to = NULL, sold_at = NULL, updated_at = ?
		 WHERE id = ? AND allocation = 'exclusive' AND sold_to IS NOT NULL`,
		now.UTC().Truncate(time.Second), socksID)
	if err != nil {
		return fmt.Errorf("释放独占 socks 失败: %w", err)
	}
	return nil
}
