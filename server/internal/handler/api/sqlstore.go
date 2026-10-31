package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"controlhub-server/internal/crypto"
	"encoding/json"
)

// sqlStore 是 Store 的 database/sql 实现。
//
// 时间口径（contracts/data-contract.md §4）：DSN 用 parseTime=true&loc=UTC，
// 把 MySQL 返回的 datetime 字面量打上 UTC 标签、不做任何换算 —— 与 Laravel
// （app.timezone=UTC，读同一份字面量）逐字一致。写入同样传 UTC 的 time.Time，
// 严禁 SET time_zone（会让 Go 与 PHP 相差 8 小时）。
type sqlStore struct{ db *sql.DB }

// NewSQLStore 用已有连接池构造 Store。
func NewSQLStore(db *sql.DB) Store { return &sqlStore{db: db} }

var errNoRows = sql.ErrNoRows

const userColumns = `id, email, password, token, protocol, plan_id,
	traffic_limit, traffic_used, monthly_traffic_used, monthly_traffic_limit,
	next_traffic_reset_at, expired_at, enabled`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var enabled int
	err := row.Scan(&u.ID, &u.Email, &u.Password, &u.Token, &u.Protocol, &u.PlanID,
		&u.TrafficLimit, &u.TrafficUsed, &u.MonthlyTrafficUsed, &u.MonthlyTrafficLimit,
		&u.NextTrafficResetAt, &u.ExpiredAt, &enabled)
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled != 0
	return &u, nil
}

func (s *sqlStore) UserByID(ctx context.Context, id uint64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

func (s *sqlStore) UserByToken(ctx context.Context, token string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE token = ?`, token))
}

func (s *sqlStore) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ? ORDER BY id LIMIT 1`, email))
}

func (s *sqlStore) EmailExists(ctx context.Context, email string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email = ?`, email).Scan(&n)
	return n > 0, err
}

// CreateUser 等价于 User::create([...]) —— 只写注册用到的列，其余走表默认值。
// created_at/updated_at 与 Laravel 一样取「当前 UTC 时刻」的字面量。
func (s *sqlStore) CreateUser(ctx context.Context, in CreateUserInput) (*User, error) {
	now := time.Now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password, token, uuid, protocol, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
		in.Email, in.PasswordHash, in.Token, in.UUID, in.Protocol, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, uint64(id))
}

func (s *sqlStore) UpdateUserPassword(ctx context.Context, id uint64, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password = ?, updated_at = ? WHERE id = ?`,
		hash, time.Now().UTC().Truncate(time.Second), id)
	return err
}

func (s *sqlStore) UpdateUserProtocol(ctx context.Context, id uint64, protocol string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET protocol = ?, updated_at = ? WHERE id = ?`,
		protocol, time.Now().UTC().Truncate(time.Second), id)
	return err
}

func (s *sqlStore) UpdateUserAppVersion(ctx context.Context, id uint64, code int64, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET app_version_code = ?, app_version_name = ?, app_version_reported_at = ? WHERE id = ?`,
		code, name, time.Now().UTC().Truncate(time.Second), id)
	return err
}

const planColumns = `id, name, type, months, monthly_traffic, period_traffic, total_traffic,
	price, reset_price, include_local, third_party_sub_ids`

func scanPlan(row interface{ Scan(...any) error }) (*Plan, error) {
	var p Plan
	var price, resetPrice []byte
	var includeLocal int
	var thirdParty sql.NullString
	err := row.Scan(&p.ID, &p.Name, &p.Type, &p.Months, &p.MonthlyTraffic, &p.PeriodTraffic,
		&p.TotalTraffic, &price, &resetPrice, &includeLocal, &thirdParty)
	if err != nil {
		return nil, err
	}
	// 金额按 string → float64 读取（decimal:2 的精度只有 2 位，float64 表达无损失；
	// 对外输出必须再按 Laravel 的 (float) 口径渲染，见 money.go）。
	if p.Price, err = parseDecimal(price); err != nil {
		return nil, err
	}
	if p.ResetPrice, err = parseDecimal(resetPrice); err != nil {
		return nil, err
	}
	p.IncludeLocal = includeLocal != 0
	if p.ThirdPartyIDs, err = parseIntJSONArray(thirdParty); err != nil {
		return nil, err
	}
	return &p, nil
}

func parseDecimal(raw []byte) (float64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	v, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("解析 decimal 失败 %q: %w", raw, err)
	}
	return v, nil
}

