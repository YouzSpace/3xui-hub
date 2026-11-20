package subscription

// std02_service_test.go 用「全量内存仓储」把订阅生成链路整条跑通（规范化-02 场景 1-10）。
//
// 为什么这一层用 fake repo 而不是真库：这里要验的是**生成逻辑**（三格式渲染、名称处理、
// 倍率、旗帜、信息节点、xray/3x-ui 分支、空列表口径），与 SQL 无关；
// 按用户绑定过滤这类「就是 SQL」的口径放在 std02_store_test.go 里打真库。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// std02Repo 是「一台 3x-ui 节点 + 一台 xray 节点」的完整内存仓储。
type std02Repo struct {
	configs      map[string]string
	users        map[string]*User
	plans        map[int64]*Plan
	nodes        []Node
	inboundIDs   map[int64][]int64
	xrayInbounds map[int64][]XrayInbound
	panelLinks   map[int64][]string
	thirdParty   []ThirdPartySub
	blankHost    bool
	failNodes    error
}

func newStd02Repo() *std02Repo {
	plan := int64(1)
	expire := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	nextReset := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	mult := 1.25
	return &std02Repo{
		configs: map[string]string{"sub_clash_enabled": "1", "sub_singbox_enabled": "1"},
		users: map[string]*User{
			"tok-ok": {
				ID: 7, Token: "tok-ok", UUID: "11111111-2222-3333-4444-555555555555",
				Protocol: "vless", PlanID: &plan, TrafficLimit: 100 << 30, TrafficUsed: 1 << 30,
				MonthlyTrafficUsed: 5 << 30, MonthlyTrafficLim: 100 << 30,
				NextTrafficResetAt: &nextReset, ExpiredAt: &expire, Enabled: true,
			},
		},
		plans: map[int64]*Plan{1: {ID: 1, Type: "period", Months: 1, IncludeLocal: true}},
		nodes: []Node{
			{
				ID: 10, Name: "日本 01", Host: "jp.example.com", Port: 443, Scheme: "https",
				Username: "admin", Password: "p", APIKey: "k", Enabled: true, Status: "online",
				TrafficMultiplier: &mult,
			},
			{
				ID: 20, Name: "美国 01", Host: "us.example.com", Port: 8443, Scheme: "https",
				Username: "admin", Password: "p", Enabled: true, Status: "online",
				DriverType: "xray",
			},
		},
		inboundIDs: map[int64][]int64{10: {1}, 20: nil},
		xrayInbounds: map[int64][]XrayInbound{
			20: {
				{
					ID: 1, NodeID: 20, Tag: "inbound-vless", Remark: "VLESS Reality", Protocol: "vless",
					Port: 8443, Listen: "0.0.0.0", Enabled: true, Sort: 1,
					Settings:       `{"clients":[{"id":"11111111-2222-3333-4444-555555555555"}],"decryption":"none"}`,
					StreamSettings: `{"network":"tcp","security":"reality","realitySettings":{"serverNames":["www.microsoft.com"],"shortIds":["ab"],"publicKey":"PUBKEY"}}`,
				},
			},
		},
		panelLinks: map[int64][]string{
			10: {"vless://11111111-2222-3333-4444-555555555555@jp.example.com:443?type=tcp&security=tls#日本 01"},
		},
	}
}

func (r *std02Repo) SiteConfigs(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r.configs {
		out[k] = v
	}
	return out, nil
}

