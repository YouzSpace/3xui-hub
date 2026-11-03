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
//	入参  {}  或  {"node_id":N}  或  {"cursor":N,"limit":200,"brief":1}  或  {... ,"bucket_id":K}
//	回执  {"clients":[{"email":"ch_user_<id>","enable":true,"totalGB":N,"expiryTime":0,
//	                    "inbounds":["vless-443","hy2"]}],
//	        "total":103,"next":200,"truncated":true,"reason":"...",
//	        "clients_fp":{"n":103,"sum":"<md5>"}}
//
// bucket_id（issue #128 B）= 只回该桶的 client（桶 = fnv32a(email) % 32），cursor/limit
// 在过滤后的列表上分页；省略/null/越界 = 全量（老面板行为不变）。clients_fp 在心跳里
// 还带 32 个桶 hash（buckets），回执里只带 n/sum（每页背 32 个 hash 白占约 1.1KB）。
//
// brief 模式下每人不再回 inbounds（面板对账只认 email/enable/totalGB/expiryTime），
// 省 30-40 字节/人，回执离 39000 字符上限更远。clients_fp 是整份配置的名单指纹
// （与心跳同口径），面板拿它确认分页各页来自同一份配置 —— agent 每条指令都重新读
// config.json，两页之间配置若被重拉更新，两页拼起来的集合就是两个版本的混合。
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
	"strings"
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
//
// cursor / limit 是分页参数（issue #127）：面板按页拉取，绕开单条回执 39000
// 字符上限。limit<=0 时回全量（老面板行为不变）；limit>0 时只回 [cursor, cursor+limit)
// 这一段，并用 next 游标告诉面板「下一页从哪开始」。
type clientListPayload struct {
	NodeID int64 `json:"node_id"`
	Cursor int64 `json:"cursor,omitempty"`
	Limit  int64 `json:"limit,omitempty"`
	// Brief = 只要对账字段，不要 inbounds 诊断信息（面板对账只读
	// email/enable/totalGB/expiryTime）。省掉 inbounds 每人约 30-40 字节。
	//
	// 用 flexBool 而不是 bool：面板发的是 brief:1（数字），严格 bool 会让整条载荷
	// 解析失败 → 整轮对账失败。flexBool 同时认 1 / true / "1"。
	Brief flexBool `json:"brief,omitempty"`
	// BucketID = 分桶拉取（issue #128 B）：只回 fnv32a(email) % 32 == BucketID 的 client，
	// cursor/limit 在过滤后的列表上分页。
	//
	// ⚠️ 必须是指针：省略（老面板）/ null / 负数 / >= 32 一律按全量。用值类型的话
	// 「老面板不发这个键」会被解成 0 = 第 0 桶，于是 agent 只回 1/32 的人 —— 老面板
	// 会把其余 31/32 全判成「节点上缺人」，每轮 bump 一次，内核反复重启。
	BucketID *int `json:"bucket_id,omitempty"`
}

// flexBool 是宽容布尔：JSON 里 true / false / 1 / 0 / "1" / "0" / "true" 都认。
// 指令载荷由面板拼装，不同版本可能给数字或字符串；这里的容错是为了「认得出版本差异」，
// 而不是让一条无害的字段把整轮读回废掉。
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	s := strings.ToLower(strings.Trim(strings.TrimSpace(string(raw)), `"`))
	*b = s == "true" || s == "1"
	return nil
}

