package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"controlhub-server/internal/driver/threexui"
	"controlhub-server/internal/driver/xray"
)

// nodeRow 是 nodes 表里采集层需要的列。
//
// password / api_key 保持库内**密文**形态（解密统一走 threexui.NewForNode，
// 与 PHP 的 Crypt::decryptString 失败即抛异常同口径 —— 解密失败 = 该节点不可用）。
type nodeRow struct {
	ID           int64
	Name         string
	Host         string
	Port         int64
	Scheme       string
	WebBasePath  string
	Username     string
	Password     sql.NullString
	APIKey       sql.NullString
	Enabled      bool
	VerifySSL    bool
	DriverType   string
	DriverConfig sql.NullString
	Status       string
	Latency      int64
	// TrafficMultiplier 为 NULL 表示「继承 site_configs.default_node_multiplier」。
	TrafficMultiplier sql.NullFloat64
	// LastCheckAt 是 xray 心跳时间（NULL = 从未上报）。
	LastCheckAt sql.NullTime
	// CPU / Mem 是最近一次健康检查采到的指标（nullable）。
	CPU sql.NullFloat64
	Mem sql.NullFloat64
}

// isXray 对应 Node::isXray(): `($driver_type ?? '3x-ui') === 'xray'`。
func (n nodeRow) isXray() bool {
	t := n.DriverType
	if t == "" {
		t = "3x-ui"
	}
	return t == "xray"
}

const nodeColumns = "id, name, host, port, scheme, web_base_path, username, password, api_key, " +
	"enabled, verify_ssl, driver_type, driver_config, status, latency, traffic_multiplier, last_check_at, cpu, mem"

func scanNode(sc interface{ Scan(...any) error }) (nodeRow, error) {
	var (
		n                nodeRow
		enabled, verify  int64
		driverTypeOrNull sql.NullString
	)
	err := sc.Scan(&n.ID, &n.Name, &n.Host, &n.Port, &n.Scheme, &n.WebBasePath, &n.Username,
		&n.Password, &n.APIKey, &enabled, &verify, &driverTypeOrNull, &n.DriverConfig,
		&n.Status, &n.Latency, &n.TrafficMultiplier, &n.LastCheckAt, &n.CPU, &n.Mem)
	if err != nil {
		return n, err
	}
	n.Enabled = enabled != 0
	n.VerifySSL = verify != 0
	n.DriverType = "3x-ui"
	if driverTypeOrNull.Valid && driverTypeOrNull.String != "" {
		n.DriverType = driverTypeOrNull.String
	}
	return n, nil
}

// loadNode 读单个节点（不存在 → (nil, nil)，对齐 Node::find()）。
func (c *Collector) loadNode(ctx context.Context, id int64) (*nodeRow, error) {
	row := c.db.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = ?", id)
	n, err := scanNode(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取节点 #%d 失败: %w", id, err)
	}
	return &n, nil
}

// enabledNodes 读全部启用节点（对齐 Node::where('enabled', true)->get()，按 id 升序）。
func (c *Collector) enabledNodes(ctx context.Context) ([]nodeRow, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT "+nodeColumns+" FROM nodes WHERE enabled = 1 ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("查询启用节点失败: %w", err)
	}
	defer rows.Close()

	var out []nodeRow
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("读取启用节点失败: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// threeXUiClient 由节点行构造 3x-ui 客户端（严格解密 password / api_key）。
func (c *Collector) threeXUiClient(n nodeRow) (*threexui.Client, error) {
	row := threexui.NodeRow{
		Scheme:      n.Scheme,
		Host:        n.Host,
		Port:        int(n.Port),
		WebBasePath: n.WebBasePath,
		Username:    n.Username,
		VerifySSL:   n.VerifySSL,
	}
	if n.Password.Valid {
		v := n.Password.String
		row.PasswordCipher = &v
	}
	if n.APIKey.Valid {
		v := n.APIKey.String
		row.APIKeyCipher = &v
	}
	return threexui.NewForNode(row, c.key)
}

// xrayDriver 由节点行构造 xray 驱动（数据来源走本包实现的 xraySource）。
func (c *Collector) xrayDriver(n nodeRow) *xray.Driver {
	cfg := xray.Config{
		NodeID:   n.ID,
		NodeName: n.Name,
		NodeHost: n.Host,
		Source:   &xraySource{c: c},
		Enqueuer: &xrayEnqueuer{c: c},
		Now:      c.now,
	}
	if n.DriverConfig.Valid {
		cfg.DriverConfig = []byte(n.DriverConfig.String)
	}
	if n.LastCheckAt.Valid {
		t := n.LastCheckAt.Time
		cfg.LastCheckAt = &t
	}
	return xray.NewDriver(cfg)
}

// ── xray 驱动的数据来源（对齐 XrayDriver 用到的 Model 查询） ──────────

// xraySource 实现 xray.Source：xray 驱动不持有 *sql.DB，所有输入由本包提供。
type xraySource struct{ c *Collector }

// UserByID 对齐 User::find()。
func (s *xraySource) UserByID(ctx context.Context, id int64) (*xray.UserRecord, error) {
	var (
		u       xray.UserRecord
		planID  sql.NullInt64
		expired sql.NullTime
		enabled int64
	)
	err := s.c.db.QueryRowContext(ctx,
		"SELECT id, uuid, protocol, plan_id, enabled, traffic_limit, expired_at FROM users WHERE id = ?", id).
		Scan(&u.ID, &u.UUID, &u.Protocol, &planID, &enabled, &u.TrafficLimit, &expired)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取用户 #%d 失败: %w", id, err)
	}
	u.Enabled = enabled != 0
	if planID.Valid {
		v := planID.Int64
		u.PlanID = &v
	}
	if expired.Valid {
		t := expired.Time
		u.ExpiredAt = &t
	}
	return &u, nil
}