func (r *std02Repo) UserByToken(_ context.Context, token string) (*User, error) {
	u, ok := r.users[token]
	if !ok {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}

func (r *std02Repo) PlanByID(_ context.Context, id int64) (*Plan, error) {
	p, ok := r.plans[id]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (r *std02Repo) SubscribableNodes(_ context.Context, protocol string, _ uint64) ([]Node, error) {
	if r.failNodes != nil {
		return nil, r.failNodes
	}
	out := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		if !n.Enabled || n.Status != "online" {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func (r *std02Repo) NodeInboundIDs(_ context.Context, nodeID int64, _ string) ([]int64, error) {
	return r.inboundIDs[nodeID], nil
}

func (r *std02Repo) XrayInbounds(_ context.Context, nodeID int64) ([]XrayInbound, error) {
	return r.xrayInbounds[nodeID], nil
}

func (r *std02Repo) ThirdPartySubs(_ context.Context, ids []int64) ([]ThirdPartySub, error) {
	var out []ThirdPartySub
	for _, s := range r.thirdParty {
		if !s.Enabled {
			continue
		}
		for _, id := range ids {
			if id == s.ID {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func (r *std02Repo) HasEnabledOnlineXrayNodeWithoutHost(context.Context) (bool, error) {
	return r.blankHost, nil
}

func (r *std02Repo) PanelLinks(_ context.Context, node Node, _ string) ([]string, error) {
	return r.panelLinks[node.ID], nil
}

func (r *std02Repo) PanelInbounds(_ context.Context, node Node) ([]PanelInbound, error) {
	return nil, nil
}

// std02Service 造一个时钟固定的服务。
func std02Service(repo Repository, now time.Time) *Service {
	svc := NewService(repo)
	svc.Now = func() time.Time { return now }
	svc.Logf = func(string, ...any) {}
	return svc
}

// std02Now 是用例统一时刻（早于用户到期时间）。
func std02Now() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }

// decodeName 把链接 #fragment 里的名称解成可读文本（订阅正文里名称是 url 编码的）。
func decodeName(line string) string {
	h := strings.LastIndex(line, "#")
	if h < 0 {
		return line
	}
	return line[:h+1] + urldecode(line[h+1:])
}

// decodedLines 解开 base64 并把每行名称还原成明文。
func decodedLines(t *testing.T, body string) []string {
	t.Helper()
	lines := decodeBase64Body(t, body)
	for i, line := range lines {
		lines[i] = decodeName(line)
	}
	return lines
}

// decodeBase64Body 把 base64 订阅解开成行列表。
func decodeBase64Body(t *testing.T, body string) []string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
	if err != nil {
		t.Fatalf("base64 订阅解不开: %v", err)
	}
	return strings.Split(string(raw), "\n")
}

// ─────────── 场景 1：正常用户拿到全部节点（含 xray 与 3x-ui 两条路径） ───────────

// TestStd02SubBase64IncludesBothNodeKinds：3x-ui 走面板链接、xray 本地拼链接，两条都在。
func TestStd02SubBase64IncludesBothNodeKinds(t *testing.T) {
	repo := newStd02Repo()
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("生成 base64 订阅失败: %v", err)
	}
	lines := decodeBase64Body(t, body)

	var hasJp, hasUs bool
	for _, line := range lines {
		if strings.Contains(line, "jp.example.com") {
			hasJp = true
		}
		if strings.Contains(line, "us.example.com") {
			hasUs = true
		}
	}
	if !hasJp {
		t.Error("3x-ui 节点的面板链接没进订阅")
	}
	if !hasUs {
		t.Error("xray 节点的本地拼链没进订阅（IsXray 分支）")
	}
	// xray 链接必须是完整的 reality 参数，不是残缺 host:port。
	for _, line := range lines {
		if strings.Contains(line, "us.example.com") {
			if !strings.Contains(line, "security=reality") || !strings.Contains(line, "pbk=") {
				t.Errorf("xray reality 链接字段不全: %s", line)
			}
		}
	}
}

// ─────────── 场景 3/4：空列表与无套餐 ───────────

// TestStd02SubNoNodesReturnsEmptyNotError：节点全离线 → 返回空正文 + 无错误（不 panic、不 500）。
func TestStd02SubNoNodesReturnsEmptyNotError(t *testing.T) {
	repo := newStd02Repo()
	repo.nodes = nil
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("没有可用节点时不该报错（用户看到空订阅即可）: %v", err)
	}
	if strings.TrimSpace(body) != "" {
		t.Fatalf("没有节点时应返回空正文，实际 %q", body)
	}
	if lines := decodeBase64Body(t, body); len(lines) > 1 || (len(lines) == 1 && lines[0] != "") {
		t.Fatalf("空订阅解出来的行数应为 0/1，实际 %d", len(lines))
	}
}

// TestStd02SubXrayNodeWithoutHostGivesActionable404：xray 节点没填地址 → 404 + 可操作提示。
func TestStd02SubXrayNodeWithoutHostGivesActionable404(t *testing.T) {
	repo := newStd02Repo()
	repo.nodes = nil // 一条链接都产不出
	repo.blankHost = true
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	_, err := svc.Generate(context.Background(), user, FormatBase64)
	if err == nil {
		t.Fatal("xray 节点缺地址时必须给出可诊断的错误，而不是静默空订阅")
	}
	var bizErr *Error
	if !errors.As(err, &bizErr) {
		t.Fatalf("错误类型 = %T，期望 *Error（handler 靠 Code 决定文案）", err)
	}
	if bizErr.Code != 404 || !strings.Contains(bizErr.Message, "连接地址") {
		t.Errorf("错误 = %d/%q，期望 404 且提示补充连接地址", bizErr.Code, bizErr.Message)
	}
}

// TestStd02SubNoPlanUser：无套餐用户 → *Error 403「无套餐」，handler 返回 200 + [403] 文案。
func TestStd02SubNoPlanUser(t *testing.T) {
	repo := newStd02Repo()
	user, _ := repo.UserByToken(context.Background(), "tok-ok")
	user.PlanID = nil
	repo.users["tok-ok"] = user
	svc := std02Service(repo, std02Now())

	_, err := svc.Generate(context.Background(), user, FormatBase64)
	var bizErr *Error
	if !errors.As(err, &bizErr) || bizErr.Code != 403 || !strings.Contains(bizErr.Message, "无套餐") {
		t.Fatalf("无套餐用户应返回 403「无套餐」，实际 %v", err)
	}

	// handler 层：HTTP 200 + X-CH-Code 403 + 正文提示（绝不能 500）。
	h := newTestHandler(repo, 0, 0, std02Now)
	h.generator = h.Service.generate // 用真实生成器（newTestHandler 默认塞的是桩）
	w := serveShow(t, h, "tok-ok", "")
	if w.Code != 200 {
		t.Errorf("无套餐必须返回 HTTP 200（业务错误不走 5xx），实际 %d", w.Code)
	}
	if got := w.Header().Get("X-CH-Code"); got != "403" {
		t.Errorf("X-CH-Code = %q，期望 403", got)
	}
	if !strings.Contains(w.Body.String(), "无套餐") {
		t.Errorf("正文应明确提示无套餐，实际 %q", w.Body.String())
	}
}

// TestStd02SubUsabilityGates：禁用 / 过期 / 超量三种不可用状态各自的口径。
func TestStd02SubUsabilityGates(t *testing.T) {
	now := std02Now()
	past := now.Add(-time.Hour)
	cases := []struct {
		name   string
		mutate func(*User)
		want   string
	}{
		{"禁用", func(u *User) { u.Enabled = false }, "账号已禁用"},
		{"过期", func(u *User) { u.ExpiredAt = &past }, "账号已过期"},
		{"超量", func(u *User) { u.TrafficUsed = u.TrafficLimit }, "流量已耗尽"},
	}
	for _, c := range cases {
		repo := newStd02Repo()
		user, _ := repo.UserByToken(context.Background(), "tok-ok")
		c.mutate(user)
		svc := std02Service(repo, now)
		_, err := svc.Generate(context.Background(), user, FormatBase64)
		var bizErr *Error
		if !errors.As(err, &bizErr) || !strings.Contains(bizErr.Message, c.want) {
			t.Errorf("%s 用户应返回 %q，实际 %v", c.name, c.want, err)
		}
	}
	// EnsureUsable 单独暴露给 handler 用的口径一致。
	repo := newStd02Repo()
	user, _ := repo.UserByToken(context.Background(), "tok-ok")
	if err := std02Service(repo, now).EnsureUsable(user); err != nil {
		t.Errorf("正常用户的 EnsureUsable 应通过，实际 %v", err)
	}
}

// ─────────── 场景 5：三种输出格式 ───────────

// TestStd02SubClashFormat：Clash 正文结构（proxies / proxy-groups / rules，信息节点不进代理组）。
func TestStd02SubClashFormat(t *testing.T) {
	repo := newStd02Repo()
	repo.configs["sub_show_expire"] = "1"
	repo.configs["sub_show_traffic"] = "1"
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatClash)
	if err != nil {
		t.Fatalf("生成 clash 订阅失败: %v", err)
	}
	for _, want := range []string{"mode: rule", "proxies:", "proxy-groups:", "rules:", "- MATCH,Proxy"} {
		if !strings.Contains(body, want) {
			t.Errorf("clash 正文缺 %q", want)
		}
	}
	// 真实节点各自一条 proxy，名字带引号。
	if !strings.Contains(body, `- name: "日本 01"`) && !strings.Contains(body, `- name: "`) {
		t.Error("clash 没有输出节点条目")
	}
	// 信息节点（到期/当月）必须存在但不进 proxy-groups。
	if !strings.Contains(body, "到期: ") {
		t.Error("开了 sub_show_expire 却没输出「到期」信息节点")
	}
	groupsIdx := strings.Index(body, "proxy-groups:")
	if groupsIdx < 0 {
		t.Fatal("没有 proxy-groups")
	}
	groupPart := body[groupsIdx:]
	if strings.Contains(groupPart, "到期: ") {
		t.Error("信息节点混进了代理组（用户会选到它，直接断网）")
	}
}

// TestStd02SubSingboxFormat：Sing-box 正文本体是合法 JSON，direct 第一、信息出站有 tag。
func TestStd02SubSingboxFormat(t *testing.T) {
	repo := newStd02Repo()
	repo.configs["sub_show_traffic"] = "1"
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatSingbox)
	if err != nil {
		t.Fatalf("生成 singbox 订阅失败: %v", err)
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("singbox 正文不是合法 JSON: %v\n%s", err, body)
	}
	if len(cfg.Outbounds) < 3 {
		t.Fatalf("出站数量 = %d，至少 direct + 信息 + 两个节点", len(cfg.Outbounds))
	}
	if cfg.Outbounds[0]["type"] != "direct" || cfg.Outbounds[0]["tag"] != "direct" {
		t.Errorf("第一个出站必须是 direct，实际 %v", cfg.Outbounds[0])
	}
	var hasInfo, hasNode bool
	for _, o := range cfg.Outbounds[1:] {
		tag := fmt.Sprintf("%v", o["tag"])
		if strings.HasPrefix(tag, "当月: ") {
			hasInfo = true
		}
		if o["type"] == "vless" || o["type"] == "trojan" || o["type"] == "shadowsocks" {
			if !strings.HasPrefix(tag, "当月: ") {
				hasNode = true
			}
		}
	}
	if !hasInfo {
		t.Error("singbox 缺「当月」信息出站")
	}
	if !hasNode {
		t.Error("singbox 没有任何真实节点出站")
	}
}

// TestStd02SubSingboxHintsWhenUnsupported：有链接但一条都解析不了 → 注入提示出站而不是空配置。
func TestStd02SubSingboxHintsWhenUnsupported(t *testing.T) {
	repo := newStd02Repo()
	repo.panelLinks[10] = []string{"ssr://unsupported-format"}
	repo.nodes = repo.nodes[:1] // 只留 3x-ui 节点，链接是 singbox 不支持的协议
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatSingbox)
	if err != nil {
		t.Fatalf("生成 singbox 订阅失败: %v", err)
	}
	var cfg struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("singbox 正文不是合法 JSON: %v", err)
	}
	hinted := false
	for _, o := range cfg.Outbounds {
		if tag, _ := o["tag"].(string); strings.Contains(tag, "不支持Sing-box") {
			hinted = true
		}
	}
	if !hinted {
		t.Errorf("全部链接不支持时必须给提示出站，实际正文:\n%s", body)
	}
}

