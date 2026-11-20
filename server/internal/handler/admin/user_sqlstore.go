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
	u.next_traffic_reset_at, u.expired_at, u.enabled, u.created_at, u.balance`

// userBalanceOrderSQL 把「余额排序」口径转成 ORDER BY 片段（白名单，不拼外部输入）。
//
// 余额相同时用 id 倒序兜底，保证分页顺序稳定（否则同余额的行每次查询顺序不同，
// 翻页会重复或漏行）。
func userBalanceOrderSQL(order string) string {
	switch order {
	case "asc":
		return "u.balance ASC, u.id DESC"
	case "desc":
		return "u.balance DESC, u.id DESC"
	default:
		return "u.id DESC"
	}
}

// UserPage 对齐 UserController::index 的查询：LEFT JOIN plans + 有效代理权益计数，
// orderByDesc('id')，50/页。search 全表查（ID 精确 / 邮箱与 Token 模糊）。
//
// cursor > 0 时走游标分页：按主键定位（WHERE u.id < cursor），不查 COUNT、不用 OFFSET。
// 深页（十万页以上）在 offset 模式下要扫过前面所有行，实测 100 万用户翻末页 14 秒、
// 200 万用户 7~35 秒；游标模式恒定为毫秒级。offset 模式保留给「跳页」和兼容旧客户端。
//
// nodeID > 0 时只保留「绑定了该节点」的用户（#地区功能-26），用 EXISTS 子查询而不是
// JOIN user_nodes —— 一个用户可绑多节点，JOIN 会把他查出多行、把分页撑歪。
// balanceOrder ∈ "asc"/"desc" 时按 users.balance 排序（余额列点按排序，白名单校验在 handler）。
func (s *adminUserSQLStore) UserPage(ctx context.Context, search string, page, perPage, cursor, nodeID int64, now time.Time, balanceOrder string) ([]UserRow, int64, error) {
	where, args := userSearchWhere(search, now)
	if nodeID > 0 {
		where += ` AND EXISTS (SELECT 1 FROM user_nodes un WHERE un.user_id = u.id AND un.node_id = ?)`
		args = append(args, nodeID)
	}

	if perPage < 1 {
		perPage = 50
	}
	if page < 1 {
		page = 1
	}

	// 余额排序与游标分页互斥：游标按主键定位（u.id < cursor），换排序键后
	// 「上一页最后一条的 id」不再是可靠的起点，会漏行/重行。调用方在带排序时必须
	// 走 offset 路径；这里再兜一层，避免误用游标。
	orderBy := userBalanceOrderSQL(balanceOrder)
	if balanceOrder != "" {
		cursor = 0
	}

	// 游标模式：不查总数（100 万行 COUNT 要几百毫秒且随规模线性增长），
	// total 返回 -1 由上层识别为「未知」。
	if cursor > 0 {
		q := fmt.Sprintf(`SELECT %s
		FROM users u
		LEFT JOIN plans p ON p.id = u.plan_id
		WHERE 1=1%s AND u.id < ?
		ORDER BY %s
		LIMIT ?`, userSelectColumns, where, orderBy)
		args2 := append([]any{}, args...)
		args2 = append(args2, cursor, perPage)
		rows, err := s.db.QueryContext(ctx, q, args2...)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		out, err := scanUserRows(rows)
		return out, -1, err
	}

	var total int64
	countSQL := `SELECT COUNT(*) FROM users u WHERE 1=1` + where
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * perPage

	q := fmt.Sprintf(`SELECT %s
		FROM users u
		LEFT JOIN plans p ON p.id = u.plan_id
		WHERE 1=1%s
		ORDER BY %s
		LIMIT ? OFFSET ?`, userSelectColumns, where, orderBy)
	args2 := append([]any{}, args...)
	args2 = append(args2, perPage, offset)

	rows, err := s.db.QueryContext(ctx, q, args2...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []UserRow{}
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

// scanUserRows 把结果集扫成 UserRow 切片，供游标分支复用。
func scanUserRows(rows *sql.Rows) ([]UserRow, error) {
	out := []UserRow{}
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
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

func scanUserRow(row interface{ Scan(...any) error }) (*UserRow, error) {
	var (
		u                               UserRow
		email, planType, planName       sql.NullString
		token, uuid, protocol           string
		planID, planMonths              sql.NullInt64
		trafficLimit, trafficUsed       int64
		monthlyUsed, monthlyLimit       int64
		nextReset, expiredAt, createdAt sql.NullTime
		enabled                         int
		balance                         int64
	)
	dest := []any{
		&u.ID, &email, &token, &uuid, &protocol, &planID,
		&planType, &planName, &planMonths,
		&trafficLimit, &trafficUsed, &monthlyUsed, &monthlyLimit,
		&nextReset, &expiredAt, &enabled, &createdAt, &balance,
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
	u.Balance = balance
	return &u, nil
}

// UserByID 详情查询。
func (s *adminUserSQLStore) UserByID(ctx context.Context, id uint64) (*UserRow, error) {
	q := fmt.Sprintf(`SELECT %s
		FROM users u
		LEFT JOIN plans p ON p.id = u.plan_id
		WHERE u.id = ?`, userSelectColumns)
	row := s.db.QueryRowContext(ctx, q, id)
	u, err := scanUserRow(row)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *adminUserSQLStore) UserExists(ctx context.Context, id uint64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// UserProxies 曾在代理商城下线时返回空列表；生产线下线后此方法一并移除。

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

// ---- 绑定节点（#地区功能-26）----

// UserNodesBindings 批量取「用户 → 绑定节点」，一次 IN 查询。
//
// 为什么批量：用户列表每页 50 行，逐行查 user_nodes 就是 50 次往返（N+1）；
// 这里用一条 WHERE user_id IN (…) 拿全，再由调用方按 user_id 分组填进响应。
func (s *adminUserSQLStore) UserNodesBindings(ctx context.Context, userIDs []uint64) (map[uint64][]UserNodeRef, error) {
	out := make(map[uint64][]UserNodeRef, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}
	// 占位符按 id 个数拼；参数逐个 append（不能用 …slice 展开，database/sql 不认）。
	var sb strings.Builder
	sb.WriteString(`SELECT un.user_id, un.node_id, n.name
		FROM user_nodes un
		LEFT JOIN nodes n ON n.id = un.node_id
		WHERE un.user_id IN (`)
	args := make([]any, 0, len(userIDs))
	for i, id := range userIDs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('?')
		args = append(args, id)
	}
	sb.WriteString(`) ORDER BY un.user_id, un.node_id`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			userID uint64
			ref    UserNodeRef
			name   sql.NullString
		)
		// 节点被删掉时 LEFT JOIN 出 NULL：照旧返回 id（前端显示节点已不在），
		// 不能因为名字为空就把这条绑定吞掉 —— 否则改绑会重新加一条重复绑定。
		if err := rows.Scan(&userID, &ref.NodeID, &name); err != nil {
			return nil, err
		}
		ref.Name = name.String
		out[userID] = append(out[userID], ref)
	}
	return out, rows.Err()
}

// NodeOptions 全部节点的下拉候选（按 id 升序，含未启用节点）。
//
// 不过滤 enabled：停用节点上可能还绑着用户，把它们藏起来会让管理员没法把
// 那些人改绑出来（筛选也筛不到）。
func (s *adminUserSQLStore) NodeOptions(ctx context.Context) ([]NodeOption, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.name, r.name
		FROM nodes n
		LEFT JOIN regions r ON r.id = n.region_id
		ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []NodeOption{}
	for rows.Next() {
		var (
			o      NodeOption
			region sql.NullString
		)
		if err := rows.Scan(&o.ID, &o.Name, &region); err != nil {
			return nil, err
		}
		o.RegionName = nullStrPtr(region)
		out = append(out, o)
	}
	return out, rows.Err()
}

// MoveUserNode 单用户改绑：一个事务里换绑定 + 更新指纹 + 记迁移历史。
//
// 三件事必须原子：只改 user_nodes 不改 users.updated_at，节点侧看不出配置变了、
// 不会重新拉用户（新节点上永远不建号）；少记一条 user_node_migrations 就查不到
// 「谁在什么时候把这台机器上的用户挪走了」。
//
// fromNodeID = 0 表示「只加不减」（用户本来没绑定 / 管理员要多挂一台）。
func (s *adminUserSQLStore) MoveUserNode(ctx context.Context, userID uint64, fromNodeID, toNodeID int64, operator *string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 目标节点必须真实存在：user_nodes 没有外键，不校验就会写出悬空绑定，
	// 之后订阅渲染查不到节点、调度统计又把它算成一个绑定用户。
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ?`, toNodeID).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return ErrNodeNotFound
		}
		return err
	}

	if fromNodeID > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM user_nodes WHERE user_id = ? AND node_id = ?`, userID, fromNodeID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT IGNORE INTO user_nodes (user_id, node_id) VALUES (?, ?)`, userID, toNodeID); err != nil {
		return err
	}
	// 指纹：节点侧按 Max(updated_at) 判断自己那份配置是否过期（与 scheduler.moveUser 同口径）。
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET updated_at = NOW() WHERE id = ?`, userID); err != nil {
		return err
	}
	// from_node_id 为空时落 NULL（列可空）：区分「从某台搬走」与「直接新增一条」。
	var from any
	if fromNodeID > 0 {
		from = fromNodeID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_node_migrations (user_id, from_node_id, to_node_id, reason, operator, created_at)
		 VALUES (?, ?, ?, 'manual', ?, NOW())`,
		userID, from, toNodeID, nullStrValue(operator)); err != nil {
		return err
	}
	return tx.Commit()
}
