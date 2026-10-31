package admin

// 管理端用户/套餐/订单/统计域（P8a）的 HTTP 入口。
//
// 逐条对齐 backend/routes/web.php 的 admin.auth 段：
//
//	UserController.php        users CRUD + reset-traffic/reset-password/protocol/renew
//	PlanController.php        plans CRUD + deactivate/activate
//	OrderController.php       orders 列表
//	StatsController.php       stats/revenue
//	UserTrafficController.php traffic/user-ranking、node-ranking、users/{user}/nodes
//
// 本域与 P8d 商店域 / P8b 节点域共享 json.go / request.go / validate.go 的**工具**
// （响应编码、输入整理、校验器），但持有自己的依赖（UserAdminStore / PlanStore /
// OrderStore / StatsStore / TrafficStore），互不牵连。

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// randRead 是 crypto/rand.Read 的薄包装（便于测试替换）。
var randRead = cryptorand.Read

// UserDomain 是用户/套餐/订单/统计域的 HTTP 入口。
type UserDomain struct {
	users   AdminUserStore
	ops     UserPanelOps
	plans   PlanStore
	orders  OrderStore
	stats   StatsStore
	traffic TrafficStore

	// notifier 把用户侧变更推给在线前端（可空：为空则只靠前端轮询）。
	notifier UserNotifier

	// alerts 解锁该用户的自动通知防重标记（购买/续费/重置/改套餐后调用）。
	// 为空时跳过（只在测试/无邮件场景出现）。
	alerts AlertRestorer

	cfg UserConfig

	// now 供测试注入固定时钟（PHP 侧全部用 now()，时间字面量口径见 data-contract §4）。
	now func() time.Time

	logger loggerLike
}

// AlertRestorer 解锁用户的通知防重标记，对齐 MailNotifyService::restoreReminders。
//
// 独立成小接口的原因同 UserPanelOps：admin 包不 import internal/task，
// 真实实现是 internal/task.SceneNotifier，由 cmd/server 注入。
type AlertRestorer interface {
	Restore(ctx context.Context, userID uint64) error
}

