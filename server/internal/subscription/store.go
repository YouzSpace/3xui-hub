package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/crypto"
)

// SQLRepository 是 Repository 的生产实现：直连 MySQL（与 PHP 共享同一库），
// 3x-ui 节点的链接走面板 HTTP API。
//
// 与 PHP 的对应关系：
//
//	SiteConfigs     ← SiteConfig::getValue()（这里一次读全表）
//	UserByToken     ← User::where('token', $token)->first()
//	PlanByID        ← $user->plan
//	SubscribableNodes ← getLinks() 的 where 条件（enabled + status=online + 协议入口）
//	NodeInboundIDs  ← $node->inbounds()->where('protocol', ...)->pluck('inbound_id')
//	XrayInbounds    ← XrayInbound::enabledFor($node)
//	PanelLinks      ← ThreeXUiClient::getClientLinks()
//	PanelInbounds   ← ThreeXUiClient::listInbounds()
//
// 加密字段：nodes.password / nodes.api_key / driver_config.node_secret 在库里是
// Crypt::encryptString 密文，读出后用 crypto 解密（PHP 侧由 Eloquent accessor 做）。
type SQLRepository struct {
	DB  *sql.DB
	Key []byte

	// HTTPClient 用于访问 3x-ui 面板；nil 时用 DefaultPanelClient。
	HTTPClient *http.Client

	// PanelTimeout 是单次面板请求的总超时（对齐 config/panel.php 的 api_timeout=15s）。
	PanelTimeout time.Duration
	// PanelConnectTimeout 是连接超时（对齐 connect_timeout=5s）。
	PanelConnectTimeout time.Duration
}

// DefaultPanelClient 是面板请求默认使用的 HTTP 客户端。
func DefaultPanelClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// NewSQLRepository 构造生产 repository。
func NewSQLRepository(db *sql.DB, appKey []byte) *SQLRepository {
	return &SQLRepository{
		DB:                  db,
		Key:                 appKey,
		HTTPClient:          DefaultPanelClient(),
		PanelTimeout:        15 * time.Second,
		PanelConnectTimeout: 5 * time.Second,
	}
}

// SiteConfigs 读全部 site_configs（key → value）。NULL 值按空串处理
// （PHP 的 getValue 返回 $config->value ?? $default，NULL 走默认值路径）。
func (r *SQLRepository) SiteConfigs(ctx context.Context) (map[string]string, error) {
	rows, err := r.DB.QueryContext(ctx, "SELECT `key`, `value` FROM site_configs")
	if err != nil {
		return nil, fmt.Errorf("读取 site_configs 失败: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var key string
		var value sql.NullString
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("扫描 site_configs 失败: %w", err)
		}
		if !value.Valid {
			continue // 与 PHP 一致：NULL 不给键，调用方取默认值
		}
		out[key] = value.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 site_configs 失败: %w", err)
	}
	return out, nil
}

// userRow 是 users 表订阅相关列的扫描目标。
type userRow struct {
	id                 int64
	token              string
	uuid               string
	protocol           string
	planID             sql.NullInt64
	trafficLimit       int64
	trafficUsed        int64
	monthlyUsed        int64
	monthlyLimit       int64
	nextTrafficResetAt sql.NullTime
	expiredAt          sql.NullTime
	enabled            bool
}

func (r *SQLRepository) scanUser(row *sql.Row) (*User, error) {
	var u userRow
	err := row.Scan(
		&u.id, &u.token, &u.uuid, &u.protocol, &u.planID,
		&u.trafficLimit, &u.trafficUsed, &u.monthlyUsed, &u.monthlyLimit,
		&u.nextTrafficResetAt, &u.expiredAt, &u.enabled,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("扫描 users 失败: %w", err)
	}

	out := &User{
		ID:                 u.id,
		Token:              u.token,
		UUID:               u.uuid,
		Protocol:           u.protocol,
		TrafficLimit:       u.trafficLimit,
		TrafficUsed:        u.trafficUsed,
		MonthlyTrafficUsed: u.monthlyUsed,
		MonthlyTrafficLim:  u.monthlyLimit,
		Enabled:            u.enabled,
	}
	if u.planID.Valid {
		id := u.planID.Int64
		out.PlanID = &id
	}
	if u.nextTrafficResetAt.Valid {
		t := u.nextTrafficResetAt.Time
		out.NextTrafficResetAt = &t
	}
	if u.expiredAt.Valid {
		t := u.expiredAt.Time
		out.ExpiredAt = &t
	}
	return out, nil
}

