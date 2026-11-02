package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runCommand 执行一条面板指令（adu / rmu / test_outbound / reality_scan / upgrade /
// geodata_sync / port_probe / firewall_hop_* / sysctl_apply / client_list /
// cert_status / cert_issue）。
//
// 这里刻意不提供通用 exec：面板密钥一旦泄露，任意 shell 等于全部节点的 root 后门。
// 每个 action 都是窄接口 + 参数白名单（issue #94 R6）。
func runCommand(p Paths, cmd Command) AckResult {
	switch cmd.Action {
	case "adu":
		return runAdu(p, cmd)
	case "rmu":
		return runRmu(p, cmd)
	case "test_outbound":
		return runTestOutbound(p, cmd)
	case "reality_scan":
		return runRealityScan(p, cmd)
	case "upgrade":
		return runUpgrade(p, cmd)
	case "geodata_sync":
		return runGeodataSync(p, cmd)
	case "port_probe":
		return runPortProbe(p, cmd)
	case "firewall_hop_apply":
		return runFirewallHopApply(p, cmd)
	case "firewall_hop_remove":
		return runFirewallHopRemove(p, cmd)
	case "firewall_hop_status":
		return runFirewallHopStatus(p, cmd)
	case "sysctl_apply":
		return runSysctlApply(p, cmd)
	case "client_list":
		return runClientList(p, cmd)
	case "cert_status":
		return runCertStatus(p, cmd)
	case "cert_issue":
		return runCertIssue(p, cmd)
	default:
		return AckResult{ID: cmd.ID, Success: false, Output: "unknown action: " + cmd.Action}
	}
}

// runAdu 新增/更新用户：载荷即完整入站片段 JSON，落盘后喂 `xray api adu`。
func runAdu(p Paths, cmd Command) AckResult {
	path := filepath.Join(p.Tmp, fmt.Sprintf("adu_%d.json", cmd.ID))
	if err := os.WriteFile(path, cmd.Payload, 0o600); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "写入载荷失败: " + err.Error()}
	}
	out, err := execXrayAPI(p, 30*time.Second, "adu", apiServerFlag(), path)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: truncate(out, 1500)}
	}
	// xray api adu 构建失败时退出码仍是 0，只把错误打在输出里（实测 v26.3.27：
	// "processing inbound: ... failed to build config: ..."）。必须按输出判失败，
	// 否则面板把「用户根本没加进内核」当成功，热应用静默失效、用户直接断连。
	if strings.Contains(strings.ToLower(out), "failed") {
		return AckResult{ID: cmd.ID, Success: false, Output: truncate(out, 1500)}
	}
	return AckResult{ID: cmd.ID, Success: true, Output: truncate(out, 1500)}
}

// runRmu 移除用户：payload = {tag, emails[]}。
func runRmu(p Paths, cmd Command) AckResult {
	var payload struct {
		Tag    string   `json:"tag"`
		Emails []string `json:"emails"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}
	if payload.Tag == "" {
		payload.Tag = "vless-reality"
	}
	if len(payload.Emails) == 0 {
		return AckResult{ID: cmd.ID, Success: true, Output: "no emails"}
	}

	args := append([]string{"rmu", apiServerFlag(), "-tag=" + payload.Tag}, payload.Emails...)
	out, err := execXrayAPI(p, 30*time.Second, args...)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: truncate(out, 1500)}
	}
	return AckResult{ID: cmd.ID, Success: true, Output: truncate(out, 1500)}
}

// runTestOutbound 出站连通测试：本机直连目标 host:port（TCP 探活）。
// payload = {host, port}；面板据回执判断网络层可达性。
func runTestOutbound(p Paths, cmd Command) AckResult {
	var payload struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}
	if payload.Host == "" || payload.Port <= 0 {
		return AckResult{ID: cmd.ID, Success: false, Output: "host/port 缺失"}
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	dialer := netDialer()
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", payload.Host, payload.Port))
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: fmt.Sprintf("不可达（%dms）: %v", elapsed, err)}
	}
	_ = conn.Close()
	return AckResult{ID: cmd.ID, Success: true, Output: fmt.Sprintf("可达 %s:%d（%dms）", payload.Host, payload.Port, elapsed)}
}

// execXrayAPI 运行 xray api 子命令，返回合并输出。
func execXrayAPI(p Paths, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.XrayBin, append([]string{"api"}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// netDialer TCP 拨号器（出站测试用）。
func netDialer() *net.Dialer {
	return &net.Dialer{Timeout: 8 * time.Second}
}

// cleanTmpAduFiles 清理 10 分钟前的指令载荷文件（保留近期供排查）。
func cleanTmpAduFiles(p Paths) {
	entries, err := os.ReadDir(p.Tmp)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-10 * time.Minute)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "adu_") {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(p.Tmp, e.Name()))
		}
	}
}