// ─────────── 场景 6/7：旗帜与正则改名 ───────────

// TestStd02SubFlagAndRename：sub_show_flag 加国旗；sub_rename_enabled 按规则改名。
func TestStd02SubFlagAndRename(t *testing.T) {
	repo := newStd02Repo()
	repo.configs["sub_show_flag"] = "1"
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	joined := strings.Join(decodedLines(t, body), "\n")
	// 「日本 01」应变成「🇯🇵 JP01」；「美国 01」→「🇺🇸 US01」。
	if !strings.Contains(joined, "JP01") && !strings.Contains(joined, "US01") {
		t.Errorf("开了国旗开关但名称没有地区代码:\n%s", joined)
	}
	if strings.Contains(joined, "%EF%BF%BD") || strings.Contains(joined, "\uFFFD") {
		t.Error("名称出现乱码（国旗编码坏了）")
	}
	// 同地区第二个节点应编号 +1（两位数字）。
	repo2 := newStd02Repo()
	repo2.configs["sub_show_flag"] = "1"
	second := repo2.nodes[0]
	second.ID = 11
	second.Name = "日本 02"
	second.Host = "jp2.example.com"
	repo2.nodes = append(repo2.nodes, second)
	repo2.inboundIDs[11] = []int64{2}
	repo2.panelLinks[11] = []string{"vless://11111111-2222-3333-4444-555555555555@jp2.example.com:443?type=tcp#日本 02"}
	svc2 := std02Service(repo2, std02Now())
	body2, err := svc2.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	joined2 := strings.Join(decodedLines(t, body2), "\n")
	if !strings.Contains(joined2, "JP01") || !strings.Contains(joined2, "JP02") {
		t.Errorf("同地区两个节点应编号 JP01/JP02:\n%s", joined2)
	}

	// 正则改名：开关关闭时保持原名。
	repo3 := newStd02Repo()
	svc3 := std02Service(repo3, std02Now())
	body3, _ := svc3.Generate(context.Background(), user, FormatBase64)
	if !strings.Contains(strings.Join(decodedLines(t, body3), "\n"), "日本 01") {
		t.Error("改名开关默认关闭时应保持原名")
	}
	// 打开后按规则替换。
	repo4 := newStd02Repo()
	repo4.configs["sub_rename_enabled"] = "1"
	repo4.configs["sub_rename_regex"] = "/日本/u"
	repo4.configs["sub_rename_replacement"] = "JP-"
	svc4 := std02Service(repo4, std02Now())
	body4, err := svc4.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("改名后生成失败: %v", err)
	}
	joined4 := strings.Join(decodedLines(t, body4), "\n")
	if !strings.Contains(joined4, "JP- 01") {
		t.Errorf("正则改名没生效:\n%s", joined4)
	}
}

