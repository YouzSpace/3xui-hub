package main

// Port hopping（issue #94 R1–R3）：port_probe / firewall_hop_apply /
// firewall_hop_remove / firewall_hop_status。
//
// 端口跳跃不是内核的活：xray 的 hysteria2 服务端只监听主端口，跳跃区间内的其它 UDP
// 端口必须由节点防火墙 REDIRECT 到主端口，客户端（?mport=A-B）跳过去才有人接。
// 这里只碰 nat 表的 HY2HOP-<inbound_id> 自定义链 + 指向它的 PREROUTING 跳转：
// 不 flush 其它链、不改 policy、不重启防火墙服务（重启会清运行态，可能瞬间断连）。
//
// 测试口径：Windows 开发机不能跑 iptables，所有判定（区间解析、空闲段挑选、
// 主端口落在区间的拒绝、幂等/自验计划）都写成不碰系统的纯函数，
// 执行层只负责按计划撒命令。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// hopChainPrefix 自定义链前缀：HY2HOP-<inbound_id>（每个入站一条链，互不影响）。
	hopChainPrefix = "HY2HOP-"
	// hopChainMaxLen iptables 链名上限 29 字符，留一位余量。
	hopChainMaxLen = 28
	// hopCmdTimeout 单条 iptables / apt-get 命令超时。
	hopCmdTimeout = 30 * time.Second
	// hopOutputMax 回执 JSON 上限（面板 ack output 上限 40000，留余量）。
	hopOutputMax = 20000
	// hopBusyMax 回执里 busy_udp / busy_tcp 最多列多少个端口。
	hopBusyMax = 512
	// hopFreeRangesMax 回执里 free_ranges 最多列多少段（picked 恒在列表内）。
	hopFreeRangesMax = 64
	// hopCountMax 单次申请连续空闲端口数上限（跳跃区间不会比这更大）。
	hopCountMax = 4096
	// hopPortMax 合法端口上限。
	hopPortMax = 65535

	procNetUDP  = "/proc/net/udp"
	procNetUDP6 = "/proc/net/udp6"
	procNetTCP  = "/proc/net/tcp"
	procNetTCP6 = "/proc/net/tcp6"

	// iptablesRulesV4 netfilter-persistent 的落盘文件（status 用它判断 persisted）。
	iptablesRulesV4 = "/etc/iptables/rules.v4"
)

// ── 命令执行缝（测试只测纯逻辑，不替换；生产走 exec） ──────────

// cmdRunner 执行外部命令并返回合并输出。留成变量是为了让逻辑与命令构造可分离。
type cmdRunner func(ctx context.Context, name string, env []string, args ...string) (string, error)

var runCmd cmdRunner = execCommand

// hopExtraPath 系统工具常见落点。systemd 单元的 PATH 只保证 /usr/bin:/bin，
// 而 iptables 在 Debian 系可能在 /usr/sbin —— agent 以 root 跑，缺 sbin 会让
// LookPath 找不到 iptables（表现为「明明装了却说缺依赖」）。
var hopExtraPath = []string{
	"/usr/local/sbin", "/usr/sbin", "/sbin",
	"/usr/local/bin", "/usr/bin", "/bin",
}

// systemPath 在当前 PATH 后面续上 hopExtraPath（保持顺序、去重）。
func systemPath() string {
	cur := os.Getenv("PATH")
	parts := strings.Split(cur, string(os.PathListSeparator))
	seen := make(map[string]bool, len(parts)+len(hopExtraPath))
	for _, p := range parts {
		if p != "" {
			seen[p] = true
		}
	}
	for _, p := range hopExtraPath {
		if !seen[p] {
			parts = append(parts, p)
			seen[p] = true
		}
	}

	return strings.Join(parts, string(os.PathListSeparator))
}

func execCommand(ctx context.Context, name string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hopCmdTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Env = append(os.Environ(), "PATH="+systemPath())
	if len(env) > 0 {
		c.Env = append(c.Env, env...)
	}
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// lookPath 在「补全后的 PATH」里找可执行文件（不依赖 agent 自身进程的 PATH）。
func lookPath(name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		return name, nil // 绝对/相对路径直接交给 exec
	}
	var err error
	for _, dir := range strings.Split(systemPath(), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		st, statErr := os.Stat(candidate)
		if statErr != nil || st.IsDir() {
			err = statErr
			continue
		}
		// Windows 上 os.Stat 不带执行位（恒为 0666），只在 Linux/Unix 校验。
		if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
			err = fmt.Errorf("%s 不可执行", candidate)
			continue
		}
		return candidate, nil
	}
	if err == nil {
		err = exec.ErrNotFound
	}

	return "", err
}

// ── 端口区间 ────────────────────────────────────────────────

// portRange 闭区间 [Start, End]（端口号，1..65535）。
type portRange struct {
	Start int
	End   int
}

