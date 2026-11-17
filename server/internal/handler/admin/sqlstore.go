package admin

// sqlStore 是本包 Store 的 database/sql 实现。
//
// 时间口径（contracts/data-contract.md §4）：DSN 用 parseTime=true&loc=UTC，
// 把 MySQL 返回的 datetime 字面量打上 UTC 标签、不做任何换算 —— 与 Laravel
// （app.timezone=UTC，读同一份字面量）逐字一致。严禁 SET time_zone。
//
// 加密字段（proxy_socks.password / proxy_accounts.password / proxy_providers.api_token）
// 用 internal/crypto 复刻的 Crypt::encryptString / decryptString，与 PHP 可互读。

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/crypto"
	"controlhub-server/internal/sockpool"
)

type sqlStore struct {
	db     *sql.DB
	appKey []byte
	now    func() time.Time
}

// NewSQLStore 用已有连接池构造 Store。appKey 用于解密/加密上游凭据。
func NewSQLStore(db *sql.DB, appKey []byte) Store {
	return &sqlStore{db: db, appKey: appKey, now: time.Now}
}

func (s *sqlStore) Close() error { return nil }

// ---- 加密小工具 ----

func (s *sqlStore) encrypt(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	return crypto.EncryptString(v, s.appKey)
}

// decryptOrRaw 解密失败时返回原文（与 Laravel accessor 在存量明文下的宽松表现一致）。
func (s *sqlStore) decryptOrRaw(v sql.NullString) *string {
	if !v.Valid || v.String == "" {
		return nil
	}
	plain, err := crypto.DecryptString(v.String, s.appKey)
	if err != nil {
		raw := v.String
		return &raw
	}
	return &plain
}

// ---- 商品与规格 ----