// ─────────── 场景 8：xray vs 3x-ui 两条链接生成路径 ───────────

// TestStd02SubXrayMissingUUID：xray 节点但用户没有 uuid → 该节点不产出链接（不是拼坏链接）。
func TestStd02SubXrayMissingUUID(t *testing.T) {
	repo := newStd02Repo()
	repo.nodes = repo.nodes[1:] // 只留 xray 节点
	user, _ := repo.UserByToken(context.Background(), "tok-ok")
	user.UUID = ""
	svc := std02Service(repo, std02Now())

	body, err := svc.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if strings.TrimSpace(body) != "" {
		t.Fatalf("没有 uuid 时 xray 节点不应产出链接，实际 %q", body)
	}
}

// ─────────── 场景 9：静态 IP 用户在订阅里无感 ───────────

// TestStd02SubStaticIPUserIsTransparent：链式在服务端做，订阅正文与普通用户完全一致。
func TestStd02SubStaticIPUserIsTransparent(t *testing.T) {
	repo := newStd02Repo()
	// 静态 IP 套餐：只剩一个「不含本地」的开关字段，订阅侧不该有任何额外标记。
	repo.plans[1] = &Plan{ID: 1, Type: "period", Months: 1, IncludeLocal: true}
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatClash)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	for _, banned := range []string{"chain", "dialer-proxy", "static_ip", "static-ip"} {
		if strings.Contains(body, banned) {
			t.Errorf("静态 IP 的链式应在服务端完成，订阅正文不该出现 %q", banned)
		}
	}
}

