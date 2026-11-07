package admin

// balanceStore 是 BalanceStore 的 database/sql 实现。
//
// 时间口径与全仓一致（data-contract §4）：DSN 已 parseTime=true&loc=UTC，
// 这里只写 UTC 字面量、不做时区换算。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// NewBalanceSQLStore 用已有连接池构造余额域 Store。
func NewBalanceSQLStore(db *sql.DB) BalanceStore {
	return &balanceSQLStore{db: db}
}

type balanceSQLStore struct {
	db *sql.DB
}

const giftcardColumns = `id, code, name, value, status, used_by, used_at,
	expires_at, created_by, note, folder_id, created_at, updated_at`

// GiftcardPage 返回当前页兑换码与总数。
//
// 归档口径对齐优惠码：FolderID=nil → 只出未归档（folder_id IS NULL）；
// FolderID 指向某 id → 只出该文件夹内的码。ValueOrder 仅影响排序（白名单）。
func (s *balanceSQLStore) GiftcardPage(ctx context.Context, f GiftcardFilter, page, perPage int64) ([]Giftcard, int64, error) {
	conds := []string{}
	args := []any{}
	if f.Status != nil {
		conds = append(conds, "status = ?")
		args = append(args, *f.Status)
	}
	if f.FolderID != nil {
		conds = append(conds, "folder_id = ?")
		args = append(args, *f.FolderID)
	} else {
		conds = append(conds, "folder_id IS NULL")
	}
	where := " WHERE " + strings.Join(conds, " AND ")

	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM giftcards"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []Giftcard{}, 0, nil
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = giftcardPerPage
	}
	offset := (page - 1) * perPage

	rows, err := s.db.QueryContext(ctx,
		"SELECT "+giftcardColumns+" FROM giftcards"+where+giftcardValueOrderSQL(f.ValueOrder)+" LIMIT ? OFFSET ?",
		append(args, perPage, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Giftcard{}
	for rows.Next() {
		g, err := scanGiftcard(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *g)
	}
	return out, total, rows.Err()
}

// GiftcardByID 取单个兑换码（查无此行时返回 sql.ErrNoRows）。
func (s *balanceSQLStore) GiftcardByID(ctx context.Context, id uint64) (*Giftcard, error) {
	return scanGiftcard(s.db.QueryRowContext(ctx,
		"SELECT "+giftcardColumns+" FROM giftcards WHERE id = ?", id))
}

// InsertGiftcards 整批落库（一个事务，要么全成功要么全不写）。
//
// 码撞唯一键（error 1062）时返回 ErrGiftcardCodeTaken，由 handler 整批重生成。
func (s *balanceSQLStore) InsertGiftcards(ctx context.Context, rows []NewGiftcard) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	placeholders := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*10)
	for _, r := range rows {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			r.Code, r.Name, r.Value, GiftcardStatusActive,
			nullTimeValue(r.ExpiresAt), nullUintValue(r.CreatedBy),
			nullStrValue(r.Note), nullUintValue(r.FolderID), r.CreatedAt, r.CreatedAt)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO giftcards (code, name, value, status, expires_at, created_by, note, folder_id, created_at, updated_at)
		 VALUES `+strings.Join(placeholders, ", "), args...)
	if err != nil {
		if isDuplicateKey(err) {
			return ErrGiftcardCodeTaken
		}
		return err
	}
	return tx.Commit()
}

// DeleteGiftcard 物理删除兑换码（任意状态），返回是否删到了行。
func (s *balanceSQLStore) DeleteGiftcard(ctx context.Context, id uint64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM giftcards WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UserBalance 取用户当前余额（分）；用户不存在返回 ErrUserNotFound。
func (s *balanceSQLStore) UserBalance(ctx context.Context, userID uint64) (int64, error) {
	var balance int64
	err := s.db.QueryRowContext(ctx, "SELECT balance FROM users WHERE id = ?", userID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, err
	}
	return balance, nil
}

// UserBalanceLogs 返回当前页流水（id 倒序）与总数。
func (s *balanceSQLStore) UserBalanceLogs(ctx context.Context, userID uint64, page, perPage int64) ([]BalanceLog, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM balance_logs WHERE user_id = ?", userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []BalanceLog{}, 0, nil
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = balanceLogPerPage
	}
	offset := (page - 1) * perPage

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, amount, type, order_id, giftcard_id, operator_id,
		        balance_after, note, created_at
		 FROM balance_logs WHERE user_id = ?
		 ORDER BY id DESC LIMIT ? OFFSET ?`, userID, perPage, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []BalanceLog{}
	for rows.Next() {
		l, err := scanBalanceLog(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *l)
	}
	return out, total, rows.Err()
}