func (r portRange) String() string {
	if r.Start == r.End {
		return strconv.Itoa(r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End)
}

// Size 区间内端口个数。
func (r portRange) Size() int { return r.End - r.Start + 1 }

// Contains 端口是否落在区间内（含两端）。
func (r portRange) Contains(port int) bool { return port >= r.Start && port <= r.End }

// iptablesExpr 转成 iptables 的端口写法（连续区间用冒号）。
func (r portRange) iptablesExpr() string {
	if r.Start == r.End {
		return strconv.Itoa(r.Start)
	}
	return fmt.Sprintf("%d:%d", r.Start, r.End)
}

// parsePortRanges 解析 "20000-50000" / "443" / "20000-20199,30000"（逗号分隔多段）。
// 空串、非数字、越界、起点大于终点一律报错——面板下发的区间不做任何猜测式修正。
func parsePortRanges(text string) ([]portRange, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("端口区间为空")
	}

	var out []portRange
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("端口区间含空段: %q", text)
		}
		lo, hi := part, part
		if idx := strings.Index(part, "-"); idx >= 0 {
			lo, hi = strings.TrimSpace(part[:idx]), strings.TrimSpace(part[idx+1:])
		}
		start, err := parsePortNumber(lo)
		if err != nil {
			return nil, err
		}
		end, err := parsePortNumber(hi)
		if err != nil {
			return nil, err
		}
		if start > end {
			return nil, fmt.Errorf("端口区间起点大于终点: %s", part)
		}
		out = append(out, portRange{Start: start, End: end})
	}

	return out, nil
}

func parsePortNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("端口不是数字: %q", s)
	}
	if n < 1 || n > hopPortMax {
		return 0, fmt.Errorf("端口越界: %d（1-%d）", n, hopPortMax)
	}
	return n, nil
}

// ── 入参规格（白名单校验，所有 action 共用） ─────────────────

// hopSpec firewall_hop_apply 的载荷。
type hopSpec struct {
	InboundID int    `json:"inbound_id"`
	MainPort  int    `json:"main_port"`
	Ports     string `json:"ports"`
	Proto     string `json:"proto"`
}

// normalized 校验并规整载荷，返回主端口与跳跃区间。
//
// 主端口落在跳跃区间内必须拒绝：否则主端口自己的流量也会被 PREROUTING 的 REDIRECT
// 绕进自定义链，主入站直接坏掉（issue #94「最容易做漏」第 3 条）。
func (s hopSpec) normalized() (hopSpec, portRange, error) {
	if s.InboundID <= 0 || s.InboundID > 2_000_000_000 {
		return s, portRange{}, fmt.Errorf("inbound_id 非法: %d", s.InboundID)
	}
	if s.MainPort < 1 || s.MainPort > hopPortMax {
		return s, portRange{}, fmt.Errorf("main_port 非法: %d", s.MainPort)
	}
	if s.Proto == "" {
		s.Proto = "udp"
	}
	s.Proto = strings.ToLower(strings.TrimSpace(s.Proto))
	if s.Proto != "udp" {
		return s, portRange{}, fmt.Errorf("proto 只接受 udp（HY2 端口跳跃走 UDP），收到 %q", s.Proto)
	}

	ranges, err := parsePortRanges(s.Ports)
	if err != nil {
		return s, portRange{}, err
	}
	if len(ranges) != 1 {
		return s, portRange{}, fmt.Errorf("ports 只接受单个连续区间，收到 %q", s.Ports)
	}
	r := ranges[0]
	if r.Contains(s.MainPort) {
		return s, portRange{}, fmt.Errorf("主端口 %d 落在跳跃区间 %s 内，拒绝下发（否则主端口流量也会被 REDIRECT 绕进去）", s.MainPort, r)
	}
	if len(hopChainName(s.InboundID)) > hopChainMaxLen {
		return s, portRange{}, fmt.Errorf("inbound_id %d 生成的链名过长", s.InboundID)
	}

	s.Ports = r.String()

	return s, r, nil
}

// hopChainName 每个入站一条独立链。
func hopChainName(inboundID int) string { return hopChainPrefix + strconv.Itoa(inboundID) }

// ── 幂等计划 / 写后自验（纯逻辑，单测目标） ─────────────────

// hopPlan 依据当前规则算出的最小动作集。
type hopPlan struct {
	StaleJumps   [][]string // 指向本链但与期望不符的 PREROUTING 跳转（先删）
	ExtraRules   [][]string // 链内与期望不符的规则（先删，避免同链多端口旧值残留）
	AddChainRule bool       // 链内缺 REDIRECT → 需 -A
	AddJump      bool       // PREROUTING 缺跳转 → 需 -A
}

