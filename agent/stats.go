package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// StatsPair 一组上下行绝对值（对齐面板 /push 协议字段）。
type StatsPair struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// apiPort xray API 端口（env XRAY_API_PORT 覆盖，默认 10085）。
func apiPort() int {
	return envInt("XRAY_API_PORT", 10085)
}

func apiServerFlag() string {
	return "--server=127.0.0.1:" + strconv.Itoa(apiPort())
}

// queryStats 执行 statsquery 并按 scope 聚合（user→email；outbound→tag）。
func queryStats(p Paths, scope string) (map[string]StatsPair, error) {
	cmd := exec.Command(p.XrayBin, "api", "statsquery", apiServerFlag(), "-pattern", scope)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("statsquery(%s) 失败: %w", scope, err)
	}
	return parseStats(out, scope)
}

// parseStats 解析 statsquery JSON（v26 输出：{"stat":[{"name":"user>>>e>>>traffic>>>uplink","value":"123"}]}）。
func parseStats(raw []byte, scope string) (map[string]StatsPair, error) {
	var envelope struct {
		Stat  []statEntry `json:"stat"`
		Stats []statEntry `json:"stats"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("statsquery 输出解析失败: %w", err)
	}
	items := envelope.Stat
	if len(items) == 0 {
		items = envelope.Stats
	}

	result := map[string]StatsPair{}
	for _, s := range items {
		parts := strings.Split(s.Name, ">>>")
		if len(parts) < 4 || parts[0] != scope || parts[2] != "traffic" {
			continue
		}
		key := parts[1]
		v := jnum(s.Value)
		row := result[key]
		switch parts[3] {
		case "uplink":
			row.Up = v
		case "downlink":
			row.Down = v
		}
		result[key] = row
	}
	return result, nil
}

type statEntry struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

// hashStats 流量表内容指纹（HTTP 降级模式"有变化才推"的去重依据）。
func hashStats(stats map[string]StatsPair) string {
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%d:%d\n", k, stats[k].Up, stats[k].Down)
	}
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// changedStats 只挑出与上轮已推送内容不同的条目（WS 主通道"有变化才带"的依据）。
//
// prev 为 nil（首连 / 刚重连）时返回全量：重连后 hub 不知道断线期间的变化，
// 必须把当前绝对值整份重推一次，让 hub 的快照重新对齐。
//
// 推的仍然是绝对值而不是增量 —— 幂等性不变，重复推无害；没变化的用户不推，
// hub 侧其快照本来就是对的，算出来的 delta 自然为 0。
func changedStats(prev, cur map[string]StatsPair) map[string]StatsPair {
	if prev == nil {
		return cur
	}

	out := make(map[string]StatsPair)
	for key, pair := range cur {
		if last, ok := prev[key]; !ok || last != pair {
			out[key] = pair
		}
	}

	return out
}

// maxStatsPerPush 单条上报允许携带的流量用户数上限。
//
// 与面板 nodeapi.MaxStatsPerPush / ws.MAX_STATS_PER_REPORT（均为 2000）对齐：
// 面板侧超限的处理是 HTTP 422 / WS 整包丢弃，也就是说超过这条线的一整批用户
// 完全不落账。所以节点侧必须在发送前自己切片，不能指望面板放宽。
const maxStatsPerPush = 2000

// chunkStats 把流量表按 email 字典序切成每片 size 条（size<=0 时用 maxStatsPerPush）。
//
// 排序只为让分片成员稳定：推的是绝对值不是增量，同一片重复推幂等；稳定的切片
// 边界也让面板侧日志/对账可复现。空表返回 nil —— 调用方据此不发 stats 键。
func chunkStats(stats map[string]StatsPair, size int) []map[string]StatsPair {
	if size <= 0 {
		size = maxStatsPerPush
	}
	if len(stats) == 0 {
		return nil
	}

	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	chunks := make([]map[string]StatsPair, 0, (len(keys)+size-1)/size)
	for start := 0; start < len(keys); start += size {
		end := start + size
		if end > len(keys) {
			end = len(keys)
		}
		chunk := make(map[string]StatsPair, end-start)
		for _, k := range keys[start:end] {
			chunk[k] = stats[k]
		}
		chunks = append(chunks, chunk)
	}

	return chunks
}

// listenPortsFromConfig 从当前 config.json 提取入站监听端口集合（连接数统计口径）。
func listenPortsFromConfig(p Paths) map[int]bool {
	ports := map[int]bool{}
	b, err := os.ReadFile(p.Config)
	if err != nil {
		return ports
	}
	var cfg struct {
		Inbounds []struct {
			Port   any    `json:"port"`
			Listen string `json:"listen"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ports
	}
	for _, in := range cfg.Inbounds {
		switch v := in.Port.(type) {
		case float64:
			ports[int(v)] = true
		case string:
			// 端口范围 "1000-2000" 或单端口
			if idx := strings.Index(v, "-"); idx > 0 {
				lo, err1 := strconv.Atoi(v[:idx])
				hi, err2 := strconv.Atoi(v[idx+1:])
				if err1 == nil && err2 == nil && hi >= lo && hi-lo < 1000 {
					for i := lo; i <= hi; i++ {
						ports[i] = true
					}
				}
			} else if n, err := strconv.Atoi(v); err == nil {
				ports[n] = true
			}
		}
	}
	return ports
}