// clientListResult 回执根。字段名即面板 JSON 契约。
type clientListResult struct {
	Clients []clientListClient `json:"clients"`
	// Total = 全量条目数（已按 email 去重，与分页无关）。便于面板看规模。
	Total int `json:"total"`
	// Next = 下一页的 cursor 起点；0 / 省略 = 没有下一页了（分页模式）。
	Next int64 `json:"next,omitempty"`
	// Truncated = 回执被字符上限截断（只回了前若干个 client）。
	// 面板看到它应当本轮按 UNKNOWN 处理，绝不能拿半份数据去删人。
	Truncated bool   `json:"truncated,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// ClientsFP = 整份配置的名单指纹（与心跳 clients_fp 同口径、同结构）。
	// 分页拉取时每一页都带同一个指纹，面板据此确认「各页来自同一份配置」：
	// agent 每条 client_list 指令都重新读 config.json，两页之间配置若被重拉更新，
	// 两页拼起来的集合就是两个版本的混合，拿去对账会误删人。
	ClientsFP *clientsFingerprintBody `json:"clients_fp,omitempty"`
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
	// 面板对账只看 email，不看这里。brief 模式下不收集 + omitempty 不出键。
	Inbounds []string `json:"inbounds,omitempty"`
}

// runClientList 只读回读入站 client 列表。
func runClientList(p Paths, cmd Command) AckResult {
	var payload clientListPayload
	// 入参允许为空（面板按「空对象 / 带 node_id」两种方式发都行）。
	if rawPayload := bytes.TrimSpace(cmd.Payload); len(rawPayload) > 0 && string(rawPayload) != "null" {
		if err := json.Unmarshal(rawPayload, &payload); err != nil {
			return AckResult{ID: cmd.ID, Success: false, Output: "载荷解析失败: " + err.Error()}
		}
	}

	raw, err := os.ReadFile(p.Config)
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "读取节点配置失败（可能还没下发过配置）: " + err.Error()}
	}

	res, err := parseClientListMode(raw, bool(payload.Brief))
	if err != nil {
		return AckResult{ID: cmd.ID, Success: false, Output: "节点配置解析失败: " + err.Error()}
	}

	// clients_fp：整份配置的名单指纹，分页各页相同 → 面板据此确认「两页来自同一份配置」。
	// 复用心跳同一个 clientsFingerprint（口径一字不差，避免出现两套算法）；
	// 它内部再解析一遍配置，代价是一次 JSON 解析（每页一次，对账 5 分钟一轮，可忽略）。
	//
	// ⚠️ 必须在按桶过滤**之前**算：分桶拉取时面板仍要靠整份指纹做跨页/跨桶一致性校验
	// （只报「这个桶」的指纹就查不出「两页来自两个版本的配置」）。
	if fp := clientsFingerprint(raw); fp != nil {
		// 回执只带 n/sum：32 个桶 hash 是**心跳**用的（面板逐桶比对靠心跳），分页/分桶
		// 的跨页一致性校验只需要整份 sum。每页都背 32 个 hash 会白占约 1.1KB 回执空间，
		// 离 39000 上限更近，没有收益。
		fp.Buckets = nil
		res.ClientsFP = fp
	}

	// 分桶过滤（issue #128 B）：bucket_id 合法才过滤；过滤后仍走同一套分页/截断兜底
	// （超大桶 39000 字符装不下时，分页模式按 next 续读、全量模式截断标 truncated）。
	if b := payload.BucketID; b != nil && *b >= 0 && *b < clientBucketCount {
		filterClientListBucket(res, *b)
	}

	return marshalClientListAck(cmd.ID, res, payload.Cursor, payload.Limit)
}

// filterClientListBucket 只保留 fnv32a(email) % 32 == bucket 的 client（issue #128 B）。
//
// Total 同步改成过滤后的条数（面板只读 clients/next，total 只是规模提示）。
// 没有 email 的 client 不进任何桶（分桶是按 email 定义的），按桶拉取时一并略过。
func filterClientListBucket(res *clientListResult, bucket int) {
	kept := res.Clients[:0]
	for _, c := range res.Clients {
		if c.Email != "" && clientBucketID(c.Email) == bucket {
			kept = append(kept, c)
		}
	}
	res.Clients = kept
	res.Total = len(kept)
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
	return parseClientListMode(config, false)
}

// parseClientListMode 是 parseClientList 的实现；brief=true 时不收集 inbounds（保持空，
// 序列化时因 omitempty 直接不出键）。面板对账只读 email/enable/totalGB/expiryTime，
// tag 纯粹是诊断信息 —— 砍掉每人省 30-40 字节，回执离 39000 上限更远。
//
// 指纹口径不受影响：clientsFingerprint 本来就不看 tag，两种模式的名单完全一致。
func parseClientListMode(config []byte, brief bool) (*clientListResult, error) {
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
			if ib.Tag != "" && !brief {
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
				if ib.Tag != "" && !brief && !slices.Contains(prev.Inbounds, ib.Tag) {
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

// marshalClientListAck 序列化回执。
//
// 分页模式（limit>0）：只回 [cursor, cursor+limit) 这一段，用 next 游标指下一页。
// 单页若超 39000 字符，二分缩小本页（next 仍指向真实位置）——分页模式下绝不会截断丢人。
//
// 全量模式（limit<=0，老面板行为不变）：整份序列化，超了就二分截断 + truncated。
// 宁可明确告诉面板「这份数据不全」，也不能回一份看着完整、实际缺人的列表 ——对账会据此删用户。
func marshalClientListAck(id int64, res *clientListResult, cursor, limit int64) AckResult {
	if limit > 0 {
		start := cursor
		if start < 0 {
			start = 0
		}
		if start > int64(len(res.Clients)) {
			start = int64(len(res.Clients))
		}
		end := start + limit
		if end > int64(len(res.Clients)) {
			end = int64(len(res.Clients))
		}
		if raw, ok := marshalClientPage(res, start, end); ok {
			return AckResult{ID: id, Success: true, Output: raw}
		}
		// 单页超上限：二分出最多能装下的条数，next 仍指向真实位置（下一页接着拉）。
		lo, hi, best := start, end-1, start-1
		var bestRaw string
		for lo <= hi {
			mid := (lo + hi) / 2
			if raw, ok := marshalClientPage(res, start, mid+1); ok {
				best, bestRaw = mid+1, raw
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		if best >= start {
			return AckResult{ID: id, Success: true, Output: bestRaw}
		}
		// 连一个 client 都装不下（单条超长，几乎不可能）：回空页 + truncated。
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

	// 全量模式：老行为。
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

// marshalClientPage 序列化 [start, end) 这一页 client，带上 next 游标。
// 返回 (raw, ok)：ok=false 表示这页超过 39000 字符上限，调用方应缩小范围。
//
// clients_fp 原样带给每一页（面板靠它做跨页一致性校验），所以指纹要跟着 res 走。
func marshalClientPage(res *clientListResult, start, end int64) (string, bool) {
	page := &clientListResult{
		Clients:   res.Clients[start:end],
		Total:     res.Total,
		ClientsFP: res.ClientsFP,
	}
	if end < int64(len(res.Clients)) {
		page.Next = end
	}
	raw, err := json.Marshal(page)
	if err != nil || len(raw) > clientListOutputMax {
		return "", false
	}
	return string(raw), true
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