func (s *sqlStore) ProductsWithSKUs(ctx context.Context) ([]Product, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, subtitle, description, image_url,
		is_active, sort_order, created_at FROM proxy_products
		ORDER BY sort_order ASC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	products := []Product{}
	index := map[uint64]int{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		index[p.ID] = len(products)
		products = append(products, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	skuRows, err := s.db.QueryContext(ctx, `SELECT `+skuColumns+` FROM proxy_skus
		ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer skuRows.Close()
	for skuRows.Next() {
		sk, err := scanSKU(skuRows)
		if err != nil {
			return nil, err
		}
		if i, ok := index[sk.ProductID]; ok {
			products[i].SKUs = append(products[i].SKUs, *sk)
		}
	}
	return products, skuRows.Err()
}

func scanProduct(row interface{ Scan(...any) error }) (*Product, error) {
	var p Product
	var subtitle, description, imageURL sql.NullString
	var isActive int
	var createdAt sql.NullTime
	if err := row.Scan(&p.ID, &p.Name, &subtitle, &description, &imageURL,
		&isActive, &p.SortOrder, &createdAt); err != nil {
		return nil, err
	}
	p.Subtitle = nullStrPtr(subtitle)
	p.Description = nullStrPtr(description)
	p.ImageURL = nullStrPtr(imageURL)
	p.IsActive = isActive != 0
	p.CreatedAt = nullTimePtr(createdAt)
	return &p, nil
}

const skuColumns = `id, product_id, name, source_type, allocation, source_id, region,
	traffic_gb, duration_days, max_thread_count, price, cost_price, stock_total,
	stock_locked, stock_sold, is_active, sort_order`

func scanSKU(row interface{ Scan(...any) error }) (*SKU, error) {
	var sk SKU
	var sourceType, allocation, region sql.NullString
	var sourceID sql.NullInt64
	var price, costPrice []byte
	var isActive int
	if err := row.Scan(&sk.ID, &sk.ProductID, &sk.Name, &sourceType, &allocation, &sourceID, &region,
		&sk.TrafficGB, &sk.DurationDays, &sk.MaxThreadCount, &price, &costPrice,
		&sk.StockTotal, &sk.StockLocked, &sk.StockSold, &isActive, &sk.SortOrder); err != nil {
		return nil, err
	}
	sk.SourceType = nullStrPtr(sourceType)
	sk.Allocation = nullStrPtr(allocation)
	sk.SourceID = nullUintPtr(sourceID)
	sk.Region = nullStrPtr(region)
	sk.Price = string(price)
	sk.CostPrice = string(costPrice)
	sk.IsActive = isActive != 0
	return &sk, nil
}

func (s *sqlStore) ProductByID(ctx context.Context, id uint64) (*Product, error) {
	p, err := scanProduct(s.db.QueryRowContext(ctx, `SELECT id, name, subtitle, description, image_url,
		is_active, sort_order, created_at FROM proxy_products WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	skus, err := s.skusOfProduct(ctx, id)
	if err != nil {
		return nil, err
	}
	p.SKUs = skus
	return p, nil
}

func (s *sqlStore) skusOfProduct(ctx context.Context, productID uint64) ([]SKU, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+skuColumns+` FROM proxy_skus
		WHERE product_id = ? ORDER BY sort_order ASC, id ASC`, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SKU{}
	for rows.Next() {
		sk, err := scanSKU(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sk)
	}
	return out, rows.Err()
}

func (s *sqlStore) CreateProduct(ctx context.Context, in ProductInput) (*Product, error) {
	now := s.now().UTC().Truncate(time.Second)
	cols := []string{"name"}
	vals := []any{in.Name}
	marks := []string{"?"}
	add := func(col string, v any) {
		cols = append(cols, col)
		vals = append(vals, v)
		marks = append(marks, "?")
	}
	if in.Subtitle != nil {
		add("subtitle", *in.Subtitle)
	}
	if in.Description != nil {
		add("description", *in.Description)
	}
	if in.ImageURL != nil {
		add("image_url", *in.ImageURL)
	}
	if in.IsActive != nil {
		add("is_active", boolToInt(*in.IsActive))
	}
	if in.SortOrder != nil {
		add("sort_order", *in.SortOrder)
	}
	add("created_at", now)
	add("updated_at", now)

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO proxy_products ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+")",
		vals...)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.ProductByID(ctx, uint64(id))
}

func (s *sqlStore) UpdateProduct(ctx context.Context, id uint64, in ProductInput) (*Product, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	// 只有请求里出现的键才更新（对齐 fill($data)->save()）。
	if in.Subtitle != nil {
		add("subtitle", *in.Subtitle)
	}
	if in.Description != nil {
		add("description", *in.Description)
	}
	if in.ImageURL != nil {
		add("image_url", *in.ImageURL)
	}
	if in.IsActive != nil {
		add("is_active", boolToInt(*in.IsActive))
	}
	if in.SortOrder != nil {
		add("sort_order", *in.SortOrder)
	}
	if len(sets) > 0 {
		add("updated_at", s.now().UTC().Truncate(time.Second))
		args = append(args, id)
		if _, err := s.db.ExecContext(ctx,
			"UPDATE proxy_products SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return nil, err
		}
	}
	return s.ProductByID(ctx, id)
}

func (s *sqlStore) ProductHasSoldSKU(ctx context.Context, id uint64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_skus WHERE product_id = ? AND stock_sold > 0", id).Scan(&n)
	return n > 0, err
}

func (s *sqlStore) DeleteProduct(ctx context.Context, id uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM proxy_skus WHERE product_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM proxy_products WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqlStore) SKUByID(ctx context.Context, id uint64) (*SKU, error) {
	return scanSKU(s.db.QueryRowContext(ctx, `SELECT `+skuColumns+` FROM proxy_skus WHERE id = ?`, id))
}

func (s *sqlStore) CreateSKU(ctx context.Context, productID uint64, in SKUInput) (*SKU, error) {
	now := s.now().UTC().Truncate(time.Second)
	cols := []string{"product_id", "name", "duration_days", "price"}
	marks := []string{"?", "?", "?", "?"}
	vals := []any{productID, in.Name, in.DurationDays, in.Price}
	add := func(col string, v any) {
		cols = append(cols, col)
		vals = append(vals, v)
		marks = append(marks, "?")
	}
	if in.SourceType != nil {
		add("source_type", *in.SourceType)
	}
	if in.Allocation != nil {
		add("allocation", *in.Allocation)
	}
	if in.SourceID != nil {
		add("source_id", *in.SourceID)
	}
	if in.Region != nil {
		add("region", *in.Region)
	}
	if in.TrafficGB != nil {
		add("traffic_gb", *in.TrafficGB)
	}
	if in.MaxThreadCount != nil {
		add("max_thread_count", *in.MaxThreadCount)
	}
	if in.CostPrice != nil {
		add("cost_price", *in.CostPrice)
	}
	if in.StockTotal != nil {
		add("stock_total", *in.StockTotal)
	}
	if in.IsActive != nil {
		add("is_active", boolToInt(*in.IsActive))
	}
	if in.SortOrder != nil {
		add("sort_order", *in.SortOrder)
	}
	add("created_at", now)
	add("updated_at", now)

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO proxy_skus ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+")",
		vals...)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.SKUByID(ctx, uint64(id))
}

func (s *sqlStore) UpdateSKU(ctx context.Context, id uint64, in SKUInput) (*SKU, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.SourceType != nil {
		add("source_type", *in.SourceType)
	}
	if in.Allocation != nil {
		add("allocation", *in.Allocation)
	}
	if in.SourceID != nil {
		add("source_id", *in.SourceID)
	} else if in.ClearSourceID {
		// 显式传 null（独占池「不指定」）：真的清空，而不是保持原值。
		sets = append(sets, "source_id = NULL")
	}
	if in.Region != nil {
		add("region", *in.Region)
	}
	if in.TrafficGB != nil {
		add("traffic_gb", *in.TrafficGB)
	}
	if in.MaxThreadCount != nil {
		add("max_thread_count", *in.MaxThreadCount)
	}
	if in.CostPrice != nil {
		add("cost_price", *in.CostPrice)
	}
	if in.StockTotal != nil {
		add("stock_total", *in.StockTotal)
	}
	if in.IsActive != nil {
		add("is_active", boolToInt(*in.IsActive))
	}
	if in.SortOrder != nil {
		add("sort_order", *in.SortOrder)
	}
	// name 与 price 是必填规则，validate 通过时必然存在。
	add("name", in.Name)
	add("duration_days", in.DurationDays)
	add("price", in.Price)

	add("updated_at", s.now().UTC().Truncate(time.Second))
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE proxy_skus SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	return s.SKUByID(ctx, id)
}

func (s *sqlStore) DeleteSKU(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM proxy_skus WHERE id = ?", id)
	return err
}

// ---- 手工 socks ----

const sockColumns = `id, name, host, port, username, password, country, city, note, is_active, created_at,
	allocation, sold_to, sold_at`

func (s *sqlStore) scanSock(row interface{ Scan(...any) error }) (*Sock, error) {
	var sk Sock
	var username, password, country, city, note sql.NullString
	var isActive int
	var createdAt, soldAt sql.NullTime
	var allocation sql.NullString
	var soldTo sql.NullInt64
	if err := row.Scan(&sk.ID, &sk.Name, &sk.Host, &sk.Port, &username, &password,
		&country, &city, &note, &isActive, &createdAt,
		&allocation, &soldTo, &soldAt); err != nil {
		return nil, err
	}
	sk.Username = nullStrPtr(username)
	sk.Password = s.decryptOrRaw(password)
	sk.Country = nullStrPtr(country)
	sk.City = nullStrPtr(city)
	sk.Note = nullStrPtr(note)
	sk.IsActive = isActive != 0
	sk.CreatedAt = nullTimePtr(createdAt)
	// 老库补列后默认值是 shared；NULL 也按共享兜底（与下单侧 sockpool.IsExclusive 同口径）。
	sk.Allocation = "shared"
	if allocation.Valid && allocation.String != "" {
		sk.Allocation = allocation.String
	}
	sk.SoldTo = nullUintPtr(soldTo)
	sk.SoldAt = nullTimePtr(soldAt)
	return &sk, nil
}

func (s *sqlStore) SocksList(ctx context.Context) ([]Sock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sockColumns+` FROM proxy_socks ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sock{}
	for rows.Next() {
		sk, err := s.scanSock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sk)
	}
	return out, rows.Err()
}

func (s *sqlStore) SockByID(ctx context.Context, id uint64) (*Sock, error) {
	return s.scanSock(s.db.QueryRowContext(ctx,
		`SELECT `+sockColumns+` FROM proxy_socks WHERE id = ?`, id))
}

func (s *sqlStore) SockByIDExists(ctx context.Context, id uint64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_socks WHERE id = ?", id).Scan(&n)
	return n > 0, err
}

// SockExists 对齐导入去重：同 host + port + username 视为同一条
// （username 为 null 时把 NULL 与空串都算命中）。
func (s *sqlStore) SockExists(ctx context.Context, host string, port int64, username *string) (bool, error) {
	var n int
	var err error
	if username == nil {
		err = s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM proxy_socks WHERE host = ? AND port = ? AND (username IS NULL OR username = '')",
			host, port).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM proxy_socks WHERE host = ? AND port = ? AND username = ?",
			host, port, *username).Scan(&n)
	}
	return n > 0, err
}

func (s *sqlStore) CreateSock(ctx context.Context, in SockCreate) (*Sock, error) {
	now := s.now().UTC().Truncate(time.Second)
	enc, err := s.encrypt(derefString(in.Password))
	if err != nil {
		return nil, err
	}
	var password any
	if in.Password != nil && *in.Password != "" {
		password = enc
	}
	allocation := sockpool.Shared
	if in.Allocation != nil && *in.Allocation == sockpool.Exclusive {
		allocation = sockpool.Exclusive
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO proxy_socks
		(name, host, port, username, password, country, allocation, is_active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		in.Name, in.Host, in.Port, nullStrValue(in.Username), password, nullStrValue(in.Country), allocation, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.SockByID(ctx, uint64(id))
}

func (s *sqlStore) UpdateSock(ctx context.Context, id uint64, in SockUpdate) (*Sock, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.Name != nil {
		add("name", *in.Name)
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
	if in.Password != nil {
		enc, err := s.encrypt(*in.Password)
		if err != nil {
			return nil, err
		}
		add("password", enc)
	}
	if in.Country != nil {
		add("country", *in.Country)
	}
	if in.City != nil {
		add("city", *in.City)
	}
	if in.Note != nil {
		add("note", *in.Note)
	}
	if in.IsActive != nil {
		add("is_active", boolToInt(*in.IsActive))
	}
	if in.Allocation != nil {
		// 回到共享 = 这条不再专属任何人：已售记录一并清掉，否则以后切回独占
		// 会带着一条永远卖不出去的 sold_to（下单侧只挑 sold_to IS NULL）。
		if *in.Allocation == sockpool.Shared {
			add("allocation", sockpool.Shared)
			sets = append(sets, "sold_to = NULL", "sold_at = NULL")
		} else {
			add("allocation", *in.Allocation)
		}
	}
	if len(sets) > 0 {
		add("updated_at", s.now().UTC().Truncate(time.Second))
		args = append(args, id)
		if _, err := s.db.ExecContext(ctx,
			"UPDATE proxy_socks SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return nil, err
		}
	}
	return s.SockByID(ctx, id)
}

func (s *sqlStore) DeleteSock(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM proxy_socks WHERE id = ?", id)
	return err
}

func (s *sqlStore) SKUExistsUsingSocks(ctx context.Context, socksID uint64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_skus WHERE source_type = 'socks' AND source_id = ?", socksID).Scan(&n)
	return n > 0, err
}

func (s *sqlStore) ActiveSocksOptions(ctx context.Context) ([]SockOption, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, host, port, country FROM proxy_socks WHERE is_active = 1 ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SockOption{}
	for rows.Next() {
		var o SockOption
		var country sql.NullString
		if err := rows.Scan(&o.ID, &o.Host, &o.Port, &country); err != nil {
			return nil, err
		}
		o.Country = nullStrPtr(country)
		out = append(out, o)
	}
	return out, rows.Err()
}

// AcquireExclusiveSock 从独占池里给用户拿一条线路（事务 + 行锁在 sockpool 里）。
func (s *sqlStore) AcquireExclusiveSock(
	ctx context.Context, renewSockID, sourceID *uint64, userID uint64, now time.Time,
) (uint64, error) {
	return sockpool.Acquire(ctx, s.db, renewSockID, sourceID, userID, now)
}

// ReleaseExclusiveSock 把线路放回池子（共享线路是空操作）。
func (s *sqlStore) ReleaseExclusiveSock(ctx context.Context, socksID uint64, now time.Time) error {
	return sockpool.Release(ctx, s.db, socksID, now)
}

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

// ---- 代理运营 ----

func (s *sqlStore) OpsStats(ctx context.Context, now time.Time) (OpsStats, error) {
	var stats OpsStats

	activeCond := "status = 'active' AND (expires_at IS NULL OR expires_at > ?)"
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_entitlements WHERE "+activeCond, now).Scan(&stats.Active); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_entitlements WHERE "+activeCond+
			" AND expires_at IS NOT NULL AND expires_at <= ?",
		now, now.AddDate(0, 0, 7)).Scan(&stats.Expiring); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proxy_entitlements
		 WHERE status = 'expired'
		    OR (status = 'active' AND expires_at IS NOT NULL AND expires_at <= ?)`,
		now).Scan(&stats.Expired); err != nil {
		return stats, err
	}

	var pending int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM orders o
		 WHERE o.order_type = 'proxy' AND o.status = 'paid'
		   AND NOT EXISTS (SELECT 1 FROM proxy_entitlements e
		                   WHERE e.order_id = o.id)`,
	).Scan(&pending); err != nil {
		return stats, err
	}
	stats.PendingOrders = pending

	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_accounts WHERE sku_id IS NOT NULL").Scan(&stats.Accounts); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT socks_id) FROM proxy_entitlements WHERE kind = 'socks' AND socks_id IS NOT NULL").
		Scan(&stats.SocksLines); err != nil {
		return stats, err
	}
	return stats, nil
}

func (s *sqlStore) OpsAccountsPage(ctx context.Context, status, search string, page, perPage int64) ([]OpsAccount, int64, error) {
	where := " WHERE a.sku_id IS NOT NULL"
	args := []any{}
	if status != "" {
		where += " AND a.status = ?"
		args = append(args, status)
	}
	if search != "" {
		like := "%" + search + "%"
		cond := " AND (a.label LIKE ? OR a.username LIKE ?"
		args = append(args, like, like)
		if isDigits(search) {
			cond += " OR a.user_id = ?"
			args = append(args, search)
		}
		cond += " OR u.email LIKE ?)"
		args = append(args, like)
		where += cond
	}

	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_accounts a LEFT JOIN users u ON u.id = a.user_id"+where,
		args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []OpsAccount{}, 0, nil
	}
	lastPage := (total + perPage - 1) / perPage
	if page > lastPage {
		page = lastPage
	}
	if page < 1 {
		page = 1
	}

	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.provider_id, a.upstream_id, a.user_id, a.sku_id,
			a.label, a.username, a.password, a.proxy_limit, a.traffic_limit_bytes, a.expires_at,
			a.order_id, a.max_thread_count, a.proxy_countries, a.used_bytes, a.status,
			a.last_synced_at, a.created_at, a.updated_at,
			u.email, p.name, k.name,
			(SELECT e.id FROM proxy_entitlements e WHERE e.account_id = a.id ORDER BY e.id DESC LIMIT 1)
		FROM proxy_accounts a
		LEFT JOIN users u ON u.id = a.user_id
		LEFT JOIN proxy_providers p ON p.id = a.provider_id
		LEFT JOIN proxy_skus k ON k.id = a.sku_id`+where+`
		ORDER BY (a.expires_at IS NULL), a.expires_at ASC, a.id DESC
		LIMIT ? OFFSET ?`,
		append(args, perPage, (page-1)*perPage)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []OpsAccount{}
	for rows.Next() {
		var a OpsAccount
		var userID, skuID, orderID, entitlementID sql.NullInt64
		var password, countries sql.NullString
		var trafficLimit sql.NullInt64
		var expiresAt, lastSynced, createdAt, updatedAt sql.NullTime
		var proxyLimit []byte
		var email, providerName, skuName sql.NullString
		if err := rows.Scan(&a.ID, &a.ProviderID, &a.UpstreamID, &userID, &skuID,
			&a.Label, &a.Username, &password, &proxyLimit, &trafficLimit, &expiresAt,
			&orderID, &a.MaxThreadCount, &countries, &a.UsedBytes, &a.Status,
			&lastSynced, &createdAt, &updatedAt,
			&email, &providerName, &skuName, &entitlementID); err != nil {
			return nil, 0, err
		}
		a.UserID = nullUintPtr(userID)
		a.SKUID = nullUintPtr(skuID)
		a.OrderID = nullUintPtr(orderID)
		a.Password = s.decryptOrRaw(password)
		a.ProxyLimit = string(proxyLimit)
		a.TrafficLimitBytes = nullIntPtr(trafficLimit)
		a.ExpiresAt = nullTimePtr(expiresAt)
		a.ProxyCountries = nullStrPtr(countries)
		a.LastSyncedAt = nullTimePtr(lastSynced)
		a.CreatedAt = nullTimePtr(createdAt)
		a.UpdatedAt = nullTimePtr(updatedAt)
		a.UserEmail = nullStrPtr(email)
		a.ProviderName = nullStrPtr(providerName)
		a.SKUName = nullStrPtr(skuName)
		a.EntitlementID = nullUintPtr(entitlementID)
		out = append(out, a)
	}
	return out, total, rows.Err()
}

