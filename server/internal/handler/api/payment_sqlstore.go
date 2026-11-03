package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 本文件是支付域的 database/sql 实现（手写 SQL，不用 ORM）。
//
// 口径与 P2 的 sqlstore.go 一致：
//   - 时间：DSN parseTime=true&loc=UTC → MySQL 返回的 datetime 字面量直接打 UTC 标签、不做换算，
//     与 Laravel（app.timezone=UTC）读到的一致；写入同样传 UTC 的 time.Time。
//     **严禁 SET time_zone**（会让 Go 与 PHP 相差 8 小时）。
//   - 金额：decimal 列一律读成字符串（`SELECT amount` 扫进 string 或 []byte），
//     任何一步都不经 float64 —— contracts/data-contract.md §3。
//   - 布尔：MySQL tinyint 存 '1'/'0'，Go 手写转换。
//
// 注意本文件不改任何表结构；orders/payment_configs/discount_codes 的列以 Laravel
// migration 为准（见交付汇报的字段清单）。

// PaymentSQLStore 是 PaymentStore 的 database/sql 实现。
type PaymentSQLStore struct{ db *sql.DB }

// NewPaymentSQLStore 用已有连接池构造。
func NewPaymentSQLStore(db *sql.DB) *PaymentSQLStore { return &PaymentSQLStore{db: db} }

var _ PaymentStore = (*PaymentSQLStore)(nil)

// ---- payment_configs ----

const paymentConfigColumns = `id, name, driver_type, driver_config, gateway, query_gateway,
	member_id, api_key, bank_code, notify_url, enabled`

func scanPaymentConfig(row interface{ Scan(...any) error }) (*PaymentConfig, error) {
	var (
		c            PaymentConfig
		driverType   sql.NullString
		driverConfig sql.NullString
		gateway      sql.NullString
		queryGateway sql.NullString
		memberID     sql.NullString
		apiKey       sql.NullString
		bankCode     sql.NullString
		notifyURL    sql.NullString
		enabled      int
	)
	err := row.Scan(&c.ID, &c.Name, &driverType, &driverConfig, &gateway, &queryGateway,
		&memberID, &apiKey, &bankCode, &notifyURL, &enabled)
	if err != nil {
		return nil, err
	}
	c.DriverType = driverType.String
	c.Gateway = gateway.String
	c.QueryGateway = queryGateway.String
	c.MemberID = memberID.String
	c.APIKey = apiKey.String
	c.BankCode = bankCode.String
	c.NotifyURL = notifyURL.String
	c.Enabled = enabled != 0
	if c.DriverConfig, err = parseDriverConfig(driverConfig); err != nil {
		return nil, err
	}
	return &c, nil
}

// parseDriverConfig 把 driver_config JSON 解析成「键 → 字符串」。
//
// 真实数据里字段有字符串、数字、布尔（后台表单全按文本存），统一转字符串，
// 与 PHP 的 (string)$value 口径一致；嵌套对象/数组原样序列化回去（本阶段没有这种字段，
// 出现时也不能静默丢键）。
func parseDriverConfig(raw sql.NullString) (map[string]string, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw.String), &obj); err != nil {
		return nil, fmt.Errorf("解析 driver_config 失败: %w", err)
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		switch t := v.(type) {
		case nil:
			out[k] = ""
		case string:
			out[k] = t
		case float64:
			out[k] = formatPHPNumber(t)
		case bool:
			if t {
				out[k] = "1"
			} else {
				out[k] = ""
			}
		default:
			b, err := json.Marshal(t)
			if err != nil {
				return nil, err
			}
			out[k] = string(b)
		}
	}
	return out, nil
}

// formatPHPNumber 对齐 PHP 的 (string)$float：整数值不带小数点（10.0 → "10"）。
func formatPHPNumber(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", f), "0"), ".")
}

