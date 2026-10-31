package admin

// 管理端节点域（/admin-api/nodes…）：nodes CRUD + xray 入站/出站/路由/WARP/出站订阅
// + 第三方订阅。逐条对齐 backend/app/Http/Controllers/Admin 下的：
//
//	NodeController.php                节点 CRUD / test / upgrade-agent / probe-inbounds / 倍率
//	XrayInboundController.php         入站 CRUD / batch-delete / reality-keypair / reality-scan
//	XrayOutboundController.php        出站 CRUD / import / link / test / relay / WARP 合并
//	XrayRoutingController.php         路由规则 + balancer + settings + test
//	NodeWarpController.php            WARP 注册 / 应用 / 换 IP / license / 删除
//	OutboundSubscriptionController.php 出站订阅（服务器池）CRUD + refresh
//	ThirdPartyController.php          第三方机场订阅 + 总开关
//
// 本域与 P8d 商店域共享 json.go / request.go / validate.go 的**工具**（响应编码、输入整理、
// 校验器），但持有自己的依赖（NodeStore / NodeOps），互不影响。

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// NodeDomain 是节点域的 HTTP 入口。
type NodeDomain struct {
	store NodeStore
	ops   NodeOps
	cfg   NodeConfig

	// tasks 把「建好的 async_tasks」派进队列。为空时只落库不执行（仅供测试/无队列场景）。
	// 节点类任务（初始化 / 入站同步 / 删除清理）必须经它派发，否则任务永远是 pending。
	tasks NodeTaskDispatcher

	// now 供测试注入固定时钟（PHP 侧全部用 now()，时区 UTC）。
	now func() time.Time

	logger *log.Logger

	// pollInterval 是同步等待指令回执的轮询间隔（PHP usleep(300_000) / 500_000）。
	// 测试注入小值加速。
	pollInterval time.Duration
}

// NodeTaskDispatcher 把异步任务派进执行队列（对齐 PHP 的 dispatchAfterCommit）。
//
// 独立成小接口：admin 包不能 import internal/task（会与 task 反向依赖），
// 真实实现在 cmd/server（nodeTaskDispatcher）。
type NodeTaskDispatcher interface {
	// Dispatch 按 pending item 逐批派发（node_init → NodeInitUserJob、
	// cleanup_clients → NodeClientsCleanupJob、sync_inbounds → NodeInboundSyncJob）。
	Dispatch(ctx context.Context, taskID uint64) error
	// DispatchInboundScan 派发「入站同步扫描」Job（一台节点一个，不占 item）：
	// 先拉一次客户端清单做 diff，只有缺挂载的才逐个 attach。
	DispatchInboundScan(ctx context.Context, taskID uint64, nodeID uint64) error
}

// NodeDeps 是 NodeDomain 的依赖清单（显式传参，避免测试构造半吊子实例）。
type NodeDeps struct {
	Store NodeStore
	Ops   NodeOps
	// Tasks 为空时创建的任务不会被执行（生产必须注入）。
	Tasks  NodeTaskDispatcher
	Config NodeConfig
	Now    func() time.Time
	Logger *log.Logger
}