func (s *sqlStore) SocksGroupIDs(ctx context.Context, page, perPage int64) ([]uint64, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT socks_id) FROM proxy_entitlements WHERE kind = 'socks' AND socks_id IS NOT NULL").
		Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}
	lastPage := (total + perPage - 1) / perPage
	if page > lastPage {
		page = lastPage
	}
	if page < 1 {
		page = 1
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT socks_id FROM proxy_entitlements
		 WHERE kind = 'socks' AND socks_id IS NOT NULL
		 GROUP BY socks_id ORDER BY socks_id DESC LIMIT ? OFFSET ?`,
		perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		out = append(out, id)
	}
	return out, total, rows.Err()
}

func (s *sqlStore) SocksGroupRows(ctx context.Context, socksIDs []uint64) ([]SocksGroupRow, error) {
	if len(socksIDs) == 0 {
		return nil, nil
	}
	marks := make([]string, 0, len(socksIDs))
	args := make([]any, 0, len(socksIDs))
	for _, id := range socksIDs {
		marks = append(marks, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.user_id, e.sku_id, e.socks_id, e.expires_at, e.status,
			sk.host, sk.port, sk.country, sk.is_active, u.email, k.name
		FROM proxy_entitlements e
		LEFT JOIN proxy_socks sk ON sk.id = e.socks_id
		LEFT JOIN users u ON u.id = e.user_id
		LEFT JOIN proxy_skus k ON k.id = e.sku_id
		WHERE e.kind = 'socks' AND e.socks_id IN (`+strings.Join(marks, ",")+`)
		ORDER BY e.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SocksGroupRow{}
	for rows.Next() {
		var r SocksGroupRow
		var socksID sql.NullInt64
		var expiresAt sql.NullTime
		var host, country, email, skuName sql.NullString
		var port sql.NullInt64
		var socksActive sql.NullInt64
		if err := rows.Scan(&r.EntitlementID, &r.UserID, &r.SKUID, &socksID, &expiresAt, &r.Status,
			&host, &port, &country, &socksActive, &email, &skuName); err != nil {
			return nil, err
		}
		r.SocksID = nullUintPtr(socksID)
		r.ExpiresAt = nullTimePtr(expiresAt)
		r.SocksHost = nullStrPtr(host)
		r.SocksPort = nullIntPtr(port)
		r.SocksCountry = nullStrPtr(country)
		if socksActive.Valid {
			v := socksActive.Int64 != 0
			r.SocksActive = &v
		}
		r.UserEmail = nullStrPtr(email)
		r.SKUName = nullStrPtr(skuName)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqlStore) PendingOrdersPage(ctx context.Context, page, perPage int64) ([]PendingOrder, int64, error) {
	const base = ` FROM orders o
		LEFT JOIN users u ON u.id = o.user_id
		LEFT JOIN proxy_skus k ON k.id = o.proxy_sku_id
		WHERE o.order_type = 'proxy' AND o.status = 'paid'
		  AND NOT EXISTS (SELECT 1 FROM proxy_entitlements e WHERE e.order_id = o.id)`

	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*)"+base).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []PendingOrder{}, 0, nil
	}
	lastPage := (total + perPage - 1) / perPage
	if page > lastPage {
		page = lastPage
	}
	if page < 1 {
		page = 1
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT o.id, o.order_no, o.amount, o.paid_at, o.proxy_renewal, o.user_id, u.email, k.name"+base+
			" ORDER BY o.id DESC LIMIT ? OFFSET ?", perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PendingOrder{}
	for rows.Next() {
		var p PendingOrder
		var amount []byte
		var paidAt sql.NullTime
		var renewal int
		var email, skuName sql.NullString
		if err := rows.Scan(&p.ID, &p.OrderNo, &amount, &paidAt, &renewal, &p.UserID, &email, &skuName); err != nil {
			return nil, 0, err
		}
		p.Amount = string(amount)
		p.PaidAt = nullTimePtr(paidAt)
		p.Renewal = renewal != 0
		p.UserEmail = nullStrPtr(email)
		p.SKUName = nullStrPtr(skuName)
		out = append(out, p)
	}
	return out, total, rows.Err()
}

const accountColumns = `id, provider_id, upstream_id, user_id, sku_id, label, username, password,
	proxy_limit, traffic_limit_bytes, expires_at, order_id, max_thread_count, proxy_countries,
	used_bytes, status, last_synced_at, created_at, updated_at`

func (s *sqlStore) scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var userID, skuID, orderID, trafficLimit sql.NullInt64
	var password, countries sql.NullString
	var proxyLimit []byte
	var expiresAt, lastSynced, createdAt, updatedAt sql.NullTime
	if err := row.Scan(&a.ID, &a.ProviderID, &a.UpstreamID, &userID, &skuID, &a.Label, &a.Username,
		&password, &proxyLimit, &trafficLimit, &expiresAt, &orderID, &a.MaxThreadCount,
		&countries, &a.UsedBytes, &a.Status, &lastSynced, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	a.UserID = nullUintPtr(userID)
	a.SKUID = nullUintPtr(skuID)
	a.OrderID = nullUintPtr(orderID)
	a.Password = s.decryptOrRaw(password)
	a.ProxyLimit = string(proxyLimit)
	a.TrafficLimitBytes = nullIntPtr(trafficLimit)
	a.ExpiresAt = nullTimePtr(expiresAt)
	a.ProxyCountries = nullStrPtr(countries)
	a.LastSyncedAt = nullTimePtr(lastSynced)
	a.CreatedAt = nullTimePtr(createdAt)
	a.UpdatedAt = nullTimePtr(updatedAt)
	return &a, nil
}

func (s *sqlStore) ProxyAccountByID(ctx context.Context, id uint64) (*Account, error) {
	return s.AccountByID(ctx, id)
}

func (s *sqlStore) AccountByID(ctx context.Context, id uint64) (*Account, error) {
	return s.scanAccount(s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM proxy_accounts WHERE id = ?`, id))
}

