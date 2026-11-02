package main

// sysctl / BBR 调优（issue #94 R4 + R5）。
//
// 只写 /etc/sysctl.d/99-controlhub-tuning.conf，只接受 key 白名单 —— 面板密钥一旦泄露，
// 任意 key 写入等同于任意内核参数改写（可以关掉 rp_filter、改 netfilter 行为）。
//
// 回执带「读 /proc/sys 得到的实际值」，不是「我写了什么」：sysctl -p 对不支持的 key
// 只是打一行 warning 就继续，静默不生效是这里最容易漏的失败模式。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	// sysctlConfPath 唯一允许写入的文件（issue #94 R4）。
	sysctlConfPath = "/etc/sysctl.d/99-controlhub-tuning.conf"
	// procSysRoot 读实际生效值的根（测试可替换）。
	procSysRoot = "/proc/sys"

	// sysctlMaxKeys 单次下发 key 数上限。
	sysctlMaxKeys = 16
)

// sysctlWhitelist 允许的 key → 值校验器。
//
// BBR 那两项对 HY2 无效（HY2 走 UDP/QUIC，QUIC BBR 由内核 quicParams.congestion 控制），
// 这里管的是系统 TCP BBR —— 对 vless/vmess/trojan/ss 有效。
var sysctlWhitelist = map[string]func(string) error{
	"net.ipv4.tcp_congestion_control":                    sysctlCheckCongestion,
	"net.core.default_qdisc":                             sysctlCheckQdisc,
	"net.core.rmem_max":                                  sysctlCheckUint,
	"net.core.wmem_max":                                  sysctlCheckUint,
	"net.ipv4.tcp_rmem":                                  sysctlCheckTriple,
	"net.ipv4.tcp_wmem":                                  sysctlCheckTriple,
	"net.ipv4.tcp_fastopen":                              sysctlCheckUint,
	"net.core.somaxconn":                                 sysctlCheckUint,
	"net.core.netdev_max_backlog":                        sysctlCheckUint,
	"net.ipv4.ip_local_port_range":                       sysctlCheckPortPair,
	"net.netfilter.nf_conntrack_max":                     sysctlCheckUint,
	"net.netfilter.nf_conntrack_tcp_timeout_established": sysctlCheckUint,
}

// sysctlProfileBodies 预置 profile 的键值（面板可以只传 profile，不带 keys）。
var sysctlProfileBodies = map[string]map[string]string{
	"bbr": {
		"net.core.default_qdisc":          "fq",
		"net.ipv4.tcp_congestion_control": "bbr",
	},
	"conntrack": {
		"net.netfilter.nf_conntrack_max": "262144",
	},
}

type sysctlPayload struct {
	Profile string            `json:"profile"`
	Keys    map[string]string `json:"keys"`
}

type sysctlResult struct {
	Applied   map[string]string `json:"applied"`
	Modules   []string          `json:"modules,omitempty"`
	Path      string            `json:"path,omitempty"`
	Unchanged bool              `json:"unchanged,omitempty"`
	Reason    string            `json:"reason,omitempty"`
}

// runSysctlApply 把白名单键值写进专属 conf，modprobe 后用 sysctl -p 应用并读回实际值。
func runSysctlApply(_ Paths, cmd Command) AckResult {
	var payload sysctlPayload
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}

	keys, err := mergeSysctlKeys(payload)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: err.Error()}
	}

	ctx := context.Background()
	res := sysctlResult{Path: sysctlConfPath, Applied: map[string]string{}}

	// 先装模块再 apply：模块没加载时 sysctl -p 对 bbr 只会打 warning，
	// tcp_congestion_control 静默停在 cubic —— 回执里也会看到实际值不是 bbr。
	modules, err := modprobeForKeys(ctx, keys)
	res.Modules = modules
	if err != nil {
		res.Reason = err.Error()
		return marshalSysctlAck(cmd.ID, false, res)
	}

	body := renderSysctlConf(keys)
	if err := os.MkdirAll("/etc/sysctl.d", 0o755); err != nil {
		res.Reason = "创建 /etc/sysctl.d 失败: " + err.Error()
		return marshalSysctlAck(cmd.ID, false, res)
	}
	if err := atomicWriteFile(sysctlConfPath, []byte(body), 0o644); err != nil {
		res.Reason = "写入 " + sysctlConfPath + " 失败: " + err.Error()
		return marshalSysctlAck(cmd.ID, false, res)
	}

	if out, err := runCmd(ctx, "sysctl", nil, "-p", sysctlConfPath); err != nil {
		res.Reason = "sysctl -p 失败: " + truncate(out, 300)
		return marshalSysctlAck(cmd.ID, false, res)
	}

	// 回执用 /proc/sys 的实际值：写成功 ≠ 生效。
	var mismatched []string
	for _, key := range sortedKeys(keys) {
		got, ok := readProcSys(key)
		if !ok {
			mismatched = append(mismatched, key+"（读不到 /proc/sys 值）")
			continue
		}
		res.Applied[key] = got
		if !sysctlValuesEqual(got, keys[key]) {
			mismatched = append(mismatched, fmt.Sprintf("%s=%s（期望 %s）", key, got, keys[key]))
		}
	}
	if len(mismatched) > 0 {
		res.Reason = "以下参数未实际生效: " + strings.Join(mismatched, "; ")
		return marshalSysctlAck(cmd.ID, false, res)
	}

	return marshalSysctlAck(cmd.ID, true, res)
}

