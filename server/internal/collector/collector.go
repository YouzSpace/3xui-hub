// Package collector 是 P6 采集常驻层：流量同步、健康检查、机器流量监控、封禁扫描、月重置。
//
// 逐条对齐：
//
//	backend/app/Services/TrafficSyncService.php   （695 行，核心）
//	backend/app/Services/{HealthCheckService,MonitorService,BanService}.php
//	backend/app/Console/Commands/{SyncTrafficCommand,MonthlyResetTrafficCommand}.php
//	backend/app/Jobs/{SyncNodeTrafficJob,HealthCheckJob,BanCheckJob}.php
//
// 口径铁律（.hermes/plans/go-migration/contracts/{behavior,data}-contract.md）：
//   - 快照表字段是 traffic_snapshots.upload / .download；up/down 只是 3x-ui 上报的变量名；
//   - 增量 = 本轮累计 − 上轮快照；本轮 < 上轮 → 计费增量记 0，快照推进为新基准；
//   - 加权 = floor((up_delta+down_delta) × 倍率)，倍率只乘 delta、绝不进快照；
//   - 周期套餐（plan.type='period'）同时累加 monthly_traffic_used；
//   - 封禁永不封号：只关 3x-ui 流量（users.enabled 永真）；
//   - 月重置只清已用流量，next_traffic_reset_at += 30 天，不动套餐时长/额度。
//
// 与其他 Go 模块的边界：
//   - 依赖 P5 驱动（internal/driver/threexui、internal/driver/xray）、P0 的 database/sql，
//     以及 internal/nodeapi 的指令 action 常量（裸内核节点只能经 node_api_commands 下发
//     client_list，见 reconcile_bare.go）；
//   - 不碰 handler/admin（P8a/P8b 负责后台入口），不碰 routes.go / cmd（P9 负责调度）；
//   - 时间列一律按库内字面量读写（DSN loc=UTC 打标签、不换算），与 P4/P8 同口径。
package collector

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"controlhub-server/internal/tgbot"
)

// Options 是采集层的构造参数。
type Options struct {
	// DB 是连接池（P0 store.Open 的产物）。
	DB *sql.DB
	// AppKey 是 APP_KEY 解出的 32 字节 AES 密钥（节点 password/api_key 解密用）。
	AppKey []byte
	// Now 可注入（测试固定时间用）；nil 时用 time.Now().UTC()。
	Now func() time.Time
	// Logf 是日志出口（默认丢弃；P9 接 slog 时注入）。
	Logf func(format string, args ...any)
	// TGSender Telegram 通知（节点离线告警；nil 则跳过 TG 推送）。
	TGSender *tgbot.Sender
}

// Collector 是采集层的入口，持有数据库连接与配置。
//
// 无状态：所有任务方法都可以被并发调用（同一节点的流量同步靠 cache_locks 互斥，
// 与 PHP 的 Cache::lock('traffic-sync:node:{id}') 共用同一张表）。
type Collector struct {
	db     *sql.DB
	key    []byte
	nowFn  func() time.Time
	logf   func(format string, args ...any)
	tg     *tgbot.Sender
	logger sync.Mutex // 只用于序列化 Logf 调用，避免业务日志交错
	// inbounds 是节点级 inbounds/list 共享快照（TTL + singleflight，见 inbound_cache.go）：
	// traffic-sync 与 reconcile 共用，避免同一份大 JSON 每 5 分钟被拉两遍。
	inbounds inboundSnapshotCache
	// notifier 是流量用尽通知的接缝（nil = 不发通知）。启动时注入，之后只读。
	notifier MailNotifier
}

// MailNotifier 是「按场景给用户发一封通知邮件」的接缝。
//
// 实现见 internal/task.SceneNotifier（渲染 + 防重 + 邮件队列都在那边），
// 由 cmd/server 组装时注入 —— collector 不 import task（task 已 import collector）。
type MailNotifier interface {
	NotifyScene(ctx context.Context, scene string, userID int64) error
}

// SetMailNotifier 注入通知器（对齐 PHP 里 BanService 直接调 MailNotifyService）。
func (c *Collector) SetMailNotifier(n MailNotifier) {
	c.notifier = n
}

// New 构造采集器。
func New(opts Options) (*Collector, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("collector: DB 不能为空")
	}
	if len(opts.AppKey) != 32 {
		return nil, fmt.Errorf("collector: AppKey 必须是 32 字节（APP_KEY 解出的 AES-256 密钥）")
	}
	c := &Collector{
		db:       opts.DB,
		key:      opts.AppKey,
		nowFn:    opts.Now,
		logf:     opts.Logf,
		tg:       opts.TGSender,
		inbounds: inboundSnapshotCache{ttl: defaultInboundSnapshotTTL},
	}
	if c.nowFn == nil {
		c.nowFn = func() time.Time { return time.Now().UTC() }
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	return c, nil
}

// now 返回 UTC 当前时刻（与 Laravel app.timezone=UTC 的 now() 一致）。
func (c *Collector) now() time.Time { return c.nowFn().UTC() }

func (c *Collector) logf_(format string, args ...any) {
	c.logger.Lock()
	defer c.logger.Unlock()
	c.logf(format, args...)
}