const entitlementColumns = `id, user_id, order_id, sku_id, kind, account_id, socks_id, expires_at, status`

func scanEntitlement(row interface{ Scan(...any) error }) (*Entitlement, error) {
	var e Entitlement
	var orderID, accountID, socksID sql.NullInt64
	var expiresAt sql.NullTime
	if err := row.Scan(&e.ID, &e.UserID, &orderID, &e.SKUID, &e.Kind, &accountID, &socksID,
		&expiresAt, &e.Status); err != nil {
		return nil, err
	}
	e.OrderID = nullUintPtr(orderID)
	e.AccountID = nullUintPtr(accountID)
	e.SocksID = nullUintPtr(socksID)
	e.ExpiresAt = nullTimePtr(expiresAt)
	return &e, nil
}

func (s *sqlStore) EntitlementByID(ctx context.Context, id uint64) (*Entitlement, error) {
	return scanEntitlement(s.db.QueryRowContext(ctx,
		`SELECT `+entitlementColumns+` FROM proxy_entitlements WHERE id = ?`, id))
}

func (s *sqlStore) LatestEntitlementForAccount(ctx context.Context, accountID uint64) (*Entitlement, error) {
	return scanEntitlement(s.db.QueryRowContext(ctx,
		`SELECT `+entitlementColumns+` FROM proxy_entitlements
		 WHERE account_id = ? ORDER BY id DESC LIMIT 1`, accountID))
}