// EnabledInbounds 对齐 XrayInbound::enabledFor(): enabled=1 ORDER BY sort, id。
func (s *xraySource) EnabledInbounds(ctx context.Context, nodeID int64) ([]xray.Inbound, error) {
	rows, err := s.c.db.QueryContext(ctx,
		"SELECT id, tag, remark, protocol, port, listen, settings, stream_settings, sniffing, reality_private_key "+
			"FROM xray_inbounds WHERE node_id = ? AND enabled = 1 ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询 xray 入站失败: %w", err)
	}
	defer rows.Close()

	var out []xray.Inbound
	for rows.Next() {
		var (
			ib                       xray.Inbound
			remark, settings, stream sql.NullString
			sniff                    sql.NullString
			realityPrivateKey        sql.NullString
		)
		if err := rows.Scan(&ib.ID, &ib.Tag, &remark, &ib.Protocol, &ib.Port, &ib.Listen,
			&settings, &stream, &sniff, &realityPrivateKey); err != nil {
			return nil, fmt.Errorf("读取 xray 入站失败: %w", err)
		}
		if remark.Valid {
			v := remark.String
			ib.Remark = &v
		}
		ib.Settings = rawOrNil(settings)
		ib.StreamSettings = rawOrNil(stream)
		ib.Sniffing = rawOrNil(sniff)
		if realityPrivateKey.Valid {
			v := realityPrivateKey.String
			ib.RealityPrivateKey = &v
		}
		out = append(out, ib)
	}
	return out, rows.Err()
}