// buildHopPlan 幂等判定：期望规则已存在就什么都不做（先 -C 再 -A 的等价物）。
// 区间被改过时旧跳转必须删掉，否则同一入站会有两条 --dport，新区间生效但旧区间仍在。
func buildHopPlan(spec hopSpec, r portRange, chainRules, prerouting [][]string) hopPlan {
	chain := hopChainName(spec.InboundID)
	plan := hopPlan{AddChainRule: true, AddJump: true}

	for _, rule := range prerouting {
		if ruleJumpTarget(rule) != chain {
			continue
		}
		// 第一条正确跳转即满足期望；之后参数不符或重复的同款跳转都算 stale 删掉，
		// 否则自验会因为「跳转有 2 条」永远失败。
		if plan.AddJump && ruleMatchesJump(rule, spec.Proto, r, chain) {
			plan.AddJump = false
			continue
		}
		plan.StaleJumps = append(plan.StaleJumps, rule)
	}
	matchedRule := false
	for _, rule := range chainRules {
		if ruleMatchesRedirect(rule, spec.MainPort) {
			matchedRule = true
			continue
		}
		// 本链是 agent 独占的（链名带 inbound_id），链里的旧规则只可能是历史主端口。
		// 留着会让「主端口换过」的入站同时 REDIRECT 到新旧两个端口（新端口的包被旧规则先命中）。
		plan.ExtraRules = append(plan.ExtraRules, rule)
	}
	plan.AddChainRule = !matchedRule

	return plan
}

// verifyHopState 读回自验：PREROUTING 恰好一条指向本链且参数正确，链内恰好有主端口 REDIRECT。
// 返回 (verified, 原因)。
func verifyHopState(spec hopSpec, r portRange, chainRules, prerouting [][]string) (bool, string) {
	chain := hopChainName(spec.InboundID)
	matched := 0
	for _, rule := range prerouting {
		if ruleJumpTarget(rule) != chain {
			continue
		}
		if !ruleMatchesJump(rule, spec.Proto, r, chain) {
			return false, fmt.Sprintf("PREROUTING 仍残留指向 %s 的非预期跳转: %s", chain, joinArgs(rule))
		}
		matched++
	}
	switch {
	case matched == 0:
		return false, fmt.Sprintf("PREROUTING 缺少 --dport %s -j %s", r.iptablesExpr(), chain)
	case matched > 1:
		return false, fmt.Sprintf("PREROUTING 指向 %s 的跳转有 %d 条（应恰好 1 条）", chain, matched)
	}

	matchedRule := false
	for _, rule := range chainRules {
		if ruleMatchesRedirect(rule, spec.MainPort) {
			matchedRule = true
			continue
		}
		return false, fmt.Sprintf("%s 残留非预期规则: %s", chain, joinArgs(rule))
	}

	if !matchedRule {
		return false, fmt.Sprintf("%s 缺少 REDIRECT --to-ports %d", chain, spec.MainPort)
	}

	return true, ""
}
// ruleJumpTarget 规则的目标链（-j / --jump）。
func ruleJumpTarget(args []string) string { return ruleFields(args)["jump"] }

// ruleMatchesJump PREROUTING 跳转是否正是「-p udp --dport A:B -j chain」。
func ruleMatchesJump(args []string, proto string, r portRange, chain string) bool {
	f := ruleFields(args)
	if f["jump"] != chain {
		return false
	}
	if strings.ToLower(f["proto"]) != strings.ToLower(proto) {
		return false
	}

	return normalizePortExpr(f["dport"]) == normalizePortExpr(r.iptablesExpr())
}

// ruleMatchesRedirect 链内规则是否正是「--dport 主端口 -j REDIRECT --to-ports 主端口」。
func ruleMatchesRedirect(args []string, mainPort int) bool {
	f := ruleFields(args)
	if f["jump"] != "REDIRECT" {
		return false
	}
	want := strconv.Itoa(mainPort)

	return normalizePortExpr(f["dport"]) == want && normalizePortExpr(f["to-ports"]) == want
}

// iptablesFlagAliases 规则里需要取值的短/长选项 → 规范键名。
var iptablesFlagAliases = map[string]string{
	"-p": "proto", "--protocol": "proto",
	"-j": "jump", "--jump": "jump",
	"--dport": "dport", "--destination-port": "dport",
	"--to-ports": "to-ports", "--to-port": "to-ports",
	"-m": "match", "--match": "match",
	"-s": "src", "--source": "src",
	"-d": "dst", "--destination": "dst",
	"-i": "in", "--in-interface": "in",
	"-o": "out", "--out-interface": "out",
	"--comment": "comment",
}

// ruleFields 把规则 token 解析成 {规范键: 首个取值}（同键重复时保留首个，够判定用）。
func ruleFields(args []string) map[string]string {
	fields := make(map[string]string, 8)
	for i := 0; i < len(args); i++ {
		key, ok := iptablesFlagAliases[args[i]]
		if !ok || i+1 >= len(args) {
			continue
		}
		if _, exists := fields[key]; !exists {
			fields[key] = args[i+1]
		}
		i++
	}

	return fields
}

