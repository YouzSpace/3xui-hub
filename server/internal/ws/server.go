package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"controlhub-server/internal/nodeapi"

	"github.com/gorilla/websocket"
)

// Server 是节点 WS 通道服务（NodeWsServer.php 的 Go 版，gorilla/websocket）。
//
// 进程形态：常驻（systemd 前台）。Run 起两条内部节拍 ——
// POLL_INTERVAL=2s 指令+配置变更推送、PING_INTERVAL=30s 心跳与僵死清理；
// 单条连接的读泵负责上行事件分发。
type Server struct {
	backend  Backend
	outbound OutboundStore
	appKey   []byte
	registry *Registry
	upgrader websocket.Upgrader

	// 节拍（默认取协议常量；测试可缩短）。
	pingInterval time.Duration
	staleAfter   time.Duration
	pollInterval time.Duration
	authTimeout  time.Duration
	overflowLog  time.Duration

	nowFn func() time.Time
	logf  func(format string, args ...any)
}

// Options 是 Server 的构造参数。
type Options struct {
	// Backend 是落库实现（生产传 nodeapi.NewRepo(db)）。
	Backend Backend
	// Outbound 是出站流量落表（nil → NopOutboundStore，表未建时的常态）。
	Outbound OutboundStore
	// AppKey 是 APP_KEY 解出的 32 字节 AES 密钥（节点密钥解密用）。
	AppKey []byte
	// Now 可注入（测试固定时间用）；nil 时用 time.Now()。
	Now func() time.Time
	// Logf 是日志出口（默认 log.Printf）。
	Logf func(format string, args ...any)
}

// NewServer 构造 WS 服务。protocol 常量在包级保留，节拍字段取默认值。
func NewServer(opts Options) *Server {
	s := &Server{
		backend:      opts.Backend,
		outbound:     opts.Outbound,
		appKey:       opts.AppKey,
		registry:     NewRegistry(),
		pingInterval: time.Duration(PING_INTERVAL) * time.Second,
		staleAfter:   time.Duration(STALE_SECONDS) * time.Second,
		pollInterval: time.Duration(POLL_INTERVAL) * time.Second,
		authTimeout:  time.Duration(AUTH_TIMEOUT) * time.Second,
		overflowLog:  time.Duration(OVERFLOW_LOG_INTERVAL) * time.Second,
		nowFn:        opts.Now,
		logf:         opts.Logf,
	}
	if s.outbound == nil {
		s.outbound = NopOutboundStore{}
	}
	if s.nowFn == nil {
		s.nowFn = time.Now
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	s.upgrader = websocket.Upgrader{
		// 与 workerman 一致：不限制 Origin（agent 是原生客户端，不发 Origin 头）。
		CheckOrigin: func(*http.Request) bool { return true },
	}
	return s
}

func (s *Server) now() time.Time { return s.nowFn().UTC() }

// Registry 暴露连接注册表（诊断/统计用）。
func (s *Server) Registry() *Registry { return s.registry }

// ServeHTTP 是 WS 握手入口（http.Handler）。路径由调用方决定（routes.go 不在本切片范围）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 鉴权超时：AUTH_TIMEOUT 内没完成鉴权就断开防挂连接。
	// PHP 的语义是 onConnect 起 10s 定时器、nodeId 仍为 0 即 close；
	// 这里等价地包住「升级 + 鉴权 + 注册」：超时后强关底层连接。
	// （「TCP 连上但不发 WS 握手」那一段由 http.Server 的 ReadHeaderTimeout 兜底，
	// 该超时由 cmd/server 配置，不在本切片范围。）
	var authMu sync.Mutex
	var upgraded *websocket.Conn
	authDone := false
	timer := time.AfterFunc(s.authTimeout, func() {
		authMu.Lock()
		defer authMu.Unlock()
		if authDone {
			return
		}
		authDone = true // 防止注册完成后再被本定时器关掉
		if upgraded != nil {
			_ = upgraded.Close()
		}
		s.logf("[node-ws] 握手鉴权超时（%ds），断开连接", AUTH_TIMEOUT)
	})
	defer timer.Stop()

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade 已写过 HTTP 错误响应，这里只记日志。
		s.logf("[node-ws] 升级失败：%v", err)
		return
	}
	authMu.Lock()
	upgraded = conn
	if authDone {
		// 定时器刚刚把连接关了，别继续用。
		authMu.Unlock()
		return
	}
	authMu.Unlock()

	c := newConn(conn, 0, s.now())

	secret := headerValue(r, "X-Node-Secret")
	if secret == "" {
		s.closeWith(c, "missing node secret")
		return
	}
	node, err := s.backend.Authenticate(r.Context(), secret, s.appKey)
	if err != nil {
		// 与 PHP 一致：missing / invalid 两种文案都直接断开。
		msg := "invalid node secret"
		if err == nodeapi.ErrMissingSecret {
			msg = "missing node secret"
		}
		s.closeWith(c, msg)
		return
	}

	c.node = node.ID
	c.lastSeen = s.now()
	c.notifiedVersion = node.ConfigVersion()

	authMu.Lock()
	authDone = true // 鉴权已完成，定时器不再强关
	authMu.Unlock()

	if old := s.registry.Add(node.ID, c); old != nil {
		// 同一节点重复连接：踢掉旧连接（agent 重启场景）。
		s.logf("[node-ws] node#%d 重复连接，踢掉旧连接", node.ID)
		s.closeQuiet(old)
	}

	s.logf("[node-ws] node#%d(%s) 已连接 remote=%s total=%d",
		node.ID, node.Name, r.RemoteAddr, s.registry.Count())

	s.readLoop(r.Context(), c, node.ID)
}

