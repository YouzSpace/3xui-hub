package admin

// 系统调优的节点侧联动（issue #94 R4：sysctl_apply）。
//
// 面板不自己改内核参数：只把**白名单载荷**入队成一条 node_api_commands，
// 再同步等回执 —— 与端口跳跃 / reality_scan 同一条指令通道，不另造队列。
//
//	POST /nodes/:node/sysctl  {profile?, keys?}  →  sysctl_apply
//
// 边界（与 agent/sysctl.go 对齐）：
//   - 只写 /etc/sysctl.d/99-controlhub-tuning.conf，agent 侧固定，面板不传路径；
//   - key 必须白名单，profile 只有预置的几个；
//   - 回执里的 applied 是 agent 读 /proc/sys 得到的**实际生效值**，原样透给前端。

import (
	"sort"
	"strconv"
	"strings"

	"controlhub-server/internal/nodeapi"

	"github.com/gin-gonic/gin"
)

const (
	// minSysctlAgentVersion 是 sysctl_apply 所需的最低 agent 版本。
	// 与端口跳跃同一批加进 agent（issue #94 R1-R5 一起实现），门槛相同。
	minSysctlAgentVersion = "1.0.8"

	// sysctlMaxKeys 与 agent 侧 sysctlMaxKeys 对齐：单次下发最多 16 个 key。
	sysctlMaxKeys = 16
)

// sysctlProfileKeys 是面板认识的预置 profile → 它包含的 key
// （与 agent/sysctl.go 的 sysctlProfileBodies 一一对应）。
var sysctlProfileKeys = map[string][]string{
	"bbr": {
		"net.core.default_qdisc",
		"net.ipv4.tcp_congestion_control",
	},
	"conntrack": {
		"net.netfilter.nf_conntrack_max",
	},
}

// sysctlKeyWhitelist 是允许下发的 key（与 agent/sysctl.go 的 sysctlWhitelist 逐条对应）。
//
// 面板先挡一道：写错的 key 在 agent 侧会直接失败，没必要等一次节点往返才报错。
// **值**的合法区间不在这里校验（依赖内核实际支持：拥塞算法是否编进内核、qdisc 是否可加载）,
// 由 agent 的 modprobe + sysctl -p + 读回 /proc/sys 判定，失败原文透给前端。
var sysctlKeyWhitelist = map[string]bool{
	"net.ipv4.tcp_congestion_control":                    true,
	"net.core.default_qdisc":                             true,
	"net.core.rmem_max":                                  true,
	"net.core.wmem_max":                                  true,
	"net.ipv4.tcp_rmem":                                  true,
	"net.ipv4.tcp_wmem":                                  true,
	"net.ipv4.tcp_fastopen":                              true,
	"net.core.somaxconn":                                 true,
	"net.core.netdev_max_backlog":                        true,
	"net.ipv4.ip_local_port_range":                       true,
	"net.netfilter.nf_conntrack_max":                     true,
	"net.netfilter.nf_conntrack_tcp_timeout_established": true,
}

// sysctlApply 转发 sysctl_apply：把系统调优参数写进节点专属 conf 并立即生效。
//
// 参数 {profile?, keys?} 至少一项：只传 profile 走预置（「一键开启 BBR」就是这个），
// 传 keys 走显式键值（两项合并时 keys 覆盖 profile 的同名值，与 agent 一致）。
func (d *NodeDomain) sysctlApply(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	if !d.requireAgentAction(c, node, minSysctlAgentVersion, "系统调优（BBR）") {
		return
	}

	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("profile", "nullable", "string", "max:32")
	v.field("keys", "nullable", "array", "max:16")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	profile := strings.ToLower(strings.TrimSpace(nodeStringField(input, "profile")))
	if profile != "" {
		if _, known := sysctlProfileKeys[profile]; !known {
			d.abort422(c, "未知 profile: "+profile+"（只支持 "+strings.Join(sysctlProfileNames(), " / ")+"）")
			return
		}
	}

	keys, badKey := sysctlKeysFrom(input["keys"])
	if badKey != "" {
		d.abort422(c, badKey)
		return
	}
	if profile == "" && len(keys) == 0 {
		d.abort422(c, "profile / keys 至少要有一项：只开 BBR 传 {\"profile\":\"bbr\"}")
		return
	}

	payload := jo()
	if profile != "" {
		payload.Put("profile", profile)
	}
	if len(keys) > 0 {
		// 排序输出：同一份输入恒等（便于复现与对拍），不改变 agent 侧的语义。
		names := make([]string, 0, len(keys))
		for name := range keys {
			names = append(names, name)
		}
		sort.Strings(names)
		kv := jo()
		for _, name := range names {
			kv.Put(name, keys[name])
		}
		payload.Put("keys", kv)
	}

	d.runNodeCommand(c, node, nodeapi.ActionSysctlApply, payload, d.cfg.SysctlWait)
}

// sysctlKeysFrom 校验并取出显式 keys：键必须白名单、值必须是非空字符串。
//
// 返回 badKey 非空时是给用户看的原文（已带具体是哪个 key 出错）。
// 值不做区间校验 —— 见 sysctlKeyWhitelist 的注释。
func sysctlKeysFrom(raw any) (map[string]string, string) {
	if raw == nil {
		return nil, ""
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		// 校验器已按 array 拦过非数组；能进这里的只有 JSON 数组（[..]），
		// 那种形状 agent 解不出来（它要 map[string]string）。
		return nil, "keys 需要是「键 → 值」对象，例如 {\"net.core.default_qdisc\":\"fq\"}"
	}
	if len(fields) > sysctlMaxKeys {
		// 路由层已用 max:16 挡过（Laravel 文案），这里只是兜底：直接调用本函数时
		// 也不能把超量 key 拼进指令。
		return nil, "keys 超过上限 " + strconv.Itoa(sysctlMaxKeys) + " 项"
	}

	out := make(map[string]string, len(fields))
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		key := strings.TrimSpace(name)
		if !sysctlKeyWhitelist[key] {
			return nil, "key 不在白名单: " + key
		}
		// jsonInput 已按 Laravel 的 TrimStrings + ConvertEmptyStringsToNull 整理过：
		// 空白值到这里是 nil，非字符串（数字/布尔）仍是原生类型。
		if fields[name] == nil {
			return nil, key + " 的值为空"
		}
		value, isStr := fields[name].(string)
		if !isStr {
			return nil, key + " 的值需要是字符串（例如 \"fq\" / \"262144\"）"
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, key + " 的值为空"
		}
		out[key] = value
	}
	return out, ""
}

// sysctlProfileNames 列出预置 profile（排序后拼错误文案用）。
func sysctlProfileNames() []string {
	names := make([]string, 0, len(sysctlProfileKeys))
	for name := range sysctlProfileKeys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
