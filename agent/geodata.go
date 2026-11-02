package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// geo 数据同步（面板 geodata_sync 指令，issue #40）。
//
// payload = {"files":[{"name","size","sha256","url"}...]}：
//   - name 白名单（geoip.dat / geosite.dat），防任意文件写入
//   - url 是面板上的相对路径，用 state.json 里的 hub_url 拼（口径同 upgrade：节点只连面板）
//   - 本地同名文件 sha256 相同 → 跳过（不写盘、不 reload）
//   - 下载到同目录 .new → sha256 校验 → 原子 rename；任何一步失败旧文件原样保留
//   - 有文件实际更新才 restartXray（geo 数据只在 xray 启动时加载，必须重启才生效）
const geodataMaxFiles = 4

// geodataReload 触发内核重载（有文件实际更新时调用；测试可替换）。
var geodataReload = restartXray

var geodataAllowlist = map[string]bool{
	"geoip.dat":   true,
	"geosite.dat": true,
}

// geodataFileSpec 面板下发的单文件描述（字段名即面板侧 JSON 契约）。
type geodataFileSpec struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

func runGeodataSync(p Paths, cmd Command) AckResult {
	var payload struct {
		Files []geodataFileSpec `json:"files"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
	}
	if len(payload.Files) == 0 || len(payload.Files) > geodataMaxFiles {
		return AckResult{ID: cmd.ID, Success: false, Output: "载荷文件列表为空或超限"}
	}

	st, err := loadState(p.State)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "读取 state.json 失败: " + err.Error()}
	}
	base := strings.TrimRight(st.HubURL, "/")
	if base == "" {
		return AckResult{ID: cmd.ID, Success: false, Output: "state.json 缺少 hub_url"}
	}

	updated, skipped := 0, 0
	var fails []string

	for _, f := range payload.Files {
		name := strings.TrimSpace(f.Name)
		if !geodataAllowlist[name] || strings.ContainsAny(name, "/\\") {
			fails = append(fails, name+": 不在白名单")
			continue
		}
		if f.SHA256 == "" || f.URL == "" {
			fails = append(fails, name+": 载荷缺少 sha256/url")
			continue
		}

		target := filepath.Join(p.Dir, name)

		// 幂等：本地已是同一份 → 跳过（不写盘、不触发 reload）
		if sum, err := sha256File(target); err == nil && strings.EqualFold(sum, f.SHA256) {
			skipped++
			continue
		}

		// 临时文件放同目录：rename 才是原子的（跨文件系统 rename 会失败）
		tmp := target + ".new"
		_ = os.Remove(tmp) // 清掉上次失败的残留

		sum, err := downloadFile(base+"/"+strings.TrimLeft(f.URL, "/"), tmp, 300*time.Second)
		if err != nil {
			_ = os.Remove(tmp)
			fails = append(fails, name+": 下载失败 "+err.Error())
			continue
		}
		if !strings.EqualFold(sum, f.SHA256) {
			_ = os.Remove(tmp)
			fails = append(fails, name+": sha256 校验失败（已放弃替换，保留旧文件）")
			continue
		}
		if err := os.Rename(tmp, target); err != nil {
			_ = os.Remove(tmp)
			fails = append(fails, name+": 替换失败 "+err.Error())
			continue
		}
		updated++
	}

	// 有文件实际更新才重启内核：geo 数据只在启动时加载；无变化不打扰存量服务
	if updated > 0 {
		geodataReload(p)
	}

	out := fmt.Sprintf("geo 同步：更新 %d，跳过 %d", updated, skipped)
	if len(fails) > 0 {
		out += "；失败 " + strings.Join(fails, " / ")
	}

	return AckResult{ID: cmd.ID, Success: len(fails) == 0, Output: out}
}
