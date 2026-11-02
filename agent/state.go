package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State 节点本地状态（state.json，600 权限；安装脚本写入 hub/name/secret）。
type State struct {
	HubURL string `json:"hub_url"`
	// HubURLV6 是可选的 IPv6 兜底地址（issue #88）：面板同时有 AAAA/独立 v6 域名时
	// 手工加进 state.json；hub_url 连不上（例如面板机没学到 v4 路由）才用它。
	// 缺省空串 = 不启用兜底，行为与改动前完全一致。
	HubURLV6    string `json:"hub_url_v6,omitempty"`
	NodeName    string `json:"node_name"`
	NodeSecret  string `json:"node_secret"`
	XrayVersion string `json:"xray_version"`
	KernelArch  string `json:"kernel_arch"`
}

// Paths 节点目录内全部运行时文件路径（保持与 bash 版一致，升级无感迁移）。
type Paths struct {
	Dir       string
	State     string
	Config    string
	ConfigNew string
	ConfigBak string
	ConfigBad string
	ETag      string
	Version   string
	Interval  string
	Pid       string
	Log       string
	Tmp       string
	XrayBin   string
	AgentBin  string
}

func resolvePaths() Paths {
	dir := os.Getenv("NODE_DIR")
	if dir == "" {
		dir = "/opt/controlhub-node"
	}

	// 内核路径默认 NODE_DIR/xray；env XRAY_BIN 可覆盖（本地联调/特殊部署）
	xrayBin := os.Getenv("XRAY_BIN")
	if xrayBin == "" {
		xrayBin = filepath.Join(dir, "xray")
	}

	return Paths{
		Dir:       dir,
		State:     filepath.Join(dir, "state.json"),
		Config:    filepath.Join(dir, "config.json"),
		ConfigNew: filepath.Join(dir, "config.new.json"),
		ConfigBak: filepath.Join(dir, "config.json.bak"),
		ConfigBad: filepath.Join(dir, "config.bad.json"),
		ETag:      filepath.Join(dir, "etag.txt"),
		Version:   filepath.Join(dir, "applied_version.txt"),
		Interval:  filepath.Join(dir, "pull_interval.txt"),
		Pid:       filepath.Join(dir, "xray.pid"),
		Log:       filepath.Join(dir, "xray.log"),
		Tmp:       filepath.Join(dir, "tmp"),
		XrayBin:   xrayBin,
		AgentBin:  filepath.Join(dir, "agent"),
	}
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state.json 解析失败: %w", err)
	}
	return &st, nil
}

func saveState(path string, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, b, 0o600)
}

// appliedVersion 本地已生效的 config 版本号（0 = 尚未生效任何版本）。
func appliedVersion(p Paths) int {
	v := readFileString(p.Version)
	if v == "" {
		return 0
	}
	n, err := json.Number(v).Int64()
	if err != nil {
		return 0
	}
	return int(n)
}

func pullIntervalCached(p Paths, fallback int) int {
	v := readFileString(p.Interval)
	n, err := json.Number(v).Int64()
	if err != nil || n < 5 {
		return fallback
	}
	return int(n)
}
