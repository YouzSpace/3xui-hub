package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// upgradeRequested 二进制已替换：等回执发出去就退出，由 systemd 的 Restart=always 拉起新版。
//
// 为什么用「退出」而不是自己重启：agent 是 systemd Type=simple 常驻进程，自己 exec 新二进制
// 要处理 fd/信号/环境继承一堆细节；退出让 systemd 拉起是最省事也最可靠的路径。
// xray 内核不受影响：它由 startXray 以脱离会话的方式启动（spawnAttrs，见 xrayproc.go），
// agent 退出不带走它；新 agent 起来后 superviseLoop 看到 pidfile 命中，也不会重复启动。
var upgradeRequested atomic.Bool

// runUpgrade 面板下发的 agent 自升级（指令 action = upgrade）。
//
// payload = {file, sha256, version, url}：
//   - file/sha256/version 取自面板 manifest.json 的 agent 段
//   - url 是面板上的相对路径，用本机 state.json 里已连通的 hub 地址拼 —— 节点可能用 IP 连面板，
//     不能依赖面板的 APP_URL（域名解析不到就升不了级）
//
// 安全底线：sha256 校验不过**绝不替换**（面板下发前也复验一次，双保险）；先写 .new 再原子
// rename，不会出现半个二进制；任何一步失败都保留旧二进制继续跑，绝不把自己搞挂。
func runUpgrade(p Paths, cmd Command) AckResult {
	var payload struct {
		File    string `json:"file"`
		SHA256  string `json:"sha256"`
		Version string `json:"version"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}
	if payload.SHA256 == "" || payload.URL == "" {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷缺少 sha256/url"}
	}

	// 幂等：本地这份与面板下发的就是同一份（sha 相同）→ 什么都不做，也不重启
	//（用 sha 而不是版本号判断：同版本号重新编译过的二进制也应该能升上去）
	if self, err := sha256File(p.AgentBin); err == nil && strings.EqualFold(self, payload.SHA256) {
		return AckResult{ID: cmd.ID, Success: true, Output: "已是同一份二进制（v" + payload.Version + "），无需升级"}
	}

	st, err := loadState(p.State)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "读取 state.json 失败: " + err.Error()}
	}
	base := strings.TrimRight(st.HubURL, "/")
	if base == "" {
		return AckResult{ID: cmd.ID, Success: false, Output: "state.json 缺少 hub_url"}
	}
	src := base + "/" + strings.TrimLeft(payload.URL, "/")

	// 临时文件放同目录：rename 才是原子的（跨文件系统 rename 会失败）
	tmp := p.AgentBin + ".new"
	_ = os.Remove(tmp) // 清掉上次失败的残留

	sum, err := downloadFile(src, tmp, 120*time.Second)
	if err != nil {
		_ = os.Remove(tmp)
		return AckResult{ID: cmd.ID, Success: false, Output: "下载失败: " + err.Error()}
	}
	if !strings.EqualFold(sum, payload.SHA256) {
		_ = os.Remove(tmp)
		return AckResult{ID: cmd.ID, Success: false, Output: fmt.Sprintf(
			"sha256 校验失败（期望 %s，实际 %s），已放弃替换，继续运行旧版本",
			truncate(payload.SHA256, 16), truncate(sum, 16))}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return AckResult{ID: cmd.ID, Success: false, Output: "设置可执行位失败: " + err.Error()}
	}
	// 同目录 rename = 原子替换；正在运行的旧二进制由内核持有 inode，不受影响
	if err := os.Rename(tmp, p.AgentBin); err != nil {
		_ = os.Remove(tmp)
		return AckResult{ID: cmd.ID, Success: false, Output: "替换二进制失败: " + err.Error()}
	}

	upgradeRequested.Store(true)
	return AckResult{
		ID:      cmd.ID,
		Success: true,
		Output: fmt.Sprintf("已替换为 v%s（sha256 校验通过），回执发出后退出，由 systemd 拉起新版",
			payload.Version),
	}
}

// exitIfUpgradeRequested 回执已发出：退出进程让 systemd 拉起新版（Restart=always）。
// 两条指令通道（WS 推送 / HTTP 长轮询）都在回执之后调用它。
func exitIfUpgradeRequested() {
	if !upgradeRequested.Load() {
		return
	}
	logf("agent 二进制已升级，退出进程（systemd 将拉起新版）")
	os.Exit(0)
}

// downloadFile 下载到本地文件，返回内容 sha256（十六进制小写）。
// 边落盘边算 hash，不额外读一遍文件。
func downloadFile(url, path string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 64<<20))
	closeErr := f.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// sha256File 本地文件内容指纹（判断「是不是同一份二进制」用）。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
