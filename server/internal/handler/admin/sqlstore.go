package admin

// sqlStore 是本包 Store 的 database/sql 实现。
//
// 时间口径（contracts/data-contract.md §4）：DSN 用 parseTime=true&loc=UTC，
// 把 MySQL 返回的 datetime 字面量打上 UTC 标签、不做任何换算 —— 与 Laravel
// （app.timezone=UTC，读同一份字面量）逐字一致。严禁 SET time_zone。
//
// 加密字段（users 的代理凭据 / 支付密钥等）
// 用 internal/crypto 复刻的 Crypt::encryptString / decryptString，与 PHP 可互读。

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

)

type sqlStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLStore 用已有连接池构造 Store。
//
// 代理商城下线后不再存储任何加密凭据，构造参数只剩连接池。
func NewSQLStore(db *sql.DB) *sqlStore {
	return &sqlStore{db: db, now: time.Now}
}

func (s *sqlStore) Close() error { return nil }

// ---- 站点配置 ----

func (s *sqlStore) SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = ""
	}
	if len(keys) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(keys))
	marks := make([]string, 0, len(keys))
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

// SiteConfigSetMany 对齐 SiteConfig::setMany → updateOrCreate(['key'=>$k], ['value'=>$v])。
func (s *sqlStore) SiteConfigSetMany(ctx context.Context, kv []KV) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Truncate(time.Second)
	for _, item := range kv {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO site_configs (`key`, value, created_at, updated_at) VALUES (?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = VALUES(updated_at)",
			item.Key, nullStrValue(item.Value), now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearOrderPayURLs 清空所有订单缓存的支付链接（orders.pay_url 置 NULL），返回影响行数。
// 「允许继续支付」关掉时用：整套逻辑关闭，不留任何旧链接缓存。
func (s *sqlStore) ClearOrderPayURLs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE orders SET pay_url = NULL WHERE pay_url IS NOT NULL")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- 教程 ----

const tutorialColumns = `id, title, category, content, sort, enabled, created_at, updated_at`

func scanTutorialRow(row interface{ Scan(...any) error }) (*Tutorial, error) {
	var t Tutorial
	var enabled int
	var createdAt, updatedAt sql.NullTime
	if err := row.Scan(&t.ID, &t.Title, &t.Category, &t.Content, &t.Sort, &enabled,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	t.Enabled = enabled != 0
	t.CreatedAt = nullTimePtr(createdAt)
	t.UpdatedAt = nullTimePtr(updatedAt)
	return &t, nil
}

func (s *sqlStore) TutorialsAll(ctx context.Context) ([]Tutorial, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tutorialColumns+` FROM tutorials ORDER BY sort ASC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tutorial{}
	for rows.Next() {
		t, err := scanTutorialRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *sqlStore) TutorialByID(ctx context.Context, id uint64) (*Tutorial, error) {
	return scanTutorialRow(s.db.QueryRowContext(ctx,
		`SELECT `+tutorialColumns+` FROM tutorials WHERE id = ?`, id))
}

func (s *sqlStore) CreateTutorial(ctx context.Context, in TutorialInput) (*Tutorial, error) {
	now := s.now().UTC().Truncate(time.Second)
	cols := []string{"title", "content"}
	marks := []string{"?", "?"}
	vals := []any{in.Title, in.Content}
	add := func(col string, v any) {
		cols = append(cols, col)
		vals = append(vals, v)
		marks = append(marks, "?")
	}
	if in.Category != nil {
		add("category", *in.Category)
	}
	if in.Sort != nil {
		add("sort", *in.Sort)
	}
	if in.Enabled != nil {
		add("enabled", boolToInt(*in.Enabled))
	}
	add("created_at", now)
	add("updated_at", now)

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO tutorials ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+")",
		vals...)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.TutorialByID(ctx, uint64(id))
}

func (s *sqlStore) UpdateTutorial(ctx context.Context, id uint64, in TutorialUpdate) (*Tutorial, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.Title != nil {
		add("title", *in.Title)
	}
	if in.Category != nil {
		add("category", *in.Category)
	}
	if in.Content != nil {
		add("content", *in.Content)
	}
	if in.Sort != nil {
		add("sort", *in.Sort)
	}
	if in.Enabled != nil {
		add("enabled", boolToInt(*in.Enabled))
	}
	add("updated_at", s.now().UTC().Truncate(time.Second))
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE tutorials SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	return s.TutorialByID(ctx, id)
}

func (s *sqlStore) DeleteTutorial(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM tutorials WHERE id = ?", id)
	return err
}

// ---- 优惠码 ----

const discountColumns = `c.id, c.code, c.source, c.user_id, u.email, c.discount, c.max_uses,
	c.max_uses_per_user, c.used_count, c.expires_at, c.note, c.status, c.plan_id, p.name,
	c.folder_id, c.created_at`

const discountFrom = ` FROM discount_codes c
	LEFT JOIN users u ON u.id = c.user_id
	LEFT JOIN plans p ON p.id = c.plan_id`

// discountNotDeleted 是「软删除过滤」：已删（status='deleted'）的码不对后台展示。
// 拼在 discountFrom 之后与其它 WHERE 条件合并使用（各查询自己接 WHERE/AND 前缀）。
const discountNotDeleted = "c.status <> 'deleted'"

func scanDiscount(row interface{ Scan(...any) error }) (*DiscountRow, error) {
	var d DiscountRow
	var userID, planID, folderID sql.NullInt64
	var userEmail, note, planName sql.NullString
	var discount []byte
	var expiresAt, createdAt sql.NullTime
	if err := row.Scan(&d.ID, &d.Code, &d.Source, &userID, &userEmail, &discount,
		&d.MaxUses, &d.MaxUsesPerUser, &d.UsedCount, &expiresAt, &note, &d.Status,
		&planID, &planName, &folderID, &createdAt); err != nil {
		return nil, err
	}
	d.UserID = nullUintPtr(userID)
	d.UserEmail = nullStrPtr(userEmail)
	d.Discount = string(discount)
	d.ExpiresAt = nullTimePtr(expiresAt)
	d.Note = nullStrPtr(note)
	d.PlanID = nullUintPtr(planID)
	d.PlanName = nullStrPtr(planName)
	d.FolderID = nullUintPtr(folderID)
	d.CreatedAt = nullTimePtr(createdAt)
	return &d, nil
}

// discountEffectiveStatusSQL 是「有效状态」在 SQL 侧的等价表达式，必须与
// internal/discount.EffectiveStatus() 逐字同判据（改一处必须同步另一处，否则列表与筛选会打架）。
//
//   - status 非 active → 原样（终态：used_up / refreshed / expired）
//   - active 且 expires_at 已到/已过 ?（now）→ 'expired'
//   - active 且 max_uses>0 且 used_count>=max_uses → 'used_up'
//   - 其余 → 'active'
//
// expires_at / used_count 均为非空列语义（active 分支里 expires_at 为 NULL 时
// "NULL <= ?" 为 NULL → 落到 ELSE，正合「永久有效」）。
const discountEffectiveStatusSQL = `(CASE
	WHEN c.status <> 'active' THEN c.status
	WHEN c.expires_at IS NOT NULL AND c.expires_at <= ? THEN 'expired'
	WHEN c.max_uses > 0 AND c.used_count >= c.max_uses THEN 'used_up'
	ELSE 'active'
END)`

// discountWhere 构造筛选条件（list 与 folders 计数共用以避免两处走偏）。
//
// 状态筛选走「有效状态」现算，不读库里那个会漂移的 c.status：
// 过期不靠定时任务回写，到点即按 expires_at 判成 expired。
// 先 append 状态 CASE 的 now 参数、再 append 其它条件参数，保证占位符顺序一致。
//
// 软删除：无条件先加 `c.status <> 'deleted'`，保证已删的码不出现在列表/计数里。
func discountWhere(f DiscountFilter, now time.Time) (string, []any) {
	conds := []string{discountNotDeleted}
	args := []any{}
	if f.Status != nil {
		conds = append(conds, discountEffectiveStatusSQL+" = ?")
		args = append(args, now.UTC(), *f.Status)
	}
	if f.Source != nil {
		conds = append(conds, "c.source = ?")
		args = append(args, *f.Source)
	}
	if f.User != nil {
		// 按持有用户邮箱模糊搜。LEFT JOIN 下 user_id 为 null 的管理员码天然不匹配。
		conds = append(conds, "u.email LIKE ?")
		args = append(args, "%"+*f.User+"%")
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s *sqlStore) DiscountPage(ctx context.Context, f DiscountFilter, folderID *uint64, page, perPage int64) ([]DiscountRow, int64, error) {
	where, args := discountWhere(f, s.now())
	if folderID != nil {
		where += andOrWhere(where) + "c.folder_id = ?"
		args = append(args, *folderID)
	} else {
		where += andOrWhere(where) + "c.folder_id IS NULL"
	}

	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*)"+discountFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	if total == 0 {
		return []DiscountRow{}, 0, nil
	}
	// ⚠️ 与 successPage 不同：DiscountCodeController::index 直接 forPage($page, $perPage)，
	// 页码**不做夹取**（?page=999 返回空列表、但 total/folders 照常给）。见金样本
	// discount_page_overflow：{"list":[],"total":16,"page":999}。这里必须保持一致。
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	rows, err := s.db.QueryContext(ctx,
		"SELECT "+discountColumns+discountFrom+where+" ORDER BY c.id DESC LIMIT ? OFFSET ?",
		append(args, perPage, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []DiscountRow{}
	for rows.Next() {
		d, err := scanDiscount(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *d)
	}
	return out, total, rows.Err()
}

func (s *sqlStore) DiscountFolderCounts(ctx context.Context, f DiscountFilter) ([]FolderCount, error) {
	where, args := discountWhere(f, s.now())
	// withCount 的口径：按 folder_id 分组计数，文件夹本身单独排序。
	query := "SELECT c.folder_id, COUNT(*)" + discountFrom + where
	if where == "" {
		query += " WHERE c.folder_id IS NOT NULL"
	} else {
		query += " AND c.folder_id IS NOT NULL"
	}
	query += " GROUP BY c.folder_id"

	counts := map[uint64]int64{}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var folderID sql.NullInt64
		var n int64
		if err := rows.Scan(&folderID, &n); err != nil {
			return nil, err
		}
		if folderID.Valid {
			counts[uint64(folderID.Int64)] = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frows, err := s.db.QueryContext(ctx, "SELECT id, name FROM discount_code_folders ORDER BY name ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	out := []FolderCount{}
	for frows.Next() {
		var fc FolderCount
		if err := frows.Scan(&fc.ID, &fc.Name); err != nil {
			return nil, err
		}
		fc.CodeCount = counts[fc.ID]
		out = append(out, fc)
	}
	return out, frows.Err()
}

// DiscountByIDOrCode 对齐 resolveCode：id 优先，其次大写去空白的 code。
func (s *sqlStore) DiscountByIDOrCode(ctx context.Context, raw string) (*DiscountRow, error) {
	var (
		d   *DiscountRow
		err error
	)
	if id, convErr := strconv.ParseUint(raw, 10, 64); convErr == nil && id > 0 {
		d, err = s.discountWhere1(ctx, "c.id = ?", id)
		if err == nil && d != nil {
			return d, nil
		}
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
	}
	d, err = s.discountWhere1(ctx, "c.code = ?", phpStrToUpper(phpTrimSpaceOnly(raw)))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *sqlStore) discountWhere1(ctx context.Context, cond string, arg any) (*DiscountRow, error) {
	return scanDiscount(s.db.QueryRowContext(ctx,
		"SELECT "+discountColumns+discountFrom+" WHERE "+cond+" AND "+discountNotDeleted+" LIMIT 1", arg))
}

func (s *sqlStore) DiscountsByIDs(ctx context.Context, ids []uint64) (map[uint64]*DiscountRow, error) {
	out := map[uint64]*DiscountRow{}
	if len(ids) == 0 {
		return out, nil
	}
	marks := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		marks = append(marks, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+discountColumns+" FROM discount_codes c"+
			" LEFT JOIN users u ON u.id = c.user_id"+
			" LEFT JOIN plans p ON p.id = c.plan_id"+
			" WHERE c.id IN ("+strings.Join(marks, ",")+") AND "+discountNotDeleted, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDiscount(rows)
		if err != nil {
			return nil, err
		}
		out[d.ID] = d
	}
	return out, rows.Err()
}

func (s *sqlStore) ExistingCodes(ctx context.Context, codes []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if len(codes) == 0 {
		return out, nil
	}
	marks := make([]string, 0, len(codes))
	args := make([]any, 0, len(codes))
	for _, c := range codes {
		marks = append(marks, "?")
		args = append(args, c)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT code FROM discount_codes WHERE code IN ("+strings.Join(marks, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out[code] = struct{}{}
	}
	return out, rows.Err()
}

// InsertDiscountCodes 对齐批量 insert（整批一个事务，分块 100）。
func (s *sqlStore) InsertDiscountCodes(ctx context.Context, rows []NewDiscountCode) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const batch = 100
	for start := 0; start < len(rows); start += batch {
		end := start + batch
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		cols := `code, source, user_id, discount, max_uses, used_count, max_uses_per_user,
			expires_at, note, status, plan_id, folder_id, created_at, updated_at`
		placeholders := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*14)
		for _, r := range chunk {
			placeholders = append(placeholders, "(?, 'admin', NULL, ?, ?, 0, ?, ?, ?, 'active', ?, ?, ?, ?)")
			args = append(args,
				r.Code, r.Discount, r.MaxUses, r.MaxUsesPerUser,
				nullTimeValue(r.ExpiresAt), nullStrValue(r.Note),
				nullUintValue(r.PlanID), nullUintValue(r.FolderID),
				r.CreatedAt, r.CreatedAt)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO discount_codes ("+cols+") VALUES "+strings.Join(placeholders, ", "), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqlStore) UpdateDiscount(ctx context.Context, id uint64, in DiscountUpdate) error {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.Discount != nil {
		add("discount", *in.Discount)
	}
	if in.MaxUses != nil {
		add("max_uses", *in.MaxUses)
	}
	if in.MaxUsesPerUser != nil {
		add("max_uses_per_user", *in.MaxUsesPerUser)
	}
	if in.ExpiresAtSet {
		add("expires_at", nullTimeValue(in.ExpiresAt))
	}
	if in.NoteSet {
		add("note", nullStrValue(in.Note))
	}
	if in.Status != nil {
		add("status", *in.Status)
	}
	if in.PlanIDSet {
		add("plan_id", nullUintValue(in.PlanID))
	}
	if len(sets) == 0 {
		return nil
	}
	add("updated_at", s.now().UTC().Truncate(time.Second))
	args = append(args, id)
	_, err := s.db.ExecContext(ctx,
		"UPDATE discount_codes SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

// DeleteDiscount 软删除：把码置为 status='deleted'，行保留。
//
// 为什么不物理删：订单 orders.discount_code_id 逻辑引用着码行（无外键），物理删会让
// 历史订单「用了哪个码」永远查不到（悬空）。软删后行还在 → 订单可反查码文本；
// 同时 status='deleted' 在凭码下单的校验里本就不是 'active'，天然买不了。
// 不用加列/加表（用户要求，控库体积）：只复用既有 status 列，'deleted' 在 varchar(16) 内。
func (s *sqlStore) DeleteDiscount(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE discount_codes SET status = 'deleted', updated_at = ? WHERE id = ?",
		s.now().UTC().Truncate(time.Second), id)
	return err
}

func (s *sqlStore) FolderCreate(ctx context.Context, name string) (*Folder, error) {
	now := s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO discount_code_folders (name, created_at, updated_at) VALUES (?, ?, ?)",
		name, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Folder{ID: uint64(id), Name: name}, nil
}

func (s *sqlStore) FolderByID(ctx context.Context, id uint64) (*Folder, error) {
	var f Folder
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name FROM discount_code_folders WHERE id = ?", id).Scan(&f.ID, &f.Name)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *sqlStore) FolderByName(ctx context.Context, name string) (*Folder, error) {
	var f Folder
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name FROM discount_code_folders WHERE name = ? ORDER BY id LIMIT 1", name).
		Scan(&f.ID, &f.Name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *sqlStore) FolderUpdate(ctx context.Context, id uint64, name string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE discount_code_folders SET name = ?, updated_at = ? WHERE id = ?",
		name, s.now().UTC().Truncate(time.Second), id)
	return err
}

func (s *sqlStore) FolderDelete(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM discount_code_folders WHERE id = ?", id)
	return err
}

func (s *sqlStore) DetachCodesFromFolder(ctx context.Context, folderID uint64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE discount_codes SET folder_id = NULL, updated_at = ? WHERE folder_id = ?",
		s.now().UTC().Truncate(time.Second), folderID)
	return err
}

func (s *sqlStore) DiscountCodesInFolder(ctx context.Context, folderID uint64) ([]DiscountRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+discountColumns+discountFrom+" WHERE c.folder_id = ? AND "+discountNotDeleted+
			" ORDER BY c.id DESC", folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiscountRow{}
	for rows.Next() {
		d, err := scanDiscount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ---- 库存（条件更新，并发不超卖）----

// ---- 互斥锁（对齐 Cache::lock 的 database 实现：cache_locks 表）----

// LockAcquire 尝试取锁：先删过期行再 INSERT，主键冲突 = 已被别人持有。
func (s *sqlStore) LockAcquire(ctx context.Context, name string, seconds int64, owner string) (bool, error) {
	now := s.now().Unix()
	expiresAt := now + seconds

	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM cache_locks WHERE `key` = ? AND expiration <= ?", name, now); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT IGNORE INTO cache_locks (`key`, owner, expiration) VALUES (?, ?, ?)",
		name, owner, expiresAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *sqlStore) LockRelease(ctx context.Context, name, owner string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM cache_locks WHERE `key` = ? AND owner = ?", name, owner)
	return err
}

// ---- 通用存在性 ----

// ExistsValue 实现 exists:table,column 规则（只允许白名单表，避免 SQL 注入面）。
func (s *sqlStore) ExistsValue(ctx context.Context, table, column string, value any) (bool, error) {
	allowed := map[string]map[string]bool{
		"users":                 {"id": true},
		"plans":                 {"id": true},
		"discount_code_folders": {"id": true},
	}
	columns, ok := allowed[table]
	if !ok || !columns[column] {
		return false, fmt.Errorf("admin: exists 规则不允许的表 %q.%q", table, column)
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `"+table+"` WHERE `"+column+"` = ?", phpScalarString(value)).Scan(&n)
	return n > 0, err
}

func (s *sqlStore) UserExists(ctx context.Context, id uint64) (bool, error) {
	return s.ExistsValue(ctx, "users", "id", id)
}

func (s *sqlStore) PlanExists(ctx context.Context, id uint64) (bool, error) {
	return s.ExistsValue(ctx, "plans", "id", id)
}

func (s *sqlStore) UserEmail(ctx context.Context, id uint64) (*string, error) {
	var email sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT email FROM users WHERE id = ?", id).Scan(&email); err != nil {
		return nil, err
	}
	return nullStrPtr(email), nil
}

// ---- 小工具 ----

func nullStrPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullStrValue(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func nullTimeValue(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullUintPtr(v sql.NullInt64) *uint64 {
	if !v.Valid {
		return nil
	}
	u := uint64(v.Int64)
	return &u
}

func nullUintValue(v *uint64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullIntPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// andOrWhere 拼接条件：已有条件时用 " AND "，否则用 " WHERE "。
func andOrWhere(existing string) string {
	if existing == "" {
		return " WHERE "
	}
	return " AND "
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