// normalizePortExpr 统一端口的书写差异：冒号/连字符等价、单端口 A:A 与 A 等价。
// 只做书写归一，不做服务名换算（iptables -S 输出即数字）。
func normalizePortExpr(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ":", "-")
	if parts := strings.Split(s, "-"); len(parts) == 2 && parts[0] == parts[1] {
		return parts[0]
	}

	return s
}

func joinArgs(args []string) string { return strings.Join(args, " ") }

// ── iptables -S 输出解析（纯逻辑） ──────────────────────────

// iptablesRule 一行 `iptables -t nat -S` 输出。
type iptablesRule struct {
	Action string   // -A / -N / -P
	Chain  string   // 归属链
	Args   []string // -A 时：CHAIN 之后的 token（可直接喂给 -D）
	Raw    string
}

// parseIptablesS 解析 -S 输出；空行与注释行忽略。
func parseIptablesS(out string) []iptablesRule {
	var rules []iptablesRule
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens := tokenizeIptablesLine(line)
		if len(tokens) < 2 || !strings.HasPrefix(tokens[0], "-") {
			continue
		}
		rule := iptablesRule{Action: tokens[0], Chain: tokens[1], Raw: line}
		if len(tokens) > 2 {
			rule.Args = append([]string{}, tokens[2:]...)
		}
		rules = append(rules, rule)
	}

	return rules
}

// tokenizeIptablesLine 按空白切分，双引号内的空格保留（comment 里可能有空格）。
func tokenizeIptablesLine(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()

	return out
}

// chainExists 链是否已存在（-S 会给用户自定义链打一行 -N）。
func chainExists(rules []iptablesRule, chain string) bool {
	for _, r := range rules {
		if r.Chain == chain && (r.Action == "-N" || r.Action == "-A") {
			return true
		}
	}

	return false
}

// rulesOf 取某链下所有 -A 规则的参数。
func rulesOf(rules []iptablesRule, chain string) [][]string {
	var out [][]string
	for _, r := range rules {
		if r.Action == "-A" && r.Chain == chain {
			out = append(out, r.Args)
		}
	}

	return out
}

// ── 后端探测 / 依赖安装 ─────────────────────────────────────

// hopBackend iptables 后端（iptables-legacy / iptables-nft）。
type hopBackend struct {
	Name string // iptables-legacy | iptables-nft | unknown
	Bin  string // 实际可执行名；空 = 未安装
}

// parseIptablesBackend 从 `iptables --version`（"iptables v1.8.9 (nf_tables)"）判后端。
func parseIptablesBackend(version string) string {
	v := strings.ToLower(version)
	switch {
	case strings.Contains(v, "nf_tables"):
		return "iptables-nft"
	case strings.Contains(v, "legacy"):
		return "iptables-legacy"
	case strings.Contains(v, "nft"):
		return "iptables-nft"
	}

	return ""
}

// detectHopBackend 探测本机 iptables 是否可用及其后端类型（只读）。
func detectHopBackend(ctx context.Context) hopBackend {
	const bin = "iptables"
	if _, err := lookPath(bin); err != nil {
		return hopBackend{}
	}
	name := "unknown"
	if out, err := runCmd(ctx, bin, nil, "--version"); err == nil {
		if parsed := parseIptablesBackend(out); parsed != "" {
			name = parsed
		}
	}

	return hopBackend{Name: name, Bin: bin}
}

// probeBackendName port_probe / status 的 backend 字段（绝不安装任何东西）。
func probeBackendName(ctx context.Context) string {
	if b := detectHopBackend(ctx); b.Bin != "" {
		return b.Name
	}
	if _, err := lookPath("nft"); err == nil {
		return "nftables"
	}

	return "unknown"
}

// ensureHopBackend 确保 iptables（以及需要持久化时的 netfilter-persistent）可用；
// 缺了就用 apt-get 装（issue #94 R2 第 6 条），返回值里带上装了什么。
func ensureHopBackend(ctx context.Context, needPersist bool) (hopBackend, []string, error) {
	backend := detectHopBackend(ctx)
	if backend.Bin != "" && (!needPersist || commandExists("netfilter-persistent")) {
		return backend, nil, nil
	}

	installed, err := installHopTools(ctx)
	if err != nil {
		return backend, installed, err
	}
	backend = detectHopBackend(ctx)
	if backend.Bin == "" {
		return backend, installed, fmt.Errorf("安装后仍找不到 iptables")
	}
	if needPersist && !commandExists("netfilter-persistent") {
		return backend, installed, fmt.Errorf("安装后仍找不到 netfilter-persistent")
	}

	return backend, installed, nil
}

func commandExists(name string) bool {
	_, err := lookPath(name)

	return err == nil
}

