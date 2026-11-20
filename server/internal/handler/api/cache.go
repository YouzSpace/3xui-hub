package api

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件是 Laravel Cache（database store）的 Go 侧实现，用于与 PHP 并存期共享
// email_verify_{email} / pwd_reset_{email} / rate:* 这些键。
//
// 对齐的是 Illuminate\Cache\DatabaseStore 的可观测行为：
//
//	表：cache(key PRIMARY, value mediumtext, expiration bigint)
//	键：Str::slug(APP_NAME).'-cache-'.key（本机 APP_NAME=Laravel → "laravel-cache-"）
//	值：PHP serialize() 文本，整数是 "i:3;"，字符串是 "s:6:\"123456\";"
//	过期：expiration 是 unix 秒；读到过期行时按 Laravel 的行为顺手删除
//
// 键前缀按 config/cache.php 的口径由 APP_NAME 推出（与 RateLimiter/database 驱动的默认一致），
// 因此 PHP 与 Go 读写的是同一批行，切进程后验证码与限流状态不丢。

// cachePrefixOf 复刻 config/cache.php 的默认前缀：Str::slug(APP_NAME).'-cache-'。
// 本机 .env 里 APP_NAME=Laravel（且 CACHE_PREFIX 留空），即 "laravel-cache-"。
// APP_NAME 为空时 config/app.php 的 env() 默认值也是 'Laravel'，这里同样兜底。
func cachePrefixOf(appName string) string {
	if appName == "" {
		appName = "laravel"
	}
	if slug := slugify(appName); slug != "" {
		return slug + "-cache-"
	}
	return "laravel-cache-"
}

// slugify 复刻 Str::slug($value)（默认参数、language=en）的主要路径：
// ASCII 化 → 非字母数字转 '-' → 合并 → 去首尾。APP_NAME 只可能是简单标识，
// 这里不实现完整的音译表（如 "中文" → 空串），遇到时退回 "laravel" 的默认值由调用方处理。
func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// DBCache 是 Cache 的 database-store 实现。
type DBCache struct {
	db     *sql.DB
	prefix string
	now    func() time.Time // 便于测试注入时钟
}

// NewDBCache 构造 database-store 缓存。appName 对应 .env 的 APP_NAME。
func NewDBCache(db *sql.DB, appName string) Cache {
	if appName == "" {
		appName = "laravel"
	}
	return &DBCache{db: db, prefix: cachePrefixOf(appName), now: time.Now}
}

// fullKey 给业务键加前缀（缓存表里存的永远带前缀，与 Laravel 一致）。
func (c *DBCache) fullKey(key string) string { return c.prefix + key }

// Get 读一个键；过期视为不存在（并顺手删除，与 DatabaseStore::many 的行为一致）。
func (c *DBCache) Get(ctx context.Context, key string) (any, bool, error) {
	full := c.fullKey(key)
	var value string
	var expiration int64
	err := c.db.QueryRowContext(ctx,
		"SELECT value, expiration FROM cache WHERE `key` = ?", full).Scan(&value, &expiration)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache get %q: %w", key, err)
	}
	if expiration <= c.now().Unix() {
		_, _ = c.db.ExecContext(ctx, "DELETE FROM cache WHERE `key` = ? AND expiration <= ?", full, c.now().Unix())
		return nil, false, nil
	}
	v, err := phpUnserializeScalar(value)
	if err != nil {
		return nil, false, fmt.Errorf("cache get %q: %w", key, err)
	}
	return v, true, nil
}

// Put 写入（覆盖）。ttl<=0 表示不过期（Laravel 的 forever 用 10 年近似，这里同口径）。
func (c *DBCache) Put(ctx context.Context, key string, val any, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 10 * 365 * 24 * time.Hour
	}
	_, err := c.db.ExecContext(ctx,
		"INSERT INTO cache (`key`, value, expiration) VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE value = VALUES(value), expiration = VALUES(expiration)",
		c.fullKey(key), phpSerializeScalar(val), c.now().Add(ttl).Unix())
	if err != nil {
		return fmt.Errorf("cache put %q: %w", key, err)
	}
	return nil
}

// Add 仅当键不存在或已过期时写入；返回是否真的写入。
//
// Laravel 的 add() 是「先 get，为 null 再 insertOrIgnore」，并在 insert 时不检查过期；
// 这里把「过期行也算可覆盖」处理好（先删过期行再抢插），语义更严，但对外可观测行为一致：
// 未过期 → 返回 false；不存在/已过期 → 写入并返回 true。
func (c *DBCache) Add(ctx context.Context, key string, val any, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = 10 * 365 * 24 * time.Hour
	}
	full := c.fullKey(key)
	expiration := c.now().Add(ttl).Unix()

	// 先清掉已过期的同键行（Laravel 的 add 在 get 阶段就会回收，这里显式做，避免 INSERT IGNORE 被死行挡住）。
	if _, err := c.db.ExecContext(ctx,
		"DELETE FROM cache WHERE `key` = ? AND expiration <= ?", full, c.now().Unix()); err != nil {
		return false, fmt.Errorf("cache add %q: %w", key, err)
	}

	res, err := c.db.ExecContext(ctx,
		"INSERT IGNORE INTO cache (`key`, value, expiration) VALUES (?, ?, ?)",
		full, phpSerializeScalar(val), expiration)
	if err != nil {
		return false, fmt.Errorf("cache add %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cache add %q: %w", key, err)
	}
	return n > 0, nil
}

