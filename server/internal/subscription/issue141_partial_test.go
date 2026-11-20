package subscription

// issue #141：订阅偶发丢节点。
//
// 现场三条链：
//  1. service.xraySubscriptionURIs 里 repo.XrayInbounds 出错时直接 return nil，没有任何日志；
//  2. fetchBatchConcurrently 把单节点错误吞掉；
//  3. handler 把这次**残缺**结果缓存 TTL（默认 60s）—— 用户重试还是少的。
//
// 本文件钉住修复后的两条口径：
//  - 取入站失败必须留日志，且区分「请求取消/超时」与「节点查询失败」；
//  - 本次渲染只要有节点取数失败，就不许写缓存，下一次请求必须重新取。
//
// 变异测试靶子（去掉即必红）：
//  - service.go 里 xraySubscriptionURIs 的 `return nil, err`（改回 `return nil, nil`）→ 日志/残缺用例红；
//  - handler.go load 里的 `if err == nil && cacheable`（去掉 cacheable）→ 本文件两条「不缓存」用例红；
//  - 反过来把缓存整个关掉（`if err == nil` 里永不 store）→ CompleteRenderIsCached 用例红。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ───────────────────────── 测试替身 ─────────────────────────

// issue141Repo 是「3 台 xray 节点」的内存仓储，可让指定节点取入站失败。
type issue141Repo struct {
	user     *User
	plan     *Plan
	configs  map[string]string
	nodes    []Node
	inbounds map[int64][]XrayInbound

	// failNodeID 非 0 时该节点的 XrayInbounds 固定返回 failErr（模拟单台节点查询失败）。
	failNodeID int64
	failErr    error

	// inboundCalls 是 XrayInbounds 的总调用次数：
	// 用它证明「第二次请求真的重新取了」—— 吃到缓存就不会再进来。
	inboundCalls atomic.Int64
}

// issue141Now 是用例统一时刻（用户无到期时间，故不受时钟影响）。
func issue141Now() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }

// issue141Inbound 造一条「vless + reality」入站（与 std02 用例同模板，确定能拼出链接）。
func issue141Inbound(port int64) []XrayInbound {
	return []XrayInbound{{
		ID: 1, Tag: "inbound-vless", Remark: "VLESS Reality", Protocol: "vless",
		Port: port, Listen: "0.0.0.0", Enabled: true, Sort: 1,
		Settings:       `{"clients":[{"id":"11111111-2222-3333-4444-555555555555"}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"reality","realitySettings":{"serverNames":["www.microsoft.com"],"shortIds":["ab"],"publicKey":"PUBKEY"}}`,
	}}
}

// newIssue141Repo 造三台 xray 节点（jp / us / sg），默认全部取数成功。
func newIssue141Repo() *issue141Repo {
	plan := int64(1)
	return &issue141Repo{
		user: &User{
			ID: 7, Token: "tok", UUID: "11111111-2222-3333-4444-555555555555",
			Protocol: "vless", PlanID: &plan, TrafficLimit: 100 << 30, TrafficUsed: 1 << 30,
			Enabled: true,
		},
		plan:    &Plan{ID: 1, Type: "period", Months: 1, IncludeLocal: true},
		configs: map[string]string{},
		nodes: []Node{
			{ID: 101, Name: "日本 01", Host: "jp.example.com", Port: 443, Scheme: "https",
				Enabled: true, Status: "online", DriverType: "xray"},
			{ID: 102, Name: "美国 02", Host: "us.example.com", Port: 8443, Scheme: "https",
				Enabled: true, Status: "online", DriverType: "xray"},
			{ID: 103, Name: "新加坡 03", Host: "sg.example.com", Port: 2096, Scheme: "https",
				Enabled: true, Status: "online", DriverType: "xray"},
		},
		inbounds: map[int64][]XrayInbound{
			101: issue141Inbound(443),
			102: issue141Inbound(8443),
			103: issue141Inbound(2096),
		},
	}
}

func (r *issue141Repo) SiteConfigs(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r.configs {
		out[k] = v
	}
	return out, nil
}

func (r *issue141Repo) UserByToken(_ context.Context, token string) (*User, error) {
	if r.user == nil || token != r.user.Token {
		return nil, nil
	}
	cp := *r.user
	return &cp, nil
}

func (r *issue141Repo) PlanByID(_ context.Context, id int64) (*Plan, error) {
	if r.plan == nil || id != r.plan.ID {
		return nil, nil
	}
	cp := *r.plan
	return &cp, nil
}

func (r *issue141Repo) SubscribableNodes(context.Context, string, uint64) ([]Node, error) {
	return r.nodes, nil
}

func (r *issue141Repo) NodeInboundIDs(context.Context, int64, string) ([]int64, error) {
	return nil, nil
}