// installHopTools 非交互安装 iptables + iptables-persistent（只走 apt 系；其它包管理器不猜）。
func installHopTools(ctx context.Context) ([]string, error) {
	pkgs := []string{"iptables", "iptables-persistent"}
	if !commandExists("apt-get") {
		return nil, fmt.Errorf("缺少 iptables/netfilter-persistent，且系统无 apt-get，请手工安装")
	}
	if !amRoot() {
		return nil, fmt.Errorf("缺少 iptables/netfilter-persistent，安装需要 root 权限（agent 应由 systemd 以 root 运行）")
	}
	args := []string{
		"install", "-y", "-q",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"--no-install-recommends",
	}
	args = append(args, pkgs...)
	out, err := runCmd(ctx, "apt-get", []string{"DEBIAN_FRONTEND=noninteractive"}, args...)
	if err != nil {
		return nil, fmt.Errorf("apt-get 安装 iptables 失败: %s", truncate(out, 400))
	}

	return pkgs, nil
}

// iptRun 包一层 -w（等 xtables 锁），避免与并发 iptables 调用互相失败。
// 链名/端口/协议都来自白名单校验后的载荷；错误里带的是 stderr，不会回显密钥类内容。
func iptRun(ctx context.Context, b hopBackend, args ...string) (string, error) {
	out, err := runCmd(ctx, b.Bin, nil, append([]string{"-w"}, args...)...)
	if err != nil {
		return out, fmt.Errorf("%s: %s", err, truncate(out, 200))
	}

	return out, nil
}

// readNatRules 读整个 nat 表（一次 -S 同时拿到自定义链和 PREROUTING）。
// 不加 -n：iptables -S 不接受 -n（v1.8.9 报 Illegal option '-n'），而 -S 输出端口本来就是数字。
func readNatRules(ctx context.Context, b hopBackend) ([]iptablesRule, error) {
	out, err := iptRun(ctx, b, "-t", "nat", "-S")
	if err != nil {
		return nil, fmt.Errorf("%s", truncate(out, 300))
	}

	return parseIptablesS(out), nil
}

// persistHopRules 落盘运行态规则（唯一允许的重启后保活手段；不重启服务）。
func persistHopRules(ctx context.Context) error {
	if !commandExists("netfilter-persistent") {
		return fmt.Errorf("netfilter-persistent 不可用，无法持久化（重启后规则会消失）")
	}
	out, err := runCmd(ctx, "netfilter-persistent", nil, "save")
	if err != nil {
		return fmt.Errorf("netfilter-persistent save 失败: %s", truncate(out, 300))
	}

	return nil
}

// ── R1 port_probe（只读） ──────────────────────────────────

type portProbePayload struct {
	Pool    string `json:"pool"`
	Count   int    `json:"count"`
	Exclude []int  `json:"exclude"`
}

