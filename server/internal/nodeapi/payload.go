package nodeapi

import (
	"fmt"
	"strings"
)

// 请求体校验链：逐行对齐 NodeApiController 的 $request->validate([...])。
//
// 规则顺序与 PHP 完全一致（决定多条错误时的首条文案）；参数上限也照抄
// （push 2000 条、ack 50 条、output 40000 字符）。

const (
	// MaxStatsPerPush 是 push 单次接受的流量用户数上限（与 NodeWsServer::MAX_STATS_PER_REPORT 一致）。
	MaxStatsPerPush = 2000

	// MaxAckResults 是 ack 单次接受的回执条数上限。
	MaxAckResults = 50

	// ackOutputMaxLen 是单条回执输出的字符上限（reality_scan 的结果 JSON 也落在这里）。
	ackOutputMaxLen = 40000
)

// validateRegister 对齐 register() 的校验：
//
//	'xray_version'  => nullable|string|max:64
//	'agent_version' => nullable|string|max:64
//	'kernel_arch'   => nullable|string|max:32
//	'hostname'      => nullable|string|max:128
func validateRegister(data any) error {
	v := newValidator(data)
	v.field("xray_version", Nullable(), String(), Max(64))
	v.field("agent_version", Nullable(), String(), Max(64))
	v.field("kernel_arch", Nullable(), String(), Max(32))
	v.field("hostname", Nullable(), String(), Max(128))
	return collect(v)
}

// validatePush 对齐 push() 的校验：
//
//	'stats'         => required|array|max:2000
//	'stats.*.up'    => nullable|integer|min:0
//	'stats.*.down'  => nullable|integer|min:0
func validatePush(data any) error {
	v := newValidator(data)
	v.field("stats", Required(), ArrayRule(), Max(MaxStatsPerPush))
	v.field("stats.*.up", Nullable(), Integer(), Min(0))
	v.field("stats.*.down", Nullable(), Integer(), Min(0))
	return collect(v)
}

// validateAlive 对齐 alive() 的校验（字段顺序即报错优先级）。
func validateAlive(data any) error {
	v := newValidator(data)
	v.field("xray_version", Nullable(), String(), Max(64))
	v.field("config_version", Nullable(), Integer(), Min(0))
	v.field("cpu", Nullable(), Numeric(), Min(0), Max(100))
	v.field("mem", Nullable(), Numeric(), Min(0), Max(100))
	v.field("uptime", Nullable(), Integer(), Min(0))
	v.field("conns", Nullable(), Integer(), Min(0))
	v.field("active_users", Nullable(), Integer(), Min(0))
	v.field("up_speed", Nullable(), Numeric(), Min(0))
	v.field("down_speed", Nullable(), Numeric(), Min(0))
	v.field("net_total_up", Nullable(), Integer(), Min(0))
	v.field("net_total_down", Nullable(), Integer(), Min(0))
	// issue #94 R5：BBR 运行态（整块可选，四个字段各自也可选）。
	// 不校验 key 数量：未知键在 BBRStatusPatch 里按白名单丢弃，不因为多传一个键就
	// 让整条心跳 500（心跳失败会连带 status 不刷新）。
	v.field("bbr", Nullable(), ArrayRule())
	v.field("bbr.tcp_congestion_control", Nullable(), String(), Max(64))
	v.field("bbr.default_qdisc", Nullable(), String(), Max(64))
	v.field("bbr.bbr_module", Nullable(), Boolean())
	v.field("bbr.conntrack_max", Nullable(), Integer(), Min(0))
	return collect(v)
}

// validateAck 对齐 ack() 的校验：
//
//	'results'         => required|array|max:50
//	'results.*.id'    => required|integer
//	'results.*.success' => required|boolean
//	'results.*.output'  => nullable|string|max:40000
func validateAck(data any) error {
	v := newValidator(data)
	v.field("results", Required(), ArrayRule(), Max(MaxAckResults))
	v.field("results.*.id", Required(), Integer())
	v.field("results.*.success", Required(), Boolean())
	v.field("results.*.output", Nullable(), String(), Max(ackOutputMaxLen))
	return collect(v)
}

func collect(v *validator) error {
	msgs := v.run()
	if len(msgs) == 0 {
		return nil
	}
	return &ValidationError{Messages: msgs}
}

// PushStats 是 push 的入参（用户 email → 原始累计字节）。
type PushStats struct {
	Email string
	Up    int64
	Down  int64
}

// ParsePushStats 复刻 controller 里对 validated stats 的归一化：
// 只接受「键是字符串、值是数组」的条目，up/down 缺省 0。
func ParsePushStats(ordered any) []PushStats {
	data, _ := mapToPlain(ordered).(map[string]any)
	raw, _ := data["stats"].(map[string]any)
	if raw == nil {
		return nil
	}
	out := make([]PushStats, 0, len(raw))
	for email, pair := range raw {
		p, ok := pair.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, PushStats{
			Email: email,
			Up:    intOf(p["up"]),
			Down:  intOf(p["down"]),
		})
	}
	return out
}

