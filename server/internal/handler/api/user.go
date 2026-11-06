package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// handleMe 对齐 UserController::me（逐字段，含类型与时间格式）。
//
// 字段来源：
//   - users.* 直出（整数按 int，布尔按 bool）
//   - plan 三件套（type/name/price…）来自 left join 语义：无套餐时 type/name/months 为 null，
//     plan_include_local 为 true（PHP: $user->plan ? (bool)$plan->includesLocal() : true）
//   - next_traffic_reset_at / expired_at：Carbon toIso8601String() → +00:00 结尾
func (h *Handler) handleMe(c *gin.Context) {
	user := currentUser(c)
	ctx := c.Request.Context()

	var plan *Plan
	if user.PlanID.Valid {
		p, err := h.store.PlanByID(ctx, uint64(user.PlanID.Int64))
		if err != nil && !IsNoRows(err) {
			h.logger.Printf("/api/me 查询套餐失败: %v", err)
		}
		plan = p
	}

	includeLocal := true
	var planType, planName any
	var planMonths any
	var planPrice, planResetPrice float64
	if plan != nil {
		includeLocal = plan.IncludeLocal
		planType = plan.Type
		planName = plan.Name
		if plan.Months.Valid {
			planMonths = plan.Months.Int64
		}
		planPrice = plan.Price
		planResetPrice = plan.ResetPrice
	}

	var planID any
	if user.PlanID.Valid {
		planID = user.PlanID.Int64
	}

	success(c, jo(
		"id", user.ID,
		"email", nullString(user.Email),
		"token", user.Token,
		"protocol", user.Protocol,
		"plan_id", planID,
		"plan_type", planType,
		"plan_name", planName,
		// 套餐是否含本地节点（纯第三方套餐：用户端不显示流量，只显示到期日期）
		"plan_include_local", includeLocal,
		"plan_months", planMonths,
		"plan_price", phpFloatOf(planPrice),
		"plan_reset_price", phpFloatOf(planResetPrice),
		"traffic_limit", user.TrafficLimit,
		"traffic_used", user.TrafficUsed,
		"monthly_traffic_used", user.MonthlyTrafficUsed,
		"monthly_traffic_limit", user.MonthlyTrafficLimit,
		"next_traffic_reset_at", nullableISOTime(user.NextTrafficResetAt),
		"expired_at", nullableISOTime(user.ExpiredAt),
		"enabled", user.Enabled,
	))
}

