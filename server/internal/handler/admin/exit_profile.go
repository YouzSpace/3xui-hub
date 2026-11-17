package admin

// 管理端静态 IP 出口分类域（/admin-api/exit-profiles…）—— #静态IP。
//
//	GET    /admin-api/exit-profiles        列表（每项含 node_ids / node_count / provider_name）
//	GET    /admin-api/exit-profiles/:id    详情（含前置节点列表 nodes）
//	POST   /admin-api/exit-profiles        新建
//	PUT    /admin-api/exit-profiles/:id    修改（整行替换）
//	DELETE /admin-api/exit-profiles/:id    删除（有套餐绑着时拒绝，文案含套餐数）
//
// 只做数据层：购买流程（调 webshare 开号）与出站生成（渲染 xray）都是后续任务，
// 本文件不 import 任何上游客户端、不改 xray_inbounds / xray_outbounds。
//
// 路由参数用 :id：/exit-profiles 是新的一段路径，与 /nodes/:node、/plans/:plan
// 不在同一棵子树上，不存在 Gin 通配名冲突。

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// 字段上限（与建表列宽对齐：name/socks_host 超长直接 422，不靠 MySQL 截断）。
const (
	exitProfileNameMaxRunes  = 64
	exitProfileHostMaxRunes  = 190
	exitProfileMaxNodeCount  = 200
	exitProfileMaxPortNumber = 65535
)

// ExitProfileDeps 是出口分类域的依赖清单。
type ExitProfileDeps struct {
	Store  ExitProfileStore
	Logger loggerLike
	// Now 供测试注入固定时钟（落库时间统一走 UTC，与全仓口径一致）。
	Now func() time.Time
}

// ExitProfileDomain 是管理端出口分类域的 HTTP 入口。
type ExitProfileDomain struct {
	store  ExitProfileStore
	logger loggerLike
	now    func() time.Time
}

// NewExitProfileDomain 组装出口分类域。
func NewExitProfileDomain(d ExitProfileDeps) *ExitProfileDomain {
	if d.Store == nil {
		panic("admin: ExitProfileStore 不能为空")
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &ExitProfileDomain{store: d.Store, logger: logger, now: now}
}

// Register 把出口分类域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
func (d *ExitProfileDomain) Register(r gin.IRouter) {
	r.GET("/exit-profiles", d.exitProfileIndex)
	r.POST("/exit-profiles", d.exitProfileStore)
	r.GET("/exit-profiles/:id", d.exitProfileShow)
	r.PUT("/exit-profiles/:id", d.exitProfileUpdate)
	r.DELETE("/exit-profiles/:id", d.exitProfileDestroy)
}

// ---- 响应封装（与地区/余额/模板域同口径：HTTP 200 + PHP json_encode 字节）----

func (d *ExitProfileDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *ExitProfileDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *ExitProfileDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// notFound 与其它域同文案（Laravel 隐式绑定失败 / 路由不存在都回这个）。
func (d *ExitProfileDomain) notFound(c *gin.Context) { d.fail(c, "接口不存在", 404) }

// noRowsToNotFound 把「查无此行」映射成 404；其余错误返回 false。
func (d *ExitProfileDomain) noRowsToNotFound(c *gin.Context, err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		d.notFound(c)
		return true
	}
	return false
}

// ---- 列表 / 详情 ----

// exitProfileIndex 对齐 GET /admin-api/exit-profiles：全部分类 + node_ids / provider_name。
//
// 不分页：分类是运营手工配的「十几个」量级，一次给全前端才能直接做套餐下拉；
// 真要分页也应该是列表自己的需求，不该照抄用户列表的 50/页。
func (d *ExitProfileDomain) exitProfileIndex(c *gin.Context) {
	rows, err := d.store.ExitProfileList(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询出口分类", err)
		return
	}
	items := make(jsArr, 0, len(rows))
	for i := range rows {
		items = append(items, presentExitProfile(&rows[i]))
	}
	d.success(c, items)
}

// exitProfileShow 对齐 GET /admin-api/exit-profiles/:id：分类 + 前置节点明细。
func (d *ExitProfileDomain) exitProfileShow(c *gin.Context) {
	id, ok := parseUint(c.Param("id"))
	if !ok {
		d.notFound(c)
		return
	}
	p, err := d.store.ExitProfileByID(ctxOf(c), id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "查询出口分类", err)
		return
	}
	nodes, err := d.store.ExitProfileNodes(ctxOf(c), id)
	if err != nil {
		d.failServer(c, "查询出口分类节点", err)
		return
	}
	out := presentExitProfile(p)
	out.put("nodes", presentExitProfileNodes(nodes))
	d.success(c, out)
}