// dbDateTime 是 MySQL datetime 字面量（Y-m-d H:i:s，UTC）。
//
// DSN 已 parseTime=true&loc=UTC：Go 读到的时间带 UTC 标签、不做换算，
// 写回时用同一格式，PHP 读到的字符串与写入时完全一致（data-contract §4）。
func dbDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// inTx 执行一个事务；fn 返回错误则整体回滚。
func (c *Collector) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// placeholders 生成 "?,?,?"。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ── Laravel cache 兼容（锁 + 防重入标记） ──────────────────────────
//
// 与 PHP 共用 cache / cache_locks 两张表：key 前缀必须逐字节一致，
// 否则两侧的节点流量锁互相看不见（behavior-contract §6 的并发前提）。

// cacheKeyPrefix 复刻 config/cache.php 的 prefix（CACHE_PREFIX 或 slug(APP_NAME)+'-cache-'）。
func cacheKeyPrefix() string {
	if p := strings.TrimSpace(os.Getenv("CACHE_PREFIX")); p != "" {
		return p
	}
	slug := slugify(os.Getenv("APP_NAME"))
	if slug == "" {
		slug = "laravel"
	}
	return slug + "-cache-"
}

// slugify 复刻 Str::slug() 的常见场景（小写、非字母数字转 -、去首尾 -）。
func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// phpSerializeString 生成 PHP serialize() 的字符串形态（s:3:"abc";）。
//
// 长度是字节数（PHP strlen），内容原样输出。缓存值只需 PHP 能 unserialize；
// 两侧都只把该键当「存在性标记」用，不依赖具体值。
func phpSerializeString(s string) string {
	return "s:" + strconv.Itoa(len(s)) + `:"` + s + `";`
}

// cacheAdd 复刻 Cache::add()：已存在且未过期返回 false，否则写入并返回 true。
func (c *Collector) cacheAdd(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	fullKey := cacheKeyPrefix() + key
	expiration := c.now().Unix() + int64(ttl.Seconds())

	var exp int64
	err := c.db.QueryRowContext(ctx,
		"SELECT expiration FROM cache WHERE `key` = ?", fullKey).Scan(&exp)
	serialized := phpSerializeString(value)
	if err == nil && exp > c.now().Unix() {
		return false, nil
	}

	if err == sql.ErrNoRows {
		res, ierr := c.db.ExecContext(ctx,
			"INSERT IGNORE INTO cache (`key`, value, expiration) VALUES (?, ?, ?)",
			fullKey, serialized, expiration)
		if ierr != nil {
			return false, ierr
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	if err != nil {
		return false, err
	}

	// 已过期 → 覆盖（对齐 DatabaseStore::add 的插入语义）。
	res, uerr := c.db.ExecContext(ctx,
		"UPDATE cache SET value = ?, expiration = ? WHERE `key` = ?", serialized, expiration, fullKey)
	if uerr != nil {
		return false, uerr
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// acquireNodeLock 对齐 Laravel DatabaseLock::acquire（ttl 120s，与 PHP 一致）。
//
// 先 INSERT IGNORE；被占用时只在「已过期或本来就是自己」时接管。
func (c *Collector) acquireNodeLock(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	fullKey := cacheKeyPrefix() + key
	owner := randomOwner()
	expires := c.now().Unix() + int64(ttl.Seconds())

	res, err := c.db.ExecContext(ctx,
		"INSERT IGNORE INTO cache_locks (`key`, owner, expiration) VALUES (?, ?, ?)",
		fullKey, owner, expires)
	if err != nil {
		return "", false, fmt.Errorf("获取节点锁失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return owner, true, nil
	}

	res, err = c.db.ExecContext(ctx,
		"UPDATE cache_locks SET owner = ?, expiration = ? WHERE `key` = ? AND (owner = ? OR expiration <= ?)",
		owner, expires, fullKey, owner, c.now().Unix())
	if err != nil {
		return "", false, fmt.Errorf("接管节点锁失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n >= 1 {
		return owner, true, nil
	}
	return "", false, nil
}

// releaseNodeLock 只删除自己持有的那一行（对齐 DatabaseLock::release）。
func (c *Collector) releaseNodeLock(ctx context.Context, key, owner string) {
	fullKey := cacheKeyPrefix() + key
	if _, err := c.db.ExecContext(ctx,
		"DELETE FROM cache_locks WHERE `key` = ? AND owner = ?", fullKey, owner); err != nil {
		// 释放失败只会让锁在 ttl 后自然过期，不影响已完成的落账。
		c.logf_("释放节点锁失败（将在 ttl 后自然过期）: %v", err)
	}
}

func randomOwner() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// envOr 是 os.Getenv 的「空串回落」形态（与 config.envOr 同语义，包内自用）。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt 读整数 env（非法/缺失回落 def）。
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envFloat 读浮点 env（非法/缺失回落 def）。
func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// siteConfigValue 复刻 SiteConfig::getValue($key, $default)。
func (c *Collector) siteConfigValue(ctx context.Context, key, def string) string {
	var value sql.NullString
	err := c.db.QueryRowContext(ctx,
		"SELECT value FROM site_configs WHERE `key` = ? LIMIT 1", key).Scan(&value)
	if err != nil || !value.Valid {
		return def
	}
	return value.String
}
