package admin

// StatsStore 的 database/sql 实现（统计域）。
//
// 只统计 status='paid'，金额取 orders.amount（实付，已扣优惠码）。
//
// 排除余额支付（payment_driver_type='balance'）：余额支付的订单钱在充值时
// 已经进过账（充值订单本身也是一笔 paid 订单），这里再算一次就是同一笔钱
// 重复计入成交额。成交额口径 = 外部渠道（支付宝/微信等）真实进账，
// 充值订单算进账、余额支付不算。

import (
	"context"
	"database/sql"
	"time"
)

// paidExternalWhere 是「已支付且非余额支付」的公共过滤条件。
const paidExternalWhere = `status = 'paid' AND (payment_driver_type IS NULL OR payment_driver_type <> 'balance')`

// NewStatsSQLStore 用已有连接池构造统计域的 StatsStore。
func NewStatsSQLStore(db *sql.DB) StatsStore {
	return &statsSQLStore{db: db}
}

type statsSQLStore struct {
	db *sql.DB
}

func (s *statsSQLStore) RevenueToday(ctx context.Context, start *time.Time) (string, int64, error) {
	q := `SELECT COALESCE(SUM(amount), 0), COUNT(*) FROM orders WHERE ` + paidExternalWhere + ` AND paid_at >= ?`
	var (
		amount sql.NullString
		orders int64
	)
	if err := s.db.QueryRowContext(ctx, q, nullTimeValue(start)).Scan(&amount, &orders); err != nil {
		return "", 0, err
	}
	if !amount.Valid {
		return "0.00", orders, nil
	}
	return amount.String, orders, nil
}

func (s *statsSQLStore) RevenueTotal(ctx context.Context) (string, int64, error) {
	q := `SELECT COALESCE(SUM(amount), 0), COUNT(*) FROM orders WHERE ` + paidExternalWhere
	var (
		amount sql.NullString
		orders int64
	)
	if err := s.db.QueryRowContext(ctx, q).Scan(&amount, &orders); err != nil {
		return "", 0, err
	}
	if !amount.Valid {
		return "0.00", orders, nil
	}
	return amount.String, orders, nil
}