// UsersWithPlan 对齐 User::whereNotNull('plan_id')->get()。
func (s *xraySource) UsersWithPlan(ctx context.Context) ([]xray.UserRecord, error) {
	rows, err := s.c.db.QueryContext(ctx,
		"SELECT id, uuid, protocol, plan_id, enabled, traffic_limit, expired_at FROM users WHERE plan_id IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("查询套餐用户失败: %w", err)
	}
	defer rows.Close()

	var out []xray.UserRecord
	for rows.Next() {
		var (
			u       xray.UserRecord
			planID  sql.NullInt64
			expired sql.NullTime
			enabled int64
		)
		if err := rows.Scan(&u.ID, &u.UUID, &u.Protocol, &planID, &enabled, &u.TrafficLimit, &expired); err != nil {
			return nil, fmt.Errorf("读取套餐用户失败: %w", err)
		}
		u.Enabled = enabled != 0
		if planID.Valid {
			v := planID.Int64
			u.PlanID = &v
		}
		if expired.Valid {
			t := expired.Time
			u.ExpiredAt = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TrafficSnapshot 对齐 TrafficSnapshot::where(user_id)->where(node_id)->first()：
// 返回 upload/download（表字段名，data-contract §1）。
func (s *xraySource) TrafficSnapshot(ctx context.Context, userID, nodeID int64) (int64, int64, bool, error) {
	var up, down int64
	err := s.c.db.QueryRowContext(ctx,
		"SELECT upload, download FROM traffic_snapshots WHERE user_id = ? AND node_id = ? ORDER BY id DESC LIMIT 1",
		userID, nodeID).Scan(&up, &down)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("读取流量快照失败: %w", err)
	}
	return up, down, true, nil
}

// RelayClients 对齐 XrayRelayClient::forInbound()（按 id 升序）。
func (s *xraySource) RelayClients(ctx context.Context, nodeID int64) ([]xray.RelayClient, error) {
	rows, err := s.c.db.QueryContext(ctx,
		"SELECT c.inbound_id, c.client_id FROM xray_relay_clients c "+
			"JOIN xray_inbounds i ON i.id = c.inbound_id "+
			"WHERE i.node_id = ? ORDER BY c.id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询中转凭据失败: %w", err)
	}
	defer rows.Close()

	var out []xray.RelayClient
	for rows.Next() {
		var rc xray.RelayClient
		if err := rows.Scan(&rc.InboundID, &rc.ClientID); err != nil {
			return nil, fmt.Errorf("读取中转凭据失败: %w", err)
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// xrayEnqueuer 把驱动的指令落进 node_api_commands（status='pending'）。
//
// payload 用 PHP json_encode 默认参数编码（phpjson.EncodeDefault），与 PHP 侧写入的
// 字节形态一致 —— agent 两侧都读，格式不能分叉。
type xrayEnqueuer struct{ c *Collector }

func (e *xrayEnqueuer) Enqueue(ctx context.Context, cmd xray.Command) error {
	_, err := e.c.enqueueNodeCommand(ctx, cmd.NodeID, cmd.Action, cmd.Payload, cmd.UserID)
	return err
}

func rawOrNil(s sql.NullString) json.RawMessage {
	if !s.Valid {
		return nil
	}
	return json.RawMessage(s.String)
}

// jsonObjectField 取 driver_config 里的一个子对象（缺键 / 不是对象 / JSON 非法 → false）。
//
// 数字用 json.Number 保留原文：累计字节可能超过 float64 的安全整数范围，
// 解析成 float64 再转会丢精度（与 P4 的 phpjson.Decode 同口径）。
func jsonObjectField(raw []byte, field string) (map[string]any, bool) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	sub, ok := obj[field].(map[string]any)
	if !ok {
		return nil, false
	}
	return sub, true
}

// parseClientEmail 复刻 `/^ch_user_(\d+)$/`（TrafficSyncService::parseUserIds）。
func parseClientEmail(email string) (int64, bool) {
	const prefix = "ch_user_"
	if !strings.HasPrefix(email, prefix) {
		return 0, false
	}
	digits := email[len(prefix):]
	if digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// errUnsupportedDriver 对齐 NodeDriverFactory 的 `Unsupported node driver: {type}`。
func errUnsupportedDriver(driverType string) error {
	return fmt.Errorf("Unsupported node driver: %s", driverType)
}

// NodeDriver 是启用节点的驱动句柄（供 P8a 用户域下发复用，避免 handler 层自己构造驱动）。
type NodeDriver struct {
	NodeID int64
	// Status 是节点最近一次健康检查的状态（online / offline / 空）。
	// 下发前据此跳过离线的 3x-ui 节点：对离线节点发 HTTP 只能干等超时。
	Status string
	ThreeX *threexui.Client // 非空 = 3x-ui 节点（driver_type 空也归 3x-ui）
	Xray   *xray.Driver     // 非空 = xray 节点
}

// NodeDrivers 返回全部启用节点的驱动句柄（3x-ui 直连客户端 / xray 指令驱动）。
//
// 供 cmd/server 组装 UserPanelOps（建号/重置/开关/删号遍历节点下发）复用；
// 各驱动内部已带好解密凭据（3x-ui）与 Source/Enqueuer（xray）。
func (c *Collector) NodeDrivers(ctx context.Context) ([]NodeDriver, error) {
	nodes, err := c.enabledNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeDriver, 0, len(nodes))
	for _, n := range nodes {
		nd := NodeDriver{NodeID: n.ID, Status: n.Status}
		if n.isXray() {
			nd.Xray = c.xrayDriver(n)
		} else {
			client, err := c.threeXUiClient(n)
			if err != nil {
				return nil, fmt.Errorf("构造节点 #%d 3x-ui 客户端失败: %w", n.ID, err)
			}
			nd.ThreeX = client
		}
		out = append(out, nd)
	}
	return out, nil
}

// InboundIDs 返回该节点上协议匹配的 3x-ui 入站 ID（对齐 Node::inboundIdsFor）。
//
// 供 cmd/server 组装 UserPanelOps 时给 3x-ui 节点取 inboundIds（AddClient/AttachClient 入参）。
func (c *Collector) InboundIDs(ctx context.Context, nodeID int64, protocol string) ([]int64, error) {
	return c.inboundIDsFor(ctx, nodeID, protocol)
}
