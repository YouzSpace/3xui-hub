package threexui

import (
	"context"
	"math"
	"net/http"
	"strconv"

	"controlhub-server/internal/driver/internal/panel"
	"controlhub-server/internal/driver/internal/phpjson"
)

// 面板 API 方法：逐条对齐 ThreeXUiClient.php 的公开方法。
//
// 返回值口径（与 PHP 一致，调用方按此处理）：
//   - 会「可能不存在」的查询（GetClient/GetClientTraffic）→ (nil, nil)
//   - GetClientLinks：client 不存在 → 空切片（合法状态），其它错误上抛
//   - 其它失败 → error

// ── Clients ─────────────────────────────────────────────────

// ListClients 对应 listClients()：GET /panel/api/clients/list → 全部 client 数组。
func (c *Client) ListClients(ctx context.Context) ([]map[string]any, error) {
	obj, err := c.request(ctx, http.MethodGet, EPClientsList, nil, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	return asObjectList(obj), nil
}

// GetClient 对应 getClient()：client 不存在或请求失败时返回 (nil, nil)。
//
// 回执把 client 对象包在 client 键里（实测：{"client":{"id":131,"uuid":"…"}}），
// 个别版本直接返回 client 对象。统一剥成 client 对象：调用方若把 id
// （clients 表自增行号）当成 uuid 用，会让对端覆盖真 uuid、导致凭据失效。
func (c *Client) GetClient(ctx context.Context, email string) (map[string]any, error) {
	obj, err := c.requestNullable(ctx, http.MethodGet, EPClientsGet+escapeEmail(email), nil, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	cli, ok := obj.(map[string]any)
	if !ok {
		return nil, nil
	}
	if inner, ok := cli["client"].(map[string]any); ok {
		return inner, nil
	}
	return cli, nil
}

// GetClientStrict 与 GetClient 读同一条 GET，但**不吞业务错误**，把三种含义分开：
//   - (client, nil)              节点上确实有这条 client；
//   - (nil, ClientNotFoundError)  节点明确说没有（200 + record not found / data:null）；
//   - (nil, err)                  传输失败或其它业务失败 = **未知**。
//
// 为什么必须有这条：GetClient 走 requestNullable，按 PHP 口径把「真没有」和
// 「面板回了 success=false 的其它业务错（登录态被顶掉、被限流、SQL 异常…）」一起
// 折叠成 (nil, nil)。对账语义里这两件事相反：前者可以判定「等同关闭 / 确实缺人」，
// 后者只能算未知 —— 拿未知去清标记或补注册，就是二次事故。
func (c *Client) GetClientStrict(ctx context.Context, email string) (map[string]any, error) {
	obj, err := c.request(ctx, http.MethodGet, EPClientsGet+escapeEmail(email), nil, c.apiTimeout)
	if err != nil {
		if IsClientNotFound(err) {
			return nil, asNotFound(err)
		}
		return nil, err
	}
	cli, ok := obj.(map[string]any)
	if !ok {
		// success:true 但没有对象（data:null）→ 面板上没有这条 client。
		return nil, &ClientNotFoundError{Message: "client " + email + " not found"}
	}
	if inner, ok := cli["client"].(map[string]any); ok {
		return inner, nil
	}
	return cli, nil
}

// AddClient 对应 addClient()：POST {client, inboundIds}；uuid/password 由面板生成。
//
// 部分 3x-ui 版本成功建 client 但不回传 obj（返回 null）。此时回查一次确认 client
// 是否真的建上，避免上层误判「创建失败」而回滚、残留孤儿 client。
func (c *Client) AddClient(ctx context.Context, client map[string]any, inboundIDs []int64) (map[string]any, error) {
	body, err := phpjson.EncodeRequest(map[string]any{
		"client":     client,
		"inboundIds": int64Slice(inboundIDs),
	})
	if err != nil {
		return nil, err
	}
	obj, err := c.request(ctx, http.MethodPost, EPClientsAdd, body, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	// 与 PHP 一致：addClient 不解包（getClient 才剥 client 键）。
	if created, ok := obj.(map[string]any); ok {
		return created, nil
	}

	email, _ := client["email"].(string)
	if email == "" {
		return nil, nil
	}
	return c.GetClient(ctx, email)
}

// UpdateClient 对应 updateClient()：POST /update/{email}，body = 完整 client（替换非 patch）。
func (c *Client) UpdateClient(ctx context.Context, email string, client map[string]any, inboundID *int64) error {
	body, err := phpjson.EncodeRequest(client)
	if err != nil {
		return err
	}
	path := EPClientsUpdate + escapeEmail(email)
	if inboundID != nil {
		// PHP 用的是复数键名 inboundIds（不是 inboundId），逐字对齐。
		path += "?inboundIds=" + strconv.FormatInt(*inboundID, 10)
	}
	_, err = c.request(ctx, http.MethodPost, path, body, c.apiTimeout)
	return err
}

// DeleteClient 对应 deleteClient()：POST /del/{email}?keepTraffic=0|1，全量删除该 email。
//
// PHP 的 deleteClient 第三个参数 inboundId 被完全忽略（见
// test_delete_client_ignores_unsupported_inbound_id_query），故 Go 侧不暴露。
func (c *Client) DeleteClient(ctx context.Context, email string, keepTraffic bool) error {
	keep := "0"
	if keepTraffic {
		keep = "1"
	}
	path := EPClientsDel + escapeEmail(email) + "?keepTraffic=" + keep
	_, err := c.request(ctx, http.MethodPost, path, nil, c.apiTimeout)
	return err
}

// AttachClient 对应 attachClient()：POST /{email}/attach，body {inboundIds}。
func (c *Client) AttachClient(ctx context.Context, email string, inboundIDs []int64) error {
	body, err := phpjson.EncodeRequest(map[string]any{"inboundIds": int64Slice(inboundIDs)})
	if err != nil {
		return err
	}
	_, err = c.request(ctx, http.MethodPost, EPClientsAttach+escapeEmail(email)+"/attach", body, c.apiTimeout)
	return err
}

// DetachClient 对应 detachClient()：POST /{email}/detach，body {inboundIds}。
func (c *Client) DetachClient(ctx context.Context, email string, inboundIDs []int64) error {
	body, err := phpjson.EncodeRequest(map[string]any{"inboundIds": int64Slice(inboundIDs)})
	if err != nil {
		return err
	}
	_, err = c.request(ctx, http.MethodPost, EPClientsDetach+escapeEmail(email)+"/detach", body, c.apiTimeout)
	return err
}

// ResetClientTraffic 对应 resetClientTraffic()：POST /resetTraffic/{email}。
func (c *Client) ResetClientTraffic(ctx context.Context, email string) error {
	_, err := c.request(ctx, http.MethodPost, EPClientsReset+escapeEmail(email), nil, c.apiTimeout)
	return err
}

// GetClientLinks 对应 getClientLinks()：GET /links/{email} → ["vless://..."]。
//
// 「该 email 在这个面板上不存在」是合法状态（节点重接 / 初始化未建号期间就是缺 client），
// 降级为空数组、不报错 —— 订阅少一个节点链接即可。
// 真正的 API 错误（网络/鉴权/5xx/响应格式非法）仍照原样抛出。
func (c *Client) GetClientLinks(ctx context.Context, email string) ([]string, error) {
	obj, err := c.request(ctx, http.MethodGet, EPClientsLinks+escapeEmail(email), nil, c.apiTimeout)
	if err != nil {
		if IsClientNotFound(err) {
			return []string{}, nil
		}
		return nil, err
	}
	return asStringList(obj), nil
}

// GetClientTraffic 对应 getClientTraffic()：{up,down,total,expiryTime,enable,...}；不存在 → (nil, nil)。
func (c *Client) GetClientTraffic(ctx context.Context, email string) (map[string]any, error) {
	obj, err := c.requestNullable(ctx, http.MethodGet, EPClientsTraffic+escapeEmail(email), nil, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	m, ok := obj.(map[string]any)
	if !ok {
		return nil, nil
	}
	return m, nil
}

// GetOnlineClients 对应 getOnlineClients()：GET /panel/api/clients/onlines。
func (c *Client) GetOnlineClients(ctx context.Context) ([]any, error) {
	obj, err := c.requestNullable(ctx, http.MethodGet, EPClientsOnlines, nil, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	arr, ok := obj.([]any)
	if !ok {
		return []any{}, nil
	}
	return arr, nil
}

// ── Inbounds ────────────────────────────────────────────────

// ListInbounds 对应 listInbounds()：GET /panel/api/inbounds/list → 全量（含 clientStats）。
func (c *Client) ListInbounds(ctx context.Context) ([]map[string]any, error) {
	obj, err := c.request(ctx, http.MethodGet, EPInboundsList, nil, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	return asObjectList(obj), nil
}

// InboundOptions 对应 inboundOptions()：轻量 picker [{id,protocol,port,tag,remark,tlsFlowCapable}]。
func (c *Client) InboundOptions(ctx context.Context) ([]map[string]any, error) {
	obj, err := c.request(ctx, http.MethodGet, EPInboundsOptions, nil, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	return asObjectList(obj), nil
}

// GetInbound 对应 getInbound()：GET /get/{id}；不存在返回 (nil, nil)。
func (c *Client) GetInbound(ctx context.Context, id int64) (map[string]any, error) {
	obj, err := c.requestNullable(ctx, http.MethodGet, EPInboundsGet+strconv.FormatInt(id, 10), nil, c.apiTimeout)
	if err != nil {
		return nil, err
	}
	m, ok := obj.(map[string]any)
	if !ok {
		return nil, nil
	}
	return m, nil
}

// ── 批量流量 ────────────────────────────────────────────────

// GetClientStatsByEmail 对应 getClientStatsByEmail()：一次 listInbounds，按 email 去重。
//
// 去重依据（3x-ui / Xray 官方源码）：
//   - Xray 按 user>>>email 计数，一个 email 只有一个 counter，跨其所有 inbound 聚合；
//   - 3x-ui client_traffics 表 Email 唯一，节点 API 每个 email 只返回一行。
//
// 故同一 email 出现在多个 inbound 下时携带的是【同一份全局累计值】，
// 只取首次出现的值（对齐 3x-ui 的 emailTrafficMap），**绝不跨 inbound 累加**
// （那会让挂 N 个入站的用户被记成 N 倍）。
func (c *Client) GetClientStatsByEmail(ctx context.Context) (map[string]panel.ClientTraffic, error) {
	inbounds, err := c.ListInbounds(ctx)
	if err != nil {
		return nil, err
	}
	return clientStatsFromInbounds(inbounds), nil
}

// GetClientStatsGroupedByInbound 对应 getClientStatsGroupedByInbound()：
// inboundId → email → {up,down}。同一 inbound 内不做跨字段合并。
func (c *Client) GetClientStatsGroupedByInbound(ctx context.Context) (map[int64]map[string]panel.ClientTraffic, error) {
	inbounds, err := c.ListInbounds(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[int64]map[string]panel.ClientTraffic, len(inbounds))
	for _, inbound := range inbounds {
		rawID, hasID := inbound["id"]
		if !hasID || rawID == nil {
			continue
		}
		inboundID := asInt64(rawID)
		stats := map[string]panel.ClientTraffic{}
		for _, stat := range asObjectList(inbound["clientStats"]) {
			email, _ := stat["email"].(string)
			if email == "" {
				continue
			}
			stats[email] = panel.ClientTraffic{
				Up:     asInt64(stat["up"]),
				Down:   asInt64(stat["down"]),
				Enable: enableOr(stat, true),
			}
		}
		result[inboundID] = stats
	}
	return result, nil
}

// clientStatsFromInbounds 是同步/批量两条路径共用的映射（同 email 跨入站只取首次）。
func clientStatsFromInbounds(inbounds []map[string]any) map[string]panel.ClientTraffic {
	result := map[string]panel.ClientTraffic{}
	for _, inbound := range inbounds {
		for _, stat := range asObjectList(inbound["clientStats"]) {
			email, _ := stat["email"].(string)
			if email == "" {
				continue
			}
			if _, seen := result[email]; seen {
				continue // 同 email 跨入站重复，只取首次
			}
			result[email] = panel.ClientTraffic{
				Up:   asInt64(stat["up"]),
				Down: asInt64(stat["down"]),
				// 缺字段按 true（老版本 3x-ui 不返回时不能误判成关闭）——
				// 同步流量时顺带做漂移检测（零额外请求）。
				Enable: enableOr(stat, true),
			}
		}
	}
	return result
}

// enableOr 复刻 `(bool) ($stat['enable'] ?? $default)`。
func enableOr(stat map[string]any, def bool) bool {
	v, ok := stat["enable"]
	if !ok || v == nil {
		return def
	}
	return truthy(v)
}

// ── Server ──────────────────────────────────────────────────

// HealthCheck 对应 healthCheck()：GET /panel/api/server/status → 归一化结果。
// 请求异常不抛错（返回 OK=false + Error），但 ctx 取消仍会上抛（Go 侧语义）。
func (c *Client) HealthCheck(ctx context.Context) (panel.Health, error) {
	start := c.now()
	raw, err := c.send(ctx, http.MethodGet, EPServerStatus, nil, c.healthTimeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return unhealthy(0, cerr.Error()), nil
		}
		return unhealthy(0, err.Error()), nil
	}

	latency := c.now().Sub(start).Milliseconds()
	env, ok := parseEnvelope(raw)
	if !ok || !env.Success {
		msg := "unhealthy"
		if ok && env.Msg != "" {
			msg = env.Msg
		}
		return unhealthy(latency, msg), nil
	}

	obj, _ := env.Obj.(map[string]any)
	// netTraffic 结构异常（不是对象）时整段当没有，绝不因为一个附加字段把 ok=true 的探测搞崩
	netTraffic, _ := obj["netTraffic"].(map[string]any)

	return panel.Health{
		OK:        true,
		LatencyMs: latency,
		CPU:       obj["cpu"],
		Mem:       memPercent(obj["mem"]),
		XrayState: xrayState(obj["xray"]),
		NetSent:   netValue(netTraffic["sent"]),
		NetRecv:   netValue(netTraffic["recv"]),
	}, nil
}

// unhealthy 对应 unhealthy()：失败分支与成功分支同构，键在、值为 nil
// （不写 0，采样层据此跳过）。
func unhealthy(latencyMs int64, msg string) panel.Health {
	return panel.Health{OK: false, LatencyMs: latencyMs, Error: msg}
}

// netValue 对应 netValue()：只认**非负数值**，其余一律 nil。
//
// 面板不同版本可能给数字、数字字符串，或干脆不给（旧面板没有 netTraffic）。
// 负数不是合法的累计字节（计数器不可能为负），当无效值丢弃。
func netValue(v any) *int64 {
	if !isNumeric(v) {
		return nil
	}
	n := asInt64(v)
	if n < 0 {
		return nil
	}
	return &n
}

// memPercent 对应 memPercent()：3x-ui v2.8.11 起 mem 是 {current,total} 对象。
//
//   - 对象且 total > 0 → current/total*100，保留 1 位小数
//   - 数字/数字字符串 → 原样转 float
//   - 缺字段 / total<=0 / 非数值 → nil
func memPercent(v any) *float64 {
	if obj, ok := v.(map[string]any); ok {
		current, hasCurrent := obj["current"]
		total, hasTotal := obj["total"]
		if hasCurrent && hasTotal && isNumeric(current) && isNumeric(total) {
			t := asFloat64(total)
			if t > 0 {
				pct := round1(asFloat64(current) / t * 100)
				return &pct
			}
		}
		return nil
	}
	if isNumeric(v) {
		f := asFloat64(v)
		return &f
	}
	return nil
}

// xrayState 对应 `$obj['xray']['state'] ?? null`。
func xrayState(v any) *string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	state, ok := obj["state"].(string)
	if !ok {
		return nil
	}
	return &state
}

// NewUUID 对应 newUuid()：GET /panel/api/server/getNewUUID → UUID 字符串。
func (c *Client) NewUUID(ctx context.Context) (string, error) {
	obj, err := c.request(ctx, http.MethodGet, EPServerNewUUID, nil, c.apiTimeout)
	if err != nil {
		return "", err
	}
	if s, ok := obj.(string); ok {
		return s, nil
	}
	return "", nil
}

// ── 值转换小工具 ─────────────────────────────────────────────

// asObjectList 把 JSON 数组收敛成 []map[string]any（非数组 → 空切片，对齐 PHP 的 is_array 判定）。
func asObjectList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// asStringList 复刻 array_values(array_filter($obj, 'is_string'))。
func asStringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// int64Slice 保证 JSON 编码成数组（nil → 空数组，对齐 PHP 的 []）。
func int64Slice(in []int64) []int64 {
	if in == nil {
		return []int64{}
	}
	return in
}

func isNumeric(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case int, int32, int64, float32, float64:
		return true
	case interface{ Float64() (float64, error) }:
		_, err := t.Float64()
		return err == nil
	case string:
		return isNumericString(t)
	}
	return false
}

// isNumericString 复刻 PHP is_numeric() 的十进制形态（前后允许空白）。
//
//   - 可选符号 + 数字串（可带小数点）+ 可选指数（e/E 带可选符号与数字）
//   - 十六进制（0x1A）、"INF"/"NAN"、千分位 一律 false（PHP 同样 false）
//
// 3x-ui 的 netTraffic 只会给十进制定点/整数，指数形态按 PHP 口径收下即可。
func isNumericString(s string) bool {
	trimmed := trimSpaceASCII(s)
	i := 0
	if i < len(trimmed) && (trimmed[i] == '+' || trimmed[i] == '-') {
		i++
	}
	digits := 0
	dot := false
	for ; i < len(trimmed); i++ {
		c := trimmed[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		case c == 'e' || c == 'E':
			// 指数段：必须有尾数，且后面是可选符号 + 至少一位数字
			if digits == 0 {
				return false
			}
			i++
			if i < len(trimmed) && (trimmed[i] == '+' || trimmed[i] == '-') {
				i++
			}
			expDigits := 0
			for ; i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9'; i++ {
				expDigits++
			}
			return expDigits > 0 && i == len(trimmed)
		default:
			return false
		}
	}
	return digits > 0
}

func trimSpaceASCII(s string) string {
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// asInt64 复刻 PHP 的 (int) 转换（字符串取数字前缀；浮点截断）。
func asInt64(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case interface{ Int64() (int64, error) }:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if fv, ok := v.(interface{ Float64() (float64, error) }); ok {
			if f, err := fv.Float64(); err == nil {
				return int64(f)
			}
		}
		return 0
	case string:
		return parseIntPrefix(t)
	}
	return 0
}

// asFloat64 复刻 PHP 的 (float) 转换。
func asFloat64(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case float64:
		return t
	case float32:
		return float64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case interface{ Float64() (float64, error) }:
		f, _ := t.Float64()
		return f
	case string:
		return parseFloatPrefix(t)
	}
	return 0
}

// parseIntPrefix 取字符串开头的整数（PHP 的 (int)"12abc" === 12）。
func parseIntPrefix(s string) int64 {
	s = trimSpaceASCII(s)
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseFloatPrefix 取字符串开头的浮点（PHP 的 (float)"1.5x" === 1.5）。
func parseFloatPrefix(s string) float64 {
	s = trimSpaceASCII(s)
	end := 0
	seenDot, seenExp := false, false
	if end < len(s) && (s[end] == '+' || s[end] == '-') {
		end++
	}
	for ; end < len(s); end++ {
		c := s[end]
		switch {
		case c >= '0' && c <= '9':
		case c == '.' && !seenDot && !seenExp:
			seenDot = true
		case (c == 'e' || c == 'E') && !seenExp && end > 0:
			seenExp = true
			if end+1 < len(s) && (s[end+1] == '+' || s[end+1] == '-') {
				end++
			}
		default:
			goto done
		}
	}
done:
	n, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0
	}
	return n
}

// round1 复刻 PHP round($x, 1)（half away from zero）。
func round1(f float64) float64 {
	return math.Round(f*10) / 10
}
