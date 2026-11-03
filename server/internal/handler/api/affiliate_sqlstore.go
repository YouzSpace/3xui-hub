package api

// 本文件是邀请返佣域的 database/sql 实现（手写 SQL，不用 ORM）。
//
// 表结构由 cmd/server/schema.go 的 ensureSchema 建好（affiliate_rebates），本文件不建表。
// 时间口径与全仓一致：DSN parseTime=true&loc=UTC，只写 UTC 字面量，**严禁 SET time_zone**。
//
// 幂等的唯一保障是 affiliate_rebates.order_id 上的唯一键：
//   - 网关回调重推 / 余额支付重复调用 → 第二次 INSERT 直接撞 1062，
//     事务回滚（不加余额、不写流水），对外表现为 granted=false。
//   - 不要用「先查再插」代替它：两个回调并发时查都是空，唯一键才挡得住。

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"
)

var _ AffiliateStore = (*PaymentSQLStore)(nil)

// InviterOf 取邀请人 id（没有邀请关系返回 0）。
func (s *PaymentSQLStore) InviterOf(ctx context.Context, userID uint64) (uint64, error) {
	var inviter sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT dc.user_id FROM users u
		 JOIN discount_codes dc ON dc.id = u.used_invite_code_id
		 WHERE u.id = ? AND dc.source = 'invite'
		 LIMIT 1`, userID).Scan(&inviter)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !inviter.Valid || inviter.Int64 <= 0 {
		return 0, nil
	}
	return uint64(inviter.Int64), nil
}

// GrantRebate 发一笔返佣：写返佣记录 → 邀请人余额加钱 → 写 balance_logs，一个事务。
//
// 锁顺序与余额域一致：先写 affiliate_rebates（失败即回滚）→ 锁 users 行（FOR UPDATE）→
// 改余额 → 写流水。只锁邀请人一行，不碰被邀请人的余额，不会与 PayOrderWithBalance 成环。
func (s *PaymentSQLStore) GrantRebate(ctx context.Context, in GrantRebateInput) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := in.Now.UTC()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO affiliate_rebates (inviter_id, invitee_id, order_id, order_no, amount, rate, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.InviterID, in.InviteeID, in.OrderID, in.OrderNo, in.AmountCents, in.Rate, now); err != nil {
		if isDuplicateKey(err) {
			// 同一笔订单已经返过（回调重推 / 重复完成）：不加钱、不写流水，事务回滚。
			return false, nil
		}
		return false, err
	}

	var current sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM users WHERE id = ? FOR UPDATE`, in.InviterID).Scan(&current); err != nil {
		return false, err
	}
	balanceAfter := current.Int64 + in.AmountCents
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET balance = ?, updated_at = ? WHERE id = ?`,
		balanceAfter, now, in.InviterID); err != nil {
		return false, err
	}

	note := "邀请返佣"
	if in.OrderNo != "" {
		note = "邀请返佣-" + in.OrderNo
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO balance_logs (user_id, amount, type, order_id, balance_after, note, created_at)
		 VALUES (?, ?, 'rebate', ?, ?, ?, ?)`,
		in.InviterID, in.AmountCents, in.OrderID, balanceAfter, note, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// RebateSummary 取累计返佣（分）与被邀请人列表（每人累计贡献）。
//
// 「被邀请人」= users.used_invite_code_id 指向的邀请码属于我（source='invite'），
// 从没返过佣的人也出现在列表里（rebate=0）—— 邀请人数统计的是「拉来的人」，不是「产生过返佣的人」。
//
// 刻意**不**过滤 users.deleted_at：列表里每人的贡献合计必须等于累计返佣（SUM 不区分删号），
// 而且全仓没有别处按 deleted_at 过滤用户，这里单独加一条只会让两个数字对不上。
func (s *PaymentSQLStore) RebateSummary(ctx context.Context, inviterID uint64) (int64, []AffiliateInvitee, error) {
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(amount) FROM affiliate_rebates WHERE inviter_id = ?`, inviterID).Scan(&total); err != nil {
		return 0, nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.email, u.created_at, COALESCE(SUM(r.amount), 0)
		 FROM users u
		 JOIN discount_codes dc
		   ON dc.id = u.used_invite_code_id AND dc.user_id = ? AND dc.source = 'invite'
		 LEFT JOIN affiliate_rebates r ON r.invitee_id = u.id AND r.inviter_id = ?
		 GROUP BY u.id, u.email, u.created_at
		 ORDER BY u.id DESC`, inviterID, inviterID)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	out := []AffiliateInvitee{}
	for rows.Next() {
		var it AffiliateInvitee
		if err := rows.Scan(&it.UserID, &it.Email, &it.InvitedAt, &it.Rebate); err != nil {
			return 0, nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return total.Int64, out, nil
}

// isDuplicateKey 判定 MySQL「唯一键冲突」（error 1062）。
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