// readLoop 读上行消息直到断开（对应 Workerman onMessage + onClose）。
func (s *Server) readLoop(ctx context.Context, c *Conn, nodeID int64) {
	defer s.onClose(c, nodeID)

	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		c.Touch(s.now())

		msg, ok := decodeInbound(raw)
		if !ok {
			continue // PHP：!is_array($msg) → return
		}

		// 未鉴权连接不应有消息（鉴权在握手时完成）；防御性断开。
		if c.NodeID() <= 0 {
			s.closeQuiet(c)
			return
		}

		func() {
			defer func() {
				if rec := recover(); rec != nil {
					s.logf("[node-ws] 处理 %s 失败（panic）：%v", msg.Event, rec)
				}
			}()
			s.dispatch(ctx, c, nodeID, msg)
		}()
	}
}

// dispatch 按事件分发（PHP match($event)）。
func (s *Server) dispatch(ctx context.Context, c *Conn, nodeID int64, msg inbound) {
	var err error
	switch msg.Event {
	case EventHello:
		err = s.handleHello(ctx, c, nodeID, msg.Data)
	case EventReport:
		err = s.handleReport(ctx, c, nodeID, msg.Data)
	case EventAck:
		err = s.handleAck(ctx, nodeID, msg.Data)
	case EventPong:
		// lastSeen 已刷新，无需处理。
	default:
		// 未知事件：PHP default => null，静默忽略。
	}
	if err != nil {
		s.logf("[node-ws] 处理 %s 失败：%v（node_id=%d）", msg.Event, err, nodeID)
	}
}

// ── 上行消息处理 ─────────────────────────────────────────