// NewUserDomain 组装用户/套餐/订单/统计域。
func NewUserDomain(d UserDeps) *UserDomain {
	if d.Users == nil && d.Store == nil {
		panic("admin: AdminUserStore 不能为空")
	}
	users := d.Users
	if users == nil {
		users = d.Store
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	cfg := d.Config
	if cfg.BcryptRounds < 4 || cfg.BcryptRounds > 31 {
		cfg.BcryptRounds = 12
	}
	ops := d.Ops
	if ops == nil {
		ops = NoopUserPanelOps{}
	}
	return &UserDomain{
		users:    users,
		ops:      ops,
		notifier: d.Notifier,
		alerts:   d.Alerts,
		plans:    d.Plans,
		orders:   d.Orders,
		stats:   d.Stats,
		traffic: d.Traffic,
		cfg:     cfg,
		now:     now,
		logger:  logger,
	}
}

// Register 把本域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
//
// 顺序逐条对齐 backend/routes/web.php：静态段先于参数段（Gin 同级要求静态优先）。
func (d *UserDomain) Register(r gin.IRouter) {
	// ── 用户 ──
	r.GET("/users", d.userIndex)
	r.POST("/users", d.userStore)
	r.GET("/users/:user", d.userShow)
	r.PUT("/users/:user", d.userUpdate)
	r.DELETE("/users/:user", d.userDestroy)
	r.POST("/users/:user/reset-traffic", d.userResetTraffic)
	r.POST("/users/:user/reset-password", d.userResetPassword)
	// 重置订阅：换 token + uuid，旧订阅链接与旧节点凭据全部作废（用户定稿）。
	r.POST("/users/:user/reset-subscription", d.userResetSubscription)
	r.POST("/users/:user/protocol", d.userSwitchProtocol)
	r.POST("/users/:user/renew", d.userRenew)

	// ── 套餐 ──
	r.GET("/plans", d.planIndex)
	r.POST("/plans", d.planStore)
	r.GET("/plans/:plan", d.planShow)
	r.PUT("/plans/:plan", d.planUpdate)
	r.POST("/plans/:plan/deactivate", d.planDeactivate)
	r.POST("/plans/:plan/activate", d.planActivate)

	// ── 订单 / 统计 ──
	r.GET("/orders", d.orderIndex)
	r.GET("/stats/revenue", d.statsRevenue)

	// ── 流量排行（只读）──
	r.GET("/traffic/user-ranking", d.trafficUserRanking)
	r.GET("/traffic/node-ranking", d.trafficNodeRanking)
	r.GET("/traffic/users/:user/nodes", d.trafficUserNodes)
}

// ---- 响应封装（PHP ApiResponse 口径，HTTP 200 + php_encode 字节）----

func (d *UserDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *UserDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// failValidation 渲染 Laravel 校验失败：HTTP 200 + code 500 + 合并文案。
func (d *UserDomain) failValidation(c *gin.Context, summary string) {
	d.fail(c, summary, 500)
}

func (d *UserDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	d.fail(c, "服务器错误", 500)
}

// notFound 对齐 bootstrap/app.php 对 NotFoundHttpException 的统一改写。
func (d *UserDomain) notFound(c *gin.Context) { d.fail(c, "接口不存在", 404) }

// noRowsToNotFound 把查无此行映射成 404；其余错误返回 false 交给调用方。
func (d *UserDomain) noRowsToNotFound(c *gin.Context, err error) bool {
	if errors.Is(err, sql.ErrNoRows) || NoRows(err) {
		d.notFound(c)
		return true
	}
	return false
}

// successPage 对齐 ApiResponse::successPage（固定 50/页，越界夹回最后一页）。
func (d *UserDomain) successPage(c *gin.Context, items []any, page, perPage, total int64) {
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page < 1 {
		page = 1
	}
	if page > lastPage {
		page = lastPage
	}
	writeJSON(c, phpEncode(jo(
		"code", 0,
		"msg", "ok",
		"data", jsArr(items),
		"pagination", jo(
			"current_page", page,
			"last_page", lastPage,
			"total", total,
			"per_page", perPage,
		),
	)))
}

// ---- 路径参数 ----

// loadUser 按路由参数取用户；非数字 / 不存在 → 404（对齐隐式模型绑定）。
func (d *UserDomain) loadUser(c *gin.Context) (*UserRow, bool) {
	id, ok := parseUint(c.Param("user"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	u, err := d.users.UserByID(ctxOf(c), id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询用户", err)
		return nil, false
	}
	return u, true
}

// loadPlan 按路由参数取套餐；非数字 / 不存在 → 404。
func (d *UserDomain) loadPlan(c *gin.Context) (*PlanRow, bool) {
	id, ok := parseUint(c.Param("plan"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	p, err := d.plans.PlanByID(ctxOf(c), id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询套餐", err)
		return nil, false
	}
	return p, true
}

// ---- 校验（email 规则在共享 validate.go 之外，这里补上）----

// emailValid 对齐 Laravel `email` 规则的 RFC 口径（egulias RFCValidation）。
//
// 与注册用的 filter_var 不同：RFC 允许无点域名（a@b）与部分 unicode，
// 但拒绝空白、控制字符、空 local/domain。可观测边界见 user_validate_test.go。
func emailValid(s string) bool {
	if s == "" || strings.ContainsAny(s, "\r\n\t ") {
		return false
	}
	if strings.ContainsAny(s, "\x00") {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	local, domain := s[:at], s[at+1:]
	if !strings.HasPrefix(local, `"`) && strings.Contains(local, "@") {
		return false
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for i := 0; i < len(local); i++ {
		if local[i] < 0x20 || local[i] == 0x7F {
			return false
		}
	}
	if strings.HasPrefix(domain, "[") {
		return strings.HasSuffix(domain, "]") && len(domain) > 2
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	for i := 0; i < len(domain); i++ {
		c := domain[i]
		if c == '.' || c == '-' || c == '_' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c >= 0x80 {
			continue
		}
		return false
	}
	return true
}

// validateMessages 复刻 ValidationException::summarize 的合并文案（第一条 + N more）。
func validateSummary(errs []string) string {
	if len(errs) == 0 {
		return ""
	}
	first := errs[0]
	if n := len(errs) - 1; n > 0 {
		plural := "error"
		if n > 1 {
			plural = "errors"
		}
		return first + " (and " + itoaInt(n) + " more " + plural + ")"
	}
	return first
}

// ---- 业务小工具 ----

// randomToken 对齐 Laravel Str::random($n)：[A-Za-z0-9]，熵源用 crypto/rand
// （PHP 的 Str::random 走 random_bytes，是 CSPRNG；用户订阅令牌不能用 math/rand）。
func randomToken(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	buf := make([]byte, n)
	if _, err := randRead(buf); err != nil {
		// 熵源失败时退回 math/rand，避免建号接口 500（令牌仍满足格式）。
		return randomAlnum(n)
	}
	out := make([]byte, n)
	for i := range buf {
		out[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(out)
}

// bcryptHash 对齐 Hash::make（cost 取 BCRYPT_ROUNDS，默认 12）。
func bcryptHash(plain string, cost int) (string, error) {
	if cost < 4 || cost > 31 {
		cost = 12
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// dateOnly 把时间规范成 Y-m-d（对齐 Carbon::toDateString()）。
func dateOnly(t time.Time) string { return t.UTC().Format("2006-01-02") }

// parseDateOnly 解析 Y-m-d → 当日 UTC 零点；失败返回 nil。
func parseDateOnly(s string) *time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return nil
	}
	return &t
}

// parseDateTimeLenient 对齐 Carbon::parse 的可观测子集：Y-m-d[ H:i[:s]]（UTC）。
func parseDateTimeLenient(s string) *time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05", "2006-01-02 15:04:05",
		"2006-01-02T15:04:05.000000Z", "2006-01-02 15:04:05.000000", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

// jsonRawOf 把 third_party_sub_ids 之类的 JSON 文本解析成 int64 列表（坏数据 → 空）。
func jsonIntList(raw *string) []int64 {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	var list []int64
	if err := json.Unmarshal([]byte(*raw), &list); err != nil {
		// Laravel 的 array cast 解析失败会抛异常；这里宽容成空，避免读接口 500。
		return nil
	}
	out := make([]int64, 0, len(list))
	for _, v := range list {
		out = append(out, v)
	}
	return out
}

// intListToJSON 把 ID 列表编码成 JSON 数组文本（对齐 json_encode 的紧凑输出）。
func intListToJSON(ids []int64) string {
	if len(ids) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, v := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(itoaInt64(v))
	}
	sb.WriteByte(']')
	return sb.String()
}

func itoaInt64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// pageSize 对齐 paginate(50)：固定 50，页码从 ?page 取（非数字 → 1）。
const userPagePerPage int64 = 50

func pageParam(c *gin.Context) int64 {
	n := queryInt(c, "page", 1)
	if n < 1 {
		n = 1
	}
	return n
}