// XrayInbounds 复刻真实驱动的两条失败路径：
//   - 请求已取消/超时 → 返回 ctx.Err()（sql 驱动就是这么的）；
//   - 单台节点查询失败 → 返回 failErr。
func (r *issue141Repo) XrayInbounds(ctx context.Context, nodeID int64) ([]XrayInbound, error) {
	r.inboundCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.failNodeID != 0 && nodeID == r.failNodeID {
		return nil, r.failErr
	}
	return r.inbounds[nodeID], nil
}

func (r *issue141Repo) ThirdPartySubs(context.Context, []int64) ([]ThirdPartySub, error) {
	return nil, nil
}

func (r *issue141Repo) HasEnabledOnlineXrayNodeWithoutHost(context.Context) (bool, error) {
	return false, nil
}

func (r *issue141Repo) PanelLinks(context.Context, Node, string) ([]string, error) {
	return nil, nil
}

func (r *issue141Repo) PanelInbounds(context.Context, Node) ([]PanelInbound, error) {
	return nil, nil
}

// ───────────────────────── 辅助 ─────────────────────────

// logRecorder 收集 Service 的 logf 输出（并发安全：并发取链会同时写）。
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

// captureSubLogs 把 svc 的日志接到记录器上。
func captureSubLogs(svc *Service) *logRecorder {
	rec := &logRecorder{}
	svc.Logf = func(format string, args ...any) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.lines = append(rec.lines, fmt.Sprintf(format, args...))
	}
	return rec
}