func marshalSysctlAck(id int64, success bool, res sysctlResult) AckResult {
	raw, err := json.Marshal(res)
	if err != nil {
		return AckResult{ID: id, Success: false, Output: "结果序列化失败: " + err.Error()}
	}

	return AckResult{ID: id, Success: success, Output: string(raw)}
}

// mergeSysctlKeys 合并 profile 与显式 keys，逐项过白名单与值校验。
func mergeSysctlKeys(payload sysctlPayload) (map[string]string, error) {
	out := map[string]string{}
	if payload.Profile != "" {
		body, ok := sysctlProfileBodies[strings.ToLower(strings.TrimSpace(payload.Profile))]
		if !ok {
			return nil, fmt.Errorf("未知 profile: %q", payload.Profile)
		}
		for k, v := range body {
			out[k] = v
		}
	}
	for k, v := range payload.Keys {
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("profile / keys 至少要有一项")
	}
	if len(out) > sysctlMaxKeys {
		return nil, fmt.Errorf("keys 超过上限 %d 项", sysctlMaxKeys)
	}
	for k, v := range out {
		check, ok := sysctlWhitelist[k]
		if !ok {
			return nil, fmt.Errorf("key 不在白名单: %q", k)
		}
		if err := check(v); err != nil {
			return nil, fmt.Errorf("%s: %v", k, err)
		}
	}

	return out, nil
}

// renderSysctlConf 生成 conf 文本：排序输出（同一份输入恒等，幂等覆盖）。
func renderSysctlConf(keys map[string]string) string {
	var b strings.Builder
	b.WriteString("# 由 ControlHub agent 管理（sysctl_apply），请勿手工编辑：\n")
	b.WriteString("# 本文件里的键值会被面板下发覆盖。\n")
	for _, k := range sortedKeys(keys) {
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(keys[k])
		b.WriteString("\n")
	}

	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}

// modprobeForKeys 按 key 需要预加载模块（tcp_bbr / nf_conntrack），已加载则跳过。
func modprobeForKeys(ctx context.Context, keys map[string]string) ([]string, error) {
	var want []string
	if strings.EqualFold(keys["net.ipv4.tcp_congestion_control"], "bbr") {
		want = append(want, "tcp_bbr")
	}
	for k := range keys {
		if strings.HasPrefix(k, "net.netfilter.") {
			want = append(want, "nf_conntrack")
			break
		}
	}

	loaded := loadedKernelModules()
	var done []string
	for _, mod := range want {
		if loaded[mod] {
			continue
		}
		out, err := runCmd(ctx, "modprobe", nil, mod)
		if err != nil {
			detail := truncate(out, 200)
			if detail == "" {
				detail = err.Error()
			}
			return done, fmt.Errorf("modprobe %s 失败: %s", mod, detail)
		}
		done = append(done, mod)
	}

	return done, nil
}

// readKernelModules 从 /proc/modules 取已加载模块名集合。
// ok=false 表示读不到（非 Linux）—— 调用方据此区分「没有 tcp_bbr」与「根本读不到」。
func readKernelModules() (map[string]bool, bool) {
	if !fileExists("/proc/modules") {
		return nil, false
	}
	text := readFileString("/proc/modules")
	if text == "" {
		// BBR 编进内核时 /proc/modules 里不会有 tcp_bbr，但文件仍然是可读的。
		return map[string]bool{}, true
	}

	return parseKernelModules(text), true
}

// loadedKernelModules 取已加载模块名集合（读不到时返回空集，供内部判定用）。
func loadedKernelModules() map[string]bool {
	mods, _ := readKernelModules()

	return mods
}