// portProbeResult 字段名即面板 JSON 契约（issue #94 R1）。
type portProbeResult struct {
	Backend       string   `json:"backend"`
	BusyUDP       []int    `json:"busy_udp"`
	BusyTCP       []int    `json:"busy_tcp"`
	FreeRanges    []string `json:"free_ranges"`
	Picked        string   `json:"picked"`
	BusyTruncated bool     `json:"busy_truncated,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

// runPortProbe 在 pool 里挑 count 个连续空闲端口；只读 /proc/net/*，不改任何配置。
func runPortProbe(_ Paths, cmd Command) AckResult {
	var payload portProbePayload
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}

	ranges, err := parsePortRanges(payload.Pool)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "pool 非法: " + err.Error()}
	}
	if len(ranges) != 1 {
		return AckResult{ID: cmd.ID, Success: false, Output: "pool 只接受单个连续区间"}
	}
	pool := ranges[0]
	if payload.Count < 1 || payload.Count > hopCountMax {
		return AckResult{ID: cmd.ID, Success: false, Output: fmt.Sprintf("count 必须在 1-%d 之间", hopCountMax)}
	}
	if payload.Count > pool.Size() {
		return AckResult{ID: cmd.ID, Success: false, Output: fmt.Sprintf("count %d 超出 pool %s 的长度 %d", payload.Count, pool, pool.Size())}
	}

	exclude := make(map[int]bool, len(payload.Exclude))
	for _, p := range payload.Exclude {
		if p < 1 || p > hopPortMax {
			return AckResult{ID: cmd.ID, Success: false, Output: fmt.Sprintf("exclude 含非法端口: %d", p)}
		}
		exclude[p] = true
	}

	busyUDP := readPortSet([]string{procNetUDP, procNetUDP6}, nil)
	// TCP 只认 LISTEN：出站连接的临时端口是瞬态噪声，会把区间切得七零八落。
	busyTCP := readPortSet([]string{procNetTCP, procNetTCP6}, map[string]bool{"0A": true})

	res := portProbeResult{
		Backend:    probeBackendName(context.Background()),
		BusyUDP:    []int{},
		BusyTCP:    []int{},
		FreeRanges: []string{},
	}

	var busySeen bool
	res.BusyUDP, busySeen = sortBusyPorts(busyUDP, pool, exclude)
	res.BusyTCP, _ = sortBusyPorts(busyTCP, pool, exclude)
	res.BusyTruncated = busySeen

	freeRuns := freeRangesIn(pool, busyUDP, exclude)
	picked, ok := pickRange(freeRuns, payload.Count)
	if !ok {
		res.Reason = fmt.Sprintf("pool %s 内凑不出 %d 个连续空闲端口（最长空闲段 %d）", pool, payload.Count, longestRun(freeRuns))
		return marshalProbeAck(cmd.ID, false, res)
	}
	res.Picked = picked.String()
	for i, run := range freeRuns {
		if i >= hopFreeRangesMax {
			break
		}
		res.FreeRanges = append(res.FreeRanges, run.String())
	}

	return marshalProbeAck(cmd.ID, true, res)
}

func marshalProbeAck(id int64, success bool, res portProbeResult) AckResult {
	raw, err := json.Marshal(res)
	if err != nil {
		return AckResult{ID: id, Success: false, Output: "结果序列化失败: " + err.Error()}
	}
	if len(raw) > hopOutputMax {
		// 极端情况（区间碎成上千段）：缩短 busy 列表再编，保证回执始终是合法 JSON。
		res.BusyUDP = nil
		res.BusyTCP = nil
		res.BusyTruncated = true
		raw, _ = json.Marshal(res)
	}

	return AckResult{ID: id, Success: success, Output: string(raw)}
}

// readPortSet 汇总多个 /proc/net/* 表里被占用的本地端口。
// states 为 nil 时收全部条目（UDP 没有 LISTEN 状态）；否则只收状态在集合里的行。
func readPortSet(paths []string, states map[string]bool) map[int]bool {
	ports := make(map[int]bool)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue // 非 Linux / 该地址族未启用
		}
		for _, port := range parseProcNetPorts(data, states) {
			ports[port] = true
		}
	}

	return ports
}

// parseProcNetPorts 解析 /proc/net/{udp,tcp}(6)：取 local_address 的十六进制端口。
// 头部行、畸形行、端口 0（未绑定）跳过。
func parseProcNetPorts(data []byte, states map[string]bool) []int {
	var out []int
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first { // 表头
			first = false
			continue
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if states != nil && !states[strings.ToUpper(fields[3])] {
			continue
		}
		idx := strings.LastIndex(fields[1], ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.ParseInt(fields[1][idx+1:], 16, 32)
		if err != nil || port <= 0 || port > hopPortMax {
			continue
		}
		out = append(out, int(port))
	}

	return out
}

// sortBusyPorts 只保留落进 pool 或被 exclude 的端口（区外的对本次挑选没意义），排序并截断。
func sortBusyPorts(set map[int]bool, pool portRange, exclude map[int]bool) ([]int, bool) {
	out := make([]int, 0, len(set))
	for p := range set {
		if pool.Contains(p) || exclude[p] {
			out = append(out, p)
		}
	}
	sort.Ints(out)
	if len(out) > hopBusyMax {
		return out[:hopBusyMax], true
	}

	return out, false
}

// freeRangesIn 求 pool 内的最大连续空闲段（busy + exclude 都算占用）。
func freeRangesIn(pool portRange, busy, exclude map[int]bool) []portRange {
	var runs []portRange
	start := -1
	for p := pool.Start; p <= pool.End; p++ {
		if busy[p] || exclude[p] {
			if start >= 0 {
				runs = append(runs, portRange{Start: start, End: p - 1})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = p
		}
	}
	if start >= 0 {
		runs = append(runs, portRange{Start: start, End: pool.End})
	}

	return runs
}

// pickRange 取第一个长度够 count 的空闲段的前 count 个端口（确定性：从低位开始）。
func pickRange(runs []portRange, count int) (portRange, bool) {
	for _, run := range runs {
		if run.Size() >= count {
			return portRange{Start: run.Start, End: run.Start + count - 1}, true
		}
	}

	return portRange{}, false
}

func longestRun(runs []portRange) int {
	max := 0
	for _, r := range runs {
		if r.Size() > max {
			max = r.Size()
		}
	}

	return max
}

// ── R2 firewall_hop_apply ──────────────────────────────────

// hopChangeResult apply / remove 的公共回执。
type hopChangeResult struct {
	Backend   string   `json:"backend,omitempty"`
	Chain     string   `json:"chain,omitempty"`
	Verified  bool     `json:"verified"`
	Persisted bool     `json:"persisted"`
	Installed []string `json:"installed,omitempty"`
	Changes   []string `json:"changes,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

func marshalHopAck(id int64, success bool, res any) AckResult {
	raw, err := json.Marshal(res)
	if err != nil {
		return AckResult{ID: id, Success: false, Output: "结果序列化失败: " + err.Error()}
	}

	return AckResult{ID: id, Success: success, Output: string(raw)}
}

// runFirewallHopApply 幂等地把「区间 UDP → 主端口」落到 nat 表，写后读回自验并持久化。
func runFirewallHopApply(_ Paths, cmd Command) AckResult {
	var spec hopSpec
	if err := json.Unmarshal(cmd.Payload, &spec); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}
	spec, r, err := spec.normalized()
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: err.Error()}
	}

	ctx := context.Background()
	backend, installed, err := ensureHopBackend(ctx, true)
	if err != nil {
		return marshalHopAck(cmd.ID, false, hopChangeResult{Installed: installed, Reason: err.Error()})
	}
	chain := hopChainName(spec.InboundID)
	res := hopChangeResult{Backend: backend.Name, Chain: chain, Installed: installed}

	rules, err := readNatRules(ctx, backend)
	if err != nil {
		res.Reason = "读取 nat 表失败: " + err.Error()
		return marshalHopAck(cmd.ID, false, res)
	}

	if !chainExists(rules, chain) {
		if _, err := iptRun(ctx, backend, "-t", "nat", "-N", chain); err != nil {
			res.Reason = "创建链 " + chain + " 失败: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-N "+chain)
		if rules, err = readNatRules(ctx, backend); err != nil {
			res.Reason = "读取 nat 表失败: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
	}

	plan := buildHopPlan(spec, r, rulesOf(rules, chain), rulesOf(rules, "PREROUTING"))

	for _, stale := range plan.StaleJumps {
		if _, err := iptRun(ctx, backend, append([]string{"-t", "nat", "-D", "PREROUTING"}, stale...)...); err != nil {
			res.Reason = "删除旧跳转失败（" + joinArgs(stale) + "）: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-D PREROUTING "+joinArgs(stale))
	}
	for _, extra := range plan.ExtraRules {
		if _, err := iptRun(ctx, backend, append([]string{"-t", "nat", "-D", chain}, extra...)...); err != nil {
			res.Reason = "删除链内旧规则失败（" + joinArgs(extra) + "）: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-D "+chain+" "+joinArgs(extra))
	}
	if plan.AddChainRule {
		args := []string{"-t", "nat", "-A", chain, "-p", spec.Proto, "--dport", strconv.Itoa(spec.MainPort), "-j", "REDIRECT", "--to-ports", strconv.Itoa(spec.MainPort)}
		if _, err := iptRun(ctx, backend, args...); err != nil {
			res.Reason = "写入 REDIRECT 规则失败: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-A "+chain+" -p "+spec.Proto+" --dport "+strconv.Itoa(spec.MainPort)+" -j REDIRECT --to-ports "+strconv.Itoa(spec.MainPort))
	}
	if plan.AddJump {
		args := []string{"-t", "nat", "-A", "PREROUTING", "-p", spec.Proto, "--dport", r.iptablesExpr(), "-j", chain}
		if _, err := iptRun(ctx, backend, args...); err != nil {
			res.Reason = "写入 PREROUTING 跳转失败: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-A PREROUTING -p "+spec.Proto+" --dport "+r.iptablesExpr()+" -j "+chain)
	}

	// 写后读回自验：用同一后端读，不一致绝不算成功。
	after, err := readNatRules(ctx, backend)
	if err != nil {
		res.Reason = "自验读取失败: " + err.Error()
		return marshalHopAck(cmd.ID, false, res)
	}
	verified, why := verifyHopState(spec, r, rulesOf(after, chain), rulesOf(after, "PREROUTING"))
	res.Verified = verified
	if !verified {
		res.Reason = "自验不一致: " + why
		return marshalHopAck(cmd.ID, false, res)
	}

	// 持久化失败必须显式失败：重启后规则消失比不配更坑。
	if err := persistHopRules(ctx); err != nil {
		res.Reason = err.Error()
		return marshalHopAck(cmd.ID, false, res)
	}
	res.Persisted = true

	return marshalHopAck(cmd.ID, true, res)
}

