package admin

// MailStore 的 database/sql 实现（邮件域）。
//
// 时间口径同 P8d（data-contract §4）：parseTime=true&loc=UTC，字面量打 UTC 标签。
// smtp_password 用 internal/crypto 复刻的 Crypt::encryptString（与 PHP 可互读）。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NewMailSQLStore 用已有连接池构造邮件域的 MailStore。
//
// appKey 用于 smtp_accounts.password_enc（与站点配置的 smtp_password 同口径）；
// 不传时账号池只能以空密码发信（存量调用点保持旧签名可用，见 NewMailSQLStoreWithKey）。
func NewMailSQLStore(db *sql.DB) MailStore {
	return &mailSQLStore{db: db}
}

// NewMailSQLStoreWithKey 带 APP_KEY 构造（smtp_accounts 密码加解密需要）。
func NewMailSQLStoreWithKey(db *sql.DB, appKey []byte) MailStore {
	return &mailSQLStore{db: db, appKey: appKey}
}

type mailSQLStore struct {
	db     *sql.DB
	appKey []byte
}

func (s *mailSQLStore) MailConfigMany(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = ""
	}
	if len(keys) == 0 {
		return out, nil
	}
	marks := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		marks = append(marks, "?")
		args = append(args, k)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT `key`, value FROM site_configs WHERE `key` IN ("+strings.Join(marks, ",")+")", args...)
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
		out[k] = v.String
	}
	return out, rows.Err()
}