// parseKernelModules 解析 /proc/modules：首列即模块名（下划线/连字符两种写法都收）。
func parseKernelModules(text string) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		out[fields[0]] = true
		out[strings.ReplaceAll(fields[0], "-", "_")] = true
	}

	return out
}

// readProcSys 读 /proc/sys 下的实际值（key 的 . → /，连字符保持）。
func readProcSys(key string) (string, bool) {
	v := readFileString(procSysRoot + "/" + strings.ReplaceAll(key, ".", "/"))
	if v == "" {
		return "", false
	}

	return strings.Join(strings.Fields(v), " "), true
}

// sysctlValuesEqual 实际值 vs 期望值：空白归一后比较（内核可能回显多空格）。
func sysctlValuesEqual(got, want string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	return norm(got) == norm(want)
}

// ── 值校验器（白名单的一部分） ─────────────────────────────

// sysctlCheckUint 非负十进制整数。
func sysctlCheckUint(v string) error {
	if v == "" {
		return fmt.Errorf("值为空")
	}
	if _, err := strconv.ParseUint(v, 10, 64); err != nil {
		return fmt.Errorf("需要非负整数，收到 %q", v)
	}

	return nil
}

// sysctlCheckTriple "min default max" 三个非负整数。
func sysctlCheckTriple(v string) error {
	parts := strings.Fields(v)
	if len(parts) != 3 {
		return fmt.Errorf("需要 \"min default max\" 三个整数，收到 %q", v)
	}
	for _, p := range parts {
		if err := sysctlCheckUint(p); err != nil {
			return err
		}
	}

	return nil
}

// sysctlCheckPortPair "lo hi" 两个合法端口（lo <= hi）。
func sysctlCheckPortPair(v string) error {
	parts := strings.Fields(v)
	if len(parts) != 2 {
		return fmt.Errorf("需要 \"lo hi\" 两个端口，收到 %q", v)
	}
	var ports [2]int
	for i, p := range parts {
		n, err := parsePortNumber(p)
		if err != nil {
			return err
		}
		ports[i] = n
	}
	if ports[0] > ports[1] {
		return fmt.Errorf("区间起点大于终点: %q", v)
	}

	return nil
}

// sysctlCongestionAlgorithms 允许的拥塞控制算法（名字来自内核注册表，不是任意串）。
var sysctlCongestionAlgorithms = map[string]bool{
	"bbr":   true,
	"cubic": true,
	"reno":  true,
}

func sysctlCheckCongestion(v string) error {
	if !sysctlCongestionAlgorithms[strings.ToLower(v)] {
		return fmt.Errorf("只接受 bbr/cubic/reno，收到 %q", v)
	}

	return nil
}

// sysctlQdiscs 允许的队列规则。
var sysctlQdiscs = map[string]bool{
	"fq":         true,
	"fq_codel":   true,
	"cake":       true,
	"pfifo_fast": true,
	"noqueue":    true,
}

func sysctlCheckQdisc(v string) error {
	if !sysctlQdiscs[strings.ToLower(v)] {
		return fmt.Errorf("只接受 fq/fq_codel/cake/pfifo_fast/noqueue，收到 %q", v)
	}

	return nil
}

// ── R5 心跳 BBR 状态 ──────────────────────────────────────

// bbrStatus 心跳里的 BBR / 连接跟踪状态（面板节点页直接展示，不用登机查）。
//
// 字段用指针：读不到 /proc/sys（非 Linux、或 conntrack 模块未加载）时必须是 null，
// 不能是 "" / false / 0 —— 面板要能区分「没上报」和「上报了关」。
type bbrStatus struct {
	CongestionControl *string `json:"tcp_congestion_control"`
	DefaultQdisc      *string `json:"default_qdisc"`
	BBRModule         *bool   `json:"bbr_module"`
	ConntrackMax      *int64  `json:"conntrack_max"`
}

// collectBBRStatus 采集当前 BBR 状态（全只读；非 Linux 时全为 null）。
func collectBBRStatus() bbrStatus {
	st := bbrStatus{}
	if v, ok := readProcSys("net.ipv4.tcp_congestion_control"); ok {
		st.CongestionControl = &v
	}
	if v, ok := readProcSys("net.core.default_qdisc"); ok {
		st.DefaultQdisc = &v
	}
	if mods, ok := readKernelModules(); ok {
		loaded := mods["tcp_bbr"]
		st.BBRModule = &loaded
	}
	if v, ok := readProcSys("net.netfilter.nf_conntrack_max"); ok {
		if n, err := strconv.ParseInt(strings.Fields(v)[0], 10, 64); err == nil {
			st.ConntrackMax = &n
		}
	}

	return st
}