const userColumns = "id, token, uuid, protocol, plan_id, traffic_limit, traffic_used, " +
	"monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at, enabled"

// UserByToken 按 users.token 精确查询（PHP：User::where('token', $token)->first()）。
func (r *SQLRepository) UserByToken(ctx context.Context, token string) (*User, error) {
	row := r.DB.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE token = ? LIMIT 1", token)
	return r.scanUser(row)
}

// PlanByID 按 plans.id 查询。
func (r *SQLRepository) PlanByID(ctx context.Context, id int64) (*Plan, error) {
	var (
		planType     string
		months       sql.NullInt64
		includeLocal sql.NullBool
		thirdParty   sql.NullString
	)
	err := r.DB.QueryRowContext(ctx,
		"SELECT type, months, include_local, third_party_sub_ids FROM plans WHERE id = ? LIMIT 1", id,
	).Scan(&planType, &months, &includeLocal, &thirdParty)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("扫描 plans 失败: %w", err)
	}

	plan := &Plan{ID: id, Type: planType, Months: months.Int64}
	// Plan::includesLocal() => (bool) ($this->include_local ?? true)
	plan.IncludeLocal = !includeLocal.Valid || includeLocal.Bool
	plan.ThirdPartySubIDs = parseIDList(thirdParty)
	return plan, nil
}