// ---- 新建 / 修改 / 删除 ----

// exitProfileStore 对齐 POST /admin-api/exit-profiles。
func (d *ExitProfileDomain) exitProfileStore(c *gin.Context) {
	in, msg, ok := d.parseExitProfileInput(c)
	if !ok {
		// msg 为空表示解析过程已经自己写过响应（查库失败 → 500），不能再写一次。
		if msg != "" {
			d.fail(c, msg, 422)
		}
		return
	}

	taken, err := d.store.ExitProfileNameTaken(ctxOf(c), in.Name, 0)
	if err != nil {
		d.failServer(c, "检查出口分类名称", err)
		return
	}
	if taken {
		d.fail(c, "分类名称已存在", 422)
		return
	}

	rec, err := d.store.ExitProfileCreate(ctxOf(c), in)
	if err != nil {
		if errors.Is(err, ErrExitProfileNameTaken) {
			// 并发下预检查没用，唯一键兜底（仍然是 422，不是 500）。
			d.fail(c, "分类名称已存在", 422)
			return
		}
		d.failServer(c, "创建出口分类", err)
		return
	}
	d.success(c, presentExitProfile(rec), "创建成功")
}

// exitProfileUpdate 对齐 PUT /admin-api/exit-profiles/:id：整行替换（关联表同步）。
func (d *ExitProfileDomain) exitProfileUpdate(c *gin.Context) {
	id, ok := parseUint(c.Param("id"))
	if !ok {
		d.notFound(c)
		return
	}
	in, msg, ok := d.parseExitProfileInput(c)
	if !ok {
		// msg 为空表示解析过程已经自己写过响应（查库失败 → 500），不能再写一次。
		if msg != "" {
			d.fail(c, msg, 422)
		}
		return
	}

	taken, err := d.store.ExitProfileNameTaken(ctxOf(c), in.Name, id)
	if err != nil {
		d.failServer(c, "检查出口分类名称", err)
		return
	}
	if taken {
		d.fail(c, "分类名称已存在", 422)
		return
	}

	rec, err := d.store.ExitProfileUpdate(ctxOf(c), id, in)
	if err != nil {
		if errors.Is(err, ErrExitProfileNameTaken) {
			d.fail(c, "分类名称已存在", 422)
			return
		}
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "修改出口分类", err)
		return
	}
	d.success(c, presentExitProfile(rec), "保存成功")
}

// exitProfileDestroy 对齐 DELETE /admin-api/exit-profiles/:id。
//
// 还有套餐绑着时**拒绝删除**，文案带上套餐数与名字：
// 直接删会让 plans.exit_profile_id 悬空，出站生成（后续任务）拿到一个不存在的分类。
func (d *ExitProfileDomain) exitProfileDestroy(c *gin.Context) {
	id, ok := parseUint(c.Param("id"))
	if !ok {
		d.notFound(c)
		return
	}

	bindings, err := d.store.ExitProfileBindings(ctxOf(c), id)
	if err != nil {
		d.failServer(c, "统计出口分类绑定", err)
		return
	}
	if len(bindings) > 0 {
		d.fail(c, exitProfileInUseMessage(bindings), 422)
		return
	}

	if err := d.store.ExitProfileDelete(ctxOf(c), id); err != nil {
		var inUse *ErrExitProfileInUse
		if errors.As(err, &inUse) {
			// 守卫通过之后有人新绑了套餐（并发）：事务里复查挡下的，仍然是 422。
			d.fail(c, fmt.Sprintf("该分类还有 %d 个套餐在用，请先解绑", inUse.Count), 422)
			return
		}
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "删除出口分类", err)
		return
	}
	d.success(c, nil, "删除成功")
}

