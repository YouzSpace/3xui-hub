package admin

// NodeStore 的 database/sql 实现（节点域：nodes / xray_* / warp / 第三方 / 出站订阅 / 异步任务）。
//
// 时间口径与 P8d 的 sqlStore 完全一致（contracts/data-contract.md §4）：
// DSN 用 parseTime=true&loc=UTC，把 MySQL 返回的字面量打 UTC 标签、不做换算，
// 与 Laravel（app.timezone=UTC）读到的一致。严禁 SET time_zone。
//
// 加密字段（nodes.password / api_key / driver_config.node_secret /
// xray_inbounds.reality_private_key / xray_inbounds.salamander_password /
// xray_outbounds.secret_key /
// node_warp_accounts.access_token|private_key）全部走 internal/crypto，
// 与 Laravel 的 Crypt::encryptString 可互读。

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"controlhub-server/internal/crypto"
)

// NewNodeSQLStore 用已有连接池构造节点域的 NodeStore。
func NewNodeSQLStore(db *sql.DB, appKey []byte) NodeStore {
	return &nodeSQLStore{db: db, appKey: appKey, now: time.Now}
}

type nodeSQLStore struct {
	db     *sql.DB
	appKey []byte
	now    func() time.Time
}

// ---- 加密小工具 ----

func (s *nodeSQLStore) encryptString(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	return crypto.EncryptString(v, s.appKey)
}

// decryptOrRaw 解密失败时返回原文（Laravel accessor 对存量明文的宽松表现）。
func (s *nodeSQLStore) decryptOrRaw(v sql.NullString) *string {
	if !v.Valid || v.String == "" {
		return nil
	}
	plain, err := crypto.DecryptString(v.String, s.appKey)
	if err != nil {
		raw := v.String
		return &raw
	}
	return &plain
}

// ---- 站点配置 ----

func (s *nodeSQLStore) SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		placeholders[i] = "?"
		args[i] = k
		out[k] = ""
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT `key`, value FROM site_configs WHERE `key` IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value sql.NullString
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value.String
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) SiteConfigSet(ctx context.Context, key string, value *string) error {
	if value == nil {
		// 删行（show_node_multiplier 的关闭语义：用户端靠「有没有这行」判断）。
		_, err := s.db.ExecContext(ctx, "DELETE FROM site_configs WHERE `key` = ?", key)
		return err
	}
	now := s.now().UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO site_configs (`key`, value, created_at, updated_at) VALUES (?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = VALUES(updated_at)",
		key, *value, now, now)
	return err
}

// ---- 节点 ----

const nodeColumns = `id, name, host, port, scheme, web_base_path, username, password, api_key,
	enabled, verify_ssl, status, latency, cpu, mem, traffic_multiplier, driver_type, driver_version,
	driver_config, last_check_at, created_at, updated_at`

