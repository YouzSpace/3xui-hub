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

// std03-split: 节点域 store 已按职责拆分为 node_task_store.go / node_command_store.go /
// node_stats_store.go / node_xray_store.go / node_sub_store.go / node_warp_store.go，
// 本文件只保留构造、加密、站点配置、scan 辅助与节点 CRUD。纯结构改动，无逻辑变化。

import (
	"context"
	"database/sql"
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
	driver_config, last_check_at, created_at, updated_at, region_id, capacity`

func (s *nodeSQLStore) scanNode(row interface{ Scan(...any) error }) (*NodeRecord, error) {
	var n NodeRecord
	var password, apiKey, cpu, mem, multiplier, driverVersion sql.NullString
	var driverConfig, rawPassword, rawAPIKey sql.NullString
	var enabled, verifySSL int
	var lastCheck, createdAt, updatedAt sql.NullTime
	var regionID, capacity sql.NullInt64
	if err := row.Scan(&n.ID, &n.Name, &n.Host, &n.Port, &n.Scheme, &n.WebBasePath, &n.Username,
		&rawPassword, &rawAPIKey, &enabled, &verifySSL, &n.Status, &n.Latency, &cpu, &mem,
		&multiplier, &n.DriverType, &driverVersion, &driverConfig, &lastCheck, &createdAt, &updatedAt,
		&regionID, &capacity); err != nil {
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
	if regionID.Valid {
		v := uint64(regionID.Int64)
		n.RegionID = &v
	}
	if capacity.Valid {
		v := capacity.Int64
		n.Capacity = &v
	}
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

// DeleteNode 删除节点行，替代过去一句 `DELETE FROM nodes` 裸删。
//
// 为什么要有护栏：user_nodes 没有外键（见 cmd/server/schema.go），直接删节点会把
// 绑在上面的用户变成「悬空」—— 订阅里拉不到任何节点，而面板看不出原因。
//   - Force=false：先数绑定用户，有就返回 *NodeBoundUsersError（HTTP 层回 422）；
//   - Force=true：同一事务里解绑、清关联表、删节点行，并写一条审计。
//
// 关联表显式清理而不是只靠 ON DELETE CASCADE：老库（Laravel 时期一路 migrate 上来的）
// 不一定还有外键，缺外键时 xray_inbounds 等会留下孤儿行，后续按 node_id 查会读到幽灵入站。
func (s *nodeSQLStore) DeleteNode(ctx context.Context, in NodeDeleteInput) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 行锁内数绑定：SELECT ... FOR UPDATE 会在 user_nodes 的 idx_node 上取间隙锁，
	// 挡住「查完是 0、删之前又有人绑进来」的并发窗口。
	var bound int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_nodes WHERE node_id = ? FOR UPDATE", in.ID).Scan(&bound); err != nil {
		return err
	}
	if bound > 0 && !in.Force {
		return &NodeBoundUsersError{NodeID: in.ID, Count: bound}
	}

	// 顺序按「子表 → 父表」：xray_relay_clients 挂在 xray_inbounds 上（级联），先不单独处理。
	for _, table := range []string{"xray_inbounds", "xray_outbounds", "xray_routing_rules", "node_inbounds"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE node_id = ?", in.ID); err != nil {
			return fmt.Errorf("清理 %s 失败: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM user_nodes WHERE node_id = ?", in.ID); err != nil {
		return fmt.Errorf("解绑用户失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM nodes WHERE id = ?", in.ID); err != nil {
		return err
	}

	// 审计与删除同事务：不允许「节点没了但没留痕」，也不允许「留了痕其实没删」。
	if in.Force {
		action := fmt.Sprintf("强制删除节点（含 %d 个用户绑定）", bound)
		var actor any
		if strings.TrimSpace(in.Actor) != "" {
			actor = strings.TrimSpace(in.Actor)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO operation_logs (type, actor_name, action, method, path, status, error, created_at) "+
				"VALUES ('admin', ?, ?, 'DELETE', ?, 200, ?, ?)",
			actor, action, fmt.Sprintf("/admin-api/nodes/%d", in.ID),
			fmt.Sprintf("解绑 %d 个用户并清理入站/出站/路由规则/入站挂载", bound), s.now().UTC()); err != nil {
			return fmt.Errorf("写强制删除审计失败: %w", err)
		}
	}

	return tx.Commit()
}

// NodeBoundUserCount 数该节点绑定的用户数（删除护栏的预检查）。
func (s *nodeSQLStore) NodeBoundUserCount(ctx context.Context, nodeID uint64) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_nodes WHERE node_id = ?", nodeID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
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

// 编译期断言。
var _ NodeStore = (*nodeSQLStore)(nil)

// unusedGuard 防止 fmt 被裁掉（错误文案里用到时保留 import 的意图）。
var _ = fmt.Sprintf