// exitProfileInUseMessage 生成删除守卫的文案（含套餐数与套餐名，最多列 3 个）。
//
// 只报数不列名：管理员还得自己去套餐页搜「哪个绑了它」；全列出来又可能几十个字。
// 折中做法与地区删除的「还有 N 台节点，请先移出」同一风格。
func exitProfileInUseMessage(bindings []ExitProfileBinding) string {
	shown := len(bindings)
	if shown > 3 {
		shown = 3
	}
	names := make([]string, 0, shown+1)
	for i := 0; i < shown; i++ {
		names = append(names, bindings[i].Name)
	}
	listed := strings.Join(names, "、")
	if len(bindings) > shown {
		listed += " 等"
	}
	return fmt.Sprintf("该分类还有 %d 个套餐在用（%s），请先解绑", len(bindings), listed)
}

// ---- 输入整理 ----

// parseExitProfileInput 解析并校验创建/修改共用的整份表单。
//
// 为什么不用 validate.go 的 validator：那套是复刻 Laravel 规则的（period/total 套餐
// 那套语义），这里没有 PHP 对应实现，文案也不需要与 PHP 逐字一致；
// 直接手写能得到更准确的中文提示（前端原样展示），且不引入一层「为了像 PHP 而像」的间接。
//
// 返回 (input, 失败文案, ok)。
func (d *ExitProfileDomain) parseExitProfileInput(c *gin.Context) (ExitProfileInput, string, bool) {
	input := jsonInput(c)
	in := ExitProfileInput{
		Enabled:      true,
		DurationDays: 30,
		SocksPort:    1080,
		NodeIDs:      []uint64{},
	}

	// name：必填、去空白后非空、不超 64 个字符（与 varchar(64) 对齐）。
	name, _ := inputString(input["name"])
	name = strings.TrimSpace(name)
	if name == "" {
		return in, "分类名称不能为空", false
	}
	if utf8.RuneCountInString(name) > exitProfileNameMaxRunes {
		return in, fmt.Sprintf("分类名称不能超过 %d 个字符", exitProfileNameMaxRunes), false
	}
	in.Name = name

	// provider_id：必填、正整数、必须真实存在（指向 proxy_providers）。
	rawProvider, exists := input["provider_id"]
	if !exists || rawProvider == nil {
		return in, "请选择 webshare 账号", false
	}
	providerID, valid := exitProfileUintInput(rawProvider)
	if !valid || providerID == 0 {
		return in, "webshare 账号不合法", false
	}
	ok, err := d.store.ProviderExists(ctxOf(c), providerID)
	if err != nil {
		d.failServer(c, "校验 webshare 账号", err)
		return in, "", false
	}
	if !ok {
		return in, "webshare 账号不存在", false
	}
	in.ProviderID = providerID

	// traffic_gb / duration_days / max_thread_count：缺省走建表默认值，传了就必须是合法整数。
	if v, exists := input["traffic_gb"]; exists && v != nil {
		n, valid := exitProfileIntInput(v)
		if !valid || n < 0 {
			return in, "每个用户流量需为不小于 0 的整数（GB）", false
		}
		in.TrafficGB = n
	}
	if v, exists := input["duration_days"]; exists && v != nil {
		n, valid := exitProfileIntInput(v)
		if !valid || n <= 0 {
			return in, "有效期天数需为正整数", false
		}
		in.DurationDays = n
	}
	if v, exists := input["max_thread_count"]; exists && v != nil {
		n, valid := exitProfileIntInput(v)
		if !valid || n < 0 {
			return in, "并发数需为不小于 0 的整数", false
		}
		in.MaxThreadCount = n
	}

	// socks_host：可空（分类先建、入口后填），但不能超长。
	if v, exists := input["socks_host"]; exists && v != nil {
		host, _ := inputString(v)
		host = strings.TrimSpace(host)
		if utf8.RuneCountInString(host) > exitProfileHostMaxRunes {
			return in, fmt.Sprintf("socks 入口不能超过 %d 个字符", exitProfileHostMaxRunes), false
		}
		in.SocksHost = host
	}
	// socks_port：1-65535（0 或负数不是一个可用端口）。
	if v, exists := input["socks_port"]; exists && v != nil {
		n, valid := exitProfileIntInput(v)
		if !valid || n <= 0 || n > exitProfileMaxPortNumber {
			return in, fmt.Sprintf("端口需为 1-%d 的整数", exitProfileMaxPortNumber), false
		}
		in.SocksPort = n
	}

	// enabled：缺省 true（新建就是启用）；显式传 false 才是停用。
	if v, exists := input["enabled"]; exists && v != nil {
		in.Enabled = inputBool(v)
	}

	// node_ids：可选；每个元素必须是真实存在的节点，否则回 422 并指出是哪个。
	rawNodes, exists := input["node_ids"]
	if !exists || rawNodes == nil {
		return in, "", true
	}
	items, isArr := rawNodes.([]any)
	if !isArr {
		return in, "前置节点需为数组", false
	}
	if len(items) > exitProfileMaxNodeCount {
		return in, fmt.Sprintf("前置节点最多 %d 台", exitProfileMaxNodeCount), false
	}
	ids := make([]uint64, 0, len(items))
	for _, item := range items {
		n, valid := exitProfileUintInput(item)
		if !valid || n == 0 {
			return in, "前置节点 id 不合法", false
		}
		ids = append(ids, n)
	}
	ids = normalizeNodeIDs(ids)
	if len(ids) > 0 {
		found, err := d.store.NodesByIDs(ctxOf(c), ids)
		if err != nil {
			d.failServer(c, "校验前置节点", err)
			return in, "", false
		}
		if len(found) != len(ids) {
			existing := make(map[uint64]struct{}, len(found))
			for _, n := range found {
				existing[n.ID] = struct{}{}
			}
			missing := make([]string, 0, len(ids)-len(found))
			for _, id := range ids {
				if _, ok := existing[id]; !ok {
					missing = append(missing, "#"+strconv.FormatUint(id, 10))
				}
			}
			return in, "前置节点不存在：" + strings.Join(missing, "、"), false
		}
	}
	in.NodeIDs = ids
	return in, "", true
}

