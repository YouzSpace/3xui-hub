package admin

// std03-split: 由 node_sqlstore.go 按职责拆出（纯结构移动，未改 SQL / 签名 / 错误信息）。
// 职责：Xray 出站 / 路由规则 / 负载均衡 / 中转 / 出站流量。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

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