// bbrHeartbeatKeys 是心跳 bbr 对象的四个白名单字段，顺序即落库键序
// （与 agent/sysctl.go 的 bbrStatus 结构一一对应）。
var bbrHeartbeatKeys = []string{"tcp_congestion_control", "default_qdisc", "bbr_module", "conntrack_max"}

// BBRStatusPatch 把心跳里的 bbr 对象转成 driver_config.bbr 的补丁（issue #94 R5）。
//
// ok=false 的三种情况：心跳没带 bbr 键（老 agent）、显式传 null、形状不是对象。
// 这三种都**不覆盖**已有的 bbr —— 「这次没上报」不等于「节点上没有 BBR 状态」，
// 写 null 会把上一次的真实值抹掉，面板上就变成看不出所以然。
//
// 带了对象但某个字段是 null（agent 读不到 /proc/sys：非 Linux、conntrack 模块未加载）
// 时按原样写 null：面板要能区分「没上报」与「上报了关」。未知键在这里丢弃，不落库。
func BBRStatusPatch(data map[string]any) (Object, bool) {
	raw, exists := data["bbr"]
	if !exists || raw == nil {
		return Object{}, false
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return Object{}, false
	}

	out := Object{}
	for _, key := range bbrHeartbeatKeys {
		value := fields[key]
		switch {
		case value == nil:
			out.Set(key, nil)
		case key == "bbr_module":
			out.Set(key, boolOf(value))
		case key == "conntrack_max":
			out.Set(key, intOf(value))
		default:
			if s, isStr := value.(string); isStr {
				out.Set(key, s)
			} else {
				out.Set(key, nil)
			}
		}
	}
	return out, true
}

// ClientsFingerprintPatch 把心跳里的 clients_fp 转成 driver_config.agent.clients_fp 的值
// （issue #115 3.5：面板据它短路「名单没变」时的 client_list 全量读回）。
//
// 返回 nil 的三种情况：心跳没带该键（老 agent）、显式传 null、形状不对（缺 sum 或 sum 为空）。
// 三种都**写 null**：旧 agent 覆盖新 agent（降级）时，必须让上一轮留下的指纹基线失效，
// 否则面板会拿一个没人再更新的旧指纹短路掉对账，节点漂移永远发现不了。
//
// 刻意不放进 validateAlive 的校验链：心跳里多一个形状不对的键就让整条心跳 500，会连带
// status/last_check_at 不刷新（节点看起来掉线）。这里按「不认识就按 null 落库」处理。
func ClientsFingerprintPatch(data map[string]any) any {
	raw, ok := data["clients_fp"].(map[string]any)
	if !ok {
		return nil
	}
	// sum 必须是字符串：md5 十六进制串。数字/布尔一律当「没有指纹」，
	// 不能靠 stringOf 把 123 转成 "123" 当成一份真指纹。
	sum, isStr := raw["sum"].(string)
	sum = strings.TrimSpace(sum)
	if !isStr || sum == "" {
		return nil
	}
	return Object{
		{Key: "n", Value: intOf(raw["n"])},
		{Key: "sum", Value: sum},
	}
}

// AckResult 是一条指令回执（对齐 ack 的 results.*）。
//
// OutputSet 对应 PHP 的 isset($row['output'])：显式传 null 与不传都算「无输出」
// （数据库写 null），显式传空串则写 ""。两者在库里的结果不同，必须区分。
type AckResult struct {
	ID        int64
	Success   bool
	Output    string
	OutputSet bool
}

// ParseAckResults 复刻 controller 对 validated results 的读取。
func ParseAckResults(ordered any) []AckResult {
	data, _ := mapToPlain(ordered).(map[string]any)
	raw, _ := data["results"].([]any)
	out := make([]AckResult, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		res := AckResult{
			ID:      intOf(m["id"]),
			Success: boolOf(m["success"]),
		}
		if rawOut, exists := m["output"]; exists && rawOut != nil {
			if s, ok := rawOut.(string); ok {
				res.Output = s
				res.OutputSet = true
			}
		}
		out = append(out, res)
	}
	return out
}

// parseBody 读取并解析请求体；空体视作空对象（PHP 对空 body 得到空数组）。
//
// 返回值保留 JSON 文档顺序（Object），因为 Laravel 的校验报错顺序依赖数据顺序。
func parseBody(raw []byte) (any, error) {
	if len(raw) == 0 {
		return Object{}, nil
	}
	parsed, err := ParseJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if parsed == nil {
		return Object{}, nil
	}
	if _, ok := parsed.(Object); !ok {
		// PHP 的 $request->validate 对 JSON 数组请求体同样按「无字段」处理。
		return Object{}, nil
	}
	return parsed, nil
}