// ── R3 firewall_hop_remove ─────────────────────────────────

type hopRemovePayload struct {
	InboundID int `json:"inbound_id"`
}

// runFirewallHopRemove 删跳转 → 清本链规则 → 删链 → save；只动本入站自己的链。
func runFirewallHopRemove(_ Paths, cmd Command) AckResult {
	var payload hopRemovePayload
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}
	if payload.InboundID <= 0 || payload.InboundID > 2_000_000_000 {
		return AckResult{ID: cmd.ID, Success: false, Output: fmt.Sprintf("inbound_id 非法: %d", payload.InboundID)}
	}

	ctx := context.Background()
	backend, installed, err := ensureHopBackend(ctx, true)
	if err != nil {
		return marshalHopAck(cmd.ID, false, hopChangeResult{Installed: installed, Reason: err.Error()})
	}
	chain := hopChainName(payload.InboundID)
	res := hopChangeResult{Backend: backend.Name, Chain: chain, Installed: installed}

	rules, err := readNatRules(ctx, backend)
	if err != nil {
		res.Reason = "读取 nat 表失败: " + err.Error()
		return marshalHopAck(cmd.ID, false, res)
	}

	for _, jump := range rulesOf(rules, "PREROUTING") {
		if ruleJumpTarget(jump) != chain {
			continue
		}
		if _, err := iptRun(ctx, backend, append([]string{"-t", "nat", "-D", "PREROUTING"}, jump...)...); err != nil {
			res.Reason = "删除跳转失败（" + joinArgs(jump) + "）: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-D PREROUTING "+joinArgs(jump))
	}
	if chainExists(rules, chain) {
		if _, err := iptRun(ctx, backend, "-t", "nat", "-F", chain); err != nil {
			res.Reason = "清空链 " + chain + " 失败: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-F "+chain)
		if _, err := iptRun(ctx, backend, "-t", "nat", "-X", chain); err != nil {
			res.Reason = "删除链 " + chain + " 失败: " + err.Error()
			return marshalHopAck(cmd.ID, false, res)
		}
		res.Changes = append(res.Changes, "-X "+chain)
	}

	after, err := readNatRules(ctx, backend)
	if err != nil {
		res.Reason = "自验读取失败: " + err.Error()
		return marshalHopAck(cmd.ID, false, res)
	}
	res.Verified = !chainExists(after, chain) && countJumpsTo(after, chain) == 0
	if !res.Verified {
		res.Reason = "自验不一致: 链或跳转仍存在"
		return marshalHopAck(cmd.ID, false, res)
	}

	if err := persistHopRules(ctx); err != nil {
		res.Reason = err.Error()
		return marshalHopAck(cmd.ID, false, res)
	}
	res.Persisted = true

	return marshalHopAck(cmd.ID, true, res)
}