// Forget 删除一个键（不存在不报错）。
func (c *DBCache) Forget(ctx context.Context, key string) error {
	_, err := c.db.ExecContext(ctx, "DELETE FROM cache WHERE `key` = ?", c.fullKey(key))
	if err != nil {
		return fmt.Errorf("cache forget %q: %w", key, err)
	}
	return nil
}

// Increment 递增一个整数键，返回新值与是否命中（键不存在/已过期 → (0,false)）。
//
// 对齐 DatabaseStore::incrementOrDecrement：行级锁读 → 反序列化 → 加 1 → 写回，
// 只是**不延长 expiration**（RateLimiter::hit 的窗口由 :timer 键的 TTL 控制）。
func (c *DBCache) Increment(ctx context.Context, key string) (int64, bool, error) {
	full := c.fullKey(key)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("cache incr %q: %w", key, err)
	}
	defer func() { _ = tx.Rollback() }()

	var value string
	var expiration int64
	err = tx.QueryRowContext(ctx,
		"SELECT value, expiration FROM cache WHERE `key` = ? FOR UPDATE", full).Scan(&value, &expiration)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("cache incr %q: %w", key, err)
	}
	if expiration <= c.now().Unix() {
		return 0, false, nil
	}

	cur, err := phpUnserializeScalar(value)
	if err != nil {
		return 0, false, fmt.Errorf("cache incr %q: %w", key, err)
	}
	var n int64
	switch v := cur.(type) {
	case int64:
		n = v + 1
	case string:
		parsed, convErr := strconv.ParseInt(v, 10, 64)
		if convErr != nil {
			return 0, false, fmt.Errorf("cache incr %q: 值不是整数: %q", key, v)
		}
		n = parsed + 1
	default:
		return 0, false, fmt.Errorf("cache incr %q: 值不是数值", key)
	}

	if _, err := tx.ExecContext(ctx, "UPDATE cache SET value = ? WHERE `key` = ?", phpSerializeScalar(n), full); err != nil {
		return 0, false, fmt.Errorf("cache incr %q: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("cache incr %q: %w", key, err)
	}
	return n, true, nil
}

// ---- PHP serialize/unserialize（只覆盖本包写入的标量）----

// phpSerializeScalar 生成 PHP serialize() 文本。
//
// 支持的输入：
//
//	int/int64/uint64 → "i:3;"
//	string           → "s:6:\"123456\";"
//	bool             → "b:1;"
//	nil              → "N;"
func phpSerializeScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "N;"
	case string:
		return "s:" + strconv.Itoa(len(t)) + ":\"" + t + "\";"
	case bool:
		if t {
			return "b:1;"
		}
		return "b:0;"
	case int:
		return "i:" + strconv.Itoa(t) + ";"
	case int32:
		return "i:" + strconv.FormatInt(int64(t), 10) + ";"
	case int64:
		return "i:" + strconv.FormatInt(t, 10) + ";"
	case uint64:
		return "i:" + strconv.FormatUint(t, 10) + ";"
	case float64:
		// PHP 的 serialize 浮点用 17 位精度；业务上只写整数计数，这里保守处理。
		return "d:" + strconv.FormatFloat(t, 'g', 17, 64) + ";"
	default:
		return "N;"
	}
}

// phpUnserializeScalar 解析 PHP serialize() 文本里的标量。
//
// 处理 "i:N;" / "s:LEN:\"...\";" / "b:0|1;" / "N;" / "d:...;"；
// 其它类型（数组/对象）返回错误 —— cache 表里的业务键只有标量，
// 遇到复杂值说明键被别的写入方占用了，宁可报错也不要静默解成 nil。
func phpUnserializeScalar(s string) (any, error) {
	if s == "N;" {
		return nil, nil
	}
	if len(s) < 2 || s[1] != ':' {
		return nil, fmt.Errorf("非法序列化值: %q", truncate(s, 60))
	}
	body := s[2:]
	switch s[0] {
	case 'i':
		end := strings.IndexByte(body, ';')
		if end < 0 {
			return nil, fmt.Errorf("非法整数序列化: %q", truncate(s, 60))
		}
		n, err := strconv.ParseInt(body[:end], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("非法整数序列化: %q", truncate(s, 60))
		}
		return n, nil

	case 'b':
		return strings.HasPrefix(body, "1"), nil

	case 'd':
		end := strings.IndexByte(body, ';')
		if end < 0 {
			return nil, fmt.Errorf("非法浮点序列化: %q", truncate(s, 60))
		}
		f, err := strconv.ParseFloat(body[:end], 64)
		if err != nil {
			return nil, fmt.Errorf("非法浮点序列化: %q", truncate(s, 60))
		}
		return f, nil

	case 's':
		// s:LEN:"...";
		colon := strings.IndexByte(body, ':')
		if colon < 0 {
			return nil, fmt.Errorf("非法字符串序列化: %q", truncate(s, 60))
		}
		n, err := strconv.Atoi(body[:colon])
		if err != nil || n < 0 {
			return nil, fmt.Errorf("非法字符串长度: %q", truncate(s, 60))
		}
		rest := body[colon+1:]
		if len(rest) < n+2 || rest[0] != '"' || rest[n+1] != '"' {
			return nil, fmt.Errorf("非法字符串序列化: %q", truncate(s, 60))
		}
		return rest[1 : n+1], nil
	}
	return nil, fmt.Errorf("不支持的序列化类型 %q: %q", s[0], truncate(s, 60))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