// AdjustUserBalance 事务内加锁改余额并写流水。
//
// 步骤与 payment_sqlstore.go 的 ConsumeDiscountCode 同一模式：
// 开事务 → SELECT balance FOR UPDATE（锁住这一行）→ 算新余额 → 校验不为负 →
// UPDATE users → INSERT balance_logs（amount 与本次变动额一致）→ 提交。
// 校验失败一律回滚，不留半条流水。
func (s *balanceSQLStore) AdjustUserBalance(ctx context.Context, in BalanceAdjust) (*BalanceAdjustment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var current int64
	err = tx.QueryRowContext(ctx, "SELECT balance FROM users WHERE id = ? FOR UPDATE", in.UserID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}

	next := current + in.Amount
	if next < 0 {
		return nil, ErrBalanceNotEnough
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET balance = ?, updated_at = ? WHERE id = ?", next, in.Now, in.UserID); err != nil {
		return nil, err
	}

	log := buildBalanceLog(in.UserID, in.Amount, BalanceTypeAdmin, in.OperatorID, next, in.Note, in.Now)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO balance_logs (user_id, amount, type, order_id, giftcard_id, operator_id,
		                           balance_after, note, created_at)
		 VALUES (?, ?, ?, NULL, NULL, ?, ?, ?, ?)`,
		log.UserID, log.Amount, log.Type, nullUintValue(log.OperatorID),
		log.BalanceAfter, nullStrValue(log.Note), in.Now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &BalanceAdjustment{Balance: next, Log: log}, nil
}

// ---- 内部 ----

// buildBalanceLog 组装一次余额变动的流水行。
//
// amount 必须**原样**记录本次变动额（正=加 / 负=扣），balance_after 是变动后余额：
// 这两列是后台对账的唯一依据，任何「记 0 」「记绝对值」的改动都是错的。
func buildBalanceLog(userID uint64, amount int64, logType string, operatorID *uint64, balanceAfter int64, note *string, at time.Time) *BalanceLog {
	createdAt := at
	return &BalanceLog{
		UserID:       userID,
		Amount:       amount,
		Type:         logType,
		OperatorID:   operatorID,
		BalanceAfter: balanceAfter,
		Note:         note,
		CreatedAt:    &createdAt,
	}
}

func scanGiftcard(row interface{ Scan(...any) error }) (*Giftcard, error) {
	var g Giftcard
	var usedBy, createdBy, folderID sql.NullInt64
	var usedAt, expiresAt, createdAt, updatedAt sql.NullTime
	var note sql.NullString
	if err := row.Scan(&g.ID, &g.Code, &g.Name, &g.Value, &g.Status,
		&usedBy, &usedAt, &expiresAt, &createdBy, &note, &folderID, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	g.UsedBy = nullUintPtr(usedBy)
	g.UsedAt = nullTimePtr(usedAt)
	g.ExpiresAt = nullTimePtr(expiresAt)
	g.CreatedBy = nullUintPtr(createdBy)
	g.Note = nullStrPtr(note)
	g.FolderID = nullUintPtr(folderID)
	g.CreatedAt = nullTimePtr(createdAt)
	g.UpdatedAt = nullTimePtr(updatedAt)
	return &g, nil
}

// —— 兑换码文件夹（对齐优惠码 folder 口径）——

// GiftcardFolderCounts 返回全部文件夹 + 当前筛选条件下各文件夹的码数。
func (s *balanceSQLStore) GiftcardFolderCounts(ctx context.Context, f GiftcardFilter) ([]GiftcardFolder, error) {
	// 计数只按「状态」这条硬筛选算（folder 条件本身在按 folder_id 分组时无意义）。
	countWhere := ""
	countArgs := []any{}
	if f.Status != nil {
		countWhere = " AND status = ?"
		countArgs = append(countArgs, *f.Status)
	}
	counts := map[uint64]int64{}
	crows, err := s.db.QueryContext(ctx,
		"SELECT folder_id, COUNT(*) FROM giftcards WHERE folder_id IS NOT NULL"+countWhere+
			" GROUP BY folder_id", countArgs...)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var fid sql.NullInt64
		var n int64
		if err := crows.Scan(&fid, &n); err != nil {
			crows.Close()
			return nil, err
		}
		if fid.Valid {
			counts[uint64(fid.Int64)] = n
		}
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, err
	}
	crows.Close()

	frows, err := s.db.QueryContext(ctx,
		"SELECT id, name FROM giftcard_folders ORDER BY name ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	out := []GiftcardFolder{}
	for frows.Next() {
		var fc GiftcardFolder
		if err := frows.Scan(&fc.ID, &fc.Name); err != nil {
			return nil, err
		}
		fc.CodeCount = counts[fc.ID]
		out = append(out, fc)
	}
	return out, frows.Err()
}

// GiftcardFolderByID 取单个文件夹（查无此行返回 sql.ErrNoRows）。
func (s *balanceSQLStore) GiftcardFolderByID(ctx context.Context, id uint64) (*GiftcardFolder, error) {
	var f GiftcardFolder
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name FROM giftcard_folders WHERE id = ?", id).Scan(&f.ID, &f.Name)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// GiftcardFolderByName 按名字取文件夹（不存在返回 nil, nil）。
func (s *balanceSQLStore) GiftcardFolderByName(ctx context.Context, name string) (*GiftcardFolder, error) {
	var f GiftcardFolder
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name FROM giftcard_folders WHERE name = ? ORDER BY id LIMIT 1", name).
		Scan(&f.ID, &f.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// GiftcardFolderCreate 新建文件夹。
func (s *balanceSQLStore) GiftcardFolderCreate(ctx context.Context, name string) (*GiftcardFolder, error) {
	now := s.nowUTC()
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO giftcard_folders (name, created_at, updated_at) VALUES (?, ?, ?)",
		name, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &GiftcardFolder{ID: uint64(id), Name: name}, nil
}

// GiftcardFolderUpdate 文件夹改名。
func (s *balanceSQLStore) GiftcardFolderUpdate(ctx context.Context, id uint64, name string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE giftcard_folders SET name = ?, updated_at = ? WHERE id = ?",
		name, s.nowUTC(), id)
	return err
}

// GiftcardFolderDelete 删文件夹本身（调用方负责先摘码）。
func (s *balanceSQLStore) GiftcardFolderDelete(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM giftcard_folders WHERE id = ?", id)
	return err
}

// DetachGiftcardsFromFolder 把该文件夹内的码退回「未归档」。
func (s *balanceSQLStore) DetachGiftcardsFromFolder(ctx context.Context, folderID uint64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE giftcards SET folder_id = NULL, updated_at = ? WHERE folder_id = ?",
		s.nowUTC(), folderID)
	return err
}

// GiftcardIDsInFolder 取该文件夹内所有码的 id。
func (s *balanceSQLStore) GiftcardIDsInFolder(ctx context.Context, folderID uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id FROM giftcards WHERE folder_id = ? ORDER BY id DESC", folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// nowUTC 与全仓时间口径一致（UTC 截到秒）。
func (s *balanceSQLStore) nowUTC() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

func scanBalanceLog(row interface{ Scan(...any) error }) (*BalanceLog, error) {
	var l BalanceLog
	var orderID, giftcardID, operatorID sql.NullInt64
	var note sql.NullString
	var createdAt sql.NullTime
	if err := row.Scan(&l.ID, &l.UserID, &l.Amount, &l.Type, &orderID, &giftcardID,
		&operatorID, &l.BalanceAfter, &note, &createdAt); err != nil {
		return nil, err
	}
	l.OrderID = nullUintPtr(orderID)
	l.GiftcardID = nullUintPtr(giftcardID)
	l.OperatorID = nullUintPtr(operatorID)
	l.Note = nullStrPtr(note)
	l.CreatedAt = nullTimePtr(createdAt)
	return &l, nil
}

// isDuplicateKey 判定 MySQL「唯一键冲突」（error 1062）。
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