// ─────────── 场景 10：流量信息节点 ───────────

// TestStd02SubInfoNodesText：到期/当月/重置三项口径（重置只在周期且月数>1 时出现）。
func TestStd02SubInfoNodesText(t *testing.T) {
	repo := newStd02Repo()
	repo.configs["sub_show_expire"] = "1"
	repo.configs["sub_show_traffic"] = "1"
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, _ := svc.Generate(context.Background(), user, FormatBase64)
	joined := strings.Join(decodedLines(t, body), "\n")
	if !strings.Contains(joined, "到期: 2027-01-01") {
		t.Errorf("到期日期口径不对（应为 Y-m-d，UTC）:\n%s", joined)
	}
	if !strings.Contains(joined, "当月: 5 GB / 100 GB") {
		t.Errorf("当月流量口径不对:\n%s", joined)
	}
	// 月数=1 的周期套餐不显示重置天数。
	if strings.Contains(joined, "重置: ") {
		t.Errorf("月数=1 的套餐不该显示重置天数:\n%s", joined)
	}

	// 月数>1 → 显示重置天数。
	repo.plans[1] = &Plan{ID: 1, Type: "period", Months: 3, IncludeLocal: true}
	body2, _ := svc.Generate(context.Background(), user, FormatBase64)
	joined2 := strings.Join(decodedLines(t, body2), "\n")
	if !strings.Contains(joined2, "重置: ") {
		t.Errorf("月数>1 的周期套餐应显示重置天数:\n%s", joined2)
	}

	// 永久套餐（无到期时间）→ 显示「永久」。
	noExpire := *user
	noExpire.ExpiredAt = nil
	body3, _ := svc.Generate(context.Background(), &noExpire, FormatBase64)
	if !strings.Contains(strings.Join(decodedLines(t, body3), "\n"), "到期: 永久") {
		t.Error("没有到期时间时应显示「永久」")
	}
}