func (s *nodeSQLStore) scanNode(row interface{ Scan(...any) error }) (*NodeRecord, error) {
	var n NodeRecord
	var password, apiKey, cpu, mem, multiplier, driverVersion sql.NullString
	var driverConfig, rawPassword, rawAPIKey sql.NullString
	var enabled, verifySSL int
	var lastCheck, createdAt, updatedAt sql.NullTime
	if err := row.Scan(&n.ID, &n.Name, &n.Host, &n.Port, &n.Scheme, &n.WebBasePath, &n.Username,
		&rawPassword, &rawAPIKey, &enabled, &verifySSL, &n.Status, &n.Latency, &cpu, &mem,
		&multiplier, &n.DriverType, &driverVersion, &driverConfig, &lastCheck, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	_ = password
	_ = apiKey
	n.PasswordEnc = nullStrPtr(rawPassword)
	n.APIKeyEnc = nullStrPtr(rawAPIKey)
	n.Password = s.decryptOrRaw(rawPassword)
	n.APIKey = s.decryptOrRaw(rawAPIKey)
	n.Enabled = enabled != 0
	n.VerifySSL = verifySSL != 0
	n.CPU = nullStrPtr(cpu)
	n.Mem = nullStrPtr(mem)
	n.TrafficMultiplier = nullStrPtr(multiplier)
	n.DriverVersion = nullStrPtr(driverVersion)
	if driverConfig.Valid && driverConfig.String != "" {
		n.DriverConfigRaw = []byte(driverConfig.String)
	}
	n.LastCheckAt = nullTimePtr(lastCheck)
	n.CreatedAt = nullTimePtr(createdAt)
	n.UpdatedAt = nullTimePtr(updatedAt)

	n.NodeSecret = s.nodeSecretOf(n.DriverConfigRaw)
	n.ConfigVersion = driverConfigInt(n.DriverConfigRaw, "config_version")
	return &n, nil
}

// nodeSecretOf 解密 driver_config.node_secret（手工写的明文原样返回）。
func (s *nodeSQLStore) nodeSecretOf(cfg []byte) *string {
	if len(cfg) == 0 {
		return nil
	}
	val, err := ordParse(cfg)
	if err != nil {
		return nil
	}
	obj, ok := val.(ordObj)
	if !ok {
		return nil
	}
	raw, ok := obj.Get("node_secret")
	if !ok || raw == nil {
		return nil
	}
	secret, isStr := raw.(string)
	if !isStr || secret == "" {
		return nil
	}
	if plain, err := crypto.DecryptString(secret, s.appKey); err == nil {
		return &plain
	}
	return &secret
}

// driverConfigInt 读 driver_config 里的整数字段（缺失/坏 JSON → 0）。
func driverConfigInt(cfg []byte, field string) int64 {
	if len(cfg) == 0 {
		return 0
	}
	val, err := ordParse(cfg)
	if err != nil {
		return 0
	}
	obj, ok := val.(ordObj)
	if !ok {
		return 0
	}
	v, _ := obj.Get(field)
	return int64(ordFloat(v))
}

func (s *nodeSQLStore) NodesPage(ctx context.Context, all bool, page, perPage int64) ([]NodeRecord, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes").Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT " + nodeColumns + " FROM nodes ORDER BY id DESC"
	args := []any{}
	if !all {
		if page < 1 {
			page = 1
		}
		lastPage := (total + perPage - 1) / perPage
		if lastPage < 1 {
			lastPage = 1
		}
		if page > lastPage {
			page = lastPage
		}
		query += " LIMIT ? OFFSET ?"
		args = append(args, perPage, (page-1)*perPage)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	nodes := []NodeRecord{}
	index := map[uint64]int{}
	for rows.Next() {
		n, err := s.scanNode(rows)
		if err != nil {
			return nil, 0, err
		}
		index[n.ID] = len(nodes)
		nodes = append(nodes, *n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(nodes) == 0 {
		return nodes, total, nil
	}

	ids := make([]any, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")

	// 预加载 inbounds / xrayInbounds（对齐 PHP 的 with(['inbounds','xrayInbounds'])）。
	inboundRows, err := s.db.QueryContext(ctx,
		"SELECT id, node_id, protocol, inbound_id FROM node_inbounds WHERE node_id IN ("+placeholders+") ORDER BY id", ids...)
	if err != nil {
		return nil, 0, err
	}
	defer inboundRows.Close()
	for inboundRows.Next() {
		var row NodeInboundRecord
		if err := inboundRows.Scan(&row.ID, &row.NodeID, &row.Protocol, &row.InboundID); err != nil {
			return nil, 0, err
		}
		if i, ok := index[row.NodeID]; ok {
			nodes[i].Inbounds = append(nodes[i].Inbounds, row)
		}
	}
	if err := inboundRows.Err(); err != nil {
		return nil, 0, err
	}

	xrayRows, err := s.db.QueryContext(ctx, "SELECT "+xrayInboundColumns+
		" FROM xray_inbounds WHERE node_id IN ("+placeholders+") ORDER BY sort, id", ids...)
	if err != nil {
		return nil, 0, err
	}
	defer xrayRows.Close()
	for xrayRows.Next() {
		ib, err := s.scanXrayInbound(xrayRows)
		if err != nil {
			return nil, 0, err
		}
		if i, ok := index[ib.NodeID]; ok {
			nodes[i].XrayInbounds = append(nodes[i].XrayInbounds, *ib)
		}
	}
	return nodes, total, xrayRows.Err()
}

func (s *nodeSQLStore) NodeByID(ctx context.Context, id uint64) (*NodeRecord, error) {
	n, err := s.scanNode(s.db.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = ?", id))
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT id, node_id, protocol, inbound_id FROM node_inbounds WHERE node_id = ? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row NodeInboundRecord
		if err := rows.Scan(&row.ID, &row.NodeID, &row.Protocol, &row.InboundID); err != nil {
			return nil, err
		}
		n.Inbounds = append(n.Inbounds, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	xrayRows, err := s.db.QueryContext(ctx, "SELECT "+xrayInboundColumns+
		" FROM xray_inbounds WHERE node_id = ? ORDER BY sort, id", id)
	if err != nil {
		return nil, err
	}
	defer xrayRows.Close()
	for xrayRows.Next() {
		ib, err := s.scanXrayInbound(xrayRows)
		if err != nil {
			return nil, err
		}
		n.XrayInbounds = append(n.XrayInbounds, *ib)
	}
	return n, xrayRows.Err()
}

func (s *nodeSQLStore) CreateNode(ctx context.Context, in NodeCreateInput) (*NodeRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	encPassword, err := s.encryptOptional(in.Password)
	if err != nil {
		return nil, err
	}
	encAPIKey, err := s.encryptOptional(in.APIKey)
	if err != nil {
		return nil, err
	}

	var driverConfig any
	if in.DriverType == "xray" {
		cfg := ordObj{}
		if in.NodeSecret != "" {
			enc, err := s.encryptString(in.NodeSecret)
			if err != nil {
				return nil, err
			}
			cfg = append(cfg, ordMember{Key: "node_secret", Value: enc})
		}
		if in.PullInterval != nil {
			cfg = append(cfg, ordMember{Key: "pull_interval", Value: *in.PullInterval})
		}
		// issue #88：IPv6 入口地址（订阅按入站地址族选地址时读它）。
		if in.HostV6 != nil && phpTrimSpaceOnly(*in.HostV6) != "" {
			cfg = append(cfg, ordMember{Key: "host_v6", Value: phpTrimSpaceOnly(*in.HostV6)})
		}
		body, err := ordEncode(cfg)
		if err != nil {
			return nil, err
		}
		driverConfig = string(body)
	}

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO nodes (name, host, port, scheme, web_base_path, username, password, api_key, "+
			"enabled, verify_ssl, status, latency, driver_type, driver_config, traffic_multiplier, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 'offline', 0, ?, ?, ?, ?, ?)",
		in.Name, in.Host, in.Port, in.Scheme, in.WebBasePath, in.Username,
		nullStrValue(encPassword), nullStrValue(encAPIKey),
		boolToInt(in.VerifySSL), in.DriverType, driverConfig,
		nullStrValue(in.TrafficMultiplier), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	if in.DriverType != "xray" && len(in.Inbounds) > 0 {
		if _, err := s.ReplaceNodeInbounds(ctx, uint64(id), in.Inbounds); err != nil {
			return nil, err
		}
	}
	return s.NodeByID(ctx, uint64(id))
}

// encryptOptional 加密可空字符串（null/空串 → NULL，与 Node accessor 一致）。
func (s *nodeSQLStore) encryptOptional(v *string) (*string, error) {
	if v == nil || *v == "" {
		return nil, nil
	}
	enc, err := s.encryptString(*v)
	if err != nil {
		return nil, err
	}
	return &enc, nil
}

func (s *nodeSQLStore) UpdateNode(ctx context.Context, id uint64, in NodeUpdateInput) (*NodeRecord, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, val any) {
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}

	if in.Name != nil {
		add("name", *in.Name)
	}
	if in.Host != nil {
		add("host", *in.Host)
	}
	if in.Port != nil {
		add("port", *in.Port)
	}
	if in.Scheme != nil {
		add("scheme", *in.Scheme)
	}
	if in.WebBasePath != nil {
		add("web_base_path", *in.WebBasePath)
	}
	if in.Username != nil {
		add("username", *in.Username)
	}
	if in.PasswordSet {
		enc, err := s.encryptOptional(in.Password)
		if err != nil {
			return nil, err
		}
		add("password", nullStrValue(enc))
	}
	if in.APIKeySet {
		enc, err := s.encryptOptional(in.APIKey)
		if err != nil {
			return nil, err
		}
		add("api_key", nullStrValue(enc))
	}
	if in.Enabled != nil {
		add("enabled", boolToInt(*in.Enabled))
	}
	if in.VerifySSL != nil {
		add("verify_ssl", boolToInt(*in.VerifySSL))
	}
	if in.DriverType != nil {
		add("driver_type", *in.DriverType)
	}
	if in.MultiplierSet {
		add("traffic_multiplier", nullStrValue(in.TrafficMultiplier))
	}

	if len(sets) > 0 {
		sets = append(sets, "updated_at = ?")
		args = append(args, s.now().UTC().Truncate(time.Second), id)
		if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return nil, err
		}
	}
	return s.NodeByID(ctx, id)
}

func (s *nodeSQLStore) DeleteNode(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM nodes WHERE id = ?", id)
	return err
}

// ReplaceNodeInbounds 全量替换 node_inbounds，并回报「确实变化的协议 → 新入站集合」。
//
// 入参是「协议 → 入站 id 列表」的全集，**不能只认 vless/trojan**：3x-ui 上的
// hysteria（HY2）/ vmess / shadowsocks 入站一样要落进 node_inbounds，否则建号时
// 按 users.protocol 查不到入站，整台节点被静默跳过（症状：面板里入站在、用户数恒 0）。
func (s *nodeSQLStore) ReplaceNodeInbounds(ctx context.Context, nodeID uint64, inbounds map[string][]int64) (map[string][]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	old := map[string][]int64{}
	rows, err := tx.QueryContext(ctx, "SELECT protocol, inbound_id FROM node_inbounds WHERE node_id = ?", nodeID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var proto string
		var inboundID int64
		if err := rows.Scan(&proto, &inboundID); err != nil {
			rows.Close()
			return nil, err
		}
		old[proto] = append(old[proto], inboundID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM node_inbounds WHERE node_id = ?", nodeID); err != nil {
		return nil, err
	}

	// 协议键排序后逐条写入：同一份入参每次落库顺序一致，便于比对与排错。
	protos := make([]string, 0, len(inbounds))
	for proto := range inbounds {
		protos = append(protos, proto)
	}
	sort.Strings(protos)

	now := s.now().UTC().Truncate(time.Second)
	changed := map[string][]int64{}
	for _, proto := range protos {
		ids := append([]int64{}, inbounds[proto]...)
		for _, inboundID := range ids {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO node_inbounds (node_id, protocol, inbound_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
				nodeID, proto, inboundID, now, now); err != nil {
				return nil, err
			}
		}
		if !int64SetsEqual(old[proto], ids) && len(ids) > 0 {
			sorted := append([]int64{}, ids...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			changed[proto] = sorted
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changed, nil
}

func (s *nodeSQLStore) SetNodeSecret(ctx context.Context, id uint64, secret string) error {
	enc, err := s.encryptString(secret)
	if err != nil {
		return err
	}
	return s.mergeDriverConfig(ctx, id, map[string]any{"node_secret": enc})
}

func (s *nodeSQLStore) MergeDriverConfig(ctx context.Context, id uint64, patch map[string]any) error {
	// 把 map 转成保序对象时按键排序（patch 的键都是本域自家键，顺序无业务含义）。
	return s.mergeDriverConfig(ctx, id, patch)
}

// mergeDriverConfig 行锁内重读 → array_replace_recursive 语义合并 → 回写。
//
// 只 merge 自家键：整块回写会把并发 bump 的 config_version 覆盖回旧值
// （contracts/data-contract.md §6 + behavior-contract.md §6）。
func (s *nodeSQLStore) mergeDriverConfig(ctx context.Context, id uint64, patch map[string]any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT driver_config FROM nodes WHERE id = ? FOR UPDATE", id).Scan(&raw); err != nil {
		return err
	}

	current := ordObj{}
	if raw.Valid && raw.String != "" {
		parsed, err := ordParse([]byte(raw.String))
		if err == nil {
			if obj, ok := parsed.(ordObj); ok {
				current = obj
			}
		}
	}

	// PHP 的 array_replace_recursive：同键值对象递归合并，其余覆盖。
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		current = mergeConfigValue(current, k, patch[k])
	}

	body, err := ordEncode(current)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET driver_config = ?, updated_at = ? WHERE id = ?",
		string(body), s.now().UTC().Truncate(time.Second), id); err != nil {
		return err
	}
	return tx.Commit()
}

// mergeConfigValue 复刻 array_replace_recursive 的单键合并。
func mergeConfigValue(current ordObj, key string, value any) ordObj {
	existing, has := current.Get(key)
	if !has {
		current.Set(key, value)
		return current
	}
	existingObj, ok1 := existing.(ordObj)
	patchObj, ok2 := toPHPObject(value)
	if !ok1 || !ok2 {
		current.Set(key, value)
		return current
	}
	for _, m := range patchObj {
		existingObj = mergeConfigValue(existingObj, m.Key, m.Value)
	}
	current.Set(key, existingObj)
	return current
}

// toPHPObject 把 map[string]any / ordObj 统一成保序对象。
func toPHPObject(v any) (ordObj, bool) {
	switch t := v.(type) {
	case ordObj:
		return t, true
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := ordObj{}
		for _, k := range keys {
			out = append(out, ordMember{Key: k, Value: t[k]})
		}
		return out, true
	}
	return nil, false
}

// BumpConfigVersion 复刻 Node::bumpXrayConfigVersion()：
// 行锁内重读再递增，并写 config_changed_at（节点页据此显示「配置滞后 N 秒」）。
func (s *nodeSQLStore) BumpConfigVersion(ctx context.Context, id uint64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT driver_config FROM nodes WHERE id = ? FOR UPDATE", id).Scan(&raw); err != nil {
		return 0, err
	}

	current := ordObj{}
	if raw.Valid && raw.String != "" {
		parsed, err := ordParse([]byte(raw.String))
		if err == nil {
			if obj, ok := parsed.(ordObj); ok {
				current = obj
			}
		}
	}
	version := driverConfigInt(rawBytesOf(raw), "config_version") + 1
	current.Set("config_version", version)
	current.Set("config_changed_at", s.now().UTC().Format("2006-01-02T15:04:05+00:00"))

	body, err := ordEncode(current)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET driver_config = ?, updated_at = ? WHERE id = ?",
		string(body), s.now().UTC().Truncate(time.Second), id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return version, nil
}

func rawBytesOf(v sql.NullString) []byte {
	if !v.Valid || v.String == "" {
		return nil
	}
	return []byte(v.String)
}

func (s *nodeSQLStore) UpdateNodeStatus(ctx context.Context, id uint64, status string, latency int64, touchCheckAt bool) error {
	now := s.now().UTC().Truncate(time.Second)
	if touchCheckAt {
		_, err := s.db.ExecContext(ctx,
			"UPDATE nodes SET status = ?, latency = ?, last_check_at = ?, updated_at = ? WHERE id = ?",
			status, latency, now, now, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE nodes SET status = ?, latency = ?, updated_at = ? WHERE id = ?",
		status, latency, now, id)
	return err
}

// ---- 用户口径 ----

// EligibleProtocolCounts 复刻 XrayConfigService::eligibleUsers() 的协议分布
// （present 里的 client_count：有套餐 + 启用 + 未被关闭流量）。
func (s *nodeSQLStore) EligibleProtocolCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT protocol, COUNT(*) FROM users WHERE plan_id IS NOT NULL AND enabled = 1 "+
			"AND traffic_disabled_at IS NULL GROUP BY protocol")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var proto sql.NullString
		var n int64
		if err := rows.Scan(&proto, &n); err != nil {
			return nil, err
		}
		out[proto.String] = n
	}
	return out, rows.Err()
}

// AllUserIDs 返回**套餐现在还能用**的用户 id（建号任务的候选集）。
//
// 「还能用」= 有套餐 + 未过期 + 总量未超 + 当月未超，与封禁扫描器 banReasonAt 同判据。
// 以前这里取全部用户：几万个过期/无套餐的人也被拆成 item 派下去，每个都要一次建号判断，
// 白白浪费队列吞吐（线上表现为「新接节点初始化要跑很久」）。
// 现在只取真正会建号的人；用户续费/重置流量后再跑初始化仍会被重新算进来。
func (s *nodeSQLStore) AllUserIDs(ctx context.Context) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id FROM users WHERE plan_id IS NOT NULL AND enabled = 1 "+
			"AND (expired_at IS NULL OR expired_at > NOW()) "+
			"AND (monthly_traffic_limit = 0 OR monthly_traffic_used < monthly_traffic_limit) "+
			"AND (traffic_limit = 0 OR traffic_used < traffic_limit) "+
			"ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) UserIDsWithPlanByProtocols(ctx context.Context, protocols []string) ([]uint64, error) {
	// 有套餐的人全都要同步到新入站：用户全协议通用，不再按 protocol 过滤
	// （按协议过滤会让「新加的 hy2 入站」只同步 vless 用户，其余人永远缺挂载）。
	rows, err := s.db.QueryContext(ctx,
		"SELECT id FROM users WHERE plan_id IS NOT NULL ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- 异步任务 ----

func (s *nodeSQLStore) ActiveNodeInitTask(ctx context.Context, nodeID uint64) (*NodeTaskRecord, error) {
	var t NodeTaskRecord
	var meta sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT id, type, status, total, failed, meta, max_attempts FROM async_tasks "+
			"WHERE type = 'node_init' AND subject_type = 'node' AND subject_id = ? "+
			"AND status IN ('pending','running') ORDER BY id DESC LIMIT 1", nodeID).
		Scan(&t.ID, &t.Type, &t.Status, &t.Total, &t.Failed, &meta, &t.MaxTries)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if meta.Valid {
		t.Meta = []byte(meta.String)
	}
	return &t, nil
}

func (s *nodeSQLStore) CreateTask(ctx context.Context, in TaskCreateInput) (*NodeTaskRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now().UTC().Truncate(time.Second)
	var metaVal any
	if len(in.Meta) > 0 {
		metaVal = string(in.Meta)
	}
	maxAttempts := in.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 3
	}

	res, err := tx.ExecContext(ctx,
		"INSERT INTO async_tasks (type, status, subject_type, subject_id, total, completed, failed, "+
			"attempts, max_attempts, meta, created_at, updated_at) "+
			"VALUES (?, 'pending', ?, ?, 0, 0, 0, 0, ?, ?, ?, ?)",
		in.Type, in.SubjectType, in.SubjectID, maxAttempts, metaVal, now, now)
	if err != nil {
		return nil, err
	}
	taskID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	total := 0
	for _, key := range in.ItemKeys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		// insertOrIgnore 对应唯一键 (task_id,item_key) 的冲突语义。
		if _, err := tx.ExecContext(ctx,
			"INSERT IGNORE INTO async_task_items (task_id, item_key, status, attempts, created_at, updated_at) "+
				"VALUES (?, ?, 'pending', 0, ?, ?)", taskID, key, now, now); err != nil {
			return nil, err
		}
		total++
	}
	if _, err := tx.ExecContext(ctx, "UPDATE async_tasks SET total = ? WHERE id = ?", total, taskID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &NodeTaskRecord{
		ID: uint64(taskID), Type: in.Type, Status: "pending", Total: int64(total), MaxTries: maxAttempts,
		Meta: in.Meta,
	}, nil
}

func (s *nodeSQLStore) UpdateTaskDesiredEnabled(ctx context.Context, taskID, nodeID uint64, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	var status string
	if err := tx.QueryRowContext(ctx,
		"SELECT meta, status FROM async_tasks WHERE id = ? FOR UPDATE", taskID).Scan(&raw, &status); err != nil {
		return err
	}
	if status != "pending" && status != "running" {
		return nil
	}

	meta := ordObj{}
	if raw.Valid && raw.String != "" {
		if parsed, err := ordParse([]byte(raw.String)); err == nil {
			if obj, ok := parsed.(ordObj); ok {
				meta = obj
			}
		}
	}
	meta.Set("should_enable", enabled)
	body, err := ordEncode(meta)
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx, "UPDATE async_tasks SET meta = ?, updated_at = ? WHERE id = ?",
		string(body), now, taskID); err != nil {
		return err
	}
	// NodeInitService::updateDesiredEnabled 同时把节点压成 disabled/offline。
	if _, err := tx.ExecContext(ctx,
		"UPDATE nodes SET enabled = 0, status = 'offline', updated_at = ? WHERE id = ?", now, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *nodeSQLStore) CompleteEmptyTask(ctx context.Context, taskID uint64) (*NodeTaskRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE async_tasks SET status = 'succeeded', total = 0, completed = 0, failed = 0, error = NULL, "+
			"finished_at = ?, updated_at = ? WHERE id = ?", now, now, taskID); err != nil {
		return nil, err
	}
	return &NodeTaskRecord{ID: taskID, Status: "succeeded", Total: 0}, nil
}

// FinalizeInitTask 复刻 NodeInitService::maybeFinalize 的核心判定：
// 全部 item 终态后，成功才按 should_enable 启用；有失败只标 offline（绝不改 enabled）。
func (s *nodeSQLStore) FinalizeInitTask(ctx context.Context, nodeID, taskID uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status, rawMeta sql.NullString
	var failed int64
	if err := tx.QueryRowContext(ctx,
		"SELECT status, meta, failed FROM async_tasks WHERE id = ? FOR UPDATE", taskID).
		Scan(&status, &rawMeta, &failed); err != nil {
		return err
	}

	var active int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM async_task_items WHERE task_id = ? AND status IN ('pending','running')", taskID).
		Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return nil
	}

	now := s.now().UTC().Truncate(time.Second)
	if status.String != "succeeded" || failed > 0 {
		_, err := tx.ExecContext(ctx, "UPDATE nodes SET status = 'offline', updated_at = ? WHERE id = ?", now, nodeID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}

	shouldEnable := false
	if rawMeta.Valid && rawMeta.String != "" {
		if parsed, err := ordParse([]byte(rawMeta.String)); err == nil {
			if obj, ok := parsed.(ordObj); ok {
				if v, exists := obj.Get("should_enable"); exists {
					shouldEnable = ordBool(v)
				}
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE nodes SET enabled = ?, status = 'offline', updated_at = ? WHERE id = ?",
		boolToInt(shouldEnable), now, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- 节点指令 ----

func (s *nodeSQLStore) CreateCommand(ctx context.Context, nodeID uint64, action string, payload any) (*CommandRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	var payloadVal any
	if payload != nil {
		body, err := ordEncode(toPHPOrdered(payload))
		if err != nil {
			return nil, err
		}
		payloadVal = string(body)
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at) "+
			"VALUES (?, NULL, ?, ?, 'pending', ?, ?)", nodeID, action, payloadVal, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &CommandRecord{ID: uint64(id), NodeID: nodeID, Action: action, Status: "pending"}, nil
}

// toPHPOrdered 把 admin 的保序结构转成驱动层的保序结构（编码用）。
func toPHPOrdered(v any) any {
	switch t := v.(type) {
	case *jsObj:
		out := ordObj{}
		for i, k := range t.keys {
			out = append(out, ordMember{Key: k, Value: toPHPOrdered(t.vals[i])})
		}
		return out
	case jsArr:
		out := ordArr{}
		for _, item := range t {
			out = append(out, toPHPOrdered(item))
		}
		return out
	}
	return v
}

func (s *nodeSQLStore) CommandByID(ctx context.Context, id uint64) (*CommandRecord, error) {
	var c CommandRecord
	var result sql.NullString
	if err := s.db.QueryRowContext(ctx,
		"SELECT id, node_id, action, status, result FROM node_api_commands WHERE id = ?", id).
		Scan(&c.ID, &c.NodeID, &c.Action, &c.Status, &result); err != nil {
		return nil, err
	}
	c.Result = nullStrPtr(result)
	return &c, nil
}

func (s *nodeSQLStore) HasInFlightUpgrade(ctx context.Context, nodeID uint64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM node_api_commands WHERE node_id = ? AND action = 'upgrade' "+
			"AND status IN ('pending','acknowledged')", nodeID).Scan(&n)
	return n > 0, err
}

// ---- xray 入站 ----

const xrayInboundColumns = `id, node_id, tag, remark, protocol, port, listen, settings, stream_settings,
	sniffing, reality_private_key, salamander_password, enabled, sort, created_at, updated_at`

func (s *nodeSQLStore) scanXrayInbound(row interface{ Scan(...any) error }) (*XrayInboundRecord, error) {
	var ib XrayInboundRecord
	var remark, settings, stream, sniffing, privateKey, salamander sql.NullString
	var enabled int
	var createdAt, updatedAt sql.NullTime
	if err := row.Scan(&ib.ID, &ib.NodeID, &ib.Tag, &remark, &ib.Protocol, &ib.Port, &ib.Listen,
		&settings, &stream, &sniffing, &privateKey, &salamander, &enabled, &ib.Sort, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	ib.Remark = nullStrPtr(remark)
	ib.Settings = rawBytesOf(settings)
	ib.StreamSettings = rawBytesOf(stream)
	ib.Sniffing = rawBytesOf(sniffing)
	ib.RealityPrivateKey = s.decryptOrRaw(privateKey)
	ib.SalamanderPassword = s.decryptOrRaw(salamander)
	ib.Enabled = enabled != 0
	ib.CreatedAt = nullTimePtr(createdAt)
	ib.UpdatedAt = nullTimePtr(updatedAt)
	return &ib, nil
}

func (s *nodeSQLStore) XrayInboundsByNode(ctx context.Context, nodeID uint64) ([]XrayInboundRecord, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+xrayInboundColumns+
		" FROM xray_inbounds WHERE node_id = ? ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []XrayInboundRecord{}
	for rows.Next() {
		ib, err := s.scanXrayInbound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ib)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) XrayInboundByID(ctx context.Context, nodeID, id uint64) (*XrayInboundRecord, error) {
	return s.scanXrayInbound(s.db.QueryRowContext(ctx, "SELECT "+xrayInboundColumns+
		" FROM xray_inbounds WHERE node_id = ? AND id = ?", nodeID, id))
}

func (s *nodeSQLStore) XrayInboundTagTaken(ctx context.Context, nodeID uint64, tag string, excludeID *uint64) (bool, error) {
	query := "SELECT COUNT(*) FROM xray_inbounds WHERE node_id = ? AND tag = ?"
	args := []any{nodeID, tag}
	if excludeID != nil {
		query += " AND id <> ?"
		args = append(args, *excludeID)
	}
	var n int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n > 0, err
}

// XrayInboundPortTaken 判断同节点内「传输族有交集」的启用入站是否已占用该端口。
//
// 必须按 TCP/UDP 分家：443/tcp 的 vless 与 443/udp 的 hysteria 在同一个 socket 空间里
// 互不冲突（内核各绑各的协议栈），只比端口数字会误报「端口已被占用」。
// 反过来，tunnel 的 settings.network=tcp,udp 会同时占两族，只比单族会漏报。
func (s *nodeSQLStore) XrayInboundPortTaken(ctx context.Context, nodeID uint64, port int64, families []string, excludeID *uint64) (bool, error) {
	query := "SELECT protocol, settings FROM xray_inbounds WHERE node_id = ? AND port = ? AND enabled = 1"
	args := []any{nodeID, port}
	if excludeID != nil {
		query += " AND id <> ?"
		args = append(args, *excludeID)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			protocol string
			settings sql.NullString
		)
		if err := rows.Scan(&protocol, &settings); err != nil {
			return false, err
		}
		occupied := inboundFamilies(protocol, settings.String)
		for _, family := range families {
			for _, has := range occupied {
				if has == family {
					return true, nil
				}
			}
		}
	}
	return false, rows.Err()
}

// inboundFamilies 返回入站在内核里实际占用的传输族（tcp / udp，可能两个都占）。
//
//   - hysteria / wireguard / amneziawg 恒为 UDP（QUIC / 自有 UDP 栈，与 streamSettings.network 无关）；
//   - tunnel 是 dokodemo-door，L4 网络在 settings.network（tcp / udp / tcp,udp）——
//     写进 streamSettings.network 会被内核直接拒（unknown transport protocol: udp）；
//   - 其余（vless / vmess / trojan / shadowsocks / socks / http）走 TCP。
func inboundFamilies(protocol, settings string) []string {
	network := ""
	if protocol == "tunnel" {
		var s struct {
			Network string `json:"network"`
		}
		if err := json.Unmarshal([]byte(settings), &s); err == nil {
			network = s.Network
		}
	}
	return inboundFamiliesOf(protocol, network)
}

// inboundFamiliesOf 是 family 判定的纯函数版：network 只对 tunnel 有意义
// （建入站时取负载里的 tunnel_network）。
func inboundFamiliesOf(protocol, network string) []string {
	switch protocol {
	case "hysteria", "wireguard", "amneziawg":
		return []string{"udp"}
	case "tunnel":
		switch network {
		case "udp":
			return []string{"udp"}
		case "tcp,udp", "udp,tcp":
			return []string{"tcp", "udp"}
		default:
			return []string{"tcp"}
		}
	default:
		return []string{"tcp"}
	}
}

// XrayCertificates 读 TLS 证书库全表（证书库是人工维护的小表）。
func (s *nodeSQLStore) XrayCertificates(ctx context.Context) ([]XrayCertificate, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, domain, cert_pem, key_enc, not_after FROM xray_certificates ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []XrayCertificate{}
	for rows.Next() {
		var (
			c        XrayCertificate
			domain   sql.NullString
			certPEM  sql.NullString
			keyEnc   sql.NullString
			notAfter sql.NullTime
		)
		if err := rows.Scan(&c.ID, &c.Name, &domain, &certPEM, &keyEnc, &notAfter); err != nil {
			return nil, err
		}
		c.Domain = nullStrPtr(domain)
		c.CertPEM = certPEM.String
		c.KeyEnc = keyEnc.String
		if notAfter.Valid {
			t := notAfter.Time
			c.NotAfter = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateXrayCertificate 写一条证书（私钥加密落 key_enc）。
//
// not_after 由 cert_pem 解析得出（页面上显示「还剩 N 天」）；解析不了就留空，
// 不阻塞保存 —— 证书链里第一张叶子证书才是有效期来源。
func (s *nodeSQLStore) CreateXrayCertificate(ctx context.Context, in XrayCertificateInput) (*XrayCertificate, error) {
	keyEnc, err := s.encryptString(in.KeyPEM)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	var notAfter any
	if t := certificateNotAfter(in.CertPEM); t != nil {
		notAfter = t.UTC().Truncate(time.Second)
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_certificates (name, domain, cert_pem, key_enc, not_after, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		in.Name, nullStrValue(in.Domain), in.CertPEM, keyEnc, notAfter, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	all, err := s.XrayCertificates(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == uint64(id) {
			return &all[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

// UpdateXrayCertificate 覆盖一条证书的 PEM 与 not_after（本机目录同步用）。
//
// 私钥同样重新加密写 key_enc（换证书就是换私钥，不能留着旧密文）；
// domain 传 nil 时保持原值（COALESCE），免得「只想刷新 PEM」把域名抹掉。
func (s *nodeSQLStore) UpdateXrayCertificate(ctx context.Context, id uint64, in XrayCertificateInput) error {
	keyEnc, err := s.encryptString(in.KeyPEM)
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Second)
	var notAfter any
	if t := certificateNotAfter(in.CertPEM); t != nil {
		notAfter = t.UTC().Truncate(time.Second)
	}
	_, err = s.db.ExecContext(ctx,
		"UPDATE xray_certificates SET domain = COALESCE(?, domain), cert_pem = ?, key_enc = ?, "+
			"not_after = ?, updated_at = ? WHERE id = ?",
		nullStrValue(in.Domain), in.CertPEM, keyEnc, notAfter, now, id)
	return err
}

func (s *nodeSQLStore) DeleteXrayCertificate(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM xray_certificates WHERE id = ?", id)
	return err
}

// certificateNotAfter 取 PEM 里第一张证书的 NotAfter；解析失败 → nil。
func certificateNotAfter(pemText string) *time.Time {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	t := cert.NotAfter
	return &t
}

func (s *nodeSQLStore) XrayInboundTags(ctx context.Context, nodeID uint64, excludeID *uint64) ([]string, error) {
	query := "SELECT tag FROM xray_inbounds WHERE node_id = ?"
	args := []any{nodeID}
	if excludeID != nil {
		query += " AND id <> ?"
		args = append(args, *excludeID)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) CreateXrayInbound(ctx context.Context, in XrayInboundInput) (*XrayInboundRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	enabled := 1
	if in.Enabled != nil {
		enabled = boolToInt(*in.Enabled)
	}
	var sortVal int64
	if in.Sort != nil {
		sortVal = *in.Sort
	}
	encryptedKey, err := s.encryptOptional(in.RealityPrivateKey)
	if err != nil {
		return nil, err
	}
	encryptedSalamander, err := s.encryptOptional(in.SalamanderPassword)
	if err != nil {
		return nil, err
	}

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_inbounds (node_id, tag, remark, protocol, port, listen, settings, stream_settings, "+
			"sniffing, reality_private_key, salamander_password, enabled, sort, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.NodeID, in.Tag, nullStrValue(in.Remark), in.Protocol, in.Port, in.Listen,
		jsonTextValue(in.Settings), jsonTextValue(in.StreamSettings), jsonTextValue(in.Sniffing),
		nullStrValue(encryptedKey), nullStrValue(encryptedSalamander), enabled, sortVal, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.XrayInboundByID(ctx, in.NodeID, uint64(id))
}

func (s *nodeSQLStore) UpdateXrayInbound(ctx context.Context, id uint64, in XrayInboundInput) (*XrayInboundRecord, error) {
	sets := []string{"tag = ?", "remark = ?", "protocol = ?", "port = ?", "listen = ?",
		"settings = ?", "stream_settings = ?", "sniffing = ?"}
	args := []any{in.Tag, nullStrValue(in.Remark), in.Protocol, in.Port, in.Listen,
		jsonTextValue(in.Settings), jsonTextValue(in.StreamSettings), jsonTextValue(in.Sniffing)}

	if in.RealityKeySet {
		encryptedKey, err := s.encryptOptional(in.RealityPrivateKey)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "reality_private_key = ?")
		args = append(args, nullStrValue(encryptedKey))
	}
	if in.SalamanderPasswordSet {
		encryptedSalamander, err := s.encryptOptional(in.SalamanderPassword)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "salamander_password = ?")
		args = append(args, nullStrValue(encryptedSalamander))
	}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if in.Sort != nil {
		sets = append(sets, "sort = ?")
		args = append(args, *in.Sort)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, s.now().UTC().Truncate(time.Second), id)

	if _, err := s.db.ExecContext(ctx, "UPDATE xray_inbounds SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	var nodeID uint64
	if err := s.db.QueryRowContext(ctx, "SELECT node_id FROM xray_inbounds WHERE id = ?", id).Scan(&nodeID); err != nil {
		return nil, err
	}
	return s.XrayInboundByID(ctx, nodeID, id)
}

func (s *nodeSQLStore) DeleteXrayInbound(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM xray_inbounds WHERE id = ?", id)
	return err
}

// jsonTextValue 把 JSON 字节转成列值（空 → NULL，与 PHP 的 null 列一致）。
func jsonTextValue(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// ---- xray 出站 ----

const xrayOutboundColumns = `id, node_id, tag, protocol, settings, stream_settings, secret_key,
	enabled, sort, remark, relay_target_node_id, relay_target_inbound_id, created_at, updated_at`

func (s *nodeSQLStore) scanXrayOutbound(row interface{ Scan(...any) error }) (*XrayOutboundRecord, error) {
	var ob XrayOutboundRecord
	var settings, stream, secretKey, remark sql.NullString
	var relayNode, relayInbound sql.NullInt64
	var enabled int
	var createdAt, updatedAt sql.NullTime
	if err := row.Scan(&ob.ID, &ob.NodeID, &ob.Tag, &ob.Protocol, &settings, &stream, &secretKey,
		&enabled, &ob.Sort, &remark, &relayNode, &relayInbound, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	ob.Settings = rawBytesOf(settings)
	ob.StreamSettings = rawBytesOf(stream)
	ob.SecretKey = s.decryptOrRaw(secretKey)
	ob.Enabled = enabled != 0
	ob.Remark = nullStrPtr(remark)
	ob.RelayTargetNodeID = nullUintPtr(relayNode)
	ob.RelayTargetInboundID = nullUintPtr(relayInbound)
	ob.CreatedAt = nullTimePtr(createdAt)
	ob.UpdatedAt = nullTimePtr(updatedAt)
	return &ob, nil
}

func (s *nodeSQLStore) XrayOutboundsByNode(ctx context.Context, nodeID uint64, enabledOnly bool) ([]XrayOutboundRecord, error) {
	query := "SELECT " + xrayOutboundColumns + " FROM xray_outbounds WHERE node_id = ?"
	if enabledOnly {
		query += " AND enabled = 1"
	}
	query += " ORDER BY sort, id"
	rows, err := s.db.QueryContext(ctx, query, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []XrayOutboundRecord{}
	for rows.Next() {
		ob, err := s.scanXrayOutbound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ob)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) XrayOutboundByID(ctx context.Context, nodeID, id uint64) (*XrayOutboundRecord, error) {
	return s.scanXrayOutbound(s.db.QueryRowContext(ctx, "SELECT "+xrayOutboundColumns+
		" FROM xray_outbounds WHERE node_id = ? AND id = ?", nodeID, id))
}

func (s *nodeSQLStore) XrayOutboundByTag(ctx context.Context, nodeID uint64, tag string) (*XrayOutboundRecord, error) {
	ob, err := s.scanXrayOutbound(s.db.QueryRowContext(ctx, "SELECT "+xrayOutboundColumns+
		" FROM xray_outbounds WHERE node_id = ? AND tag = ?", nodeID, tag))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return ob, err
}

func (s *nodeSQLStore) RelayOutboundByTarget(ctx context.Context, sourceNodeID, targetNodeID, targetInboundID uint64) (*XrayOutboundRecord, error) {
	ob, err := s.scanXrayOutbound(s.db.QueryRowContext(ctx, "SELECT "+xrayOutboundColumns+
		" FROM xray_outbounds WHERE node_id = ? AND relay_target_node_id = ? AND relay_target_inbound_id = ?",
		sourceNodeID, targetNodeID, targetInboundID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return ob, err
}

func (s *nodeSQLStore) XrayOutboundTagTaken(ctx context.Context, nodeID uint64, tag string, excludeID *uint64) (bool, error) {
	query := "SELECT COUNT(*) FROM xray_outbounds WHERE node_id = ? AND tag = ?"
	args := []any{nodeID, tag}
	if excludeID != nil {
		query += " AND id <> ?"
		args = append(args, *excludeID)
	}
	var n int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n > 0, err
}

func (s *nodeSQLStore) XrayOutboundTags(ctx context.Context, nodeID uint64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT tag FROM xray_outbounds WHERE node_id = ? ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) CreateXrayOutbound(ctx context.Context, in XrayOutboundInput) (*XrayOutboundRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	encryptedKey, err := s.encryptOptional(in.SecretKey)
	if err != nil {
		return nil, err
	}
	enabled := 1
	if in.Enabled != nil {
		enabled = boolToInt(*in.Enabled)
	}
	var sortVal int64
	if in.Sort != nil {
		sortVal = *in.Sort
	}

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_outbounds (node_id, tag, protocol, settings, stream_settings, secret_key, "+
			"enabled, sort, remark, relay_target_node_id, relay_target_inbound_id, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.NodeID, in.Tag, in.Protocol, jsonTextValue(in.Settings), jsonTextValue(in.StreamSettings),
		nullStrValue(encryptedKey), enabled, sortVal, nullStrValue(in.Remark),
		nullUintValue(in.RelayTargetNodeID), nullUintValue(in.RelayTargetInboundID), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.XrayOutboundByID(ctx, in.NodeID, uint64(id))
}

func (s *nodeSQLStore) UpdateXrayOutbound(ctx context.Context, id uint64, in XrayOutboundInput) (*XrayOutboundRecord, error) {
	sets := []string{"tag = ?", "protocol = ?", "settings = ?", "stream_settings = ?", "remark = ?"}
	args := []any{in.Tag, in.Protocol, jsonTextValue(in.Settings), jsonTextValue(in.StreamSettings),
		nullStrValue(in.Remark)}

	if in.SecretKeySet {
		encryptedKey, err := s.encryptOptional(in.SecretKey)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "secret_key = ?")
		args = append(args, nullStrValue(encryptedKey))
	}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if in.Sort != nil {
		sets = append(sets, "sort = ?")
		args = append(args, *in.Sort)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, s.now().UTC().Truncate(time.Second), id)

	if _, err := s.db.ExecContext(ctx, "UPDATE xray_outbounds SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	var nodeID uint64
	if err := s.db.QueryRowContext(ctx, "SELECT node_id FROM xray_outbounds WHERE id = ?", id).Scan(&nodeID); err != nil {
		return nil, err
	}
	return s.XrayOutboundByID(ctx, nodeID, id)
}

func (s *nodeSQLStore) DeleteXrayOutbound(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM xray_outbounds WHERE id = ?", id)
	return err
}

// UpsertRelayOutbound 按幂等键（node_id + relay_target_*，或 node_id + tag 的 warp）落库。
//
// 幂等键与 PHP 一致：
//   - 中转出站：同一「源节点 × 目标节点 × 目标入站」恒为一条
//   - WARP 出站：node_id + tag='warp'
func (s *nodeSQLStore) UpsertRelayOutbound(ctx context.Context, in XrayOutboundInput) (*XrayOutboundRecord, error) {
	var existing *XrayOutboundRecord
	var err error
	if in.RelayTargetNodeID != nil && in.RelayTargetInboundID != nil {
		existing, err = s.RelayOutboundByTarget(ctx, in.NodeID, *in.RelayTargetNodeID, *in.RelayTargetInboundID)
	} else {
		existing, err = s.XrayOutboundByTag(ctx, in.NodeID, in.Tag)
	}
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return s.CreateXrayOutbound(ctx, in)
	}

	// 命中已有行时仍回写 settings/stream/remark：目标入站的凭据、reality/xhttp 参数
	// 可能在首次保存后才补齐或被改动，沿用旧值会让中转一直握着不含 uuid 的出站。
	update := in
	update.Tag = existing.Tag
	if in.SecretKey == nil {
		update.SecretKeySet = existing.SecretKey != nil
		update.SecretKey = existing.SecretKey
	}
	if in.RelayTargetNodeID == nil {
		update.RelayTargetNodeID = existing.RelayTargetNodeID
		update.RelayTargetInboundID = existing.RelayTargetInboundID
	}
	return s.UpdateXrayOutbound(ctx, existing.ID, update)
}

func (s *nodeSQLStore) OutboundTraffic(ctx context.Context, nodeID uint64) (map[string]OutboundTraffic, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT tag, upload, download, updated_at FROM xray_outbound_traffic WHERE node_id = ?", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]OutboundTraffic{}
	for rows.Next() {
		var tag string
		var t OutboundTraffic
		var updatedAt sql.NullString
		if err := rows.Scan(&tag, &t.Upload, &t.Download, &updatedAt); err != nil {
			return nil, err
		}
		if updatedAt.Valid {
			v := updatedAt.String
			t.UpdatedAt = &v
		}
		out[tag] = t
	}
	return out, rows.Err()
}

// ---- 路由规则 ----

const routingRuleColumns = `id, node_id, domains, ips, port, network, protocol, inbound_tag,
	outbound_tag, remark, enabled, sort`

func scanRoutingRule(row interface{ Scan(...any) error }) (*RoutingRuleRecord, error) {
	var r RoutingRuleRecord
	var domains, ips, port, network, protocol, inboundTag, remark sql.NullString
	var enabled int
	if err := row.Scan(&r.ID, &r.NodeID, &domains, &ips, &port, &network, &protocol, &inboundTag,
		&r.OutboundTag, &remark, &enabled, &r.Sort); err != nil {
		return nil, err
	}
	r.Domains = rawBytesOf(domains)
	r.IPs = rawBytesOf(ips)
	r.Port = nullStrPtr(port)
	r.Network = nullStrPtr(network)
	r.Protocol = nullStrPtr(protocol)
	r.InboundTag = nullStrPtr(inboundTag)
	r.Remark = nullStrPtr(remark)
	r.Enabled = enabled != 0
	return &r, nil
}

func (s *nodeSQLStore) RoutingRulesByNode(ctx context.Context, nodeID uint64) ([]RoutingRuleRecord, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+routingRuleColumns+
		" FROM xray_routing_rules WHERE node_id = ? ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoutingRuleRecord{}
	for rows.Next() {
		r, err := scanRoutingRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) RoutingRuleByID(ctx context.Context, nodeID, id uint64) (*RoutingRuleRecord, error) {
	return scanRoutingRule(s.db.QueryRowContext(ctx, "SELECT "+routingRuleColumns+
		" FROM xray_routing_rules WHERE node_id = ? AND id = ?", nodeID, id))
}

func (s *nodeSQLStore) CreateRoutingRule(ctx context.Context, in RoutingRuleInput) (*RoutingRuleRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	enabled := 1
	if in.Enabled != nil {
		enabled = boolToInt(*in.Enabled)
	}
	var sortVal int64
	if in.Sort != nil {
		sortVal = *in.Sort
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_routing_rules (node_id, domains, ips, port, network, protocol, inbound_tag, "+
			"outbound_tag, remark, enabled, sort, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.NodeID, jsonTextValue(in.Domains), jsonTextValue(in.IPs), nullStrValue(in.Port),
		nullStrValue(in.Network), nullStrValue(in.Protocol), nullStrValue(in.InboundTag),
		in.OutboundTag, nullStrValue(in.Remark), enabled, sortVal, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.RoutingRuleByID(ctx, in.NodeID, uint64(id))
}

func (s *nodeSQLStore) UpdateRoutingRule(ctx context.Context, id uint64, in RoutingRuleInput) (*RoutingRuleRecord, error) {
	sets := []string{"domains = ?", "ips = ?", "port = ?", "network = ?", "protocol = ?",
		"inbound_tag = ?", "outbound_tag = ?", "remark = ?"}
	args := []any{jsonTextValue(in.Domains), jsonTextValue(in.IPs), nullStrValue(in.Port),
		nullStrValue(in.Network), nullStrValue(in.Protocol), nullStrValue(in.InboundTag),
		in.OutboundTag, nullStrValue(in.Remark)}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if in.Sort != nil {
		sets = append(sets, "sort = ?")
		args = append(args, *in.Sort)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, s.now().UTC().Truncate(time.Second), id)

	if _, err := s.db.ExecContext(ctx, "UPDATE xray_routing_rules SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	var nodeID uint64
	if err := s.db.QueryRowContext(ctx, "SELECT node_id FROM xray_routing_rules WHERE id = ?", id).Scan(&nodeID); err != nil {
		return nil, err
	}
	return s.RoutingRuleByID(ctx, nodeID, id)
}

func (s *nodeSQLStore) DeleteRoutingRule(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM xray_routing_rules WHERE id = ?", id)
	return err
}

// ---- 负载均衡器 ----

const balancerColumns = `id, node_id, tag, selectors, strategy, fallback_tag, remark, enabled, sort`

func scanBalancer(row interface{ Scan(...any) error }) (*BalancerRecord, error) {
	var b BalancerRecord
	var selectors, fallback, remark sql.NullString
	var enabled int
	if err := row.Scan(&b.ID, &b.NodeID, &b.Tag, &selectors, &b.Strategy, &fallback, &remark, &enabled, &b.Sort); err != nil {
		return nil, err
	}
	b.Selectors = rawBytesOf(selectors)
	b.FallbackTag = nullStrPtr(fallback)
	b.Remark = nullStrPtr(remark)
	b.Enabled = enabled != 0
	return &b, nil
}

func (s *nodeSQLStore) BalancersByNode(ctx context.Context, nodeID uint64, enabledOnly bool) ([]BalancerRecord, error) {
	query := "SELECT " + balancerColumns + " FROM xray_balancers WHERE node_id = ?"
	if enabledOnly {
		query += " AND enabled = 1"
	}
	query += " ORDER BY sort, id"
	rows, err := s.db.QueryContext(ctx, query, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BalancerRecord{}
	for rows.Next() {
		b, err := scanBalancer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) BalancerByID(ctx context.Context, nodeID, id uint64) (*BalancerRecord, error) {
	return scanBalancer(s.db.QueryRowContext(ctx, "SELECT "+balancerColumns+
		" FROM xray_balancers WHERE node_id = ? AND id = ?", nodeID, id))
}

func (s *nodeSQLStore) BalancerTagTaken(ctx context.Context, nodeID uint64, tag string, excludeID *uint64) (bool, error) {
	query := "SELECT COUNT(*) FROM xray_balancers WHERE node_id = ? AND tag = ?"
	args := []any{nodeID, tag}
	if excludeID != nil {
		query += " AND id <> ?"
		args = append(args, *excludeID)
	}
	var n int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n > 0, err
}

func (s *nodeSQLStore) CreateBalancer(ctx context.Context, in BalancerInput) (*BalancerRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	enabled := 1
	if in.Enabled != nil {
		enabled = boolToInt(*in.Enabled)
	}
	var sortVal int64
	if in.Sort != nil {
		sortVal = *in.Sort
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_balancers (node_id, tag, selectors, strategy, fallback_tag, remark, enabled, sort, "+
			"created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.NodeID, in.Tag, jsonTextValue(in.Selectors), in.Strategy, nullStrValue(in.FallbackTag),
		nullStrValue(in.Remark), enabled, sortVal, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.BalancerByID(ctx, in.NodeID, uint64(id))
}

func (s *nodeSQLStore) UpdateBalancer(ctx context.Context, id uint64, in BalancerInput) (*BalancerRecord, error) {
	sets := []string{"tag = ?", "selectors = ?", "strategy = ?", "fallback_tag = ?", "remark = ?"}
	args := []any{in.Tag, jsonTextValue(in.Selectors), in.Strategy, nullStrValue(in.FallbackTag),
		nullStrValue(in.Remark)}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if in.Sort != nil {
		sets = append(sets, "sort = ?")
		args = append(args, *in.Sort)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, s.now().UTC().Truncate(time.Second), id)

	if _, err := s.db.ExecContext(ctx, "UPDATE xray_balancers SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	var nodeID uint64
	if err := s.db.QueryRowContext(ctx, "SELECT node_id FROM xray_balancers WHERE id = ?", id).Scan(&nodeID); err != nil {
		return nil, err
	}
	return s.BalancerByID(ctx, nodeID, id)
}

func (s *nodeSQLStore) DeleteBalancer(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM xray_balancers WHERE id = ?", id)
	return err
}

// ---- 中转凭据 ----

func (s *nodeSQLStore) RelayClientBySource(ctx context.Context, targetNodeID, inboundID, sourceNodeID uint64) (*RelayClientRecord, error) {
	var rc RelayClientRecord
	var sourceNode sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, node_id, inbound_id, email, client_id, source_node_id FROM xray_relay_clients "+
			"WHERE node_id = ? AND inbound_id = ? AND source_node_id = ? LIMIT 1",
		targetNodeID, inboundID, sourceNodeID).Scan(&rc.ID, &rc.NodeID, &rc.InboundID, &rc.Email, &rc.ClientID, &sourceNode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rc.SourceNodeID = nullUintPtr(sourceNode)
	return &rc, nil
}

func (s *nodeSQLStore) RelayClientsByNode(ctx context.Context, nodeID uint64) ([]RelayClientRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, node_id, inbound_id, email, client_id, source_node_id FROM xray_relay_clients "+
			"WHERE node_id = ? ORDER BY id", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RelayClientRecord{}
	for rows.Next() {
		var rc RelayClientRecord
		var sourceNode sql.NullInt64
		if err := rows.Scan(&rc.ID, &rc.NodeID, &rc.InboundID, &rc.Email, &rc.ClientID, &sourceNode); err != nil {
			return nil, err
		}
		rc.SourceNodeID = nullUintPtr(sourceNode)
		out = append(out, rc)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) InsertRelayClient(ctx context.Context, in RelayClientInput) error {
	now := s.now().UTC().Truncate(time.Second)
	var sourceInbound any
	if in.SourceInboundID != 0 {
		sourceInbound = in.SourceInboundID
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_relay_clients (node_id, inbound_id, email, client_id, source_node_id, "+
			"source_inbound_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		in.NodeID, in.InboundID, in.Email, in.ClientID, in.SourceNodeID, sourceInbound, now, now)
	return err
}

func (s *nodeSQLStore) DeleteRelayClientsBySource(ctx context.Context, targetNodeID, inboundID, sourceNodeID uint64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM xray_relay_clients WHERE node_id = ? AND inbound_id = ? AND source_node_id = ?",
		targetNodeID, inboundID, sourceNodeID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- 中转候选 ----

func (s *nodeSQLStore) RelayTargets(ctx context.Context, selfID uint64) ([]RelayTarget, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+nodeColumns+
		" FROM nodes WHERE enabled = 1 AND id <> ? ORDER BY name", selfID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RelayTarget{}
	index := map[uint64]int{}
	for rows.Next() {
		n, err := s.scanNode(rows)
		if err != nil {
			return nil, err
		}
		index[n.ID] = len(out)
		out = append(out, RelayTarget{Node: *n})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	ids := make([]any, 0, len(out))
	for _, t := range out {
		ids = append(ids, t.Node.ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")

	inboundRows, err := s.db.QueryContext(ctx,
		"SELECT id, node_id, protocol, inbound_id FROM node_inbounds WHERE node_id IN ("+placeholders+") ORDER BY id", ids...)
	if err != nil {
		return nil, err
	}
	defer inboundRows.Close()
	for inboundRows.Next() {
		var row NodeInboundRecord
		if err := inboundRows.Scan(&row.ID, &row.NodeID, &row.Protocol, &row.InboundID); err != nil {
			return nil, err
		}
		if i, ok := index[row.NodeID]; ok {
			out[i].Inbounds = append(out[i].Inbounds, row)
		}
	}
	if err := inboundRows.Err(); err != nil {
		return nil, err
	}

	xrayRows, err := s.db.QueryContext(ctx, "SELECT "+xrayInboundColumns+
		" FROM xray_inbounds WHERE enabled = 1 AND node_id IN ("+placeholders+") ORDER BY sort, id", ids...)
	if err != nil {
		return nil, err
	}
	defer xrayRows.Close()
	for xrayRows.Next() {
		ib, err := s.scanXrayInbound(xrayRows)
		if err != nil {
			return nil, err
		}
		if i, ok := index[ib.NodeID]; ok {
			out[i].XrayInbounds = append(out[i].XrayInbounds, *ib)
		}
	}
	return out, xrayRows.Err()
}

func (s *nodeSQLStore) NodeNameByID(ctx context.Context, id uint64) (*string, error) {
	var name sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT name FROM nodes WHERE id = ?", id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return nullStrPtr(name), nil
}

// ---- WARP ----

func (s *nodeSQLStore) WarpAccountByNode(ctx context.Context, nodeID uint64) (*WarpAccountRecord, error) {
	var a WarpAccountRecord
	var deviceID, accessToken, privateKey, publicKey, peerPublicKey, peerEndpoint sql.NullString
	var licenseKey, clientID sql.NullString
	var addresses, reserved sql.NullString
	var lastRotate, updatedAt sql.NullTime
	var enabled int
	err := s.db.QueryRowContext(ctx,
		"SELECT node_id, device_id, access_token, private_key, public_key, peer_public_key, peer_endpoint, "+
			"addresses, reserved, license_key, client_id, auto_rotate_hours, last_rotate_at, enabled, updated_at "+
			"FROM node_warp_accounts WHERE node_id = ?", nodeID).
		Scan(&a.NodeID, &deviceID, &accessToken, &privateKey, &publicKey, &peerPublicKey, &peerEndpoint,
			&addresses, &reserved, &licenseKey, &clientID, &a.AutoRotateHours, &lastRotate, &enabled, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.DeviceID = deviceID.String
	a.AccessToken = derefDecrypt(s.decryptOrRaw(accessToken))
	a.PrivateKey = derefDecrypt(s.decryptOrRaw(privateKey))
	a.PublicKey = publicKey.String
	a.PeerPublicKey = peerPublicKey.String
	a.PeerEndpoint = peerEndpoint.String
	a.Addresses = parseStringListJSON(rawBytesOf(addresses))
	a.Reserved = parseIntListJSON(rawBytesOf(reserved))
	a.LicenseKey = licenseKey.String
	a.ClientID = clientID.String
	a.LastRotateAt = nullTimePtr(lastRotate)
	a.Enabled = enabled != 0
	a.UpdatedAt = nullTimePtr(updatedAt)
	return &a, nil
}

func derefDecrypt(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func parseStringListJSON(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	parsed, err := ordParse(raw)
	if err != nil {
		return nil
	}
	arr, ok := parsed.(ordArr)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, isStr := item.(string); isStr {
			out = append(out, s)
		}
	}
	return out
}

func parseIntListJSON(raw []byte) []int64 {
	if len(raw) == 0 {
		return nil
	}
	parsed, err := ordParse(raw)
	if err != nil {
		return nil
	}
	arr, ok := parsed.(ordArr)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(arr))
	for _, item := range arr {
		out = append(out, int64(ordFloat(item)))
	}
	return out
}

func (s *nodeSQLStore) SaveWarpAccount(ctx context.Context, in WarpAccountRecord) error {
	accessToken, err := s.encryptString(in.AccessToken)
	if err != nil {
		return err
	}
	privateKey, err := s.encryptString(in.PrivateKey)
	if err != nil {
		return err
	}
	addresses := encodeStringListJSON(in.Addresses)
	reserved := encodeIntListJSON(in.Reserved)
	now := s.now().UTC().Truncate(time.Second)

	_, err = s.db.ExecContext(ctx,
		"INSERT INTO node_warp_accounts (node_id, device_id, access_token, private_key, public_key, "+
			"peer_public_key, peer_endpoint, addresses, reserved, license_key, client_id, auto_rotate_hours, "+
			"last_rotate_at, enabled, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE device_id = VALUES(device_id), access_token = VALUES(access_token), "+
			"private_key = VALUES(private_key), public_key = VALUES(public_key), "+
			"peer_public_key = VALUES(peer_public_key), peer_endpoint = VALUES(peer_endpoint), "+
			"addresses = VALUES(addresses), reserved = VALUES(reserved), license_key = VALUES(license_key), "+
			"client_id = VALUES(client_id), updated_at = VALUES(updated_at)",
		in.NodeID, nullStrOrNil(in.DeviceID), nullStrOrNil(accessToken), nullStrOrNil(privateKey),
		nullStrOrNil(in.PublicKey), nullStrOrNil(in.PeerPublicKey), nullStrOrNil(in.PeerEndpoint),
		addresses, reserved, nullStrOrNil(in.LicenseKey), nullStrOrNil(in.ClientID),
		in.AutoRotateHours, nullTimeValue(in.LastRotateAt), boolToInt(in.Enabled), now, now)
	return err
}

func nullStrOrNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func encodeStringListJSON(items []string) any {
	if items == nil {
		return nil
	}
	arr := ordArr{}
	for _, s := range items {
		arr = append(arr, s)
	}
	body, err := ordEncodeUnescaped(arr)
	if err != nil {
		return nil
	}
	return string(body)
}

func encodeIntListJSON(items []int64) any {
	if items == nil {
		return nil
	}
	arr := ordArr{}
	for _, n := range items {
		arr = append(arr, n)
	}
	body, err := ordEncodeUnescaped(arr)
	if err != nil {
		return nil
	}
	return string(body)
}

func (s *nodeSQLStore) PatchWarpAccount(ctx context.Context, nodeID uint64, license *string, autoRotateHours *int64, lastRotateAt *time.Time, enabled *bool) error {
	sets := []string{}
	args := []any{}
	if license != nil {
		sets = append(sets, "license_key = ?")
		args = append(args, *license)
	}
	if autoRotateHours != nil {
		sets = append(sets, "auto_rotate_hours = ?")
		args = append(args, *autoRotateHours)
	}
	if lastRotateAt != nil {
		sets = append(sets, "last_rotate_at = ?")
		args = append(args, *lastRotateAt)
	}
	if enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*enabled))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, s.now().UTC().Truncate(time.Second), nodeID)
	_, err := s.db.ExecContext(ctx, "UPDATE node_warp_accounts SET "+strings.Join(sets, ", ")+" WHERE node_id = ?", args...)
	return err
}

func (s *nodeSQLStore) DeleteWarpAccount(ctx context.Context, nodeID uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM node_warp_accounts WHERE node_id = ?", nodeID)
	return err
}

// ---- 第三方订阅 ----

const thirdPartyColumns = `id, name, url, enabled, cached_links, node_count, last_fetched_at, last_status, last_error`

func scanThirdParty(row interface{ Scan(...any) error }) (*ThirdPartySubRecord, error) {
	var t ThirdPartySubRecord
	var cached, lastError sql.NullString
	var lastFetched sql.NullTime
	var enabled int
	if err := row.Scan(&t.ID, &t.Name, &t.URL, &enabled, &cached, &t.NodeCount, &lastFetched,
		&t.LastStatus, &lastError); err != nil {
		return nil, err
	}
	t.Enabled = enabled != 0
	t.CachedLinks = nullStrPtr(cached)
	t.LastFetchedAt = nullTimePtr(lastFetched)
	t.LastError = nullStrPtr(lastError)
	return &t, nil
}

func (s *nodeSQLStore) ThirdPartySubs(ctx context.Context) ([]ThirdPartySubRecord, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+thirdPartyColumns+" FROM third_party_subs ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ThirdPartySubRecord{}
	for rows.Next() {
		t, err := scanThirdParty(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) ThirdPartySubByID(ctx context.Context, id uint64) (*ThirdPartySubRecord, error) {
	return scanThirdParty(s.db.QueryRowContext(ctx, "SELECT "+thirdPartyColumns+
		" FROM third_party_subs WHERE id = ?", id))
}

func (s *nodeSQLStore) CreateThirdPartySub(ctx context.Context, in ThirdPartySubInput) (*ThirdPartySubRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO third_party_subs (name, url, enabled, node_count, last_status, created_at, updated_at) "+
			"VALUES (?, ?, ?, 0, 'pending', ?, ?)",
		in.Name, in.URL, boolToInt(in.Enabled), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.ThirdPartySubByID(ctx, uint64(id))
}

func (s *nodeSQLStore) UpdateThirdPartySub(ctx context.Context, id uint64, in ThirdPartySubUpdate) (*ThirdPartySubRecord, error) {
	sets := []string{}
	args := []any{}
	if in.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *in.Name)
	}
	if in.URL != nil {
		sets = append(sets, "url = ?")
		args = append(args, *in.URL)
	}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if len(sets) > 0 {
		sets = append(sets, "updated_at = ?")
		args = append(args, s.now().UTC().Truncate(time.Second), id)
		if _, err := s.db.ExecContext(ctx, "UPDATE third_party_subs SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return nil, err
		}
	}
	return s.ThirdPartySubByID(ctx, id)
}

func (s *nodeSQLStore) DeleteThirdPartySub(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM third_party_subs WHERE id = ?", id)
	return err
}

// SaveThirdPartyFetch 落一次拉取结果；errMsg 非空时只记失败（保留上次成功缓存）。
func (s *nodeSQLStore) SaveThirdPartyFetch(ctx context.Context, id uint64, links []string, errMsg string) error {
	now := s.now().UTC().Truncate(time.Second)
	if errMsg != "" {
		_, err := s.db.ExecContext(ctx,
			"UPDATE third_party_subs SET last_status = 'failed', last_error = ?, last_fetched_at = ?, updated_at = ? "+
				"WHERE id = ?", truncateRunes(errMsg, 250), now, now, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE third_party_subs SET cached_links = ?, node_count = ?, last_status = 'success', "+
			"last_error = NULL, last_fetched_at = ?, updated_at = ? WHERE id = ?",
		strings.Join(links, "\n"), len(links), now, now, id)
	return err
}

// RemoveThirdPartyFromPlans 复刻 ThirdPartyController::destroy 的引用清理。
func (s *nodeSQLStore) RemoveThirdPartyFromPlans(ctx context.Context, id uint64) error {
	rows, err := s.db.QueryContext(ctx, "SELECT id, third_party_sub_ids FROM plans")
	if err != nil {
		return err
	}
	type planRow struct {
		id  uint64
		raw []byte
	}
	targets := []planRow{}
	for rows.Next() {
		var planID uint64
		var raw sql.NullString
		if err := rows.Scan(&planID, &raw); err != nil {
			rows.Close()
			return err
		}
		if !raw.Valid || raw.String == "" {
			continue
		}
		targets = append(targets, planRow{id: planID, raw: []byte(raw.String)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := s.now().UTC().Truncate(time.Second)
	for _, p := range targets {
		parsed, err := ordParse(p.raw)
		if err != nil {
			continue
		}
		arr, ok := parsed.(ordArr)
		if !ok {
			continue
		}
		kept := ordArr{}
		changed := false
		for _, item := range arr {
			if int64(ordFloat(item)) == int64(id) {
				changed = true
				continue
			}
			kept = append(kept, int64(ordFloat(item)))
		}
		if !changed {
			continue
		}
		body, err := ordEncodeUnescaped(kept)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx,
			"UPDATE plans SET third_party_sub_ids = ?, updated_at = ? WHERE id = ?", string(body), now, p.id); err != nil {
			return err
		}
	}
	return nil
}

// ---- 出站订阅 ----

const outboundSubColumns = `id, node_id, remark, url, tag_prefix, update_interval, prepend, priority,
	enabled, fetched, link_identities, content_hash, node_count, last_fetched_at, last_status, last_error`

func scanOutboundSub(row interface{ Scan(...any) error }) (*OutboundSubRecord, error) {
	var s OutboundSubRecord
	var remark, tagPrefix, fetched, identities, contentHash, lastError sql.NullString
	var lastFetched sql.NullTime
	var prepend, enabled int
	if err := row.Scan(&s.ID, &s.NodeID, &remark, &s.URL, &tagPrefix, &s.UpdateInterval, &prepend,
		&s.Priority, &enabled, &fetched, &identities, &contentHash, &s.NodeCount, &lastFetched,
		&s.LastStatus, &lastError); err != nil {
		return nil, err
	}
	s.Remark = nullStrPtr(remark)
	s.TagPrefix = nullStrPtr(tagPrefix)
	s.Prepend = prepend != 0
	s.Enabled = enabled != 0
	s.Fetched = nullStrPtr(fetched)
	s.LinkIdentities = nullStrPtr(identities)
	s.ContentHash = nullStrPtr(contentHash)
	s.LastFetchedAt = nullTimePtr(lastFetched)
	s.LastError = nullStrPtr(lastError)
	return &s, nil
}

func (s *nodeSQLStore) OutboundSubsByNode(ctx context.Context, nodeID uint64) ([]OutboundSubRecord, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+outboundSubColumns+
		" FROM xray_outbound_subscriptions WHERE node_id = ? ORDER BY priority, id", nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboundSubRecord{}
	for rows.Next() {
		sub, err := scanOutboundSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

func (s *nodeSQLStore) OutboundSubByID(ctx context.Context, nodeID, id uint64) (*OutboundSubRecord, error) {
	return scanOutboundSub(s.db.QueryRowContext(ctx, "SELECT "+outboundSubColumns+
		" FROM xray_outbound_subscriptions WHERE node_id = ? AND id = ?", nodeID, id))
}

func (s *nodeSQLStore) CreateOutboundSub(ctx context.Context, in OutboundSubInput) (*OutboundSubRecord, error) {
	now := s.now().UTC().Truncate(time.Second)
	interval := int64(600)
	if in.UpdateInterval != nil {
		interval = *in.UpdateInterval
	}
	prepend := false
	if in.Prepend != nil {
		prepend = *in.Prepend
	}
	priority := int64(0)
	if in.Priority != nil {
		priority = *in.Priority
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_outbound_subscriptions (node_id, remark, url, tag_prefix, update_interval, "+
			"prepend, priority, enabled, node_count, last_status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 'pending', ?, ?)",
		in.NodeID, nullStrValue(in.Remark), in.URL, nullStrValue(in.TagPrefix), interval,
		boolToInt(prepend), priority, boolToInt(enabled), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.OutboundSubByID(ctx, in.NodeID, uint64(id))
}

func (s *nodeSQLStore) UpdateOutboundSub(ctx context.Context, id uint64, in OutboundSubInput) (*OutboundSubRecord, error) {
	sets := []string{"url = ?", "remark = ?", "tag_prefix = ?"}
	args := []any{in.URL, nullStrValue(in.Remark), nullStrValue(in.TagPrefix)}
	if in.UpdateInterval != nil {
		sets = append(sets, "update_interval = ?")
		args = append(args, *in.UpdateInterval)
	}
	if in.Prepend != nil {
		sets = append(sets, "prepend = ?")
		args = append(args, boolToInt(*in.Prepend))
	}
	if in.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *in.Priority)
	}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, s.now().UTC().Truncate(time.Second), id)

	if _, err := s.db.ExecContext(ctx, "UPDATE xray_outbound_subscriptions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, err
	}
	var nodeID uint64
	if err := s.db.QueryRowContext(ctx, "SELECT node_id FROM xray_outbound_subscriptions WHERE id = ?", id).Scan(&nodeID); err != nil {
		return nil, err
	}
	return s.OutboundSubByID(ctx, nodeID, id)
}

func (s *nodeSQLStore) DeleteOutboundSub(ctx context.Context, id uint64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM xray_outbound_subscriptions WHERE id = ?", id)
	return err
}

func (s *nodeSQLStore) MaxOutboundSubPriority(ctx context.Context, nodeID uint64) (int64, error) {
	var maxPriority sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MAX(priority) FROM xray_outbound_subscriptions WHERE node_id = ?", nodeID).Scan(&maxPriority); err != nil {
		return 0, err
	}
	return int64ValueOr(nullIntPtr(maxPriority), 0), nil
}

// int64ValueOr 给 *int64 一个默认值（MAX() 在无行时返回 NULL）。
// 不能给 *int64 加方法（非本包类型），所以用包级函数。
func int64ValueOr(v *int64, def int64) int64 {
	if v == nil {
		return def
	}
	return *v
}

func (s *nodeSQLStore) SaveOutboundSubFetch(ctx context.Context, id uint64, r OutboundSubFetchResult) error {
	now := r.At.UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"UPDATE xray_outbound_subscriptions SET fetched = ?, link_identities = ?, content_hash = ?, "+
			"node_count = ?, last_status = 'success', last_error = NULL, last_fetched_at = ?, updated_at = ? "+
			"WHERE id = ?",
		r.Fetched, r.LinkIdentities, r.ContentHash, r.NodeCount, now, now, id)
	return err
}

func (s *nodeSQLStore) RecordOutboundSubError(ctx context.Context, id uint64, message string, at time.Time) error {
	now := at.UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		"UPDATE xray_outbound_subscriptions SET last_status = 'failed', last_error = ?, "+
			"last_fetched_at = ?, updated_at = ? WHERE id = ?",
		truncateRunes(message, 490), now, now, id)
	return err
}

// 编译期断言。
var _ NodeStore = (*nodeSQLStore)(nil)

// unusedGuard 防止 fmt 被裁掉（错误文案里用到时保留 import 的意图）。
var _ = fmt.Sprintf
