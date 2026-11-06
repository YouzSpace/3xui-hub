package nodeapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// /api/node-api/* 六端点：逐行对齐 NodeApiController，响应壳与 PHP 完全一致。
//
// 响应格式（contracts/api-contract.md §1）：
//   成功 {"code":0,"msg":"ok","data":...}
//   失败 {"code":<非0>,"msg":...,"data":null}，HTTP status 默认 200
// 唯一例外是节点鉴权失败 → http 401（§2 明确要求）。

// Handler 是六端点的 HTTP 入口。
type Handler struct {
	Repo *Repo
	// AppKey 是 AES-256-CBC 密钥（解密 driver_config.node_secret 等）。
	AppKey []byte
	// Now 可注入（测试固定时间用）。
	Now func() time.Time

	// ExecuteWait 是 execute 长轮询挂起上限（PHP EXECUTE_WAIT_SECONDS=20）。
	ExecuteWait time.Duration
	// PollInterval 是长轮询期间的轮询间隔（PHP sleep(2)）。
	PollInterval time.Duration
	// PullInterval 覆盖配置拉取周期（<=0 时读 site_configs）。
	PullIntervalOverride int
	// WSEnabled 覆盖 XRAY_WS_ENABLED（nil = 读环境变量，缺省 true）。
	WSEnabled *bool

	// authNodes 是测试注入的节点来源（生产为 nil，走 Repo.XrayNodes）。
	authNodes func() ([]Node, error)
	// configSource 是测试注入的渲染数据来源（生产为 nil，走 Repo.LoadFingerprint/LoadRenderData）。
	//
	// 注入点是「数据来源」而不是「响应拼装」：编码、ETag、304 判定仍走生产代码路径，
	// 因此 config 的逐字节对拍覆盖到真实 handler。
	configSource func(ctx context.Context, node Node) (fingerprint string, rendered []byte, configVersion int64, err error)
}

// NewHandler 构造 Handler。
func NewHandler(repo *Repo, appKey []byte) *Handler {
	return &Handler{
		Repo:         repo,
		AppKey:       appKey,
		ExecuteWait:  20 * time.Second,
		PollInterval: commandPollInterval,
	}
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// NodeAuth 是 gin 中间件版 NodeAuth：校验通过后把 *Node 放进上下文。
//
// 与 PHP 一样「逐节点解密比对」：节点数量是个位数到几十，每次鉴权全表解密
// 的开销可以接受；引入缓存会带来轮换密钥后旧 agent 仍能通过的风险。
func (h *Handler) NodeAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		secret := c.GetHeader("X-Node-Secret")
		node, err := h.authenticate(c.Request.Context(), secret)
		if err != nil {
			switch {
			case errors.Is(err, ErrMissingSecret):
				writePHPJSON(c, http.StatusUnauthorized, Object{
					{Key: "code", Value: 401},
					{Key: "msg", Value: "missing node secret"},
				})
				c.Abort()
				return
			case errors.Is(err, ErrInvalidSecret):
				writePHPJSON(c, http.StatusUnauthorized, Object{
					{Key: "code", Value: 401},
					{Key: "msg", Value: "invalid node secret"},
				})
				c.Abort()
				return
			default:
				// 数据库不可用等：PHP 侧会抛 500（http 200 信封）；这里保持同一信封语义。
				apiError(c, 500, "internal error")
				c.Abort()
				return
			}
		}
		c.Set(ctxNodeKey, node)
		c.Next()
	}
}

// authenticate 是 NodeAuth 中间件的鉴权逻辑；authNodes 非空时用注入的节点来源
// （仅测试使用，生产路径永远是 Repo.XrayNodes）。
func (h *Handler) authenticate(ctx context.Context, secret string) (Node, error) {
	if h.authNodes == nil {
		return h.Repo.Authenticate(ctx, secret, h.AppKey)
	}
	if secret == "" {
		return Node{}, ErrMissingSecret
	}
	nodes, err := h.authNodes()
	if err != nil {
		return Node{}, err
	}
	if node, ok := matchNodeSecret(nodes, secret, h.AppKey); ok {
		return node, nil
	}
	return Node{}, ErrInvalidSecret
}

const ctxNodeKey = "nodeapi.node"

