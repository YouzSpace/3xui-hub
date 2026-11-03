package api

// 本文件是余额域的 database/sql 实现（手写 SQL，不用 ORM）。
//
// 口径与 payment_sqlstore.go 一致：时间一律 UTC 字面量（DSN parseTime=true&loc=UTC，
// **严禁 SET time_zone**）；金额一律整数分，不经 float64；布尔走 tinyint。
//
// 三个写方法各自是一个完整事务，锁顺序固定为「先锁 users/giftcards 行，再动 orders/balance_logs」，
// 与支付域 ConsumeDiscountCode 的加锁方式同源（SELECT ... FOR UPDATE）。
// 表结构由 cmd/server/schema.go 的 ensureSchema 建好（users.balance / giftcards / balance_logs），
// 本文件不建表、不改表。

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const balanceLogColumns = `id, user_id, amount, type, order_id, giftcard_id, operator_id,
	balance_after, note, created_at`

// UserBalance 读当前余额（分）。
func (s *PaymentSQLStore) UserBalance(ctx context.Context, userID uint64) (int64, error) {
	return userBalanceIn(ctx, s.db, userID)
}

// userBalanceIn 是「在一个 *sql.DB / *sql.Tx 上读余额」的共用实现（balance 列可空时按 0 处理）。
func userBalanceIn(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID uint64) (int64, error) {
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT balance FROM users WHERE id = ?`, userID).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// BalanceLogs 取该用户最近 limit 条流水（offset 翻页）。
func (s *PaymentSQLStore) BalanceLogs(ctx context.Context, userID uint64, limit, offset int) ([]BalanceLog, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+balanceLogColumns+` FROM balance_logs
		 WHERE user_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []BalanceLog{}
	for rows.Next() {
		var l BalanceLog
		if err := rows.Scan(&l.ID, &l.UserID, &l.Amount, &l.Type, &l.OrderID, &l.GiftcardID,
			&l.OperatorID, &l.BalanceAfter, &l.Note, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// RedeemGiftcard 兑换（规划 §3.1.2 的实现要点）：
// 锁码行 → 校验 status/expires_at → 置 used → 余额累加 → 写流水，全程一个事务。
func (s *PaymentSQLStore) RedeemGiftcard(ctx context.Context, in RedeemGiftcardInput) (*RedeemGiftcardResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		giftcardID int64
		value      int64
		status     string
		expiresAt  sql.NullTime
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, value, status, expires_at FROM giftcards WHERE code = ? FOR UPDATE`,
		in.Code).Scan(&giftcardID, &value, &status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGiftcardNotFound
	}
	if err != nil {
		return nil, err
	}

	// 状态在过期之前判：「已使用」比「已过期」对用户更有信息量。
	switch status {
	case "active":
	case "used":
		return nil, ErrGiftcardUsed
	case "disabled":
		return nil, ErrGiftcardDisabled
	default:
		return nil, ErrGiftcardNotFound
	}
	if expiresAt.Valid && expiresAt.Time.Before(in.Now) {
		return nil, ErrGiftcardExpired
	}

	now := in.Now.UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE giftcards SET status = 'used', used_by = ?, used_at = ?, updated_at = ? WHERE id = ?`,
		in.UserID, now, now, giftcardID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET balance = COALESCE(balance, 0) + ?, updated_at = ? WHERE id = ?`,
		value, now, in.UserID); err != nil {
		return nil, err
	}
	balance, err := userBalanceIn(ctx, tx, in.UserID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO balance_logs (user_id, amount, type, giftcard_id, balance_after, note, created_at)
		 VALUES (?, ?, 'redeem', ?, ?, ?, ?)`,
		in.UserID, value, giftcardID, balance, "兑换码", now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RedeemGiftcardResult{GiftcardID: giftcardID, Added: value, Balance: balance}, nil
}

// PayOrderWithBalance 用余额支付套餐（规划 §3.1.3）：锁用户行 → 复核余额 → 扣款 → 写流水 →
// 建一笔 status=paid 的订单（payment_driver_type='balance' 标记支付方式，不新增 order_type）。
//
// 余额校验做两遍：Go 里比一次（余额不足直接拒，文案干净），SQL 再用
// `WHERE balance >= ?` 的条件更新兜底（并发下不会扣成负数）。
func (s *PaymentSQLStore) PayOrderWithBalance(ctx context.Context, in BalancePayInput) (*Order, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var locked sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM users WHERE id = ? FOR UPDATE`, in.UserID).Scan(&locked); err != nil {
		return nil, err
	}
	if err := checkBalance(locked.Int64, in.AmountCents); err != nil {
		return nil, err
	}

	now := in.Now.UTC()
	order, err := insertBalancePaidOrder(ctx, tx, in, now)
	if err != nil {
		return nil, err
	}

	if in.AmountCents > 0 {
		res, uerr := tx.ExecContext(ctx,
			`UPDATE users SET balance = COALESCE(balance, 0) - ?, updated_at = ?
			 WHERE id = ? AND COALESCE(balance, 0) >= ?`,
			in.AmountCents, now, in.UserID, in.AmountCents)
		if uerr != nil {
			return nil, uerr
		}
		affected, aerr := res.RowsAffected()
		if aerr != nil {
			return nil, aerr
		}
		if affected == 0 {
			return nil, ErrBalanceInsufficient
		}
		balanceAfter, berr := userBalanceIn(ctx, tx, in.UserID)
		if berr != nil {
			return nil, berr
		}
		if _, lerr := tx.ExecContext(ctx,
			`INSERT INTO balance_logs (user_id, amount, type, order_id, balance_after, note, created_at)
			 VALUES (?, ?, 'consume', ?, ?, ?, ?)`,
			in.UserID, -in.AmountCents, order.ID, balanceAfter, in.Note, now); lerr != nil {
			return nil, lerr
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return order, nil
}

// insertBalancePaidOrder 建一笔「已支付」的套餐订单（余额支付专用）：直接 paid，
// 不走第三方网关，因此 trade_no/payment_config_id 均为 NULL。
func insertBalancePaidOrder(ctx context.Context, tx *sql.Tx, in BalancePayInput, now time.Time) (*Order, error) {
	amount := money{cents: in.AmountCents}
	original := money{cents: in.OriginalCents}
	var discount any
	if in.DiscountCents > 0 {
		discount = money{cents: in.DiscountCents}.String()
	}
	var payIP any
	if in.PayIP != "" {
		payIP = in.PayIP
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO orders (order_no, user_id, order_type, plan_id, amount, original_amount,
		                     discount_code_id, discount_amount, status, payment_config_id,
		                     payment_driver_type, trade_no, paid_at, pay_ip, created_at, updated_at)
		 VALUES (?, ?, 'plan', ?, ?, ?, ?, ?, 'paid', NULL, 'balance', NULL, ?, ?, ?, ?)`,
		in.OrderNo, in.UserID, in.PlanID, amount.String(), original.String(),
		in.DiscountCodeID, discount, now, payIP, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Order{
		ID:             uint64(id),
		OrderNo:        in.OrderNo,
		UserID:         in.UserID,
		OrderType:      "plan",
		PlanID:         nullInt64(int64(in.PlanID)),
		Amount:         amount.String(),
		OriginalAmount: nullStr(original.String()),
		DiscountCodeID: in.DiscountCodeID,
		DiscountAmount: nullStrMaybe(discountString(in.DiscountCents)),
		Status:         "paid",
		PaidAt:         sql.NullTime{Time: now, Valid: true},
		PayIP:          sql.NullString{String: in.PayIP, Valid: in.PayIP != ""},
		CreatedAt:      now,
	}, nil
}

// discountString 把优惠金额（分）转成 decimal 文本；0 表示没有优惠。
func discountString(cents int64) string {
	if cents <= 0 {
		return ""
	}
	return money{cents: cents}.String()
}

// CreditRechargeOrder 充值订单到账（规划 §3.1.4 第 3 步）：
// 锁订单行 → 幂等判定（已 paid 直接返回）→ 置 paid → 余额累加 → 写流水，一个事务。
func (s *PaymentSQLStore) CreditRechargeOrder(ctx context.Context, in CreditRechargeInput) (bool, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = ? FOR UPDATE`, in.OrderID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, sql.ErrNoRows
	}
	if err != nil {
		return false, 0, err
	}
	balance, err := userBalanceIn(ctx, tx, in.UserID)
	if err != nil {
		return false, 0, err
	}
	if status == "paid" {
		// 已完成：不重复加钱（回调重推/查单重复完成的幂等闸门）。
		return false, balance, nil
	}

	now := in.Now.UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE orders SET status = 'paid', paid_at = ?, updated_at = ? WHERE id = ?`,
		now, now, in.OrderID); err != nil {
		return false, 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET balance = COALESCE(balance, 0) + ?, updated_at = ? WHERE id = ?`,
		in.AmountCents, now, in.UserID); err != nil {
		return false, 0, err
	}
	balance, err = userBalanceIn(ctx, tx, in.UserID)
	if err != nil {
		return false, 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO balance_logs (user_id, amount, type, order_id, balance_after, note, created_at)
		 VALUES (?, ?, 'recharge', ?, ?, ?, ?)`,
		in.UserID, in.AmountCents, in.OrderID, balance, in.Note, now); err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return true, balance, nil
}

var _ BalanceStore = (*PaymentSQLStore)(nil)
