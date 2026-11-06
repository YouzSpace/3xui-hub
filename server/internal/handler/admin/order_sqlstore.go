package admin

// OrderStore 的 database/sql 实现（订单域）。
//
// search 全表查（订单号 / 交易号 / 用户 ID、邮箱 / 套餐名），用 EXISTS 子查询
// 对齐 Laravel 的 whereHas，避免 N+1。

import (
	"context"
	"database/sql"
	"strings"
)

// NewOrderSQLStore 用已有连接池构造订单域的 OrderStore。
func NewOrderSQLStore(db *sql.DB) OrderStore {
	return &orderSQLStore{db: db}
}

type orderSQLStore struct {
	db *sql.DB
}

// orderSearchWhere 对齐 OrderController::index 的搜索分支。
func orderSearchWhere(f OrderFilter) (string, []any) {
	where := ""
	args := []any{}
	if f.OrderType != "" {
		where += " AND o.order_type = ?"
		args = append(args, f.OrderType)
	}
	search := strings.TrimSpace(f.Search)
	if search == "" {
		return where, args
	}
	like := "%" + search + "%"
	// 或组：order_no / trade_no / whereHas(user) / whereHas(plan)
	group := ` AND (o.order_no LIKE ? OR o.trade_no LIKE ?`
	args = append(args, like, like)

	// whereHas('user', ...)：数字 → id 精确 OR email 模糊；否则纯 email 模糊。
	if isDigits(search) {
		group += ` OR EXISTS (SELECT 1 FROM users u WHERE u.id = o.user_id AND (u.id = ? OR u.email LIKE ?))`
		args = append(args, search, like)
	} else {
		group += ` OR EXISTS (SELECT 1 FROM users u WHERE u.id = o.user_id AND u.email LIKE ?)`
		args = append(args, like)
	}
	group += ` OR EXISTS (SELECT 1 FROM plans p WHERE p.id = o.plan_id AND p.name LIKE ?)`
	args = append(args, like)
	group += `)`
	return where + group, args
}

func (s *orderSQLStore) OrderPage(ctx context.Context, f OrderFilter, page, perPage int64) ([]OrderRow, int64, error) {
	where, args := orderSearchWhere(f)

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM orders o WHERE 1=1`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if perPage < 1 {
		perPage = 50
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	q := `SELECT o.id, o.order_no, o.user_id, u.email, p.name, s.name,
			o.order_type, o.amount, o.status, pc.name, o.trade_no, o.paid_at, o.created_at,
			o.payment_driver_type
		FROM orders o
		LEFT JOIN users u ON u.id = o.user_id
		LEFT JOIN plans p ON p.id = o.plan_id
		LEFT JOIN proxy_skus s ON s.id = o.proxy_sku_id
		LEFT JOIN payment_configs pc ON pc.id = o.payment_config_id
		WHERE 1=1` + where + `
		ORDER BY o.id DESC
		LIMIT ? OFFSET ?`
	qArgs := append(append([]any{}, args...), perPage, offset)

	rows, err := s.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []OrderRow{}
	for rows.Next() {
		var (
			o         OrderRow
			userEmail sql.NullString
			planName  sql.NullString
			skuName   sql.NullString
			orderType sql.NullString
			amount    sql.NullString
			payment   sql.NullString
			tradeNo   sql.NullString
			paidAt    sql.NullTime
			createdAt sql.NullTime
			// paymentDriver 区分余额支付单（'balance'）：它们没有 payment_config，
			// 不读这个字段的话支付方式列只能显示「—」。
			paymentDriver sql.NullString
		)
		if err := rows.Scan(&o.ID, &o.OrderNo, &o.UserID, &userEmail, &planName, &skuName,
			&orderType, &amount, &o.Status, &payment, &tradeNo, &paidAt, &createdAt, &paymentDriver); err != nil {
			return nil, 0, err
		}
		o.UserEmail = nullStrPtr(userEmail)
		o.PlanName = nullStrPtr(planName)
		o.ProxySKUName = nullStrPtr(skuName)
		o.OrderType = nullStrPtr(orderType)
		if amount.Valid {
			o.Amount = amount.String
		} else {
			o.Amount = "0.00"
		}
		o.PaymentName = nullStrPtr(payment)
		o.PaymentDriver = nullStrPtr(paymentDriver)
		o.TradeNo = nullStrPtr(tradeNo)
		o.PaidAt = nullTimePtr(paidAt)
		o.CreatedAt = nullTimePtr(createdAt)
		out = append(out, o)
	}
	return out, total, rows.Err()
}
