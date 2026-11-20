package admin

// SMTPAccountStore 的 database/sql 实现（邮件域·账号池）。
//
// 时间口径同 email_sqlstore.go：parseTime=true&loc=UTC，字面量打 UTC 标签。
// password_enc 与 smtp_password 同口径（internal/crypto 的 Crypt::encryptString 兼容层）。

import (
	"context"
	"database/sql"
	"strings"
	"time"

	lcrypto "controlhub-server/internal/crypto"
)

const smtpAccountColumns = `id, host, port, username, password_enc, encryption, from_name,
	weight, daily_limit, sent_today, last_used_at, reset_at, status, cooldown_until,
	error_count, is_default, created_at, updated_at`

func scanSMTPAccount(row interface{ Scan(...any) error }, appKey []byte) (*SMTPAccount, error) {
	var (
		a             SMTPAccount
		passwordEnc   sql.NullString
		fromName      sql.NullString
		lastUsedAt    sql.NullTime
		resetAt       sql.NullTime
		cooldownUntil sql.NullTime
		createdAt     sql.NullTime
		updatedAt     sql.NullTime
	)
	if err := row.Scan(&a.ID, &a.Host, &a.Port, &a.Username, &passwordEnc, &a.Encryption,
		&fromName, &a.Weight, &a.DailyLimit, &a.SentToday, &lastUsedAt, &resetAt,
		&a.Status, &cooldownUntil, &a.ErrorCount, &a.IsDefault, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if passwordEnc.Valid && passwordEnc.String != "" {
		if plain, err := lcrypto.DecryptString(passwordEnc.String, appKey); err == nil {
			a.Password = plain
		}
		// 解密失败保持空串：这与 decryptSmtpPassword 的「解不开就当没配」一致，
		// 号照样参与挑号，但这次发信会因认证失败被分类（并最终 cooling/disabled）。
	}
	a.FromName = nullStrPtr(fromName)
	a.LastUsedAt = nullTimePtr(lastUsedAt)
	a.ResetAt = nullTimePtr(resetAt)
	a.CooldownUntil = nullTimePtr(cooldownUntil)
	a.CreatedAt = nullTimePtr(createdAt)
	a.UpdatedAt = nullTimePtr(updatedAt)
	return &a, nil
}

// SMTPAccountCandidates 取所有「可能可用」的号：active 与 cooling（disabled 直接排除）。
//
// 刻意只做**状态**过滤：冷却是否到期、日限额是否到量、挑号优先级全部交给
// selectSMTPAccount（用调用方注入的 now 判），保证：
//   - 判定口径只有一处，SQL 与 Go 两套写法不会漂移；
//   - 冷却/日限额的边界用例可以用注入时钟精确测试（不依赖数据库时钟）。
//
// 也不在这里 ORDER BY：挑号优先级由 selectSMTPAccount 决定，SQL 顺序不应掩盖它。
// 按 id 升序返回，让结果本身确定。
func (s *mailSQLStore) SMTPAccountCandidates(ctx context.Context) ([]SMTPAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+smtpAccountColumns+" FROM smtp_accounts "+
			"WHERE status IN (?, ?) ORDER BY id ASC",
		smtpAccountStatusActive, smtpAccountStatusCooling)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SMTPAccount
	for rows.Next() {
		a, err := scanSMTPAccount(rows, s.appKey)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// SMTPAccountUsed 发信成功：sent_today+1（跨天先清零）、last_used_at=now、
// error_count 归零、cooling 复活为 active。
//
// 跨天清零用一条 SQL 表达，避免「读-改-写」竞态：
//
//	sent_today = IF(reset_at 是今天, sent_today, 0) + 1
//	reset_at   = IF(reset_at 是今天, reset_at, now)
func (s *mailSQLStore) SMTPAccountUsed(ctx context.Context, id uint64, now time.Time) error {
	now = now.UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"UPDATE smtp_accounts SET "+
			"sent_today = IF(reset_at IS NOT NULL AND DATE(reset_at) = DATE(?), sent_today, 0) + 1, "+
			"reset_at = IF(reset_at IS NOT NULL AND DATE(reset_at) = DATE(?), reset_at, ?), "+
			"last_used_at = ?, error_count = 0, "+
			"status = IF(status = ?, ?, status), cooldown_until = NULL, updated_at = ? "+
			"WHERE id = ?",
		now, now, now, now,
		smtpAccountStatusCooling, smtpAccountStatusActive,
		now, id)
	return err
}

// SMTPAccountFailed 发信失败：认证失败 → disabled；限流/临时 → cooling + 10 分钟冷却。
//
// 其它类别（收件人永久拒收 / 未识别）不会走到这里 —— 调用方不换号直接记 failed；
// 兜底成 disabled 是防御：真的写进来了，也不该让坏号继续发信。
func (s *mailSQLStore) SMTPAccountFailed(ctx context.Context, id uint64, kind smtpFailure, now time.Time) error {
	now = now.UTC().Truncate(time.Second)
	var (
		res sql.Result
		err error
	)
	switch kind {
	case smtpFailureTemp:
		res, err = s.db.ExecContext(ctx,
			"UPDATE smtp_accounts SET status = ?, cooldown_until = ?, error_count = error_count + 1, "+
				"updated_at = ? WHERE id = ?",
			smtpAccountStatusCooling, now.Add(smtpCooldown), now, id)
	case smtpFailureAuth:
		res, err = s.db.ExecContext(ctx,
			"UPDATE smtp_accounts SET status = ?, error_count = error_count + 1, updated_at = ? WHERE id = ?",
			smtpAccountStatusDisabled, now, id)
	default:
		res, err = s.db.ExecContext(ctx,
			"UPDATE smtp_accounts SET error_count = error_count + 1, updated_at = ? WHERE id = ?",
			now, id)
	}
	if err != nil {
		return err
	}
	_, _ = res.RowsAffected()
	return nil
}

// ---- 管理端 ----

// SMTPAccounts 列表（id 倒序，与其它管理端列表一致）。
func (s *mailSQLStore) SMTPAccounts(ctx context.Context) ([]SMTPAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+smtpAccountColumns+" FROM smtp_accounts ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SMTPAccount{}
	for rows.Next() {
		a, err := scanSMTPAccount(rows, s.appKey)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *mailSQLStore) SMTPAccountByID(ctx context.Context, id uint64) (*SMTPAccount, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+smtpAccountColumns+" FROM smtp_accounts WHERE id = ?", id)
	return scanSMTPAccount(row, s.appKey)
}

func (s *mailSQLStore) CreateSMTPAccount(ctx context.Context, in SMTPAccountInput, now time.Time) (*SMTPAccount, error) {
	now = now.UTC().Truncate(time.Second)
	enc, err := s.encryptSMTPPassword(derefString(in.Password))
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO smtp_accounts (host, port, username, password_enc, encryption, from_name, "+
			"weight, daily_limit, sent_today, status, cooldown_until, error_count, is_default, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, NULL, 0, 0, ?, ?)",
		derefString(in.Host), smtpIntOr(in.Port, 587), derefString(in.Username), enc,
		smtpEncryptionOr(in.Encryption), nullStrValue(in.FromName),
		smtpIntOr(in.Weight, 1), smtpIntOr(in.DailyLimit, 0),
		smtpAccountStatusActive, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.SMTPAccountByID(ctx, uint64(id))
}

// SetSMTPAccountDefault 把某个账号设为「默认优先使用」。
//
// 池里默认账号唯一：先把所有行 is_default 清零，再把目标行置 1（同一事务）。
// 传 id=0 表示「清除默认」，即恢复纯轮换。
func (s *mailSQLStore) SetSMTPAccountDefault(ctx context.Context, id uint64, now time.Time) error {
	now = now.UTC().Truncate(time.Second)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"UPDATE smtp_accounts SET is_default = 0, updated_at = ? WHERE is_default = 1", now); err != nil {
		return err
	}
	if id > 0 {
		if _, err := tx.ExecContext(ctx,
			"UPDATE smtp_accounts SET is_default = 1, updated_at = ? WHERE id = ?", now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *mailSQLStore) UpdateSMTPAccount(ctx context.Context, id uint64, in SMTPAccountInput, now time.Time) (*SMTPAccount, error) {
	now = now.UTC().Truncate(time.Second)
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.Host != nil {
		add("host", *in.Host)
	}
	if in.Port != nil {
		add("port", *in.Port)
	}
	if in.Username != nil {
		add("username", *in.Username)
	}
	if in.Password != nil && *in.Password != "" {
		enc, err := s.encryptSMTPPassword(*in.Password)
		if err != nil {
			return nil, err
		}
		add("password_enc", enc)
	}
	if in.Encryption != nil {
		add("encryption", *in.Encryption)
	}
	if in.FromName != nil {
		add("from_name", nullStrValue(in.FromName))
	}
	if in.Weight != nil {
		add("weight", *in.Weight)
	}
	if in.DailyLimit != nil {
		add("daily_limit", *in.DailyLimit)
	}
	if len(sets) == 0 {
		return s.SMTPAccountByID(ctx, id)
	}
	add("updated_at", now)
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE smtp_accounts SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	return s.SMTPAccountByID(ctx, id)
}

func (s *mailSQLStore) DeleteSMTPAccount(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM smtp_accounts WHERE id = ?", id)
	return err
}

// SetSMTPAccountStatus 状态切换（active/disabled）：改状态时顺手清冷却，
// 否则「冷却中 → 手动恢复可用」会因 cooldown_until 还在而被挑号跳过。
func (s *mailSQLStore) SetSMTPAccountStatus(ctx context.Context, id uint64, status string, now time.Time) error {
	now = now.UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"UPDATE smtp_accounts SET status = ?, cooldown_until = NULL, updated_at = ? WHERE id = ?",
		status, now, id)
	return err
}

// ResetSMTPAccountToday 手动重置今日已发与连续失败次数。
//
// 刻意**不动** status / cooldown_until / reset_at：
//   - 状态是上线与否的开关，重置计数不该顺手把冷却中的号放行；
//   - sent_today 的跨天口径由 reset_at 决定，计数归零后两种情形都读作 0
//     （reset_at 是今天 → sent_today 本身为 0；不是今天 → smtpSentToday 直接回 0），
//     改 reset_at 反而会把「今日窗口起点」抹掉。
func (s *mailSQLStore) ResetSMTPAccountToday(ctx context.Context, id uint64, now time.Time) error {
	now = now.UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"UPDATE smtp_accounts SET sent_today = 0, error_count = 0, updated_at = ? WHERE id = ?",
		now, id)
	return err
}

// encryptSMTPPassword 空串不加密（落 NULL），其余走 Laravel 兼容加密。
func (s *mailSQLStore) encryptSMTPPassword(plain string) (any, error) {
	if plain == "" {
		return nil, nil
	}
	enc, err := lcrypto.EncryptString(plain, s.appKey)
	if err != nil {
		return nil, err
	}
	return enc, nil
}

func smtpIntOr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func smtpEncryptionOr(v *string, def ...string) string {
	if v != nil && *v != "" {
		return *v
	}
	if len(def) > 0 {
		return def[0]
	}
	return "tls"
}