func (s *mailSQLStore) MailConfigSetMany(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Truncate(time.Second)
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO site_configs (`key`, value, created_at, updated_at) VALUES (?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = VALUES(updated_at)",
			k, v, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const mailUserColumns = `u.id, u.email, u.token, p.name,
	u.traffic_limit, u.traffic_used, u.monthly_traffic_used, u.monthly_traffic_limit, u.expired_at`

func scanMailUser(row interface{ Scan(...any) error }) (*MailUser, error) {
	var (
		u       MailUser
		planNam sql.NullString
		expired sql.NullTime
	)
	if err := row.Scan(&u.ID, &u.Email, &u.Token, &planNam,
		&u.TrafficLimit, &u.TrafficUsed, &u.MonthlyTrafficUsed, &u.MonthlyTrafficLimit, &expired); err != nil {
		return nil, err
	}
	u.PlanName = nullStrPtr(planNam)
	u.ExpiredAt = nullTimePtr(expired)
	return &u, nil
}

// MailUserByLogin 对齐 MailBatchService::resolveRecipients 的 single 分支：
// 邮箱精确匹配，或（查询串为纯数字时）用户 ID 精确匹配。
func (s *mailSQLStore) MailUserByLogin(ctx context.Context, query string) (*MailUser, error) {
	id := int64(0)
	isDigits := query != ""
	for _, ch := range query {
		if ch < '0' || ch > '9' {
			isDigits = false
			break
		}
	}
	if isDigits {
		var err error
		id, err = parseInt64Strict(query)
		if err != nil {
			id = 0
		}
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT "+mailUserColumns+" FROM users u LEFT JOIN plans p ON p.id = u.plan_id "+
			"WHERE u.email = ? OR u.id = ? LIMIT 1", query, id)
	u, err := scanMailUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// MailRecipients 对齐 MailBatchService::recipientQuery（all / filter）。
func (s *mailSQLStore) MailRecipients(ctx context.Context, f MailRecipientFilter, now time.Time) ([]MailUser, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.PlanID != nil {
		where = append(where, "u.plan_id = ?")
		args = append(args, *f.PlanID)
	}
	switch f.UserStatus {
	case "over":
		where = append(where, `(
			(u.traffic_limit > 0 AND u.traffic_used >= u.traffic_limit)
			OR (u.monthly_traffic_limit > 0 AND u.monthly_traffic_used >= u.monthly_traffic_limit))`)
	case "expired":
		where = append(where, "u.expired_at IS NOT NULL AND u.expired_at < ?")
		args = append(args, now)
	default: // normal
		where = append(where, `u.plan_id IS NOT NULL AND u.enabled = 1`,
			"(u.expired_at IS NULL OR u.expired_at > ?)",
			"(u.traffic_limit <= 0 OR u.traffic_used < u.traffic_limit)")
		args = append(args, now)
	}
	q := "SELECT " + mailUserColumns + " FROM users u LEFT JOIN plans p ON p.id = u.plan_id WHERE " +
		strings.Join(where, " AND ") + " ORDER BY u.id ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailUser
	for rows.Next() {
		u, err := scanMailUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

const mailLogSelectColumns = `id, type, status, to_email, to_user_id, subject, scene, batch_id, error, created_at`

func scanMailLog(row interface{ Scan(...any) error }) (*MailLogRow, error) {
	var (
		l         MailLogRow
		toEmail   sql.NullString
		toUserID  sql.NullString // 列是 string，模型 cast 成 integer
		subject   sql.NullString
		scene     sql.NullString
		batchID   sql.NullInt64
		errText   sql.NullString
		createdAt sql.NullTime
	)
	if err := row.Scan(&l.ID, &l.Type, &l.Status, &toEmail, &toUserID, &subject, &scene,
		&batchID, &errText, &createdAt); err != nil {
		return nil, err
	}
	l.ToEmail = nullStrPtr(toEmail)
	if toUserID.Valid && toUserID.String != "" {
		if n, err := parseInt64Strict(toUserID.String); err == nil {
			l.ToUserID = &n
		}
	}
	l.Subject = nullStrPtr(subject)
	l.Scene = nullStrPtr(scene)
	if batchID.Valid {
		n := batchID.Int64
		l.BatchID = &n
	}
	l.Error = nullStrPtr(errText)
	l.CreatedAt = nullTimePtr(createdAt)
	return &l, nil
}

// MailLogPage 对齐 EmailController::mailLogs（orderByDesc id，固定 50/页）。
func (s *mailSQLStore) MailLogPage(ctx context.Context, f MailLogFilter, page, perPage int64) ([]MailLogRow, int64, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Type != "" {
		where = append(where, "type = ?")
		args = append(args, f.Type)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	whereSQL := " WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM mail_logs"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if perPage < 1 {
		perPage = 50
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	rows, err := s.db.QueryContext(ctx,
		"SELECT "+mailLogSelectColumns+" FROM mail_logs"+whereSQL+
			" ORDER BY id DESC LIMIT ? OFFSET ?",
		append(append([]any{}, args...), perPage, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []MailLogRow
	for rows.Next() {
		l, err := scanMailLog(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *l)
	}
	return out, total, rows.Err()
}

// CreateMailLog 对齐 MailLog::create（测试邮件落日志）。
func (s *mailSQLStore) CreateMailLog(ctx context.Context, row MailLogRow) error {
	now := row.CreatedAt
	if now == nil {
		t := time.Now().UTC()
		now = &t
	}
	var toUserID any
	if row.ToUserID != nil {
		toUserID = fmt.Sprintf("%d", *row.ToUserID) // 列是 string
	}
	var batchID any
	if row.BatchID != nil {
		batchID = *row.BatchID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mail_logs (type, status, to_email, to_user_id, subject, scene, batch_id, error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Type, row.Status, nullStrValue(row.ToEmail), toUserID, nullStrValue(row.Subject),
		nullStrValue(row.Scene), batchID, nullStrValue(row.Error), now, now)
	return err
}

// MailBatchProgress 按 batch_id 统计已发/失败（进度条用，只读聚合）。
func (s *mailSQLStore) MailBatchProgress(ctx context.Context, batchID int64) (*MailBatchProgress, error) {
	var (
		sent, failed          int64
		firstAt, lastAt       sql.NullTime
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT
		   SUM(CASE WHEN status = 'sent' THEN 1 ELSE 0 END) AS sent,
		   SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) AS failed,
		   MIN(created_at) AS first_at,
		   MAX(created_at) AS last_at
		 FROM mail_logs WHERE batch_id = ?`, batchID).
		Scan(&sent, &failed, &firstAt, &lastAt); err != nil {
		return nil, err
	}
	p := &MailBatchProgress{BatchID: batchID, Sent: sent, Failed: failed}
	if firstAt.Valid {
		t := firstAt.Time
		p.FirstAt = &t
	}
	if lastAt.Valid {
		t := lastAt.Time
		p.LastAt = &t
	}
	return p, nil
}

// parseInt64Strict 严格解析十进制整数。
func parseInt64Strict(s string) (int64, error) {
	var n int64
	var neg bool
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	i := 0
	if s[0] == '-' {
		neg = true
		i++
	}
	if i >= len(s) {
		return 0, fmt.Errorf("bad int")
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("bad int")
		}
		n = n*10 + int64(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}
