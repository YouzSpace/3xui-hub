package main

// client_list（issue #104）：只读回读——把节点上当前生效配置里的入站 + client 列表
// 回给面板做对账。
//
// 数据源刻意用「已落盘的 config.json」而不是内核 API：adu/rmu 热应用之后，面板下一轮
// 全量拉取会把结果写回 config.json，所以它就是节点当前生效的那份配置（内核里跑的
// 正是它）。这里只读文件，不改任何东西，也不重启内核。
//
// 面板侧契约（字段名一字不差，别改名）：
//
//	入参  {}  或  {"node_id":N}
//	回执  {"clients":[{"email":"ch_user_<id>","enable":true,"totalGB":N,"expiryTime":0,
//	                    "inbounds":["vless-443","hy2"]}],
//	        "total":103,"truncated":true,"reason":"..."}
//
// 3.1 起回执扁平化（issue #115）：协议通用后每个入站都注入同一批用户，按「入站 × client」
// 回报等于把同一批人重复 N 遍（体积 = 人数 × 入站数 × ~75 字节），5 个入站 103 人就顶到
// 39000 字符上限被截断，面板只能整轮按 UNKNOWN 跳过 —— 裸内核对账永久失效。所以这里按
// email 去重：同一个人只报一条，它出现在哪些入站收进 inbounds 数组（诊断用；面板对账
// 只认 email）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

const (
	// clientListOutputMax 单条回执 output 的字符上限。
	//
	// 面板 ack 校验是 output <= 40000 字符，超了不是截断而是整条校验失败（同一批
	// ack 里的其它指令回执会一起丢）。所以自己在节点侧收口，留 1000 字符余量。
	clientListOutputMax = 39000
)

// clientListPayload 入参。node_id 只是协议占位：一个 agent 只服务本节点，
// 传什么都不改变行为（也不校验，面板可能会带上它做日志关联）。
type clientListPayload struct {
	NodeID int64 `json:"node_id"`
}

// clientListResult 回执根。字段名即面板 JSON 契约。
type clientListResult struct {
	Clients []clientListClient `json:"clients"`
	// Total = 回执里的条目数（已按 email 去重）。截断时仍是全量数，便于面板看规模。
	Total int `json:"total"`
	// Truncated = 回执被字符上限截断（只回了前若干个 client）。
	// 面板看到它应当本轮按 UNKNOWN 处理，绝不能拿半份数据去删人。
	Truncated bool   `json:"truncated,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// clientListClient 一个 client（同一个人只出现一次，跨入站信息收在 inbounds）。
//
// totalGB / expiryTime 原样透传：内核配置里 expiryTime 既可能是数字 0，也可能是
// "2026-12-01T00:00:00Z" 字符串，面板两种都要认，节点侧不做归一（改了反而对不上）。
type clientListClient struct {
	Email      string `json:"email"`
	Enable     bool   `json:"enable"`
	TotalGB    any    `json:"totalGB"`
	ExpiryTime any    `json:"expiryTime"`
	// Inbounds 是这个 email 出现在哪些入站 tag（同节点内 tag 唯一）。诊断用：
	// 面板对账只看 email，不看这里。
	Inbounds []string `json:"inbounds"`
}

// runClientList 只读回读入站 client 列表。
func runClientList(p Paths, cmd Command) AckResult {
	// 入参允许为空（面板按「空对象 / 带 node_id」两种方式发都行）。
	if rawPayload := bytes.TrimSpace(cmd.Payload); len(rawPayload) > 0 && string(rawPayload) != "null" {
		var payload clientListPayload
		if err := json.Unmarshal(rawPayload, &payload); err != nil {
			return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
		}
	}

	raw, err := os.ReadFile(p.Config)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "读取节点配置失败（可能还没下发过配置）: " + err.Error()}
	}

	res, err := parseClientList(raw)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "节点配置解析失败: " + err.Error()}
	}

	return marshalClientListAck(cmd.ID, res)
}

// parseClientList 从内核配置里抽出「按 email 去重后的 client 列表」。
//
// 用 UseNumber：totalGB / expiryTime 的数字按原文回写（float64 转手会让
// 107374182400 这类值有精度风险，也不该被格式化）。
//
// 去重口径（issue #115）：
//   - 同一 email 跨入站只留一条，tag 收进 inbounds；
//   - totalGB / expiryTime 取第一次出现的值 —— 同一 email 在各入站是同一份全局累计值
//     （与 3x-ui 侧 GetClientStatsByEmail 同口径）；
//   - enable 取「任一处停用即停用」：用户在某个端口上连不上就是有问题，不能因为另一个
//     入站还开着就报启用（与面板旧解析的合并规则一致）；
//   - 没有 email 的 client（中转凭据这类）不参与去重 —— 它们没有身份，合并只会凭空
//     造出一条不存在的记录。
func parseClientList(config []byte) (*clientListResult, error) {
	var doc struct {
		Inbounds []struct {
			Tag      string `json:"tag"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
			Settings struct {
				Clients []map[string]any `json:"clients"`
			} `json:"settings"`
		} `json:"inbounds"`
	}

	dec := json.NewDecoder(bytes.NewReader(config))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}

	res := &clientListResult{Clients: make([]clientListClient, 0, len(doc.Inbounds)*2)}
	pos := make(map[string]int, len(res.Clients))
	for _, ib := range doc.Inbounds {
		for _, c := range ib.Settings.Clients {
			// 内核的 client 没有 enable 字段（停用靠 totalGB/expiryTime 表达），
			// 缺省就是启用；将来配置里真带了 enable 才按它报。
			cl := clientListClient{
				Email:      anyString(c["email"]),
				Enable:     true,
				TotalGB:    json.Number("0"),
				ExpiryTime: json.Number("0"),
				Inbounds:   make([]string, 0, 2),
			}
			if v, ok := c["enable"].(bool); ok {
				cl.Enable = v
			}
			// 缺字段的 client 给 0（与旧回执一致），不能变成 null。
			if v, ok := c["totalGB"]; ok && v != nil {
				cl.TotalGB = v
			}
			if v, ok := c["expiryTime"]; ok && v != nil {
				cl.ExpiryTime = v
			}
			if ib.Tag != "" {
				cl.Inbounds = append(cl.Inbounds, ib.Tag)
			}

			if cl.Email == "" {
				// 没有 email 的 client 各自成条，不参与去重。
				res.Clients = append(res.Clients, cl)
				continue
			}
			if at, dup := pos[cl.Email]; dup {
				prev := &res.Clients[at]
				prev.Enable = prev.Enable && cl.Enable
				if ib.Tag != "" && !slices.Contains(prev.Inbounds, ib.Tag) {
					prev.Inbounds = append(prev.Inbounds, ib.Tag)
				}
				continue
			}
			pos[cl.Email] = len(res.Clients)
			res.Clients = append(res.Clients, cl)
		}
	}
	res.Total = len(res.Clients)
	return res, nil
}