// has 判断是否有一条日志包含 substr。
func (r *logRecorder) has(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// dump 返回全部日志（断言失败时打印，便于定位）。
func (r *logRecorder) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// issue141Hosts 统计 base64 订阅正文里每个 host 出现了几条链接。
func issue141Hosts(t *testing.T, body string) map[string]int {
	t.Helper()
	hosts := map[string]int{}
	if strings.TrimSpace(body) == "" {
		return hosts
	}
	for _, line := range decodeBase64Body(t, body) {
		for _, host := range []string{"jp.example.com", "us.example.com", "sg.example.com"} {
			if strings.Contains(line, "@"+host+":") {
				hosts[host]++
			}
		}
	}
	return hosts
}

// newIssue141Handler 造一个「缓存开着、限流关掉、走真实生成器」的 handler。
func newIssue141Handler(repo *issue141Repo) *Handler {
	h := newTestHandler(repo, 0, 300, issue141Now)
	h.generator = h.Service.generate // 用真实生成器：要验的正是「服务报残缺 → handler 不写缓存」
	return h
}

// ───────────────────────── 1. 单台节点取入站失败 ─────────────────────────

// TestIssue141PartialRenderIsNotCached：第 2 台节点取入站报错时，
// ① 正文只少那一台（其余照旧）② 日志留痕 ③ 残缺结果不写缓存，下一次请求重新取。
//
// 变异测试：去掉 handler.load 里的 cacheable 判断（只按 err == nil 写缓存），本用例必红。
func TestIssue141PartialRenderIsNotCached(t *testing.T) {
	repo := newIssue141Repo()
	repo.failNodeID = 102
	repo.failErr = errors.New("sql: connection reset by peer")

	h := newIssue141Handler(repo)
	logs := captureSubLogs(h.Service)

	// 第一次请求：整体成功（HTTP 200 + 业务码 0），只是少一台节点。
	w := serveShow(t, h, "tok", "")
	if w.Code != 200 || w.Header().Get("X-CH-Code") != "0" {
		t.Fatalf("单节点失败不该毁掉整条订阅：code=%d X-CH-Code=%q body=%q",
			w.Code, w.Header().Get("X-CH-Code"), w.Body.String())
	}
	hosts := issue141Hosts(t, w.Body.String())
	if hosts["jp.example.com"] != 1 || hosts["sg.example.com"] != 1 {
		t.Fatalf("未失败的节点必须照旧产出链接，实际 %v（正文=%q）", hosts, w.Body.String())
	}
	if hosts["us.example.com"] != 0 {
		t.Fatalf("取入站失败的节点不该产出链接，实际 %v", hosts)
	}

	// 日志必须留痕：带节点 id/名称与原始错误。
	if !logs.has("[sub] 节点 102/美国 02 取入站失败") {
		t.Fatalf("节点取入站失败必须留日志（期望含「[sub] 节点 102/美国 02 取入站失败」），实际日志：\n%s", logs.dump())
	}
	if !logs.has("sql: connection reset by peer") {
		t.Fatalf("日志必须带原始错误，实际日志：\n%s", logs.dump())
	}
	// 普通节点故障不能被打上「请求取消」的标记（否则真正的原因被掩盖）。
	if logs.has("[sub] 节点 102/美国 02 取入站失败（请求已取消或超时）") {
		t.Fatalf("节点查询失败不该被标成请求取消，实际日志：\n%s", logs.dump())
	}

	firstCalls := repo.inboundCalls.Load()
	if firstCalls != 3 {
		t.Fatalf("首轮应按节点各取一次入站（期望 3 次），实际 %d 次", firstCalls)
	}

	// 第二次请求：残缺结果没进缓存 —— 必须重新取，且节点恢复后正文立刻完整。
	repo.failNodeID = 0
	w = serveShow(t, h, "tok", "")
	hosts = issue141Hosts(t, w.Body.String())
	for _, host := range []string{"jp.example.com", "us.example.com", "sg.example.com"} {
		if hosts[host] != 1 {
			t.Fatalf("第二次请求必须不吃缓存、重新取数（节点 %s 应恢复），实际 %v（正文=%q）",
				host, hosts, w.Body.String())
		}
	}
	if got := repo.inboundCalls.Load(); got != firstCalls+3 {
		t.Fatalf("残缺结果不得写缓存：第二次请求应重新取 3 次入站，实际总数 %d（首轮 %d）", got, firstCalls)
	}
}

// ───────────────────────── 2. 请求 ctx 取消 ─────────────────────────

// TestIssue141ContextCanceledNotCached：请求被取消（用户/客户端断开）时，
// ① 一条节点链接都取不到 ② 日志明确标出「请求已取消或超时」
// ③ 这份空正文不写缓存 —— 下一次请求必须重新取，用户重试就能拿到完整订阅。
//
// 变异测试：去掉 handler.load 里的 cacheable 判断，本用例必红（第二次会命中空缓存）。
func TestIssue141ContextCanceledNotCached(t *testing.T) {
	repo := newIssue141Repo()

	h := newIssue141Handler(repo)
	logs := captureSubLogs(h.Service)

	user, err := repo.UserByToken(context.Background(), "tok")
	if err != nil || user == nil {
		t.Fatalf("准备用户失败: user=%v err=%v", user, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 请求已取消：所有取数都返回 ctx.Err()

	body, err := h.renderCached(ctx, "tok", user, FormatBase64)
	if err != nil {
		t.Fatalf("ctx 取消不该把错误抛给调用方（响应早已断开），实际 %v", err)
	}
	if hosts := issue141Hosts(t, body); len(hosts) != 0 {
		t.Fatalf("取消后不该有任何节点链接，实际 %v", hosts)
	}
	if !logs.has("取入站失败（请求已取消或超时）") {
		t.Fatalf("ctx 取消必须与节点故障区分开记日志，实际日志：\n%s", logs.dump())
	}

	canceledCalls := repo.inboundCalls.Load()
	if canceledCalls != 3 {
		t.Fatalf("取消后仍应逐节点尝试（期望 3 次），实际 %d 次", canceledCalls)
	}

	// 第二次请求（正常 ctx）：空正文没进缓存 —— 必须重新取，三台节点全部回来。
	w := serveShow(t, h, "tok", "")
	hosts := issue141Hosts(t, w.Body.String())
	for _, host := range []string{"jp.example.com", "us.example.com", "sg.example.com"} {
		if hosts[host] != 1 {
			t.Fatalf("取消产生的空正文不得写缓存（节点 %s 应恢复），实际 %v（正文=%q）",
				host, hosts, w.Body.String())
		}
	}
	if got := repo.inboundCalls.Load(); got != canceledCalls+3 {
		t.Fatalf("取消产生的空正文不得写缓存：第二次应重新取 3 次入站，实际总数 %d（首轮 %d）", got, canceledCalls)
	}
}

// ───────────────────────── 3. 反面对照：完整渲染照旧进缓存 ─────────────────────────

// TestIssue141CompleteRenderIsCached：全部节点都成功时缓存必须照常工作，
// 防止「为了不缓存残缺结果」把缓存整体关掉（那会把面板打爆）。
func TestIssue141CompleteRenderIsCached(t *testing.T) {
	repo := newIssue141Repo()
	h := newIssue141Handler(repo)

	for i := 1; i <= 3; i++ {
		w := serveShow(t, h, "tok", "")
		if w.Header().Get("X-CH-Code") != "0" {
			t.Fatalf("第 %d 次请求应成功，实际 X-CH-Code=%q 正文=%q", i, w.Header().Get("X-CH-Code"), w.Body.String())
		}
		hosts := issue141Hosts(t, w.Body.String())
		for _, host := range []string{"jp.example.com", "us.example.com", "sg.example.com"} {
			if hosts[host] != 1 {
				t.Fatalf("第 %d 次请求节点 %s 缺失：%v", i, host, hosts)
			}
		}
	}
	if got := repo.inboundCalls.Load(); got != 3 {
		t.Fatalf("完整结果应命中缓存：三次请求只该取 3 次入站，实际 %d 次", got)
	}
}