func (s *sqlStore) OrderByID(ctx context.Context, id uint64) (*Order, error) {
	var o Order
	var skuID sql.NullInt64
	var amount []byte
	var paidAt sql.NullTime
	var renewal int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, order_no, user_id, order_type, proxy_sku_id, proxy_renewal, amount, status, paid_at
		 FROM orders WHERE id = ?`, id).
		Scan(&o.ID, &o.OrderNo, &o.UserID, &o.OrderType, &skuID, &renewal, &amount, &o.Status, &paidAt)
	if err != nil {
		return nil, err
	}
	o.SKUID = nullUintPtr(skuID)
	o.ProxyRenewal = renewal != 0
	o.Amount = string(amount)
	o.PaidAt = nullTimePtr(paidAt)
	return &o, nil
}

func (s *sqlStore) UpdateEntitlement(ctx context.Context, id uint64, in EntitlementPatch) error {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.OrderID != nil {
		add("order_id", *in.OrderID)
	}
	if in.SKUID != nil {
		add("sku_id", *in.SKUID)
	}
	if in.Status != nil {
		add("status", *in.Status)
	}
	if in.ExpiresAt != nil {
		add("expires_at", *in.ExpiresAt)
	}
	if in.AccountID != nil {
		add("account_id", *in.AccountID)
	}
	if in.SocksID != nil {
		add("socks_id", *in.SocksID)
	}
	if len(sets) == 0 {
		return nil
	}
	add("updated_at", s.now().UTC().Truncate(time.Second))
	args = append(args, id)
	_, err := s.db.ExecContext(ctx,
		"UPDATE proxy_entitlements SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

func (s *sqlStore) PatchAccount(ctx context.Context, id uint64, in AccountPatch) error {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.Password != nil {
		enc, err := s.encrypt(*in.Password)
		if err != nil {
			return err
		}
		add("password", enc)
	}
	if in.Username != nil {
		add("username", *in.Username)
	}
	if in.ProxyLimit != nil {
		add("proxy_limit", *in.ProxyLimit)
	}
	if in.MaxThreadCount != nil {
		add("max_thread_count", *in.MaxThreadCount)
	}
	if in.Status != nil {
		add("status", *in.Status)
	}
	if in.LastSyncedAt != nil {
		add("last_synced_at", *in.LastSyncedAt)
	}
	if in.UsedBytes != nil {
		add("used_bytes", *in.UsedBytes)
	}
	if in.ProxyCountries != nil {
		add("proxy_countries", *in.ProxyCountries)
	}
	if in.ExpiresAt != nil {
		add("expires_at", *in.ExpiresAt)
	}
	if in.TrafficLimitBytes != nil {
		add("traffic_limit_bytes", *in.TrafficLimitBytes)
	}
	if in.SKUID != nil {
		add("sku_id", *in.SKUID)
	}
	if in.OrderID != nil {
		add("order_id", *in.OrderID)
	}
	if len(sets) == 0 {
		return nil
	}
	add("updated_at", s.now().UTC().Truncate(time.Second))
	args = append(args, id)
	_, err := s.db.ExecContext(ctx,
		"UPDATE proxy_accounts SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

func (s *sqlStore) OrderHasEntitlement(ctx context.Context, orderID uint64) (bool, error) {
	return s.EntitlementExistsForOrder(ctx, orderID)
}

func (s *sqlStore) EntitlementExistsForOrder(ctx context.Context, orderID uint64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_entitlements WHERE order_id = ?", orderID).Scan(&n)
	return n > 0, err
}

func (s *sqlStore) FindRenewableEntitlement(ctx context.Context, userID uint64, kind string, includeExpired bool, now time.Time) (*Entitlement, error) {
	query := "SELECT " + entitlementColumns + " FROM proxy_entitlements WHERE user_id = ? AND kind = ?"
	args := []any{userID, kind}
	if includeExpired {
		query += " AND status IN ('active','expired','disabled')"
	} else {
		query += " AND status = 'active' AND (expires_at IS NULL OR expires_at > ?)"
		args = append(args, now)
	}
	query += " ORDER BY id DESC LIMIT 1"

	e, err := scanEntitlement(s.db.QueryRowContext(ctx, query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (s *sqlStore) CreateEntitlement(ctx context.Context, in EntitlementCreate) (*Entitlement, error) {
	now := s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx, `INSERT INTO proxy_entitlements
		(user_id, order_id, sku_id, kind, account_id, socks_id, expires_at, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, nullUintValue(in.OrderID), in.SKUID, in.Kind, nullUintValue(in.AccountID),
		nullUintValue(in.SocksID), nullTimeValue(in.ExpiresAt), in.Status, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.EntitlementByID(ctx, uint64(id))
}

func (s *sqlStore) SKUForOrder(ctx context.Context, orderID uint64) (*SKU, error) {
	order, err := s.OrderByID(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if order.SKUID == nil {
		return nil, sql.ErrNoRows
	}
	return s.SKUByID(ctx, *order.SKUID)
}

// ---- 库存（条件更新，并发不超卖）----

func (s *sqlStore) ReserveStock(ctx context.Context, skuID uint64, qty int64) (bool, error) {
	if qty < 1 {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_skus SET stock_locked = stock_locked + ?
		 WHERE id = ? AND stock_total - stock_locked - stock_sold >= ?`, qty, skuID, qty)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *sqlStore) ReleaseStock(ctx context.Context, skuID uint64, qty int64) error {
	if qty < 1 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE proxy_skus SET stock_locked = stock_locked - ?
		 WHERE id = ? AND stock_locked >= ?`, qty, skuID, qty)
	return err
}

func (s *sqlStore) ConsumeStock(ctx context.Context, skuID uint64, qty int64) error {
	if qty < 1 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE proxy_skus SET stock_locked = stock_locked - ?, stock_sold = stock_sold + ?
		 WHERE id = ? AND stock_locked >= ?`, qty, qty, skuID, qty)
	return err
}

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

// ---- 上游 Webshare ----

const providerColumns = `id, name, driver, api_token, enabled, last_status, last_error, last_checked_at`

func (s *sqlStore) scanProvider(row interface{ Scan(...any) error }) (*Provider, error) {
	var p Provider
	var token sql.NullString
	var enabled int
	var lastError sql.NullString
	var checkedAt sql.NullTime
	if err := row.Scan(&p.ID, &p.Name, &p.Driver, &token, &enabled, &p.LastStatus,
		&lastError, &checkedAt); err != nil {
		return nil, err
	}
	p.APIToken = derefString(s.decryptOrRaw(token))
	p.Enabled = enabled != 0
	p.LastError = nullStrPtr(lastError)
	p.LastCheckedAt = nullTimePtr(checkedAt)
	return &p, nil
}

func (s *sqlStore) Providers(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+providerColumns+` FROM proxy_providers ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		p, err := s.scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *sqlStore) ProviderByID(ctx context.Context, id uint64) (*Provider, error) {
	return s.scanProvider(s.db.QueryRowContext(ctx,
		`SELECT `+providerColumns+` FROM proxy_providers WHERE id = ?`, id))
}

func (s *sqlStore) CreateProvider(ctx context.Context, name, token string, enabled bool) (*Provider, error) {
	now := s.now().UTC().Truncate(time.Second)
	enc, err := s.encrypt(token)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO proxy_providers
		(name, driver, api_token, enabled, last_status, created_at, updated_at)
		VALUES (?, 'webshare', ?, ?, 'pending', ?, ?)`,
		name, enc, boolToInt(enabled), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.ProviderByID(ctx, uint64(id))
}

func (s *sqlStore) PatchProvider(ctx context.Context, id uint64, in ProviderPatch) (*Provider, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if in.Name != nil {
		add("name", *in.Name)
	}
	if in.APIToken != nil {
		enc, err := s.encrypt(*in.APIToken)
		if err != nil {
			return nil, err
		}
		add("api_token", enc)
	}
	if in.Enabled != nil {
		add("enabled", boolToInt(*in.Enabled))
	}
	if in.LastStatus != nil {
		add("last_status", *in.LastStatus)
	}
	if in.LastError != nil {
		add("last_error", *in.LastError)
	} else if in.LastStatus != nil && *in.LastStatus == "success" {
		// 成功时清掉上一次的失败原因（PHP 里 markProviderResult($ok=true) 写 null）。
		add("last_error", nil)
	}
	if in.CheckedAt != nil {
		add("last_checked_at", *in.CheckedAt)
	}
	if len(sets) == 0 {
		return s.ProviderByID(ctx, id)
	}
	add("updated_at", s.now().UTC().Truncate(time.Second))
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE proxy_providers SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	return s.ProviderByID(ctx, id)
}

func (s *sqlStore) DeleteProvider(ctx context.Context, id uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM proxy_accounts WHERE provider_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM proxy_providers WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqlStore) ProviderAccounts(ctx context.Context, providerID uint64) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM proxy_accounts WHERE provider_id = ? ORDER BY id DESC`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *sqlStore) ProviderActiveAccounts(ctx context.Context, providerID uint64) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM proxy_accounts
		 WHERE provider_id = ? AND status = 'active' ORDER BY id DESC`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *sqlStore) ProviderAccountCount(ctx context.Context, providerID uint64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_accounts WHERE provider_id = ?", providerID).Scan(&n)
	return n, err
}

func (s *sqlStore) CreateAccount(ctx context.Context, in AccountCreate) (*Account, error) {
	now := s.now().UTC().Truncate(time.Second)
	enc, err := s.encrypt(derefString(in.Password))
	if err != nil {
		return nil, err
	}
	var password any
	if in.Password != nil && *in.Password != "" {
		password = enc
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO proxy_accounts
		(provider_id, upstream_id, user_id, label, username, password, proxy_limit,
		 max_thread_count, proxy_countries, used_bytes, status, last_synced_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProviderID, in.UpstreamID, nullUintValue(in.UserID), in.Label, in.Username, password,
		in.ProxyLimit, in.MaxThreadCount, nullStrValue(in.ProxyCountries), in.UsedBytes,
		in.Status, nullTimeValue(in.LastSyncedAt), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.AccountByID(ctx, uint64(id))
}

// UpsertAccountByUpstream 对齐 syncAccounts：按 (provider_id, upstream_id) upsert。
//
// 新记录写 username=”（上游不返回凭据，页面显示「凭据未知」）并置 status=active；
// 已有记录**不覆盖** status（避免把本地标记的 disabled 又拉回 active）。
func (s *sqlStore) UpsertAccountByUpstream(
	ctx context.Context, providerID uint64, upstreamID string, patch AccountPatch, label, status string,
) (bool, error) {
	var id uint64
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM proxy_accounts WHERE provider_id = ? AND upstream_id = ?", providerID, upstreamID).
		Scan(&id)
	if err == sql.ErrNoRows {
		_, createErr := s.CreateAccount(ctx, AccountCreate{
			ProviderID:     providerID,
			UpstreamID:     upstreamID,
			Label:          label,
			Username:       "",
			ProxyLimit:     derefString(patch.ProxyLimit),
			MaxThreadCount: derefInt(patch.MaxThreadCount),
			ProxyCountries: patch.ProxyCountries,
			UsedBytes:      derefInt(patch.UsedBytes),
			Status:         status,
			LastSyncedAt:   patch.LastSyncedAt,
		})
		if createErr != nil {
			return false, createErr
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.PatchAccount(ctx, id, patch); err != nil {
		return false, err
	}
	return false, nil
}

func (s *sqlStore) DeleteAccount(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM proxy_accounts WHERE id = ?", id)
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