// handleHello 注册元数据（版本/主机信息），回 hello.ok（含当前 config 版本）。
func (s *Server) handleHello(ctx context.Context, c *Conn, nodeID int64, data map[string]any) error {
	node, err := s.backend.NodeByID(ctx, nodeID)
	if err != nil {
		return err
	}
	if node.ID == 0 {
		s.closeQuiet(c)
		return fmt.Errorf("节点 #%d 不存在", nodeID)
	}

	var cfg nodeapi.Object
	if v := strOf(data["xray_version"]); v != "" {
		cfg.Set("xray_version", v)
	}
	if v := strOf(data["agent_version"]); v != "" {
		cfg.Set("agent_version", v)
	}
	if v := strOf(data["hostname"]); v != "" {
		cfg.Set("hostname", v)
	}
	if v := strOf(data["kernel_arch"]); v != "" {
		cfg.Set("kernel_arch", v)
	}
	cfg.Set("ws", nodeapi.Object{
		{Key: "connected", Value: true},
		{Key: "at", Value: iso8601(s.now())},
	})

	// 只合并这几个键：整块回写会把并发 bump 的 config_version 覆盖回旧值。
	// PHP 同一段是 merge + forceFill(status=online) 两步，这里用合并版一事务完成。
	if err := s.backend.MergeDriverConfigAndMarkOnline(ctx, nodeID, cfg); err != nil {
		return err
	}

	// PHP：$conn->context->notifiedVersion = max(0, (int)($data['config_version'] ?? 0));
	// 直接赋值（允许回退）—— agent 落后时下一次轮询要补发 config.changed。
	c.ForceNotifiedVersion(intOf(data["config_version"]))

	fresh, err := s.backend.NodeByID(ctx, nodeID)
	if err != nil {
		return err
	}
	return s.send(c, EventHelloOK, helloOK{
		NodeID:        fresh.ID,
		ConfigVersion: fresh.ConfigVersion(),
		PullInterval:  s.backend.PullInterval(ctx),
	})
}

// helloOK 是 hello.ok 的下行负载（键序与 PHP 一致）。
type helloOK struct {
	NodeID        int64 `json:"node_id"`
	ConfigVersion int64 `json:"config_version"`
	PullInterval  int   `json:"pull_interval"`
}

// handleReport 处理流量绝对值 + 出站流量 + 监控指标 + config 版本回执。
//
// 流量走 Backend.SyncNodeFromPush（与 HTTP /push 完全相同的增量结算），
// 指标写 driver_config.agent（与 HTTP /alive 完全相同的字段口径）。
func (s *Server) handleReport(ctx context.Context, c *Conn, nodeID int64, data map[string]any) error {
	node, err := s.backend.NodeByID(ctx, nodeID)
	if err != nil {
		return err
	}
	if node.ID == 0 {
		s.closeQuiet(c)
		return fmt.Errorf("节点 #%d 不存在", nodeID)
	}

	// 1) 用户流量落账
	if stats, ok := data["stats"].(map[string]any); ok && len(stats) > 0 {
		if len(stats) > MAX_STATS_PER_REPORT {
			// 超限整包丢弃是既有防护（避免一条消息把进程内存吃爆），但绝不能静默：
			// 用户数超过上限后该节点流量完全不落账，没有日志就无从察觉。
			// agent 每 5s 推一轮，这里按连接节流，避免刷爆日志。
			if c.ShouldLogStatsOverflow(s.now(), s.overflowLog) {
				s.logf("[node-ws] node#%d 上报流量 %d 条，超过单次上限 %d，整包已丢弃：该节点流量不会落账（需改为只上报有变化的用户）",
					nodeID, len(stats), MAX_STATS_PER_REPORT)
			}
		} else {
			clean := cleanStats(stats)
			if len(clean) > 0 {
				if _, err := s.backend.SyncNodeFromPush(ctx, node, clean); err != nil {
					return fmt.Errorf("流量落账失败: %w", err)
				}
			}
		}
	}

	// 2) 出站流量（出站管理功能的数据源）
	if outbounds, ok := data["outbounds"].(map[string]any); ok && len(outbounds) > 0 {
		if err := s.outbound.StoreOutbound(ctx, nodeID, cleanOutbounds(outbounds)); err != nil {
			return err
		}
	}

	// 3) 监控指标 + 心跳 + 4) config 版本回执（滞后计算，与 /alive 同口径）
	cfg := reportConfig(data, node, s.now())

	// 只合并运行时键：整块回写会把并发 bump 的 config_version 覆盖回旧值。
	if err := s.backend.MergeDriverConfigAndMarkOnline(ctx, nodeID, cfg); err != nil {
		return err
	}

	if reported := intOf(data["config_version"]); reported > c.NotifiedVersion() {
		c.SetNotifiedVersion(reported)
	}
	return nil
}