// ─────────── 附加：倍率后缀、第三方链接、包「不含本地」 ───────────

// TestStd02SubMultiplierSuffix：开关打开时在名称末尾追加 -1.25x，关闭时不加。
func TestStd02SubMultiplierSuffix(t *testing.T) {
	repo := newStd02Repo()
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, _ := svc.Generate(context.Background(), user, FormatBase64)
	if strings.Contains(strings.Join(decodedLines(t, body), "\n"), "1.25x") {
		t.Error("倍率开关默认关闭时不该出现后缀")
	}

	repo.configs["show_node_multiplier"] = "1"
	body2, _ := svc.Generate(context.Background(), user, FormatBase64)
	joined := strings.Join(decodedLines(t, body2), "\n")
	if !strings.Contains(joined, "1.25x") {
		t.Errorf("开了倍率显示后应在名称里带 -1.25x:\n%s", joined)
	}
	if got := multiplierSuffix(2); got != "-2x" {
		t.Errorf("multiplierSuffix(2) = %q，期望 -2x", got)
	}
	if got := multiplierSuffix(0.85); got != "-0.85x" {
		t.Errorf("multiplierSuffix(0.85) = %q（两位小数不能四舍五入成 0.9x）", got)
	}
}

// TestStd02SubThirdPartyLinks：第三方缓存链接追加在本地链接之后；读不到不影响本地。
func TestStd02SubThirdPartyLinks(t *testing.T) {
	repo := newStd02Repo()
	repo.configs["third_party_enabled"] = "1"
	repo.plans[1] = &Plan{ID: 1, Type: "period", Months: 1, IncludeLocal: true, ThirdPartySubIDs: []int64{5}}
	repo.thirdParty = []ThirdPartySub{
		{ID: 5, Enabled: true, CachedLinks: "vless://third@third.example.com:443#第三方节点"},
	}
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !strings.Contains(strings.Join(decodedLines(t, body), "\n"), "third.example.com") {
		t.Error("套餐绑定的第三方缓存链接没进订阅")
	}
}