func nodeFrom(c *gin.Context) (Node, bool) {
	v, ok := c.Get(ctxNodeKey)
	if !ok {
		return Node{}, false
	}
	n, ok := v.(Node)
	return n, ok
}

// ── 响应辅助 ─────────────────────────────────────────────────

// success 复刻 NodeApiController 的成功响应：{"code":0,"data":…}。
//
// 注意这里**没有 msg**：NodeApiController 直接 `response()->json(['code'=>0,'data'=>…])`，
// 没有走 ApiResponse trait（与用户端/管理端不同）。多写一个 "msg":"ok" 会让响应体
// 与 PHP 不一致（已用真实 PHP 面板逐字节对拍确认）。
func success(c *gin.Context, data any) {
	writePHPJSON(c, http.StatusOK, Object{
		{Key: "code", Value: 0},
		{Key: "data", Value: data},
	})
}

// apiError 复刻全局异常渲染：http 200 + {"code":<code>,"msg":...,"data":null}。
func apiError(c *gin.Context, code int, message string) {
	writePHPJSON(c, http.StatusOK, Object{
		{Key: "code", Value: code},
		{Key: "msg", Value: message},
		{Key: "data", Value: nil},
	})
}

// writePHPJSON 按 PHP response()->json() 的口径写响应：转义斜杠与非 ASCII
// （\uXXXX），< > & 不转义，键序保留。
//
// 为什么不用 c.JSON：encoding/json 不转义斜杠、中文原样输出，与 PHP 的默认
// json_encode 参数不同。agent 的解析不受影响，但「逐字节对齐」是本项目的验收
// 铁律（也便于运维直接 diff 两份响应）。
func writePHPJSON(c *gin.Context, status int, v any) {
	var buf bytes.Buffer
	if err := appendValuePHPJSONResponse(&buf, v); err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Data(status, "application/json", buf.Bytes())
}

// validationFailed 复刻 bootstrap/app.php 里对 ValidationException 的渲染：
// code=500、msg=首条文案（含 "(and N more errors)"），http 200。
func validationFailed(c *gin.Context, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		apiError(c, 500, ve.Summary())
		return
	}
	apiError(c, 500, "服务器错误")
}

// readBody 读取请求体。上限 8MiB：push 单次最多 2000 条，正常载荷远小于此，
// 但 reality_scan 的 ack output 可到 40000 字符，留足余量。
//
// 支持 Content-Encoding: gzip —— agent 对超过 4 KiB 的 POST 报文（典型是 push 的
// 流量表）会压缩，压缩后未解压的原始字节同样按 8MiB 限制，防止解压炸弹。
func readBody(c *gin.Context) ([]byte, error) {
	const maxBody = 8 << 20
	var r io.Reader = c.Request.Body
	if strings.EqualFold(strings.TrimSpace(c.GetHeader("Content-Encoding")), "gzip") {
		gz, err := gzip.NewReader(c.Request.Body)
		if err != nil {
			return nil, fmt.Errorf("请求体 gzip 解压失败: %w", err)
		}
		defer gz.Close()
		r = gz
	}
	return io.ReadAll(io.LimitReader(r, maxBody))
}

// ── POST /api/node-api/register ──────────────────────────────

