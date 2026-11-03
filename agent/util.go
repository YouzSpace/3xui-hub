// ControlHub xray 节点 agent（Go 版）
//
// 与面板通信的双通道设计：
//   - 主通道：WebSocket 长连接（/node-ws）——面板顺着连接主动推指令、node 实时上报
//   - 兜底通道：HTTP（/api/node-api/*）——WS 断线期间业务不断，自动降级
//
// 职责（全部出连，节点零入站端口；GitHub 零依赖）：
//   1. 注册（只负责"连上"：上报版本/主机信息，不再生成业务密钥）
//   2. 按周期拉全量 config（ETag 304），xray -test 校验后原子替换、重启内核
//   3. 每 5s 读内核统计（statsquery）推流量 + 监控指标（CPU/内存/连接数/速率）
//   4. 接受面板推送的 adu/rmu 指令，本地执行后回执
//   5. 看护 xray 进程（挂了拉起）；失败绝不半写配置
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// agentVersion 由编译脚本 -ldflags 注入。
var agentVersion = "dev"

var (
	selfSHAOnce  sync.Once
	selfSHACache string
)

// binarySHA 返回当前 agent 二进制自身的 sha256（心跳/注册时上报）。
// 面板拿它和 manifest 里的 sha 比对：同版本号但二进制换过时也能发现并自动升级。
func binarySHA(p Paths) string {
	selfSHAOnce.Do(func() {
		if sum, err := sha256File(p.AgentBin); err == nil {
			selfSHACache = sum
		}
	})
	return selfSHACache
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// jnum 宽容解析 JSON 数字（xray API 的 protobuf JSON 会把值编码成字符串）。
func jnum(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return 0
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(v)
	}
	return 0
}

// jnumFloat 宽容解析 JSON 浮点。
func jnumFloat(raw json.RawMessage) float64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return 0
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	return 0
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func readFileString(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmpwrite"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// truncate 输出摘录（回执用，同 bash 版 1500 上限）。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