// NewNodeDomain 组装节点域。
func NewNodeDomain(d NodeDeps) *NodeDomain {
	if d.Store == nil {
		panic("admin: NodeStore 不能为空")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &NodeDomain{
		store:        d.Store,
		ops:          d.Ops,
		tasks:        d.Tasks,
		cfg:          nodeConfigDefaults(d.Config),
		now:          now,
		logger:       logger,
		pollInterval: 300 * time.Millisecond,
	}
}

// Register 把节点域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
//
// ⚠️ 静态段必须排在参数路由之前（PHP web.php 已按此顺序书写，Gin 同样要求同级
// 静态段优先声明，否则 third-party / multiplier / reality-keypair 会被 {node} 吞掉）。
func (d *NodeDomain) Register(r gin.IRouter) {
	// ── 节点 ──
	r.GET("/nodes", d.nodeIndex)
	r.POST("/nodes", d.nodeStore)

	// 流量倍率（原始倍率 / 用户端展示开关）
	r.PUT("/nodes/multiplier/original", d.updateOriginalMultiplier)
	r.PUT("/nodes/multiplier/display", d.updateMultiplierDisplay)

	// 第三方订阅（固定路径，必须排在 /nodes/{node} 之前）
	r.GET("/nodes/third-party", d.thirdPartyIndex)
	r.POST("/nodes/third-party", d.thirdPartyStore)
	r.PUT("/nodes/third-party/switch", d.thirdPartySwitch)
	r.POST("/nodes/third-party/:sub/refresh", d.thirdPartyRefresh)
	r.PUT("/nodes/third-party/:sub", d.thirdPartyUpdate)
	r.DELETE("/nodes/third-party/:sub", d.thirdPartyDestroy)

	// 探测入站（固定路径；PHP 写在 /nodes/{node} 之后，但方法不同不冲突）
	r.POST("/nodes/probe-inbounds", d.probeInbounds)

	r.GET("/nodes/:node", d.nodeShow)
	r.PUT("/nodes/:node", d.nodeUpdate)
	r.DELETE("/nodes/:node", d.nodeDestroy)
	r.POST("/nodes/:node/test", d.nodeTest)
	r.POST("/nodes/:node/upgrade-agent", d.upgradeAgent)

	// ── 端口跳跃（HY2 udpHop 的节点侧防火墙，issue #94）──
	// 只把白名单载荷转发成指令，不自己动防火墙；agent < 1.0.7 直接 4xx。
	r.POST("/nodes/:node/hop/probe", d.hopProbe)
	r.POST("/nodes/:node/hop/apply", d.hopApply)
	r.POST("/nodes/:node/hop/remove", d.hopRemove)
	r.POST("/nodes/:node/hop/status", d.hopStatus)

	// ── 系统调优（sysctl / BBR，issue #94 R4）──
	// 同样只转发白名单载荷；key / profile 白名单镜像 agent 侧。
	r.POST("/nodes/:node/sysctl", d.sysctlApply)

	// ── TLS 证书库（hysteria 的 TLS 证书引用源）──
	r.GET("/xray-certificates", d.xrayCertificateIndex)
	r.POST("/xray-certificates", d.xrayCertificateStore)
	// 本机证书目录同步（acme.sh 续签后刷新库里的 PEM）：静态段必须排在 :certificate 之前。
	r.POST("/xray-certificates/sync-local", d.xrayCertificateSyncLocal)
	r.DELETE("/xray-certificates/:certificate", d.xrayCertificateDestroy)

	// ── xray 入站 ──
	r.GET("/nodes/:node/xray-inbounds", d.xrayInboundIndex)
	r.POST("/nodes/:node/xray-inbounds", d.xrayInboundStore)
	r.POST("/nodes/:node/xray-inbounds/reality-keypair", d.realityKeypair)
	r.POST("/nodes/:node/xray-inbounds/batch-delete", d.xrayInboundBatchDestroy)
	r.POST("/nodes/:node/xray-inbounds/reality-scan", d.realityScan)
	r.PUT("/nodes/:node/xray-inbounds/:inbound", d.xrayInboundUpdate)
	r.DELETE("/nodes/:node/xray-inbounds/:inbound", d.xrayInboundDestroy)

	// ── xray 出站 ──
	r.GET("/nodes/:node/xray-outbounds", d.xrayOutboundIndex)
	r.POST("/nodes/:node/xray-outbounds", d.xrayOutboundStore)
	r.POST("/nodes/:node/xray-outbounds/import", d.xrayOutboundImport)
	r.GET("/nodes/:node/xray-outbounds/:outbound/link", d.xrayOutboundLink)
	r.PUT("/nodes/:node/xray-outbounds/:outbound", d.xrayOutboundUpdate)
	r.DELETE("/nodes/:node/xray-outbounds/:outbound", d.xrayOutboundDestroy)
	r.POST("/nodes/:node/xray-outbounds/:outbound/test", d.xrayOutboundTest)

	// ── 出站订阅（服务器池） ──
	r.GET("/nodes/:node/xray-outbound-subscriptions", d.outboundSubIndex)
	r.POST("/nodes/:node/xray-outbound-subscriptions", d.outboundSubStore)
	r.PUT("/nodes/:node/xray-outbound-subscriptions/:subscription", d.outboundSubUpdate)
	r.DELETE("/nodes/:node/xray-outbound-subscriptions/:subscription", d.outboundSubDestroy)
	r.POST("/nodes/:node/xray-outbound-subscriptions/:subscription/refresh", d.outboundSubRefresh)

	// ── xray 路由 ──
	r.GET("/nodes/:node/xray-routing", d.routingIndex)
	r.POST("/nodes/:node/xray-routing", d.routingStore)
	r.POST("/nodes/:node/xray-routing/test", d.routingTest)
	// 出站连通性延时测试（路由规则表「延时」列）。静态段必须排在 :rule 之前。
	r.POST("/nodes/:node/xray-routing/outbound-test", d.routingOutboundTest)
	r.POST("/nodes/:node/xray-routing/balancers", d.balancerStore)
	r.PUT("/nodes/:node/xray-routing/balancers/:balancer", d.balancerUpdate)
	r.DELETE("/nodes/:node/xray-routing/balancers/:balancer", d.balancerDestroy)
	r.PUT("/nodes/:node/xray-routing/settings", d.routingUpdateSettings)
	r.PUT("/nodes/:node/xray-routing/:rule", d.routingUpdate)
	r.DELETE("/nodes/:node/xray-routing/:rule", d.routingDestroy)

	// ── WARP ──
	r.GET("/nodes/:node/warp", d.warpShow)
	r.POST("/nodes/:node/warp/register", d.warpRegister)
	r.POST("/nodes/:node/warp/apply", d.warpApply)
	r.POST("/nodes/:node/warp/rotate", d.warpRotate)
	r.PUT("/nodes/:node/warp", d.warpUpdate)
	r.DELETE("/nodes/:node/warp", d.warpDestroy)
}

// ---- 响应封装（与 P8d handler 同口径：HTTP 200 + PHP json_encode 字节）----

func (d *NodeDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *NodeDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// failValidation 渲染 Laravel 校验失败：HTTP 200 + code 500 + 合并文案。
func (d *NodeDomain) failNodeValidation(c *gin.Context, v *nodeValidator) {
	if err := v.storeFailure(); err != nil {
		d.logger.Printf("admin 节点校验时查库失败: %v", err)
		d.fail(c, "服务器错误", 500)
		return
	}
	d.fail(c, v.summary(), 500)
}

func (d *NodeDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// abort422 对齐 PHP 的 abort(422, $msg)：HttpException 的 status 变 code、message 变 msg。
func (d *NodeDomain) abort422(c *gin.Context, msg string) { d.fail(c, msg, 422) }

// notFound 对齐 bootstrap/app.php 对 NotFoundHttpException 的统一改写。
func (d *NodeDomain) notFound(c *gin.Context) { d.fail(c, "接口不存在", 404) }

// noRowsToNotFound 把查无此行映射成 404；其余错误返回 false 交给调用方。
func (d *NodeDomain) noRowsToNotFound(c *gin.Context, err error) bool {
	if errors.Is(err, sql.ErrNoRows) || NoRows(err) {
		d.notFound(c)
		return true
	}
	return false
}

// successPage 对齐 ApiResponse::successPage（固定 50/页，越界夹回最后一页）。
func (d *NodeDomain) successPage(c *gin.Context, items []any, page, perPage, total int64) {
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

// ---- 共用：取节点 + xray 守卫 ----

// loadNode 按路由参数取节点；非数字 / 不存在 → 404（对齐隐式模型绑定）。
func (d *NodeDomain) loadNode(c *gin.Context) (*NodeRecord, bool) {
	id, ok := parseUint(c.Param("node"))
	if !ok {
		d.notFound(c)
		return nil, false
	}
	n, err := d.store.NodeByID(ctxOf(c), id)
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return nil, false
		}
		d.failServer(c, "查询节点", err)
		return nil, false
	}
	return n, true
}

// loadXrayNode 取节点并校验 driver_type=xray（PHP：ensureXray → abort(422)）。
func (d *NodeDomain) loadXrayNode(c *gin.Context) (*NodeRecord, bool) {
	n, ok := d.loadNode(c)
	if !ok {
		return nil, false
	}
	if !n.IsXray() {
		d.abort422(c, "仅 xray 节点支持该操作")
		return nil, false
	}
	return n, true
}

// sleep 是同步等待指令回执的等待点（测试注入 0 间隔避免拖慢用例）。
func (d *NodeDomain) sleep(dur time.Duration) {
	if dur > 0 {
		time.Sleep(dur)
	}
}

// timeDurationSeconds 把 PHP 的「秒」配置转成 Duration。
func timeDurationSeconds(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// noRowsToNotFoundSilent 判定「查无此行」但不写响应（batch 场景按 id 回报）。
func (d *NodeDomain) noRowsToNotFoundSilent(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || NoRows(err)
}

// ---- 小工具 ----

// phpRawURLEncode 复刻 PHP 的 rawurlencode（RFC 3986；空格 → %20）。
//
// 与 Go 的 url.PathEscape 差异：PHP 保留 ~ 不编码、编码 ! ' ( ) *；PathEscape 反之。
func phpRawURLEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// randomAlnum 复刻 Laravel Str::random($len)（字母数字随机串）。
func randomAlnum(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(b)
}

// randIntn 是 math/rand 的薄包装（中转 UUID 用）。
func randIntn(n int) int { return rand.Intn(n) }

// nodeFloatPtr 把 decimal 字符串按 PHP (float) 输出（nil → null）。
func nodeFloatPtr(v *string) any {
	if v == nil {
		return nil
	}
	f, err := strconv.ParseFloat(*v, 64)
	if err != nil {
		return nil
	}
	return phpFloat(f)
}

// nodeIntFromString 按 PHP (int) 读 decimal 字符串（解析失败 → 0）。
func nodeIntFromString(v *string) int64 {
	if v == nil {
		return 0
	}
	f, err := strconv.ParseFloat(*v, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

// ptrStringOf 便于写 `&x`。
func ptrStringOf(v string) *string { return &v }

// ptrUintOf 便于写 `&x`。
func ptrUintOf(v uint64) *uint64 { return &v }