// Register 复刻 register()：只负责「连上」，不产生配置内容、不 bump config_version。
func (h *Handler) Register(c *gin.Context) {
	node, ok := nodeFrom(c)
	if !ok {
		apiError(c, 500, "internal error")
		return
	}
	ctx := c.Request.Context()

	raw, err := readBody(c)
	if err != nil {
		apiError(c, 500, "服务器错误")
		return
	}
	body, err := parseBody(raw)
	if err != nil {
		// PHP 对非法 JSON 的请求体按空数组处理，validate 会报 required 类文案；
		// register 全是 nullable，因此这里等价于「全部缺省」。
		body = Object{}
	}
	if err := validateRegister(body); err != nil {
		validationFailed(c, err)
		return
	}
	data, _ := mapToPlain(body).(map[string]any)

	cfg := Object{}
	for _, k := range []string{"xray_version", "agent_version", "agent_sha", "kernel_arch", "hostname"} {
		if s, ok := data[k].(string); ok && s != "" {
			cfg.Set(k, s)
		}
	}
	// agent 刚从重启流程进来，WS 尚未连接（连上后由 WS 服务写 true）。
	cfg.Set("ws", Object{
		{Key: "connected", Value: false},
		{Key: "at", Value: iso8601(h.now())},
	})

	if err := h.Repo.MergeDriverConfig(ctx, node.ID, cfg); err != nil {
		apiError(c, 500, "internal error")
		return
	}

	// 连接地址自动回填：host 为空时用来源公网 IP 预填（订阅链接依赖 host）。
	if node.DriverType == "xray" && strings.TrimSpace(node.Host) == "" {
		if ip := clientIP(c.Request); ip != "" && isPublicIP(ip) {
			if _, err := h.Repo.SetHostIfEmpty(ctx, node.ID, ip); err != nil {
				// 回填失败不影响注册主流程（PHP 侧 save() 一并失败才会 500）。
				_ = err
			}
		}
	}

	if err := h.Repo.MarkOnline(ctx, node.ID); err != nil {
		apiError(c, 500, "internal error")
		return
	}

	// 重新读节点，拿到最新（可能刚被并发 bump 的）config_version。
	fresh, err := h.Repo.NodeByID(ctx, node.ID)
	if err != nil {
		apiError(c, 500, "internal error")
		return
	}

	success(c, Object{
		{Key: "node_name", Value: fresh.Name},
		{Key: "config_version", Value: fresh.ConfigVersion()},
		{Key: "pull_interval", Value: h.pullInterval(ctx)},
		{Key: "ws", Value: h.wsInfo(c.Request)},
	})
}

// ── GET /api/node-api/config ─────────────────────────────────

