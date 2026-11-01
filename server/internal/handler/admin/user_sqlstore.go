package admin

// AdminUserStore 的 database/sql 实现（用户域）。
//
// 时间口径与 P8d 的 sqlStore 完全一致（contracts/data-contract.md §4）：
// DSN 用 parseTime=true&loc=UTC，把 MySQL 返回的字面量打 UTC 标签、不做换算，
// 与 Laravel（app.timezone=UTC）读到的一致。严禁 SET time_zone。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NewAdminUserSQLStore 用已有连接池构造用户域的 AdminUserStore。
func NewAdminUserSQLStore(db *sql.DB) AdminUserStore {
	return &adminUserSQLStore{db: db}
}

type adminUserSQLStore struct {
	db *sql.DB
}

const userSelectColumns = `u.id, u.email, u.token, u.uuid, u.protocol, u.plan_id,
	p.type, p.name, p.months,
	u.traffic_limit, u.traffic_used, u.monthly_traffic_used, u.monthly_traffic_limit,
	u.next_traffic_reset_at, u.expired_at, u.enabled, u.created_at`

// UserPage 对齐 UserController::index 的查询：LEFT JOIN plans + 有效代理权益计数，
// orderByDesc('id')，50/页。search 全表查（ID 精确 / 邮箱与 Token 模糊）。
func (s *adminUserSQLStore) UserPage(ctx context.Context, search string, page, perPage int64, now time.Time) ([]UserRow, int64, error) {
	where, args := userSearchWhere(search, now)

	var total int64
	countSQL := `SELECT COUNT(*) FROM users u WHERE 1=1` + where
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if perPage < 1 {
		perPage = 50
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	q := fmt.Sprintf(`SELECT %s,
		(SELECT COUNT(*) FROM proxy_entitlements e
			WHERE e.user_id = u.id AND e.status = 'active'
			  AND (e.expires_at IS NULL OR e.expires_at > ?)) AS proxy_active_count
		FROM users u
		LEFT JOIN plans p ON p.id = u.plan_id
		WHERE 1=1%s
		ORDER BY u.id DESC
		LIMIT ? OFFSET ?`, userSelectColumns, where)
	args2 := append([]any{now}, args...)
	args2 = append(args2, perPage, offset)

	rows, err := s.db.QueryContext(ctx, q, args2...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []UserRow{}
	for rows.Next() {
		u, err := scanUserRow(rows, true)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

// userSearchWhere 复刻 index 的搜索分支（ctype_digit → 精确 id OR 模糊，否则纯模糊）。
func userSearchWhere(search string, now time.Time) (string, []any) {
	search = strings.TrimSpace(search)
	if search == "" {
		return "", nil
	}
	like := "%" + search + "%"
	if isDigits(search) {
		return ` AND (u.id = ? OR u.email LIKE ? OR u.token LIKE ?)`,
			[]any{search, like, like}
	}
	return ` AND (u.email LIKE ? OR u.token LIKE ?)`, []any{like, like}
}

func scanUserRow(row interface{ Scan(...any) error }, withProxyCount bool) (*UserRow, error) {
	var (
		u                               UserRow
		email, planType, planName       sql.NullString
		token, uuid, protocol           string
		planID, planMonths              sql.NullInt64
		trafficLimit, trafficUsed       int64
		monthlyUsed, monthlyLimit       int64
		nextReset, expiredAt, createdAt sql.NullTime
		enabled                         int
	)
	dest := []any{
		&u.ID, &email, &token, &uuid, &protocol, &planID,
		&planType, &planName, &planMonths,
		&trafficLimit, &trafficUsed, &monthlyUsed, &monthlyLimit,
		&nextReset, &expiredAt, &enabled, &createdAt,
	}
	var proxyCount int64
	if withProxyCount {
		dest = append(dest, &proxyCount)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	u.Email = nullStrPtr(email)
	u.Token = token
	u.UUID = uuid
	u.Protocol = protocol
	u.PlanID = nullUintPtr(planID)
	u.PlanType = nullStrPtr(planType)
	u.PlanName = nullStrPtr(planName)
	u.PlanMonths = nullIntPtr(planMonths)
	u.TrafficLimit = trafficLimit
	u.TrafficUsed = trafficUsed
	u.MonthlyTrafficUsed = monthlyUsed
	u.MonthlyTrafficLimit = monthlyLimit
	u.NextTrafficResetAt = nullTimePtr(nextReset)
	u.ExpiredAt = nullTimePtr(expiredAt)
	u.Enabled = enabled != 0
	u.CreatedAt = nullTimePtr(createdAt)
	if withProxyCount {
		u.ProxyActiveCount = proxyCount
	}
	return &u, nil
}

// UserByID 详情查询（带 proxy_active_count，与列表同口径）。
func (s *adminUserSQLStore) UserByID(ctx context.Context, id uint64) (*UserRow, error) {
	q := fmt.Sprintf(`SELECT %s,
		(SELECT COUNT(*) FROM proxy_entitlements e
			WHERE e.user_id = u.id AND e.status = 'active'
			  AND (e.expires_at IS NULL OR e.expires_at > ?)) AS proxy_active_count
		FROM users u
		LEFT JOIN plans p ON p.id = u.plan_id
		WHERE u.id = ?`, userSelectColumns)
	// 详情的 proxy_active_count 用「现在」；PHP 的 show 不算 count（$u->proxy_active_count ?? 0）。
	// 但 create/update 的响应是 $user->load('plan') 后 present —— 同样拿不到 withCount 属性。
	// 这里保持与列表一致的计数查询，避免两套 SQL；present 的语义由调用方决定。
	row := s.db.QueryRowContext(ctx, q, time.Now().UTC(), id)
	u, err := scanUserRow(row, true)
	if err != nil {
		return nil, err
	}
	// PHP 的详情/写响应 proxy_active_count 恒为 0（未 withCount）。对齐这一点。
	u.ProxyActiveCount = 0
	return u, nil
}

func (s *adminUserSQLStore) UserExists(ctx context.Context, id uint64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// UserProxies 对齐 $user->proxyEntitlements.sku 预加载。
func (s *adminUserSQLStore) UserProxies(ctx context.Context, userID uint64) ([]UserProxy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.kind, s.name, e.status, e.expires_at
		FROM proxy_entitlements e
		LEFT JOIN proxy_skus s ON s.id = e.sku_id
		WHERE e.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserProxy{}
	for rows.Next() {
		var (
			p         UserProxy
			skuName   sql.NullString
			expiresAt sql.NullTime
		)
		if err := rows.Scan(&p.ID, &p.Kind, &skuName, &p.Status, &expiresAt); err != nil {
			return nil, err
		}
		p.SKUName = nullStrPtr(skuName)
		p.ExpiresAt = nullTimePtr(expiresAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlanByID 取套餐换算需要的列。
func (s *adminUserSQLStore) UserPlanByID(ctx context.Context, id uint64) (*UserPlan, error) {
	var (
		p                              UserPlan
		months, monthly, period, total sql.NullInt64
		name, typ                      string
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, name, type, months, monthly_traffic, period_traffic, total_traffic
		FROM plans WHERE id = ?`, id).
		Scan(&p.ID, &name, &typ, &months, &monthly, &period, &total)
	if err != nil {
		return nil, err
	}
	p.Name = name
	p.Type = typ
	p.Months = nullIntPtr(months)
	p.MonthlyTraffic = nullIntPtr(monthly)
	p.PeriodTraffic = nullIntPtr(period)
	p.TotalTraffic = nullIntPtr(total)
	return &p, nil
}

func (s *adminUserSQLStore) PlanExists(ctx context.Context, id uint64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// CreateUser 落库建号（token/uuid 由调用方生成，对齐 Str::random / Str::uuid）。
func (s *adminUserSQLStore) CreateUser(ctx context.Context, in UserCreateInput) (*UserRow, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users
		(email, password, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		 monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at,
		 enabled, created_at, updated_at)
		VALUES (?, NULL, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?)`,
		nullStrValue(in.Email), in.Token, in.UUID, in.Protocol, nullUintValue(in.PlanID),
		in.TrafficLimit, in.MonthlyTrafficLimit,
		nullTimeValue(in.NextTrafficResetAt), nullTimeValue(in.ExpiredAt),
		boolToInt(in.Enabled), in.Now, in.Now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, uint64(id))
}

// UpdateUser 部分更新（Set 标志区分「未传」与「显式 null」）。
func (s *adminUserSQLStore) UpdateUser(ctx context.Context, id uint64, patch UserPatch) error {
	set := []string{}
	args := []any{}

	if patch.EmailSet {
		set = append(set, "email = ?")
		args = append(args, nullStrValue(patch.Email))
	}
	if patch.ProtocolSet {
		set = append(set, "protocol = ?")
		args = append(args, nullStrValue(patch.Protocol))
	}
	if patch.PlanIDSet {
		set = append(set, "plan_id = ?")
		args = append(args, nullUintValue(patch.PlanID))
	}
	if patch.TrafficLimitSet {
		set = append(set, "traffic_limit = ?")
		args = append(args, derefInt(patch.TrafficLimit))
	}
	if patch.TrafficUsedSet {
		set = append(set, "traffic_used = ?")
		args = append(args, derefInt(patch.TrafficUsed))
	}
	if patch.MonthlyUsedSet {
		set = append(set, "monthly_traffic_used = ?")
		args = append(args, derefInt(patch.MonthlyTrafficUsed))
	}
	if patch.MonthlyLimitSet {
		set = append(set, "monthly_traffic_limit = ?")
		args = append(args, derefInt(patch.MonthlyTrafficLimit))
	}
	if patch.NextResetSet {
		set = append(set, "next_traffic_reset_at = ?")
		args = append(args, nullTimeValue(patch.NextTrafficResetAt))
	}
	if patch.ExpiredAtSet {
		set = append(set, "expired_at = ?")
		args = append(args, nullTimeValue(patch.ExpiredAt))
	}
	if patch.EnabledSet {
		set = append(set, "enabled = ?")
		args = append(args, boolToInt(*patch.Enabled))
	}
	if patch.PasswordSet {
		set = append(set, "password = ?")
		args = append(args, nullStrValue(patch.Password))
	}
	if patch.TokenSet {
		set = append(set, "token = ?")
		args = append(args, nullStrValue(patch.Token))
	}
	if patch.UUIDSet {
		set = append(set, "uuid = ?")
		args = append(args, nullStrValue(patch.UUID))
	}
	if len(set) == 0 {
		return nil
	}
	set = append(set, "updated_at = ?")
	args = append(args, patch.Now)

	args = append(args, id)
	res, err := s.db.ExecContext(ctx, `UPDATE users SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *adminUserSQLStore) DeleteUser(ctx context.Context, id uint64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteAccessTokens 对齐 AccessToken::where('user_id', ...)->delete()。
func (s *adminUserSQLStore) DeleteAccessTokens(ctx context.Context, userID uint64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM access_tokens WHERE user_id = ?`, userID)
	return err
}