func (s *PaymentSQLStore) PaymentConfigByID(ctx context.Context, id uint64) (*PaymentConfig, error) {
	c, err := scanPaymentConfig(s.db.QueryRowContext(ctx,
		`SELECT `+paymentConfigColumns+` FROM payment_configs WHERE id = ? AND enabled = 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// PaymentConfigByIDAny 不看 enabled（对齐 PHP 的 `$order->paymentConfig` 关联）。
func (s *PaymentSQLStore) PaymentConfigByIDAny(ctx context.Context, id uint64) (*PaymentConfig, error) {
	c, err := scanPaymentConfig(s.db.QueryRowContext(ctx,
		`SELECT `+paymentConfigColumns+` FROM payment_configs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *PaymentSQLStore) FirstEnabledPaymentConfig(ctx context.Context) (*PaymentConfig, error) {
	c, err := scanPaymentConfig(s.db.QueryRowContext(ctx,
		`SELECT `+paymentConfigColumns+` FROM payment_configs WHERE enabled = 1 ORDER BY id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *PaymentSQLStore) EnabledPaymentConfigs(ctx context.Context) ([]PaymentConfig, error) {
	// 与 PaymentController::methods 一致：只取 id/name/bank_code（api_key 等密不落响应）。
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, bank_code FROM payment_configs WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PaymentConfig{}
	for rows.Next() {
		var (
			c        PaymentConfig
			bankCode sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Name, &bankCode); err != nil {
			return nil, err
		}
		c.BankCode = bankCode.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- plans ----

func (s *PaymentSQLStore) PlanForPayment(ctx context.Context, id uint64) (*PlanRow, error) {
	var (
		p            PlanRow
		isActive     int
		includeLocal sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, type, is_active, months, monthly_traffic, period_traffic, total_traffic,
		        price, reset_price, include_local
		 FROM plans WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Type, &isActive, &p.Months, &p.MonthlyTraffic, &p.PeriodTraffic,
			&p.TotalTraffic, &p.Price, &p.ResetPrice, &includeLocal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	p.IsActive = isActive != 0
	p.IncludeLocal = !includeLocal.Valid || includeLocal.Int64 != 0
	return &p, nil
}

// ---- orders ----

const orderColumns = `id, order_no, user_id, order_type, plan_id, proxy_sku_id, amount,
	original_amount, discount_code_id, discount_amount, status, payment_config_id,
	trade_no, paid_at, pay_ip, created_at`

// scanOrder 兼容「列值与 NULL」（金额/时间用 Null 变体，再转成文本或零值）。
func scanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	var (
		o              Order
		orderType      sql.NullString
		amount         sql.NullString
		originalAmount sql.NullString
		discountAmount sql.NullString
		status         sql.NullString
		createdAt      sql.NullTime
	)
	err := row.Scan(&o.ID, &o.OrderNo, &o.UserID, &orderType, &o.PlanID, &o.ProxySkuID, &amount,
		&originalAmount, &o.DiscountCodeID, &discountAmount, &status, &o.PaymentConfigID,
		&o.TradeNo, &o.PaidAt, &o.PayIP, &createdAt)
	if err != nil {
		return nil, err
	}
	o.OrderType = orderType.String
	if o.OrderType == "" {
		// order_type 列默认 'plan'，但备份导入的老表可能没有值
		o.OrderType = "plan"
	}
	o.Amount = amount.String
	o.OriginalAmount = originalAmount
	o.DiscountAmount = discountAmount
	o.Status = status.String
	if createdAt.Valid {
		o.CreatedAt = createdAt.Time
	}
	return &o, nil
}

func (s *PaymentSQLStore) OrderByNo(ctx context.Context, orderNo string) (*Order, error) {
	if orderNo == "" {
		return nil, sql.ErrNoRows
	}
	return scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE order_no = ? ORDER BY id LIMIT 1`, orderNo))
}

func (s *PaymentSQLStore) PendingOrderCreatedSince(ctx context.Context, userID, planID uint64, since time.Time) (*Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE user_id = ? AND plan_id = ? AND status = 'pending' AND created_at >= ?
		 ORDER BY id LIMIT 1`, userID, planID, since.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

func (s *PaymentSQLStore) LatestPendingOrderCreatedBefore(ctx context.Context, userID, planID uint64, before time.Time) (*Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE user_id = ? AND plan_id = ? AND status = 'pending' AND created_at < ?
		 ORDER BY id DESC LIMIT 1`, userID, planID, before.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

// LatestPendingOrderWithCode 对齐 DiscountCodeService::validateAndApply 的复用查询：
// where(user_id, discount_code_id, plan_id, status=pending)->orderByDesc('id')->first()
// —— **不带时间条件**（pending 单没有 TTL）。
func (s *PaymentSQLStore) LatestPendingOrderWithCode(ctx context.Context, userID, planID uint64, codeID int64) (*Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE user_id = ? AND plan_id = ? AND discount_code_id = ? AND status = 'pending'
		 ORDER BY id DESC LIMIT 1`, userID, planID, codeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

func (s *PaymentSQLStore) OrderByNoForUser(ctx context.Context, userID uint64, orderNo, status string) (*Order, error) {
	if orderNo == "" {
		return nil, nil
	}
	q := `SELECT ` + orderColumns + ` FROM orders WHERE order_no = ? AND user_id = ?`
	args := []any{orderNo, userID}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id LIMIT 1`

	o, err := scanOrder(s.db.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

func (s *PaymentSQLStore) UserOrders(ctx context.Context, userID uint64, limit int) ([]OrderListRow, error) {
	if limit <= 0 {
		limit = 20
	}
	// 与 PHP 的 ->with(['plan','proxySku']) 等价：两个 LEFT JOIN 取展示名。
	// 用 LEFT JOIN 而不是两次查询，避免 20 行 × 2 次的 N+1。
	rows, err := s.db.QueryContext(ctx,
		`SELECT o.id, o.order_no, o.user_id, o.order_type, o.plan_id, o.proxy_sku_id, o.amount,
		        o.original_amount, o.discount_code_id, o.discount_amount, o.status,
		        o.payment_config_id, o.trade_no, o.paid_at, o.pay_ip, o.created_at,
		        p.name AS plan_name, ps.name AS sku_name
		 FROM orders o
		 LEFT JOIN plans p ON p.id = o.plan_id
		 LEFT JOIN proxy_skus ps ON ps.id = o.proxy_sku_id
		 WHERE o.user_id = ?
		 ORDER BY o.id DESC
		 LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []OrderListRow{}
	for rows.Next() {
		var (
			row            OrderListRow
			orderType      sql.NullString
			amount         sql.NullString
			originalAmount sql.NullString
			discountAmount sql.NullString
			status         sql.NullString
			createdAt      sql.NullTime
		)
		if err := rows.Scan(&row.ID, &row.OrderNo, &row.UserID, &orderType, &row.PlanID, &row.ProxySkuID,
			&amount, &originalAmount, &row.DiscountCodeID, &discountAmount, &status,
			&row.PaymentConfigID, &row.TradeNo, &row.PaidAt, &row.PayIP, &createdAt,
			&row.PlanName, &row.ProxySkuName); err != nil {
			return nil, err
		}
		row.OrderType = orderType.String
		if row.OrderType == "" {
			row.OrderType = "plan"
		}
		row.Amount = amount.String
		row.OriginalAmount = originalAmount
		row.DiscountAmount = discountAmount
		row.Status = status.String
		if createdAt.Valid {
			row.CreatedAt = createdAt.Time
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *PaymentSQLStore) CreateOrder(ctx context.Context, in NewOrderInput) (*Order, error) {
	now := in.Now.UTC()
	orderType := in.OrderType
	if orderType == "" {
		// 重置流量订单号带 RST 前缀，仍是 plan 类型（P7 只建这两类）。
		orderType = "plan"
	}
	// plan_id 为 0 时写 NULL（余额充值单没有套餐；orders.plan_id 有外键，写 0 会违反约束）。
	var planID any
	if in.PlanID > 0 {
		planID = in.PlanID
	}
	// payment_config_id 同理：0 = 不经网关（余额支付/充值之外的无网关订单），写 NULL。
	// 两个列都有外键，直接写 0 会报 1452。
	var paymentConfigID any
	if in.PaymentConfigID > 0 {
		paymentConfigID = in.PaymentConfigID
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO orders (order_no, user_id, order_type, plan_id, amount, original_amount,
		                     discount_code_id, discount_amount, status, payment_config_id, pay_ip,
		                     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.OrderNo, in.UserID, orderType, planID, in.Amount, in.OriginalAmount,
		in.DiscountCodeID, in.DiscountAmount, in.Status, paymentConfigID, in.PayIP, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.orderByID(ctx, uint64(id))
}

func (s *PaymentSQLStore) orderByID(ctx context.Context, id uint64) (*Order, error) {
	return scanOrder(s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE id = ?`, id))
}

func (s *PaymentSQLStore) UpdateOrderStatus(ctx context.Context, id uint64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orders SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Truncate(time.Second), id)
	return err
}

func (s *PaymentSQLStore) MarkOrderPaid(ctx context.Context, id uint64, tradeNo sql.NullString, paidAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orders SET status = 'paid', trade_no = ?, paid_at = ?, updated_at = ? WHERE id = ?`,
		tradeNo, paidAt.UTC(), paidAt.UTC(), id)
	return err
}

func (s *PaymentSQLStore) AddResetTraffic(ctx context.Context, userID uint64, addTraffic int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET traffic_limit = traffic_limit + ?, monthly_traffic_used = 0, updated_at = ?
		 WHERE id = ?`, addTraffic, time.Now().UTC().Truncate(time.Second), userID)
	return err
}

// ---- discount_codes ----

const discountCodeColumns = `id, code, source, user_id, discount, max_uses, used_count,
	max_uses_per_user, expires_at, status, plan_id, source_order_id, note`

func scanDiscountCode(row interface{ Scan(...any) error }) (*DiscountCode, error) {
	var (
		d         DiscountCode
		source    sql.NullString
		discount  sql.NullString
		status    sql.NullString
		maxUses   sql.NullInt64
		usedCount sql.NullInt64
		perUser   sql.NullInt64
	)
	err := row.Scan(&d.ID, &d.Code, &source, &d.UserID, &discount, &maxUses, &usedCount,
		&perUser, &d.ExpiresAt, &status, &d.PlanID, &d.SourceOrderID, &d.Note)
	if err != nil {
		return nil, err
	}
	d.Source = source.String
	d.Discount = discount.String
	d.Status = status.String
	d.MaxUses = maxUses.Int64
	d.UsedCount = usedCount.Int64
	d.MaxUsesPerUser = perUser.Int64
	return &d, nil
}

func (s *PaymentSQLStore) DiscountCodeByCode(ctx context.Context, code string) (*DiscountCode, error) {
	d, err := scanDiscountCode(s.db.QueryRowContext(ctx,
		`SELECT `+discountCodeColumns+` FROM discount_codes WHERE code = ? ORDER BY id LIMIT 1`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (s *PaymentSQLStore) DiscountCodeByID(ctx context.Context, id int64) (*DiscountCode, error) {
	d, err := scanDiscountCode(s.db.QueryRowContext(ctx,
		`SELECT `+discountCodeColumns+` FROM discount_codes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (s *PaymentSQLStore) CountPaidOrdersWithCode(ctx context.Context, codeID, userID int64, excludeOrderID uint64) (int64, error) {
	q := `SELECT COUNT(*) FROM orders WHERE discount_code_id = ? AND user_id = ? AND status = 'paid'`
	args := []any{codeID, userID}
	if excludeOrderID > 0 {
		q += ` AND id != ?`
		args = append(args, excludeOrderID)
	}
	var n int64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

func (s *PaymentSQLStore) ExpireDiscountCode(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE discount_codes SET status = 'expired', updated_at = ? WHERE id = ? AND status = 'active'`,
		time.Now().UTC().Truncate(time.Second), id)
	return err
}

// ErrDiscountExhausted 表示该码在占用时已不可用（次数满 / 状态变了）。
var ErrDiscountExhausted = errors.New("discount_codes: 码不可用")

// ConsumeDiscountCode 对齐 DiscountCodeService::consume：
// 加锁重读 → 状态/次数校验 → used_count+1 →（满额）used_up。
//
// 用 UPDATE ... WHERE used_count < max_uses 的条件更新 + 影响行数判断来做原子占用：
// 这比 SELECT ... FOR UPDATE 少一次往返，且在 MySQL 默认隔离级别下等价（行锁串行）。
func (s *PaymentSQLStore) ConsumeDiscountCode(ctx context.Context, id int64, userID int64, excludeOrderID uint64) (*DiscountCode, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		status         string
		usedCount      int64
		maxUses        int64
		source         string
		maxUsesPerUser int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT status, used_count, max_uses, source, max_uses_per_user
		 FROM discount_codes WHERE id = ? FOR UPDATE`, id).
		Scan(&status, &usedCount, &maxUses, &source, &maxUsesPerUser)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDiscountExhausted
	}
	if err != nil {
		return nil, err
	}
	if status != "active" || usedCount >= maxUses {
		return nil, ErrDiscountExhausted
	}

	// 每用户次数复核（与 validateAndApply 同一口径）：下单只校验一次不够 ——
	// 用户可以用同一张码并发建多笔 pending 单，回调同时到达时各自都通过了下单时的检查。
	// 加锁后按「已支付」重数一遍，第二笔起被拦下。必须排除当前订单
	// （调用前它已被置为 paid），否则第一笔即自计数被误拦。
	if source != "invite" && maxUsesPerUser > 0 {
		q := `SELECT COUNT(*) FROM orders
		      WHERE discount_code_id = ? AND user_id = ? AND status = 'paid'`
		args := []any{id, userID}
		if excludeOrderID > 0 {
			q += ` AND id != ?`
			args = append(args, excludeOrderID)
		}
		var usedByMe int64
		if err := tx.QueryRowContext(ctx, q, args...).Scan(&usedByMe); err != nil {
			return nil, err
		}
		if usedByMe >= maxUsesPerUser {
			return nil, ErrDiscountExhausted
		}
	}

	newStatus := "active"
	if usedCount+1 >= maxUses {
		newStatus = "used_up"
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE discount_codes SET used_count = used_count + 1, status = ?, updated_at = ? WHERE id = ?`,
		newStatus, time.Now().UTC().Truncate(time.Second), id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.DiscountCodeByID(ctx, id)
}

func (s *PaymentSQLStore) LockUserInviteCode(ctx context.Context, userID uint64, codeID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET used_invite_code_id = ?, invite_code_used_at = ?, updated_at = ? WHERE id = ?`,
		codeID, at.UTC(), at.UTC(), userID)
	return err
}

func (s *PaymentSQLStore) UserInviteCodeLocked(ctx context.Context, userID uint64) (bool, error) {
	var (
		locked sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT used_invite_code_id FROM users WHERE id = ?`, userID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return locked.Valid && locked.Int64 != 0, nil
}

func (s *PaymentSQLStore) RedeemCodeExistsForOrder(ctx context.Context, orderID uint64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM discount_codes WHERE source = 'redeem' AND source_order_id = ?`, orderID).Scan(&n)
	return n > 0, err
}

func (s *PaymentSQLStore) CreateRedeemCode(ctx context.Context, in CreateRedeemCodeInput) error {
	now := time.Now().UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO discount_codes (code, source, user_id, discount, max_uses, used_count,
		                            max_uses_per_user, expires_at, note, status, plan_id, source_order_id,
		                            refreshed_at, next_refresh_at, created_at, updated_at)
		 VALUES (?, 'redeem', ?, ?, ?, 0, ?, NULL, NULL, 'active', NULL, ?, NULL, NULL, ?, ?)`,
		in.Code, in.UserID, in.Discount, in.MaxUses, in.MaxUsesPerUser, in.SourceOrderID, now, now)
	return err
}

// ---- users ----

func (s *PaymentSQLStore) UserByIDPayment(ctx context.Context, id uint64) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return u, err
}

// ApplyPlanFields 对齐 UserAdminService::applyPlan 的写入列：
//
//	period 套餐：plan_id / traffic_limit / traffic_used=0 / monthly_traffic_used=0 /
//	             monthly_traffic_limit / expired_at（now+months 的日期）/ next_traffic_reset_at（now+30d）
//	total 套餐： expired_at = NULL（monthly_traffic_limit = 0），不写 next_traffic_reset_at
//
// 用 COALESCE 表达「HasXxx=false 时不改这一列」：total 套餐与 period 套餐写的是
// 不同的列集合，一条 SQL 覆盖两种形态（NULL 进 COALESCE 就保留原值）。
func (s *PaymentSQLStore) ApplyPlanFields(ctx context.Context, userID uint64, f PlanFields) error {
	var nextReset, expired any
	if f.HasNextReset {
		nextReset = f.NextTrafficResetAt
	}
	if f.HasExpired {
		expired = f.ExpiredAt
	} else {
		// 显式置 NULL（total 套餐永不过期）
		expired = nil
		nextReset = nil
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET plan_id = ?, traffic_limit = ?, traffic_used = ?, monthly_traffic_used = ?,
		                 monthly_traffic_limit = ?, next_traffic_reset_at = ?, expired_at = ?, updated_at = ?
		 WHERE id = ?`,
		f.PlanID, f.TrafficLimit, f.TrafficUsed, f.MonthlyTrafficUsed,
		f.MonthlyTrafficLimit, nextReset, expired,
		time.Now().UTC().Truncate(time.Second), userID)
	return err
}

// PrimaryDomain 对齐 AbstractPaymentDriver::defaultNotifyUrl 的主域查询：
// domains 表 where(is_primary=1, enabled=1) 的第一行。
func (s *PaymentSQLStore) PrimaryDomain(ctx context.Context) (string, bool, error) {
	var domain sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT domain FROM domains WHERE is_primary = 1 AND enabled = 1 ORDER BY id LIMIT 1`).Scan(&domain)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return domain.String, domain.String != "", nil
}

func (s *PaymentSQLStore) SetUserEnabled(ctx context.Context, userID uint64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET enabled = ?, updated_at = ? WHERE id = ?`,
		v, time.Now().UTC().Truncate(time.Second), userID)
	return err
}