// cleanStats 复刻 PHP 对 stats 的归一化：email → {up, down}（只取 int）。
func cleanStats(stats map[string]any) []nodeapi.PushStats {
	out := make([]nodeapi.PushStats, 0, len(stats))
	for email, pair := range stats {
		m, ok := pair.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, nodeapi.PushStats{
			Email: email,
			Up:    intOf(m["up"]),
			Down:  intOf(m["down"]),
		})
	}
	return out
}

// cleanOutbounds 复刻 storeOutboundTraffic 的入参清洗：tag → {up, down}。
func cleanOutbounds(outbounds map[string]any) []OutboundRow {
	out := make([]OutboundRow, 0, len(outbounds))
	for tag, pair := range outbounds {
		m, ok := pair.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, OutboundRow{
			Tag:      tag,
			Upload:   intOf(m["up"]),
			Download: intOf(m["down"]),
		})
	}
	return out
}

// reportConfig 复刻 handleReport 第 3/4 步拼出的 driver_config 补丁。
//
// 键序与 PHP 一致：agent → lag_seconds → ws。
// 指标缺省为 null（不是 0）——「没上报」与「上报了 0」必须可区分。
func reportConfig(data map[string]any, node nodeapi.Node, now time.Time) nodeapi.Object {
	var cfg nodeapi.Object

	// agent 段：指标 + client 名单指纹。指纹与 metrics 是两件独立的事 ——
	// 心跳里的 metrics 块永远由 agent 带上（buildReportPayloads 的 base），但即便某版本
	// 没带，也**不能**顺带把指纹丢掉（否则裸内核对账的短路在本轮悄悄失效）。
	// clients_fp 缺键 → null，与 HTTP /alive 同一个补丁函数、同一套降级语义。
	if metrics, ok := data["metrics"].(map[string]any); ok {
		cfg.Set("agent", nodeapi.Object{
			{Key: "cpu", Value: nullableNumber(metrics["cpu"])},
			{Key: "mem", Value: nullableNumber(metrics["mem"])},
			{Key: "uptime", Value: nullableInt(metrics["uptime"])},
			{Key: "conns", Value: nullableInt(metrics["conns"])},
			{Key: "active_users", Value: nullableInt(metrics["active_users"])},
			{Key: "up_speed", Value: nullableNumber(metrics["up_speed"])},
			{Key: "down_speed", Value: nullableNumber(metrics["down_speed"])},
			// 机器网卡累计收发：agent 读不到 /proc/net/dev（非 Linux）时不上报 → null，不当 0。
			{Key: "net_total_up", Value: nullableInt(metrics["net_total_up"])},
			{Key: "net_total_down", Value: nullableInt(metrics["net_total_down"])},
			{Key: "clients_fp", Value: nodeapi.ClientsFingerprintPatch(data)},
			{Key: "at", Value: iso8601(now)},
		})
	} else if _, hasFP := data["clients_fp"]; hasFP {
		// 没有 metrics 块但带了指纹：只补指纹（不写其它指标，免得把上一轮的真实值抹成 null）。
		cfg.Set("agent", nodeapi.Object{
			{Key: "clients_fp", Value: nodeapi.ClientsFingerprintPatch(data)},
		})
	}

	// issue #94 R5：BBR 运行态与 /alive 同一个补丁函数（两条通道永远一致）。
	if bbr, ok := nodeapi.BBRStatusPatch(data); ok {
		cfg.Set("bbr", bbr)
	}

	current := node.ConfigVersion()
	reported := intOf(data["config_version"])
	if reported < current {
		if changedAt, ok := node.ConfigChangedAt(); ok {
			lag := int64(now.Sub(changedAt).Seconds())
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

	cfg.Set("ws", nodeapi.Object{
		{Key: "connected", Value: true},
		{Key: "at", Value: iso8601(now)},
	})
	return cfg
}

// handleAck 落指令回执（与 HTTP /ack 共用 applyAck 实现）。单次最多 50 条（PHP array_slice）。
func (s *Server) handleAck(ctx context.Context, nodeID int64, data map[string]any) error {
	results, _ := data["results"].([]any)
	if len(results) == 0 {
		return nil
	}
	if len(results) > 50 {
		results = results[:50]
	}
	parsed := make([]nodeapi.AckResult, 0, len(results))
	for _, item := range results {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		res := nodeapi.AckResult{
			ID:      intOf(m["id"]),
			Success: boolOf(m["success"]),
		}
		if rawOut, exists := m["output"]; exists && rawOut != nil {
			if str, ok := rawOut.(string); ok {
				res.Output = str
				res.OutputSet = true
			}
		}
		parsed = append(parsed, res)
	}
	if len(parsed) == 0 {
		return nil
	}
	_, err := s.backend.ApplyAcks(ctx, nodeID, parsed)
	return err
}

// ── 下行推送（定时轮询） ────────────────────────────────

// pollAndPush 复刻 2s 节拍：给已连接节点推 pending 指令 + config 变更通知。
// 只查「当前有连接」的节点，节点数少时查询为零成本。
func (s *Server) pollAndPush(ctx context.Context) {
	ids := s.registry.ConnectedNodeIds()
	if len(ids) == 0 {
		return
	}

	for _, nodeID := range ids {
		conn := s.registry.Get(nodeID)
		if conn == nil {
			continue
		}

		// 1) 指令推送（先原子认领，防止与 HTTP 长轮询重复下发）
		claimed, err := s.backend.ClaimPending(ctx, nodeID, MAX_COMMANDS_PER_PUSH)
		if err != nil {
			s.logf("[node-ws] 轮询推送失败：%v", err)
			continue
		}
		if len(claimed) > 0 {
			list := make([]commandPayload, 0, len(claimed))
			for _, cmd := range claimed {
				list = append(list, commandPayload{
					ID:      cmd.ID,
					Action:  cmd.Action,
					Payload: cmd.Payload,
				})
			}
			if err := s.send(conn, EventCommands, commandsPayload{Commands: list}); err == nil {
				s.logf("[node-ws] 推送 %d 指令 %d 条", nodeID, len(list))
			}
		}

		// 2) config 版本变更通知（node 收到后立即走 HTTP 全量拉取）
		node, err := s.backend.NodeByID(ctx, nodeID)
		if err != nil || node.ID == 0 {
			continue
		}
		current := node.ConfigVersion()
		if current > conn.NotifiedVersion() {
			if err := s.send(conn, EventConfigChanged, configChanged{ConfigVersion: current}); err == nil {
				s.logf("[node-ws] 通知 node#%d 配置变更 → v%d", nodeID, current)
			}
			// PHP 在 send 之后无条件推进 notifiedVersion（send 内部吞掉写错误）。
			conn.SetNotifiedVersion(current)
		}
	}
}

// commandPayload 是 commands 数组元素（键序与 PHP 一致：id/action/payload）。
type commandPayload struct {
	ID      int64           `json:"id"`
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

// commandsPayload 是 commands 事件的 data。
type commandsPayload struct {
	Commands []commandPayload `json:"commands"`
}

// configChanged 是 config.changed 事件的 data。
type configChanged struct {
	ConfigVersion int64 `json:"config_version"`
}

// heartbeat 复刻心跳：僵死连接断开；存活连接发 ping。
func (s *Server) heartbeat() {
	now := s.now()
	for nodeID, conn := range s.registry.All() {
		if now.Sub(conn.LastSeen()) > s.staleAfter {
			s.logf("[node-ws] node#%d 存活超时，断开连接", nodeID)
			s.closeQuiet(conn)
			continue
		}
		// PHP send($conn, 'ping', null) → {"event":"ping"}（无 data 键）。
		_ = s.send(conn, EventPing, nil)
	}
}

// Run 启动轮询与心跳节拍，直到 ctx 取消。WS 握手本身由 ServeHTTP 挂到 http.Server 上。
func (s *Server) Run(ctx context.Context) {
	s.logf("[node-ws] 服务启动")

	poll := time.NewTicker(s.pollInterval)
	ping := time.NewTicker(s.pingInterval)
	defer poll.Stop()
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logf("[node-ws] 服务停止")
			return
		case <-poll.C:
			s.pollAndPush(ctx)
		case <-ping.C:
			s.heartbeat()
		}
	}
}

// ── 工具 ─────────────────────────────────────────────────

// send 发一条下行消息（PHP send()：json_encode 失败/连接异常都静默）。
func (s *Server) send(conn *Conn, event string, data any) error {
	payload, err := eventBytes(event, data)
	if err != nil {
		return err
	}
	if err := conn.Send(payload); err != nil {
		// PHP：try { $conn->send($json); } catch (Throwable) {}
		return err
	}
	return nil
}

// closeWith 复刻 closeWith()：断开前先送一条 error 消息（reason 在 data.message）。
func (s *Server) closeWith(conn *Conn, reason string) {
	_ = conn.Send(errorPayload(reason))
	s.closeQuiet(conn)
}

// closeQuiet 静默断开（PHP 里 catch(Throwable) 包住 close）。
func (s *Server) closeQuiet(conn *Conn) {
	_ = conn.Close()
}

// onClose 复刻 onClose()：移出注册表 + 合并 ws.connected=false（只合并 ws 键）。
func (s *Server) onClose(conn *Conn, nodeID int64) {
	if nodeID <= 0 {
		s.closeQuiet(conn)
		return
	}
	s.registry.Remove(nodeID, conn)
	s.closeQuiet(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	patch := nodeapi.Object{
		{Key: "ws", Value: nodeapi.Object{
			{Key: "connected", Value: false},
			{Key: "at", Value: iso8601(s.now())},
		}},
	}
	if err := s.backend.MergeDriverConfig(ctx, nodeID, patch); err != nil {
		s.logf("[node-ws] node#%d 断线清理失败：%v", nodeID, err)
	}

	s.logf("[node-ws] node#%d 断开 total=%d", nodeID, s.registry.Count())
}

// headerValue 取请求头（大小写不敏感，go http.Header 已规范化）。
func headerValue(r *http.Request, name string) string {
	return r.Header.Get(name)
}

// ── PHP 值语义的小工具（与 nodeapi.phpvalue 同口径，这里自持以免反向依赖） ──

// strOf 是 PHP (string)$v 的安全形态（nil/非字符串 → ""）。
func strOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// intOf 是 PHP (int)$v（bool → 0/1，float 截断，字符串解析）。
func intOf(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			f, ferr := t.Float64()
			if ferr != nil {
				return 0
			}
			return int64(f)
		}
		return n
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(t, 64)
			if ferr != nil {
				return 0
			}
			return int64(f)
		}
		return n
	default:
		return 0
	}
}

// boolOf 是 PHP (bool)$v 的宽松形态（"false"/"" → false）。
func boolOf(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case int64:
		return t != 0
	case string:
		return t != "" && t != "0" && t != "false"
	default:
		return intOf(v) != 0
	}
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

// floatOf 是 PHP (float)$v。
func floatOf(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0
		}
		return f
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// jsonFloat 让整数值的浮点按 PHP 口径输出（33.0 → 33）。
//
// 说明：PHP 的 (float)33 经 json_encode 得到 33.0；但这里写入的是 driver_config，
// 不在 config 渲染对拍路径上，按整数值输出整数以避免 JSON 里出现无意义的 .0
// （与 nodeapi.handleAlive 的 jsonFloat 同口径）。
func jsonFloat(f float64) any {
	if f == float64(int64(f)) {
		return int64(f)
	}
	return f
}

// iso8601 复刻 Carbon::toIso8601String()（app 时区 UTC）：2026-10-24T07:44:27+00:00。
func iso8601(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05-07:00")
}