// parseIDList 解析 plans.third_party_sub_ids（JSON 数组文本；NULL/空 → 空列表）。
func parseIDList(raw sql.NullString) []int64 {
	if !raw.Valid || phpTrim(raw.String) == "" {
		return nil
	}
	decoded := decodeJSONAny(raw.String)
	arr, ok := decoded.([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(arr))
	for _, item := range arr {
		// Plan::thirdPartySubIdList() 用 array_filter 掉零值，这里同样丢弃 0/空。
		id := phpInt(item)
		if id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// SubscribableNodes 复刻 getLinks() 的节点查询：
//
//	WHERE enabled = 1 AND status = 'online'
//	  AND ( EXISTS(node_inbounds § protocol)  OR  (driver_type='xray' AND EXISTS(xray_inbounds § protocol, enabled)) )
//	ORDER BY id
//
// map[string]any 的协议参数与 PHP 的 $user->protocol 字符串比较同义。
func (r *SQLRepository) SubscribableNodes(ctx context.Context, protocol string) ([]Node, error) {
	// api_key 必须一起取：它决定该节点走「并发组」还是「串行组」，
	// 而 PHP 是先把并发组全部铺完、再跑串行组 —— 分组错了，链接顺序就会变。
	// driver_config 必须一起取：host_v6 存在里面（nodes 表没有该列，老库也不做 ALTER），
	// 订阅按入站地址族选地址时要用。
	const query = `
		SELECT n.id, n.name, n.host, n.port, n.enabled, n.driver_type, n.traffic_multiplier,
		       n.api_key, n.password, n.scheme, n.web_base_path, n.username, n.verify_ssl, n.driver_config
		FROM nodes n
		WHERE n.enabled = 1 AND n.status = 'online'
		  AND (
			EXISTS (SELECT 1 FROM node_inbounds ni WHERE ni.node_id = n.id)
			OR (n.driver_type = 'xray' AND EXISTS (
				SELECT 1 FROM xray_inbounds xi
				WHERE xi.node_id = n.id AND xi.enabled = 1
			))
		  )
		ORDER BY n.id`

	rows, err := r.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询可订阅节点失败: %w", err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		var (
			node     Node
			traffic  sql.NullFloat64
			driverTy sql.NullString
			apiKey   sql.NullString
			password sql.NullString
			scheme   sql.NullString
			basePath sql.NullString
			username sql.NullString
			verify   sql.NullBool
			driverCf sql.NullString
		)
		if err := rows.Scan(&node.ID, &node.Name, &node.Host, &node.Port, &node.Enabled, &driverTy,
			&traffic, &apiKey, &password, &scheme, &basePath, &username, &verify, &driverCf); err != nil {
			return nil, fmt.Errorf("扫描 nodes 失败: %w", err)
		}
		// 面板连接参数（PHP 侧由 ThreeXUiClient::fromNode 直接读模型属性）
		node.Scheme = scheme.String
		node.WebBasePath = basePath.String
		node.Username = username.String
		node.VerifySSL = verify.Bool
		// 原始 JSON 文本（host_v6 在里面；坏 JSON 由 HostV6 吞掉）
		node.DriverConfig = driverCf.String
		// PHP: ($this->driver_type ?? '3x-ui')，即 NULL/空 → '3x-ui'
		node.DriverType = driverTy.String
		if node.DriverType == "" {
			node.DriverType = "3x-ui"
		}
		if traffic.Valid {
			v := traffic.Float64
			node.TrafficMultiplier = &v
		}
		// 解密失败（APP_KEY 不匹配 / 密文损坏）按「没有 api_key」处理：
		// PHP 那边 accessor 抛的 DecryptException 会被上层 catch 成该节点无链接，
		// 这里只是把节点归到串行组，最终取链时仍会失败并被吞掉，行为一致。
		//
		// 解出来的明文字段直接写回 Node（PHP 侧 Node 模型 accessor 读出来就是明文）：
		// 下游 newPanelClient 拿到的因此是明文，不再二次解密。
		if apiKey.Valid {
			if plain, err := r.decryptField(apiKey.String); err == nil {
				node.APIKey = plain
			}
		}
		if password.Valid {
			if plain, err := r.decryptField(password.String); err == nil {
				node.Password = plain
			}
		}
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 nodes 失败: %w", err)
	}
	return out, nil
}

// NodeInboundIDs 返回该节点勾选的全部 3x-ui 入站 id（不按协议过滤，用户全协议通用）。
func (r *SQLRepository) NodeInboundIDs(ctx context.Context, nodeID int64, protocol string) ([]int64, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT inbound_id FROM node_inbounds WHERE node_id = ? ORDER BY id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询 node_inbounds 失败: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描 node_inbounds 失败: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// XrayInbounds 返回该节点启用的入站（对齐 XrayInbound::enabledFor：sort、id 升序）。
func (r *SQLRepository) XrayInbounds(ctx context.Context, nodeID int64) ([]XrayInbound, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, node_id, tag, remark, protocol, port, listen, settings, stream_settings,
		       salamander_password, enabled, sort
		FROM xray_inbounds
		WHERE node_id = ? AND enabled = 1
		ORDER BY sort, id`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询 xray_inbounds 失败: %w", err)
	}
	defer rows.Close()

	var out []XrayInbound
	for rows.Next() {
		var (
			ib         XrayInbound
			remark     sql.NullString
			listen     sql.NullString
			settings   sql.NullString
			stream     sql.NullString
			salamander sql.NullString
		)
		if err := rows.Scan(&ib.ID, &ib.NodeID, &ib.Tag, &remark, &ib.Protocol, &ib.Port,
			&listen, &settings, &stream, &salamander, &ib.Enabled, &ib.Sort); err != nil {
			return nil, fmt.Errorf("扫描 xray_inbounds 失败: %w", err)
		}
		ib.Remark = remark.String
		ib.Listen = listen.String
		ib.Settings = settings.String
		ib.StreamSettings = stream.String
		// 混淆密码（issue #94）：解不开就当没配 —— 一条密码坏掉不该让整个入站从订阅里消失。
		if plain, err := r.decryptField(salamander.String); err == nil {
			ib.SalamanderPassword = plain
		}
		out = append(out, ib)
	}
	return out, rows.Err()
}

// ThirdPartySubs 按 id 升序返回给定 id 中「已启用」的第三方订阅。
func (r *SQLRepository) ThirdPartySubs(ctx context.Context, ids []int64) ([]ThirdPartySub, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}

	query := "SELECT id, enabled, cached_links FROM third_party_subs WHERE enabled = 1 AND id IN (" +
		strings.Join(placeholders, ",") + ") ORDER BY id"
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 third_party_subs 失败: %w", err)
	}
	defer rows.Close()

	var out []ThirdPartySub
	for rows.Next() {
		var (
			sub    ThirdPartySub
			cached sql.NullString
		)
		if err := rows.Scan(&sub.ID, &sub.Enabled, &cached); err != nil {
			return nil, fmt.Errorf("扫描 third_party_subs 失败: %w", err)
		}
		sub.CachedLinks = cached.String
		out = append(out, sub)
	}
	return out, rows.Err()
}

// HasEnabledOnlineXrayNodeWithoutHost 复刻 generate() 的诊断查询。
func (r *SQLRepository) HasEnabledOnlineXrayNodeWithoutHost(ctx context.Context) (bool, error) {
	var exists bool
	err := r.DB.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM nodes
			WHERE enabled = 1 AND status = 'online' AND driver_type = 'xray'
			  AND (host IS NULL OR host = '')
		)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("诊断查询失败: %w", err)
	}
	return exists, nil
}

// PanelLinks 取 3x-ui 节点的该用户链接（GET /panel/api/clients/links/{email}）。
//
// 行为对齐 ThreeXUiClient::getClientLinks()：
//   - 该 email 在面板上不存在 → 返回空数组（不报错）；
//   - 网络/鉴权/5xx/响应格式非法 → 返回错误（服务层按「单节点失败」吞掉）。
func (r *SQLRepository) PanelLinks(ctx context.Context, node Node, email string) ([]string, error) {
	client, err := r.newPanelClient(node)
	if err != nil {
		return nil, err
	}
	obj, notFound, err := client.request(ctx, http.MethodGet, "/panel/api/clients/links/"+url.PathEscape(email), nil)
	if err != nil {
		return nil, err
	}
	if notFound || obj == nil {
		return nil, nil
	}
	arr, ok := obj.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// PanelInbounds 取 3x-ui 节点的一次性全量入站列表（GET /panel/api/inbounds/list）。
func (r *SQLRepository) PanelInbounds(ctx context.Context, node Node) ([]PanelInbound, error) {
	client, err := r.newPanelClient(node)
	if err != nil {
		return nil, err
	}
	obj, _, err := client.request(ctx, http.MethodGet, "/panel/api/inbounds/list", nil)
	if err != nil {
		return nil, err
	}
	arr, ok := obj.([]any)
	if !ok {
		return nil, nil
	}

	out := make([]PanelInbound, 0, len(arr))
	for _, item := range arr {
		inbound, ok := item.(phpMap)
		if !ok {
			continue
		}
		id, hasID := inbound.get("id")
		port, hasPort := inbound.get("port")
		if !hasID || !hasPort {
			continue // PHP: !isset($inbound['id'], $inbound['port']) → continue
		}
		out = append(out, PanelInbound{ID: phpInt(id), Port: phpInt(port)})
	}
	return out, nil
}

// panelClient 是最小 3x-ui HTTP 客户端（Bearer 优先，回落 cookie+CSRF 登录）。
type panelClient struct {
	baseURL    string
	apiKey     string
	username   string
	password   string
	verify     bool
	httpClient *http.Client

	cookieHeader  string
	csrfToken     string
	authenticated bool
}

// newPanelClient 由节点配置构造客户端（对齐 ThreeXUiClient::fromNode）。
func (r *SQLRepository) newPanelClient(node Node) (*panelClient, error) {
	scheme := node.Scheme
	if scheme == "" {
		scheme = "https"
	}
	port := node.Port
	if port == 0 {
		port = 443
	}
	basePath := strings.TrimRight(node.WebBasePath, "/")
	if basePath != "" && !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}

	// node.APIKey / node.Password 由 SubscribableNodes 解密后写入（与 PHP 的
	// Node accessor「属性即明文」一致），这里不再二次解密。
	apiKey := node.APIKey
	password := node.Password

	hc := r.HTTPClient
	if hc == nil {
		hc = DefaultPanelClient()
	}

	return &panelClient{
		// net.JoinHostPort：域名/IPv4 输出与 "host:port" 逐字节一致，IPv6 字面量补方括号。
		baseURL:    scheme + "://" + net.JoinHostPort(node.Host, strconv.FormatInt(port, 10)) + basePath,
		apiKey:     apiKey,
		username:   node.Username,
		password:   password,
		verify:     node.VerifySSL,
		httpClient: hc,
	}, nil
}

// decryptField 解密 Laravel Crypt::encryptString 的字段；空值原样返回空串。
//
// 与 PHP accessor 的差异（有意为之）：PHP 解密失败会抛 DecryptException 并被上层
// catch 成「该节点无链接」；这里同样返回错误，由调用方按单节点失败处理。
func (r *SQLRepository) decryptField(cipherText string) (string, error) {
	if cipherText == "" {
		return "", nil
	}
	if len(r.Key) != 32 {
		return "", errors.New("APP_KEY 未初始化，无法解密节点凭据")
	}
	plain, err := crypto.DecryptString(cipherText, r.Key)
	if err != nil {
		return "", fmt.Errorf("解密节点凭据失败: %w", err)
	}
	return plain, nil
}

// request 发一次面板请求并解析 {success, obj} 信封。
//
// 返回值语义对齐 ThreeXUiClient::request/requestNullable：
//   - 成功（success=true）→ (obj, false, nil)
//   - success=false 且像是 not-found → (nil, true, nil)
//   - success=false 其它 → (nil, false, error)
//   - 传输层失败 → (nil, false, error)
func (c *panelClient) request(ctx context.Context, method, path string, body []byte) (any, bool, error) {
	resp, status, err := c.send(ctx, method, path, body, false)
	if err != nil {
		return nil, false, err
	}
	_ = status

	decoded := decodeJSONAny(string(resp))
	obj, ok := decoded.(phpMap)
	if !ok {
		return nil, false, fmt.Errorf("invalid 3x-ui response: %.200s", string(resp))
	}
	success, hasSuccess := obj.get("success")
	if !hasSuccess {
		return nil, false, fmt.Errorf("invalid 3x-ui response: %.200s", string(resp))
	}
	if !phpBool(success) {
		msg := phpString(field(obj, "msg"))
		if isClientNotFoundMessage(msg) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("%s", msg)
	}
	payload, _ := obj.get("obj")
	return payload, false, nil
}

// isClientNotFoundMessage 对齐 ThreeXUiClient::isClientNotFound 的文案匹配。
func isClientNotFoundMessage(msg string) bool {
	lowered := asciiLower(msg)
	for _, needle := range []string{"record not found", "not found", "not exist", "不存在"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// send 执行一次 HTTP 往返（含鉴权头与 cookie 模式的登录）。
func (c *panelClient) send(ctx context.Context, method, path string, body []byte, isRetry bool) ([]byte, int, error) {
	if c.apiKey == "" && !c.authenticated {
		if err := c.login(ctx); err != nil {
			return nil, 0, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, fmt.Errorf("构造面板请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.cookieHeader != "" {
		req.Header.Set("Cookie", c.cookieHeader)
	}
	if c.csrfToken != "" && strings.EqualFold(method, http.MethodPost) {
		req.Header.Set("X-CSRF-Token", c.csrfToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("3x-ui request failed: %w", err)
	}
	defer resp.Body.Close()

	payload := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			payload = append(payload, buf[:n]...)
		}
		if readErr != nil {
			break
		}
	}

	// cookie 模式下 401/403 = 会话失效：清登录态重登并重试一次（Bearer 模式不重试）。
	if c.apiKey == "" && !isRetry && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		c.forgetAuth()
		if err := c.login(ctx); err != nil {
			return nil, resp.StatusCode, err
		}
		return c.send(ctx, method, path, body, true)
	}

	return payload, resp.StatusCode, nil
}

// login 对应 ThreeXUiClient::login()：POST /login → GET /panel/api/csrf-token。
func (c *panelClient) login(ctx context.Context) error {
	loginBody := jsonCompact(obj("username", c.username, "password", c.password))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/login", strings.NewReader(loginBody))
	if err != nil {
		return fmt.Errorf("构造登录请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("3x-ui login failed: %w", err)
	}
	defer resp.Body.Close()

	body := readAllLimited(resp)

	decoded := decodeJSONAny(body)
	obj, ok := decoded.(phpMap)
	if !ok || !phpBool(field(obj, "success")) {
		return fmt.Errorf("3x-ui login failed: %s", phpString(field(obj, "msg")))
	}

	c.cookieHeader = joinCookies(resp.Cookies())
	c.authenticated = true

	// CSRF 获取失败不致命（部分版本无此端点），继续尝试
	csrfReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/panel/api/csrf-token", nil)
	if err != nil {
		return nil
	}
	csrfReq.Header.Set("Accept", "application/json")
	if c.cookieHeader != "" {
		csrfReq.Header.Set("Cookie", c.cookieHeader)
	}
	csrfResp, err := c.httpClient.Do(csrfReq)
	if err != nil {
		return nil
	}
	defer csrfResp.Body.Close()
	csrf := phpTrimSet(readAllLimited(csrfResp), "\" \r\n")
	if csrf != "" {
		c.csrfToken = csrf
	}
	return nil
}

// forgetAuth 清本实例登录态（401/403 后调用）。
func (c *panelClient) forgetAuth() {
	c.authenticated = false
	c.csrfToken = ""
	c.cookieHeader = ""
}

func joinCookies(cookies []*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

func readAllLimited(resp *http.Response) string {
	var b strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String()
}

// 编译期断言：SQLRepository 实现 Repository。
var _ Repository = (*SQLRepository)(nil)