// Config 复刻 config()：轻量指纹判 304，指纹变了才全量渲染。
//
// ETag 是 render() 结果的 sha1；指纹（fingerprint）覆盖 render 的全部数据源，
// 指纹相同 ⇒ 内容必然相同 ⇒ 直接 304。
func (h *Handler) Config(c *gin.Context) {
	node, ok := nodeFrom(c)
	if !ok {
		apiError(c, 500, "internal error")
		return
	}
	ctx := c.Request.Context()

	var (
		fingerprint   string
		rendered      []byte
		configVersion int64
		err           error
	)
	if h.configSource != nil {
		fingerprint, rendered, configVersion, err = h.configSource(ctx, node)
		if err != nil {
			apiError(c, 500, "internal error")
			return
		}
	} else {
		fp, ferr := h.Repo.LoadFingerprint(ctx, node)
		if ferr != nil {
			apiError(c, 500, "internal error")
			return
		}
		if fingerprint, err = Fingerprint(fp); err != nil {
			apiError(c, 500, "internal error")
			return
		}
		configVersion = node.ConfigVersion()
	}
	etag := `"` + fingerprint + `"`

	// If-None-Match 按 RFC 7232 弱比较：CDN 压缩响应会把强 ETag 改写成 W/"…"，
	// agent 的 Go http 客户端默认带 Accept-Encoding: gzip，收到的永远是弱 ETag。
	if inm := strings.TrimSpace(c.GetHeader("If-None-Match")); inm != "" &&
		normalizeETag(inm) == normalizeETag(etag) {
		c.Header("ETag", etag)
		c.Status(http.StatusNotModified)
		return
	}

	if h.configSource == nil {
		data, derr := h.Repo.LoadRenderData(ctx, node)
		if derr != nil {
			apiError(c, 500, "internal error")
			return
		}
		if rendered, err = RenderJSON(data, h.AppKey); err != nil {
			apiError(c, 500, "internal error")
			return
		}
	}

	// 手工拼包：config 必须原样嵌入（stats 是 {} 而不是 []，xray 解析 [] 会报错）。
	var buf strings.Builder
	buf.WriteString(`{"code":0,"data":{"config":`)
	buf.Write(rendered)
	buf.WriteString(`,"config_version":`)
	buf.WriteString(strconv.FormatInt(configVersion, 10))
	buf.WriteString(`,"pull_interval":`)
	buf.WriteString(strconv.Itoa(h.pullInterval(ctx)))
	buf.WriteString(`}}`)

	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("ETag", etag)

	// 配置下发是整套系统最大的响应：百万用户约 2 MiB，5 万用户约 6 MiB。
	// 节点多为 1 Mbps 小带宽，未压缩时 2 MiB 要传 17 秒；gzip 压缩率约 19 倍
	// （每个 client 的键名与结构完全重复），实测 2 MiB → 108 KiB，降到 0.9 秒。
	// agent 的 Go http 客户端默认带 Accept-Encoding: gzip 并自动解压，无需配合改动。
	body := []byte(buf.String())
	if len(body) > 1024 && acceptsGzip(c.GetHeader("Accept-Encoding")) {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		if _, werr := zw.Write(body); werr == nil {
			if cerr := zw.Close(); cerr == nil {
				c.Header("Content-Encoding", "gzip")
				// Vary 让中间缓存（nginx/CDN）按编码区分副本，避免把压缩体喂给不支持 gzip 的客户端
				c.Header("Vary", "Accept-Encoding")
				c.Data(http.StatusOK, "application/json; charset=utf-8", gz.Bytes())
				return
			}
		}
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// acceptsGzip 判断请求头是否接受 gzip 编码。
// HTTP 令牌大小写不敏感（RFC 7230 §3.2.6），所以用 EqualFold；q 值不解析——
// 只要出现 gzip 就压缩，q=0 的罕见场景交给上游缓存处理。
func acceptsGzip(header string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

// normalizeETag 复刻 PHP 的 $normalizeEtag：剥掉可选的 W/ 前缀与引号后比较。
func normalizeETag(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == 'W' || v[0] == 'w') && v[1] == '/' {
		v = v[2:]
	}
	return strings.Trim(v, `"`)
}

// ── POST /api/node-api/push ──────────────────────────────────

// Push 复刻 push()：流量绝对值 → 增量落账。
//
// 超过单次上限时 PHP 先显式拦一道并返回 42201 / http 422（让人话错误可见），
// 这里保持同样的状态码与文案。
func (h *Handler) Push(c *gin.Context) {
	node, ok := nodeFrom(c)
	if !ok {
		apiError(c, 500, "internal error")
		return
	}
	ctx := c.Request.Context()

	raw, err := readBody(c)
	if err != nil {
		apiError(c, 500, "服务器错误")
		return
	}
	body, err := parseBody(raw)
	if err != nil {
		body = Object{}
	}

	// 显式上限拦截（PHP 在 validate 之前对**原始输入** count($rawStats) 判断，
	// 关联数组与列表都算，所以这里两种容器都要看）。
	if statsVal, ok := body.(Object).Get("stats"); ok {
		if n, countable := countContainer(statsVal); countable && n > MaxStatsPerPush {
			writePHPJSON(c, http.StatusUnprocessableEntity, Object{
				{Key: "code", Value: 42201},
				{Key: "msg", Value: fmt.Sprintf("stats 超过单次上限 %d 条，请只上报有变化的用户", MaxStatsPerPush)},
				{Key: "data", Value: nil},
			})
			return
		}
	}

	if err := validatePush(body); err != nil {
		validationFailed(c, err)
		return
	}

	stats := ParsePushStats(body)
	result, err := h.Repo.SyncNodeFromPush(ctx, node, stats)
	if err != nil {
		apiError(c, 500, "internal error")
		return
	}

	success(c, Object{
		{Key: "synced", Value: result.Acquired},
		{Key: "delta_users", Value: len(result.DeltaMap)},
	})
}

// ── POST /api/node-api/alive ─────────────────────────────────

// Alive 复刻 alive()：心跳落库 status=online + agent 指标合并进 driver_config。
func (h *Handler) Alive(c *gin.Context) {
	node, ok := nodeFrom(c)
	if !ok {
		apiError(c, 500, "internal error")
		return
	}
	ctx := c.Request.Context()

	raw, err := readBody(c)
	if err != nil {
		apiError(c, 500, "服务器错误")
		return
	}
	body, err := parseBody(raw)
	if err != nil {
		body = Object{}
	}
	if err := validateAlive(body); err != nil {
		validationFailed(c, err)
		return
	}
	data, _ := mapToPlain(body).(map[string]any)

	// agent 指标块：缺省为 null（不是 0）——「没上报」与「上报了 0」必须可区分。
	agent := Object{
		{Key: "cpu", Value: nullableNumber(data["cpu"])},
		{Key: "mem", Value: nullableNumber(data["mem"])},
		{Key: "uptime", Value: nullableInt(data["uptime"])},
		{Key: "conns", Value: nullableInt(data["conns"])},
		{Key: "active_users", Value: nullableInt(data["active_users"])},
		{Key: "up_speed", Value: nullableNumber(data["up_speed"])},
		{Key: "down_speed", Value: nullableNumber(data["down_speed"])},
		{Key: "net_total_up", Value: nullableInt(data["net_total_up"])},
		{Key: "net_total_down", Value: nullableInt(data["net_total_down"])},
		// issue #115 3.5：client 名单指纹（裸内核对账的短路基线，与 WS report 同键同名）。
		// 旧 agent 不带 → null：必须让上一轮留下的基线失效，不能拿旧指纹短路。
		{Key: "clients_fp", Value: ClientsFingerprintPatch(data)},
		{Key: "at", Value: iso8601(h.now())},
	}

	cfg := Object{{Key: "agent", Value: agent}}
	if s, ok := data["xray_version"].(string); ok && s != "" {
		cfg.Set("xray_version", s)
	}

	// issue #94 R5：BBR 运行态（agent 报的是 /proc/sys 的实际值）。老 agent 不带这个
	// 键，此时不覆盖已有的 bbr —— 见 BBRStatusPatch 的注释。
	if bbr, ok := BBRStatusPatch(data); ok {
		cfg.Set("bbr", bbr)
	}

	// 配置滞后：agent 报的版本落后于面板当前版本 → 记录「落后了多久」。
	current := node.ConfigVersion()
	reported := intOf(data["config_version"])
	if reported < current {
		if changedAt, ok := node.ConfigChangedAt(); ok {
			lag := int64(h.now().Sub(changedAt).Seconds())
			if lag < 0 {
				lag = 0
			}
			cfg.Set("lag_seconds", lag)
		} else {
			cfg.Set("lag_seconds", 0)
		}
	} else {
		cfg.Set("lag_seconds", 0)
	}

	// 只合并运行时键（整块回写会把并发 bump 的 config_version 覆盖回旧值）。
	if err := h.Repo.MergeDriverConfigAndMarkOnline(ctx, node.ID, cfg); err != nil {
		apiError(c, 500, "internal error")
		return
	}

	fresh, err := h.Repo.NodeByID(ctx, node.ID)
	if err != nil {
		apiError(c, 500, "internal error")
		return
	}

	success(c, Object{
		{Key: "config_version", Value: fresh.ConfigVersion()},
		{Key: "pull_interval", Value: h.pullInterval(ctx)},
	})
}

// nullableNumber 复刻 isset($data[k]) ? (float)$data[k] : null。
func nullableNumber(v any) any {
	if v == nil {
		return nil
	}
	f := floatOf(v)
	return jsonFloat(f)
}

// nullableInt 复刻 isset($data[k]) ? (int)$data[k] : null。
func nullableInt(v any) any {
	if v == nil {
		return nil
	}
	return intOf(v)
}

// jsonFloat 让整数值的浮点按 PHP 口径输出（33.0 → 33，与 json_encode(33.0) 的 "33.0" 不同）。
//
// 说明：PHP 的 (float)33 经 json_encode 得到 33.0；但 alive 写入的是 driver_config，
// 不在 config 渲染对拍路径上，这里按整数值输出整数以避免 JSON 里出现无意义的 .0。
func jsonFloat(f float64) any {
	if f == float64(int64(f)) {
		return int64(f)
	}
	return f
}

// ── POST /api/node-api/execute（长轮询） ─────────────────────

// Execute 复刻 execute()：≤20s 挂起，取走 pending 指令即原子认领。
//
// 长轮询的连接成本：与 PHP 一样，每个 agent 会占住一个 goroutine 最多 20 秒
// （PHP 侧占住一个 FPM 进程）。区别是 Go 的 goroutine 只有几 KB、且轮询间隔内
// 在 select 上等待（不消耗 CPU），因此不需要 PHP 的 pm.max_children 调优。
// 注意 main.go 的 http.Server WriteTimeout=60s 已大于 20s；若后续调小该值，
// 必须同步保证 > ExecuteWait，否则长轮询会被服务端提前掐断
// （agent 侧表现为「execute 偶尔失败并重连」）。
func (h *Handler) Execute(c *gin.Context) {
	node, ok := nodeFrom(c)
	if !ok {
		apiError(c, 500, "internal error")
		return
	}
	ctx := c.Request.Context()

	wait := h.ExecuteWait
	if wait <= 0 {
		wait = 20 * time.Second
	}
	interval := h.PollInterval
	if interval <= 0 {
		interval = commandPollInterval
	}
	deadline := time.Now().Add(wait)

	for {
		commands, err := h.Repo.ClaimPending(ctx, node.ID, maxCommandsPerClaim)
		if err != nil {
			apiError(c, 500, "internal error")
			return
		}
		if len(commands) > 0 {
			success(c, Object{{Key: "commands", Value: commandsToJSON(commands)}})
			return
		}
		if time.Now().After(deadline) {
			success(c, Object{{Key: "commands", Value: Array{}}})
			return
		}

		// 客户端断开（agent 超时重连）时尽快释放连接。
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func commandsToJSON(commands []Command) Array {
	out := make(Array, 0, len(commands))
	for _, cmd := range commands {
		var payload any = Object{}
		if len(cmd.Payload) > 0 {
			if parsed, err := ParseJSON(cmd.Payload); err == nil && parsed != nil {
				payload = parsed
			}
		}
		out = append(out, Object{
			{Key: "id", Value: cmd.ID},
			{Key: "action", Value: cmd.Action},
			{Key: "payload", Value: payload},
		})
	}
	return out
}

// ── POST /api/node-api/ack ───────────────────────────────────

// Ack 复刻 ack()：执行回执（success/failed + 输出摘录）。
func (h *Handler) Ack(c *gin.Context) {
	node, ok := nodeFrom(c)
	if !ok {
		apiError(c, 500, "internal error")
		return
	}
	ctx := c.Request.Context()

	raw, err := readBody(c)
	if err != nil {
		apiError(c, 500, "服务器错误")
		return
	}
	body, err := parseBody(raw)
	if err != nil {
		body = Object{}
	}
	if err := validateAck(body); err != nil {
		validationFailed(c, err)
		return
	}

	updated, err := h.Repo.ApplyAcks(ctx, node.ID, ParseAckResults(body))
	if err != nil {
		apiError(c, 500, "internal error")
		return
	}
	success(c, Object{{Key: "updated", Value: updated}})
}

// ── helpers ─────────────────────────────────────────────────

// pullInterval 复刻 pullInterval()：site_configs.xray_pull_interval，<5 回落 30。
func (h *Handler) pullInterval(ctx context.Context) int {
	if h.PullIntervalOverride >= 5 {
		return h.PullIntervalOverride
	}
	return h.Repo.PullInterval(ctx)
}

// wsEnabled 对应 config('xray.ws_enabled')（XRAY_WS_ENABLED，缺省 true）。
func (h *Handler) wsEnabled() bool {
	if h.WSEnabled != nil {
		return *h.WSEnabled
	}
	raw := strings.TrimSpace(os.Getenv("XRAY_WS_ENABLED"))
	if raw == "" {
		return true
	}
	// Laravel 的 (bool) env() 口径：PHP 里 "false"/"0" 为 false，"true"/"1" 为 true。
	switch strings.ToLower(raw) {
	case "false", "0", "off", "no", "":
		return false
	}
	return true
}

// wsPublicURL 对应 config('xray.ws_public_url')：XRAY_WS_PUBLIC_URL 整体覆盖。
func wsPublicURL() string { return strings.TrimSpace(os.Getenv("XRAY_WS_PUBLIC_URL")) }

// wsPath 对应 config('xray.ws_path')（缺省 /node-ws）。
func wsPath() string {
	if p := strings.TrimSpace(os.Getenv("XRAY_WS_PATH")); p != "" {
		return p
	}
	return "/node-ws"
}

// wsInfo 复刻 wsInfo()：register 下发给 agent 的 WS 连接信息。
//
// 配置源与 PHP 一致：XRAY_WS_ENABLED / XRAY_WS_PUBLIC_URL / XRAY_WS_PATH；
// scheme 用 https 判定 + X-Forwarded-Proto 双保险。
func (h *Handler) wsInfo(r *http.Request) Object {
	if !h.wsEnabled() {
		return Object{
			{Key: "enabled", Value: false},
			{Key: "url", Value: ""},
		}
	}
	if override := wsPublicURL(); override != "" {
		return Object{
			{Key: "enabled", Value: true},
			{Key: "url", Value: override},
		}
	}
	scheme := "ws"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	return Object{
		{Key: "enabled", Value: true},
		{Key: "url", Value: scheme + "://" + hostOfRequest(r) + wsPath()},
	}
}

// hostOfRequest 复刻 $request->getHost()：去掉端口，保留 IPv6 方括号字面量。
//
// Laravel 的 getHost() 走 Symfony Request::getHost()，只信任受信代理注入的
// X-Forwarded-Host；本项目的 TrustProxies 未配置（不信任任何代理），所以这里
// 同样只读 Host 头 —— 否则伪造头会进到 register 下发的 ws.url 里。
func hostOfRequest(r *http.Request) string {
	host := r.Host
	// 与 Symfony 一致：先剥端口。IPv6 字面量形如 [::1]:8099，端口在最后一个 ]
	// 之后；裸 IPv6（无方括号）不是合法 Host 头，不在此处理。
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return host
}

// clientIP 复刻 $request->ip()：X-Forwarded-For 首个地址，否则 RemoteAddr。
//
// 注意：Laravel 只信任受信代理注入的 XFF。本项目 install.sh 的 nginx 会注入
// X-Forwarded-For，而 TrustProxies 未配置（Laravel 默认不信任任何代理），
// 因此 PHP 实际拿到的是**直连地址**。这里保持一致：只取 RemoteAddr，
// 不读 XFF —— 否则会把伪造头写进 nodes.host，污染订阅链接。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isPublicIP 复刻 PHP 8.5 的
// filter_var($ip, FILTER_VALIDATE_IP, FILTER_FLAG_NO_PRIV_RANGE|FILTER_FLAG_NO_RES_RANGE)。
//
// 口径按真实 PHP 实测（php -r 对拍过下面每个网段）：
//
//	IPv4 拒绝：0/8、10/8、127/8、169.254/16、172.16/12、192.168/16、224/4（含 255.255.255.255）
//	IPv4 放行：192.0.0.0/24、192.0.2.0/24、198.18/15、198.51.100/24、203.0.113/24、100.64/10
//	IPv6 拒绝：::/128、::1、fc00::/7、fe80::/10、IPv4 映射地址（::ffff:a.b.c.d）
//	IPv6 放行：fec0::/10、ff00::/8、2001:db8::/32、100::/64
//
// 注意：Go 的 net.IP.IsPrivate/IsLoopback 覆盖面与 PHP 不同（例如 100.64/10 在 Go 里
// 不是 private，PHP 也放行；但 Go 不把 240/4 当 multicast，PHP 拒绝），所以这里逐段手写。
func isPublicIP(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		// Go 的 To4() 会把 ::ffff:a.b.c.d 也收敛成 IPv4；PHP 对这种映射地址判 private。
		if strings.Contains(strings.ToLower(strings.TrimSpace(s)), ":") {
			return false // IPv4 映射/IPv6 文本里的 v4 → 按 PHP 口径不放行
		}
		switch {
		case ip4[0] == 0:
			return false
		case ip4[0] == 10:
			return false
		case ip4[0] == 127:
			return false
		case ip4[0] == 169 && ip4[1] == 254:
			return false
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return false
		case ip4[0] == 192 && ip4[1] == 168:
			return false
		case ip4[0] >= 224: // 组播 224/4 + 保留 240/4 + 广播
			return false
		}
		return true
	}

	// IPv6
	if ip.Equal(net.IPv6zero) || ip.Equal(net.IPv6loopback) || ip.Equal(net.IPv6unspecified) {
		return false
	}
	if ip.To4() != nil { // ::ffff:a.b.c.d
		return false
	}
	switch {
	case ip[0]&0xfe == 0xfc: // fc00::/7 unique local
		return false
	case ip[0] == 0xfe && ip[1]&0xc0 == 0x80: // fe80::/10 link local
		return false
	}
	return true
}

// countContainer 复刻 PHP 的 is_array($v) + count($v)：只有对象/数组可计数。
func countContainer(v any) (int, bool) {
	switch t := v.(type) {
	case Object:
		return len(t), true
	case Array:
		return len(t), true
	}
	return 0, false
}