// marshalClientListAck 序列化回执；超字符上限时按「保留前 N 个 client」截断并标记
// truncated。宁可明确告诉面板「这份数据不全」，也不能回一份看着完整、实际缺人的列表
// ——对账会据此删用户。
func marshalClientListAck(id int64, res *clientListResult) AckResult {
	raw, err := json.Marshal(res)
	if err != nil {
		return AckResult{ID: id, Success: false, Output: "结果序列化失败: " + err.Error()}
	}
	if len(raw) <= clientListOutputMax {
		return AckResult{ID: id, Success: true, Output: string(raw)}
	}

	// 二分「最多能装下几个 client」（骨架里的 total / reason 也占字符，一起算进去）。
	lo, hi, best := 0, len(res.Clients), -1
	var bestRaw []byte
	for lo <= hi {
		mid := (lo + hi) / 2
		cand, candErr := json.Marshal(budgetClientList(res, mid))
		if candErr == nil && len(cand) <= clientListOutputMax {
			best, bestRaw = mid, cand
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}

	if best < 0 {
		// 连「一个 client 都不带」的回执骨架都装不下：只回个说明，面板按 UNKNOWN 处理，
		// 别拿空列表当「节点上没有用户」。
		fallback := &clientListResult{
			Clients:   []clientListClient{},
			Total:     res.Total,
			Truncated: true,
			Reason: fmt.Sprintf("回执超过 %d 字符上限（%d 个 client 都装不下），本轮请按 UNKNOWN 处理",
				clientListOutputMax, res.Total),
		}
		b, _ := json.Marshal(fallback)
		return AckResult{ID: id, Success: true, Output: string(b)}
	}

	return AckResult{ID: id, Success: true, Output: string(bestRaw)}
}

// budgetClientList 生成「按顺序只保留前 keep 个 client」的副本，并带上截断说明。
func budgetClientList(res *clientListResult, keep int) *clientListResult {
	clients := make([]clientListClient, 0, keep)
	clients = append(clients, res.Clients[:keep]...)
	return &clientListResult{
		Clients:   clients,
		Total:     res.Total,
		Truncated: true,
		Reason: fmt.Sprintf("回执超过 %d 字符上限：只回了 %d/%d 个 client，本轮请按 UNKNOWN 处理",
			clientListOutputMax, keep, res.Total),
	}
}

// anyString 宽容取字符串字段（缺字段 / 非字符串一律空串）。
func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