// handleNodes 对齐 NodeController::index。
//
// 规则（逐条对齐）：
//  0. 无套餐（plan_id 为 NULL/<=0）→ data 恒为 []（与订阅 403「无套餐，暂无流量」同口径）；
//  1. enabled=1 且 status='online'；
//  2. 该用户协议在节点上有入站（3x-ui 看 node_inbounds，xray 看 xray_inbounds.enabled）；
//  3. 纯第三方套餐（include_local=false）→ 本地节点列表整体隐藏；
//  4. port：xray 取该协议第一个启用入站的端口，没有则回退 driver_config.reality.listen_port，
//     再没有回退 443；3x-ui 直出 nodes.port；
//  5. site_configs.show_node_multiplier 命中 '1'/'true'/'on' 才追加倍率两个字段；
//  6. 末尾拼接第三方线路（中性名「地区+序号」，latency=null、status=online）。
func (h *Handler) handleNodes(c *gin.Context) {
	user := currentUser(c)
	ctx := c.Request.Context()

	// 0. 无套餐（plan_id 为 NULL 或 <= 0）→ 空列表，与订阅侧「无套餐，暂无流量」403 同口径。
	// 这里必须提前返回：一旦落到下面就会走 OnlineNodesForProtocol 把全部在线节点吐出去。
	if !user.PlanID.Valid || user.PlanID.Int64 <= 0 {
		success(c, jsArr{})
		return
	}

	// 3. 纯第三方套餐：本地节点整体隐藏（与订阅注入保持一致）。
	if user.PlanID.Valid {
		plan, err := h.store.PlanByID(ctx, uint64(user.PlanID.Int64))
		if err != nil && !IsNoRows(err) {
			h.logger.Printf("/api/nodes 查询套餐失败: %v", err)
		}
		if plan != nil && !plan.IncludeLocal {
			// 与 PHP 一致：纯第三方套餐也走 thirdPartySubIdList()（array_filter 掉 0）。
			success(c, h.thirdPartyItems(ctx, filteredThirdPartyIDs(plan)))
			return
		}
	}

	nodes, err := h.store.OnlineNodesForProtocol(ctx, user.Protocol)
	if err != nil {
		h.logger.Printf("/api/nodes 查询节点失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	configs, cfgErr := h.store.SiteConfigMany(ctx, []string{"show_node_multiplier", "default_node_multiplier"})
	if cfgErr != nil {
		h.logger.Printf("/api/nodes 读取配置失败: %v", cfgErr)
	}
	showMultiplier := isTruthyConfig(configs["show_node_multiplier"], "1", "true", "on")
	originalMultiplier := parseFloatOr(configs["default_node_multiplier"], 1.0)

	items := make(jsArr, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		item := jo(
			"id", n.ID,
			"name", n.Name,
			"host", n.Host,
			"port", nodePortForProtocol(n, user.Protocol),
			"latency", n.Latency,
			"status", n.Status,
		)
		if showMultiplier {
			// 展示值与计费值必须同源：节点有值用节点值，否则用 default_node_multiplier（缺省 1.0）。
			multiplier := originalMultiplier
			source := "default"
			if n.TrafficMultiplier != nil {
				multiplier = *n.TrafficMultiplier
				source = "node"
			}
			item.put("traffic_multiplier", phpFloatOf(multiplier))
			item.put("traffic_multiplier_source", source)
		}
		items = append(items, item)
	}

	// 第三方线路：与订阅注入同源；出错不影响本地节点列表（与 PHP 的 try/catch 同策略）。
	items = append(items, h.thirdPartyItems(ctx, h.userPlanThirdPartyIDs(ctx, user))...)

	success(c, items)
}

// userPlanThirdPartyIDs 取用户套餐勾选的第三方订阅 ID（无套餐 → 空）。
func (h *Handler) userPlanThirdPartyIDs(ctx context.Context, user *User) []int64 {
	if !user.PlanID.Valid {
		return nil
	}
	plan, err := h.store.PlanByID(ctx, uint64(user.PlanID.Int64))
	if err != nil || plan == nil {
		return nil
	}
	return filteredThirdPartyIDs(plan)
}

// filteredThirdPartyIDs 对齐 Plan::thirdPartySubIdList()：array_filter 无回调 → 掉 0/空值。
func filteredThirdPartyIDs(plan *Plan) []int64 {
	if plan == nil {
		return nil
	}
	out := make([]int64, 0, len(plan.ThirdPartyIDs))
	for _, id := range plan.ThirdPartyIDs {
		if id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// thirdPartyItems 对齐 NodeController::thirdPartyNodeItems + ThirdPartyService::linksForUser。
//
// 总开关 third_party_enabled 必须为 '1'（PHP 的严格比较 !== '1'），否则不注入。
// 名称取链接 '#' 后片段（urldecode），已被 PHP 侧改写成中性「地区+序号」；空则「线路{i+1}」。
// 去重按整条链接（PHP 的 isset($seen[$l]) 语义）。
func (h *Handler) thirdPartyItems(ctx context.Context, ids []int64) jsArr {
	out := jsArr{}
	if len(ids) == 0 {
		return out
	}
	configs, err := h.store.SiteConfigMany(ctx, []string{"third_party_enabled"})
	if err != nil {
		h.logger.Printf("/api/nodes 读取第三方开关失败: %v", err)
		return out
	}
	if configs["third_party_enabled"] != "1" {
		return out
	}

	subs, err := h.store.ThirdPartySubsByIDs(ctx, dedupeInt64(ids))
	if err != nil {
		h.logger.Printf("/api/nodes 查询第三方订阅失败: %v", err)
		return out
	}

	seen := make(map[string]struct{})
	i := 0
	for _, sub := range subs {
		for _, link := range splitLinks(sub.CachedLinks) {
			if _, dup := seen[link]; dup {
				continue
			}
			seen[link] = struct{}{}

			name := ""
			if pos := strings.LastIndexByte(link, '#'); pos >= 0 {
				name = phpURLDecode(link[pos+1:])
			}
			if name == "" {
				name = "线路" + itoaInt(i+1)
			}
			out = append(out, jo(
				"id", "tp-"+itoaInt(i), // 虚拟 ID（非面板节点），前端仅用于 key
				"name", name,
				"host", nil,
				"port", nil,
				"latency", nil,
				"status", "online",
			))
			i++
		}
	}
	return out
}

// splitLinks 对齐 ThirdPartySub::getCachedLinks：按行拆、trim、去空行。
func splitLinks(raw string) []string {
	if raw == "" {
		return nil
	}
	lines := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' })
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = phpTrim(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func dedupeInt64(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// nodePortForProtocol 计算 /api/nodes 里的 port：
//
//	xray  → 该协议第一个启用入站的端口 → driver_config.reality.listen_port → 443
//	3x-ui → nodes.port（直出）
func nodePortForProtocol(n *Node, protocol string) int {
	if n.DriverType != "xray" {
		return n.Port
	}
	for _, in := range n.XrayInbounds {
		if in.Enabled && in.Protocol == protocol {
			return in.Port
		}
	}
	// driver_config.reality.listen_port（老数据在 Reality 迁移前才有这个字段）
	if n.DriverConfig != "" {
		var cfg struct {
			Reality struct {
				ListenPort *int `json:"listen_port"`
			} `json:"reality"`
		}
		if err := json.Unmarshal([]byte(n.DriverConfig), &cfg); err == nil &&
			cfg.Reality.ListenPort != nil && *cfg.Reality.ListenPort > 0 {
			return *cfg.Reality.ListenPort
		}
	}
	return 443
}

// handleProtocol 对齐 ProtocolController::switch。
//
//	{"protocol":"vless|trojan"} → {"protocol": "<切换后>"}
//	校验失败 → PHP 形态（HTTP 200 + code 500 + 文案）
//	DomainException → 「不支持的协议」code 400；其它错误 → 「协议切换失败：<msg>」code 500
func (h *Handler) handleProtocol(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(input).field("protocol", "required", "in:vless,trojan")
	if !v.passes() {
		respondValidation(c, v)
		return
	}
	protocol := input["protocol"].(string)
	user := currentUser(c)

	if h.protocolSwitch == nil {
		fail(c, "协议切换失败：未配置节点驱动", 500)
		return
	}
	err := h.protocolSwitch.Switch(c.Request.Context(), user, protocol)
	if err != nil {
		if IsDomainError(err) {
			fail(c, err.Error(), 400)
			return
		}
		h.logger.Printf("protocol switch failed user=%d: %v", user.ID, err)
		fail(c, "协议切换失败："+err.Error(), 500)
		return
	}

	// 切换成功后回读协议（$user->fresh()->protocol）。
	updated, uerr := h.store.UserByID(c.Request.Context(), user.ID)
	if uerr != nil || updated == nil {
		updated = user
	}
	success(c, jo("protocol", updated.Protocol), "切换成功")
}

// ---- 小工具 ----

// nullString 把 sql.NullString 映射成 PHP 的 string|null。
func nullString(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

// nullableISOTime 把可空时间映射成 Carbon::toIso8601String() 或 null。
func nullableISOTime(v sql.NullTime) any {
	if !v.Valid {
		return nil
	}
	return isoTime(v.Time)
}

func isTruthyConfig(raw string, accepted ...string) bool {
	for _, a := range accepted {
		if raw == a {
			return true
		}
	}
	return false
}

// parseFloatOr 模拟 PHP 的 (float)$raw：非法值回落默认。
func parseFloatOr(raw string, def float64) float64 {
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}

// hashPassword 对齐 Hash::make（bcrypt，cost 取配置值）。
func hashPassword(plain string, cost int) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// verifyPassword 对齐 Hash::check。
func verifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