func (s *sqlStore) PlanByID(ctx context.Context, id uint64) (*Plan, error) {
	return scanPlan(s.db.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = ?`, id))
}

// ActivePlans 对齐 PlanController::index：is_active=1 按 price 升序（同价按主键，保证稳定）。
func (s *sqlStore) ActivePlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+planColumns+` FROM plans WHERE is_active = 1 ORDER BY price, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	plans := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, *p)
	}
	return plans, rows.Err()
}

func (s *sqlStore) AccessTokenByToken(ctx context.Context, token string) (*AccessToken, error) {
	var a AccessToken
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, token, expires_at FROM access_tokens WHERE token = ?`, token).
		Scan(&a.ID, &a.UserID, &a.Token, &a.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *sqlStore) CreateAccessToken(ctx context.Context, userID uint64, token string, expires time.Time) error {
	now := time.Now().UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO access_tokens (user_id, token, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		userID, token, expires.UTC().Truncate(time.Second), now, now)
	return err
}

func (s *sqlStore) TouchAccessToken(ctx context.Context, id uint64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE access_tokens SET last_used_at = ?, updated_at = ? WHERE id = ?`,
		at.UTC().Truncate(time.Second), at.UTC().Truncate(time.Second), id)
	return err
}

func (s *sqlStore) DeleteAccessTokensByUser(ctx context.Context, userID uint64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM access_tokens WHERE user_id = ?`, userID)
	return err
}

// SiteConfigMany 对齐 SiteConfig::getMany：键不存在时补空串，且返回 map 覆盖全部请求键。
func (s *sqlStore) SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = ""
	}
	if len(keys) == 0 {
		return out, nil
	}

	placeholders := make([]byte, 0, len(keys)*2)
	args := make([]any, 0, len(keys))
	for i, k := range keys {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, k)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+"`key`"+`, value FROM site_configs WHERE `+"`key`"+` IN (`+string(placeholders)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v.String // NULL 与不存在同口径：空串
	}
	return out, rows.Err()
}

// tutorialCategory 与路由里的 COALESCE(NULLIF(TRIM(category), ”), '默认') 同口径。
const tutorialCategory = `COALESCE(NULLIF(TRIM(category), ''), '默认')`

func scanTutorial(row interface{ Scan(...any) error }) (*Tutorial, error) {
	var t Tutorial
	var enabled int
	err := row.Scan(&t.ID, &t.Title, &t.Category, &t.Content, &t.Sort, &enabled, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.Enabled = enabled != 0
	return &t, nil
}

// EnabledTutorials 对齐 GET /api/tutorials：只取 enabled，按 sort 升序、id 降序。
func (s *sqlStore) EnabledTutorials(ctx context.Context) ([]Tutorial, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, `+tutorialCategory+` AS category, content, sort, enabled, created_at
		 FROM tutorials WHERE enabled = 1 ORDER BY sort ASC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Tutorial{}
	for rows.Next() {
		t, err := scanTutorial(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *sqlStore) TutorialByID(ctx context.Context, id uint64) (*Tutorial, error) {
	return scanTutorial(s.db.QueryRowContext(ctx,
		`SELECT id, title, `+tutorialCategory+` AS category, content, sort, enabled, created_at
		 FROM tutorials WHERE id = ?`, id))
}

const nodeColumns = `id, name, host, port, status, latency, driver_type, driver_config,
	traffic_multiplier, scheme, web_base_path, username, password, api_key, verify_ssl`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var driverConfig sql.NullString
	var multiplier *float64
	var verifySSL int
	err := row.Scan(&n.ID, &n.Name, &n.Host, &n.Port, &n.Status, &n.Latency, &n.DriverType,
		&driverConfig, &multiplier, &n.Scheme, &n.WebBasePath, &n.Username,
		&n.PasswordEnc, &n.APIKeyEnc, &verifySSL)
	if err != nil {
		return nil, err
	}
	n.DriverConfig = driverConfig.String
	n.TrafficMultiplier = multiplier
	n.VerifySSL = verifySSL != 0
	return &n, nil
}

// OnlineNodesForProtocol 对齐 NodeController::index 的筛选条件：
// enabled=1 且 status='online'，且有可用入站（不按用户协议过滤：用户全协议通用，
// 节点上只要配了任何入站就该出现在列表里）。
//
// 排序沿用 Eloquent->get() 的隐式主键顺序，保证输出稳定。
func (s *sqlStore) OnlineNodesForProtocol(ctx context.Context, protocol string) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes AS n
		WHERE n.enabled = 1 AND n.status = 'online'
		  AND (
		    EXISTS (SELECT 1 FROM node_inbounds ni WHERE ni.node_id = n.id)
		    OR (n.driver_type = 'xray'
		        AND EXISTS (SELECT 1 FROM xray_inbounds xi
		                    WHERE xi.node_id = n.id AND xi.enabled = 1))
		  )
		ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, *n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range nodes {
		if err := s.loadNodeInbounds(ctx, &nodes[i]); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

// EnabledNodesForSwitch 对齐 ProtocolSwitchService 里 Node::where('enabled', true)->get()。
func (s *sqlStore) EnabledNodesForSwitch(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, *n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range nodes {
		if err := s.loadNodeInbounds(ctx, &nodes[i]); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

// loadNodeInbounds 一次性补齐 node_inbounds 与 xray_inbounds（两个小查询，避免 N+1）。
func (s *sqlStore) loadNodeInbounds(ctx context.Context, n *Node) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT protocol, inbound_id FROM node_inbounds WHERE node_id = ? ORDER BY id`, n.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var in NodeInbound
		if err := rows.Scan(&in.Protocol, &in.InboundID); err != nil {
			return err
		}
		n.Inbounds = append(n.Inbounds, in)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// NodeController 用 `with('xrayInbounds')` 取的是**无排序**的关联集合（MySQL 通常按主键返回），
	// 端口取「该协议第一个启用入站」时必须与那个顺序一致，所以这里按 id 升序（不用 sort）。
	xrows, err := s.db.QueryContext(ctx,
		`SELECT id, protocol, port, enabled FROM xray_inbounds
		 WHERE node_id = ? AND enabled = 1 ORDER BY id`, n.ID)
	if err != nil {
		return err
	}
	defer xrows.Close()
	for xrows.Next() {
		var in XrayInbound
		var enabled int
		if err := xrows.Scan(&in.ID, &in.Protocol, &in.Port, &enabled); err != nil {
			return err
		}
		in.Enabled = enabled != 0
		n.XrayInbounds = append(n.XrayInbounds, in)
	}
	return xrows.Err()
}

func (s *sqlStore) ThirdPartySubsByIDs(ctx context.Context, ids []int64) ([]ThirdPartySub, error) {
	out := []ThirdPartySub{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	sqlText := `SELECT id, cached_links FROM third_party_subs WHERE enabled = 1 AND id IN (`
	for i, id := range ids {
		if i > 0 {
			sqlText += ","
		}
		sqlText += "?"
		args = append(args, id)
	}
	sqlText += `) ORDER BY id`

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sub ThirdPartySub
		var links sql.NullString
		if err := rows.Scan(&sub.ID, &links); err != nil {
			return nil, err
		}
		sub.CachedLinks = links.String
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ---- 供 handler 复用的小工具 ----

// itoa 是 uint64 → 十进制字符串（避免在热路径引 strconv 的格式解析开销）。
func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

// parseIntJSONArray 把 third_party_sub_ids 的 JSON 文本解析成 int64 列表。
//
// 对齐 PHP 的 array_values(array_map('intval', (array) ($value ?? [])))：
// null / 空串 / 非数组 / 非法 JSON 一律视为空（PHP 那边 (array) 会把非数组包一层，
// 但真实数据只有 null 与 JSON 数组两种形态）。
func parseIntJSONArray(raw sql.NullString) ([]int64, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var vals []any
	if err := json.Unmarshal([]byte(raw.String), &vals); err != nil {
		return nil, nil
	}
	out := make([]int64, 0, len(vals))
	for _, v := range vals {
		switch t := v.(type) {
		case float64:
			out = append(out, int64(t))
		case string:
			if n, err := strconv.ParseInt(t, 10, 64); err == nil {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// decryptOptional 解密 nodes.password / nodes.api_key（Crypt::encryptString 密文）。
// 明文存量/手工改库的情况按原样返回，与 Laravel accessor 的宽松口径一致。
func decryptOptional(raw sql.NullString, appKey []byte) string {
	if !raw.Valid || raw.String == "" {
		return ""
	}
	plain, err := crypto.DecryptString(raw.String, appKey)
	if err != nil {
		return raw.String
	}
	return plain
}

// IsNoRows 让上层无需直接依赖 database/sql 判定「查无此行」。
func IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