// exitProfileIntInput 严格解析整数输入：非整数（"1.5" / "abc"）返回 false。
//
// 不复用 inputInt：它把非法值悄悄当 0，traffic_gb=0 是合法值（不限流量），
// 静默变 0 就等于把一个打错的「5GB」存成「不限」，属于会改数据的误判。
func exitProfileIntInput(v any) (int64, bool) {
	return regionIntInput(v)
}

// exitProfileUintInput 严格解析无符号 id 输入（正整数）。
func exitProfileUintInput(v any) (uint64, bool) {
	n, ok := regionIntInput(v)
	if !ok || n <= 0 {
		return 0, false
	}
	return uint64(n), true
}

// ---- 展示层 ----

// presentExitProfile 输出分类一行（字段顺序固定，前端契约）。
//
// node_count 由 node_ids 长度现算：读侧只有一个真相来源（关联表），
// 不在表上再存一个会和关联行走样的冗余计数。
func presentExitProfile(p *ExitProfileRecord) *jsObj {
	return jo(
		"id", p.ID,
		"name", p.Name,
		"provider_id", p.ProviderID,
		"provider_name", p.ProviderName,
		"traffic_gb", p.TrafficGB,
		"duration_days", p.DurationDays,
		"max_thread_count", p.MaxThreadCount,
		"socks_host", p.SocksHost,
		"socks_port", p.SocksPort,
		"enabled", p.Enabled,
		"node_ids", uint64List(p.NodeIDs),
		"node_count", len(p.NodeIDs),
		"created_at", nullableISO(p.CreatedAt),
		"updated_at", nullableISO(p.UpdatedAt),
	)
}

// presentExitProfileNodes 输出前置节点数组（详情接口用）。
func presentExitProfileNodes(nodes []ExitProfileNode) jsArr {
	out := make(jsArr, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		out = append(out, jo(
			"id", n.ID,
			"name", n.Name,
			"host", n.Host,
			"driver_type", n.DriverType,
			"status", n.Status,
			"enabled", n.Enabled,
		))
	}
	return out
}

// uint64List 输出 JSON 整数数组（id 都在 uint64 范围内）。
func uint64List(ids []uint64) jsArr {
	out := make(jsArr, 0, len(ids))
	for _, v := range ids {
		out = append(out, v)
	}
	return out
}