// TestStd02SubPlanWithoutLocal：套餐「不含本地」时只出第三方链接。
func TestStd02SubPlanWithoutLocal(t *testing.T) {
	repo := newStd02Repo()
	repo.configs["third_party_enabled"] = "1"
	repo.plans[1] = &Plan{ID: 1, Type: "period", Months: 1, IncludeLocal: false, ThirdPartySubIDs: []int64{5}}
	repo.thirdParty = []ThirdPartySub{{ID: 5, Enabled: true, CachedLinks: "vless://third@third.example.com:443#third"}}
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	body, err := svc.Generate(context.Background(), user, FormatBase64)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	joined := strings.Join(decodedLines(t, body), "\n")
	if strings.Contains(joined, "jp.example.com") || strings.Contains(joined, "us.example.com") {
		t.Error("套餐不含本地节点时不该出现本地节点链接")
	}
	if !strings.Contains(joined, "third.example.com") {
		t.Error("套餐不含本地时第三方链接必须保留")
	}
}

// TestStd02SubRepoErrorPropagates：仓储报错要冒泡（不能吞掉当空订阅，否则用户以为没节点）。
func TestStd02SubRepoErrorPropagates(t *testing.T) {
	repo := newStd02Repo()
	repo.failNodes = errors.New("db is down")
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")

	if _, err := svc.Generate(context.Background(), user, FormatBase64); err == nil {
		t.Fatal("节点查询失败必须冒泡（500），不能静默返回空订阅")
	}
}

// TestStd02SubConcurrencyChunking：分批并发取链不改变顺序（Bearer 节点分组）。
func TestStd02SubConcurrencyChunking(t *testing.T) {
	items := make([]nodeEntry, 7)
	for i := range items {
		items[i] = nodeEntry{node: Node{ID: int64(i + 1)}}
	}
	chunks := chunkEntries(items, 3)
	if len(chunks) != 3 || len(chunks[0]) != 3 || len(chunks[2]) != 1 {
		t.Fatalf("分批结果不对: %d 批（大小 %d/%d/%d）", len(chunks), len(chunks[0]), len(chunks[1]), len(chunks[2]))
	}
	if chunks[0][0].node.ID != 1 || chunks[2][0].node.ID != 7 {
		t.Error("分批打乱了顺序（订阅顺序敏感）")
	}
	if got := chunkEntries(nil, 3); len(got) != 0 {
		t.Errorf("空输入应返回空批次，得到 %v", got)
	}

	// 并发取链：单节点失败只丢该节点。
	repo := newStd02Repo()
	svc := std02Service(repo, std02Now())
	user, _ := repo.UserByToken(context.Background(), "tok-ok")
	results := svc.fetchBatchConcurrently(context.Background(), []nodeEntry{
		{node: repo.nodes[0], inboundIDs: []int64{1}},
		{node: Node{ID: 99, Name: "bad", Enabled: true, Status: "online"}}, // 面板没有链接
	}, user, "ch_user_7", &siteConfigReader{values: repo.configs}, &renderStats{})
	if len(results) != 2 {
		t.Fatalf("结果数量 = %d，期望 2", len(results))
	}
	if len(results[0]) == 0 {
		t.Error("正常节点应产出链接")
	}
	if len(results[1]) != 0 {
		t.Error("取不到链接的节点不该产出任何东西")
	}
}