func countJumpsTo(rules []iptablesRule, chain string) int {
	n := 0
	for _, r := range rules {
		if r.Action == "-A" && r.Chain == "PREROUTING" && ruleJumpTarget(r.Args) == chain {
			n++
		}
	}

	return n
}

// ── R3 firewall_hop_status（只读） ─────────────────────────

// hopChainStatus 单条 HY2HOP-* 链的真实运行态。
type hopChainStatus struct {
	Name      string   `json:"name"`
	Rules     []string `json:"rules"`
	Jumps     []string `json:"jumps"`
	Persisted bool     `json:"persisted"`
}

type hopStatusResult struct {
	Backend          string           `json:"backend"`
	PersistAvailable bool             `json:"persist_available"`
	Chains           []hopChainStatus `json:"chains"`
	Reason           string           `json:"reason,omitempty"`
}

// runFirewallHopStatus 列出所有 HY2HOP-* 链与规则（面板「端口跳跃」列靠它显示真实状态，
// 不能只显示数据库里的意图）。只读，不安装任何东西。
func runFirewallHopStatus(_ Paths, cmd Command) AckResult {
	ctx := context.Background()
	res := hopStatusResult{
		Backend:          probeBackendName(ctx),
		PersistAvailable: commandExists("netfilter-persistent"),
		Chains:           []hopChainStatus{},
	}
	if !commandExists("iptables") {
		res.Reason = "未找到 iptables，无法读取运行态"
		return marshalHopAck(cmd.ID, false, res)
	}
	rules, err := readNatRules(ctx, detectHopBackend(ctx))
	if err != nil {
		res.Reason = "读取 nat 表失败: " + err.Error()
		return marshalHopAck(cmd.ID, false, res)
	}

	persistedText := ""
	if data, err := os.ReadFile(iptablesRulesV4); err == nil {
		persistedText = string(data)
	}

	seen := map[string]bool{}
	for _, r := range rules {
		name := r.Chain
		if !strings.HasPrefix(name, hopChainPrefix) || seen[name] || r.Action == "-P" {
			continue
		}
		seen[name] = true
		item := hopChainStatus{
			Name:      name,
			Rules:     []string{},
			Jumps:     []string{},
			Persisted: strings.Contains(persistedText, name),
		}
		for _, rule := range rulesOf(rules, name) {
			item.Rules = append(item.Rules, joinArgs(rule))
		}
		for _, jump := range rulesOf(rules, "PREROUTING") {
			if ruleJumpTarget(jump) == name {
				item.Jumps = append(item.Jumps, joinArgs(jump))
			}
		}
		res.Chains = append(res.Chains, item)
	}
	sort.Slice(res.Chains, func(i, j int) bool { return res.Chains[i].Name < res.Chains[j].Name })

	return marshalHopAck(cmd.ID, true, res)
}
