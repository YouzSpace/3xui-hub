package admin

// xray_templateStore 是配置模板（#地区功能-19）的 database/sql 实现。
//
// 两个职责：
//  1. 三张模板表的 CRUD（纯字典表，没有业务副作用）；
//  2. ApplyToNode —— 把整份模板写进一台 xray 节点的 xray_inbounds /
//     xray_outbounds / xray_routing_rules。每台节点一个事务：入站、出站、路由、
//     节点标记（is_custom_config=0 + 配置版本 +1）要么全成、要么全不成。
//
// reality 私钥在这里现场生成（xray.GenerateKeypair，crypto/rand），
// **绝不从模板读**：同一份模板套到 N 台节点就是 N 把不同的私钥。生成的私钥按
// 仓库既有口径经 internal/crypto 加密后落 xray_inbounds.reality_private_key，
// 同时把推导出的公钥写回该行 stream_settings.realitySettings.publicKey
// （订阅链接的 pbk 读的就是这个字段）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"controlhub-server/internal/crypto"
	"controlhub-server/internal/driver/xray"
)

// NewXrayTemplateSQLStore 用已有连接池构造配置模板 Store。
//
// appKey 用于加密新生成的 reality 私钥，必须与节点域（NewNodeSQLStore）同一个，
// 否则驱动渲染时解不开。
func NewXrayTemplateSQLStore(db *sql.DB, appKey []byte) *xrayTemplateStore {
	return &xrayTemplateStore{db: db, appKey: appKey, now: time.Now}
}

type xrayTemplateStore struct {
	db     *sql.DB
	appKey []byte
	now    func() time.Time
}

// ---- 列定义 ----

const xrayTemplateInboundColumns = `id, name, sort, tag, remark, protocol, port, listen,
	settings, stream_settings, sniffing, enabled, created_at, updated_at`

const xrayTemplateOutboundColumns = `id, name, sort, tag, protocol, settings, stream_settings,
	enabled, created_at, updated_at`

const xrayTemplateRoutingColumns = `id, name, sort, domains, ips, port, network, protocol,
	inbound_tag, outbound_tag, remark, enabled, created_at, updated_at`

// ---- 查询 ----

// TemplateSet 一次取齐三类模板（sort, id 升序 —— 与 xray 驱动的渲染顺序一致）。
func (s *xrayTemplateStore) TemplateSet(ctx context.Context) (*XrayTemplateSet, error) {
	inbounds, err := s.templateInbounds(ctx)
	if err != nil {
		return nil, err
	}
	outbounds, err := s.templateOutbounds(ctx)
	if err != nil {
		return nil, err
	}
	routing, err := s.templateRouting(ctx)
	if err != nil {
		return nil, err
	}
	return &XrayTemplateSet{Inbounds: inbounds, Outbounds: outbounds, Routing: routing}, nil
}

func (s *xrayTemplateStore) templateInbounds(ctx context.Context) ([]XrayTemplateInbound, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+xrayTemplateInboundColumns+
		" FROM xray_template_inbounds ORDER BY sort, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []XrayTemplateInbound{}
	for rows.Next() {
		var (
			t                          XrayTemplateInbound
			remark                     sql.NullString
			settings, stream, sniffing sql.NullString
			enabled                    int
			createdAt, updatedAt       sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Sort, &t.Tag, &remark, &t.Protocol, &t.Port, &t.Listen,
			&settings, &stream, &sniffing, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		t.Remark = nullStrPtr(remark)
		t.Settings = rawBytesOf(settings)
		t.StreamSettings = rawBytesOf(stream)
		t.Sniffing = rawBytesOf(sniffing)
		t.Enabled = enabled != 0
		t.CreatedAt = nullTimePtr(createdAt)
		t.UpdatedAt = nullTimePtr(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *xrayTemplateStore) templateOutbounds(ctx context.Context) ([]XrayTemplateOutbound, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+xrayTemplateOutboundColumns+
		" FROM xray_template_outbounds ORDER BY sort, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []XrayTemplateOutbound{}
	for rows.Next() {
		var (
			t                    XrayTemplateOutbound
			settings, stream     sql.NullString
			enabled              int
			createdAt, updatedAt sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Sort, &t.Tag, &t.Protocol, &settings, &stream,
			&enabled, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		t.Settings = rawBytesOf(settings)
		t.StreamSettings = rawBytesOf(stream)
		t.Enabled = enabled != 0
		t.CreatedAt = nullTimePtr(createdAt)
		t.UpdatedAt = nullTimePtr(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *xrayTemplateStore) templateRouting(ctx context.Context) ([]XrayTemplateRouting, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+xrayTemplateRoutingColumns+
		" FROM xray_template_routing ORDER BY sort, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []XrayTemplateRouting{}
	for rows.Next() {
		var (
			t                       XrayTemplateRouting
			domains, ips            sql.NullString
			port, network, protocol sql.NullString
			inboundTag, remark      sql.NullString
			enabled                 int
			createdAt, updatedAt    sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Sort, &domains, &ips, &port, &network, &protocol,
			&inboundTag, &t.OutboundTag, &remark, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		t.Domains = rawBytesOf(domains)
		t.IPs = rawBytesOf(ips)
		t.Port = nullStrPtr(port)
		t.Network = nullStrPtr(network)
		t.Protocol = nullStrPtr(protocol)
		t.InboundTag = nullStrPtr(inboundTag)
		t.Remark = nullStrPtr(remark)
		t.Enabled = enabled != 0
		t.CreatedAt = nullTimePtr(createdAt)
		t.UpdatedAt = nullTimePtr(updatedAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *xrayTemplateStore) templateInboundByID(ctx context.Context, id uint64) (*XrayTemplateInbound, error) {
	var (
		t                          XrayTemplateInbound
		remark                     sql.NullString
		settings, stream, sniffing sql.NullString
		enabled                    int
		createdAt, updatedAt       sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, "SELECT "+xrayTemplateInboundColumns+
		" FROM xray_template_inbounds WHERE id = ?", id).
		Scan(&t.ID, &t.Name, &t.Sort, &t.Tag, &remark, &t.Protocol, &t.Port, &t.Listen,
			&settings, &stream, &sniffing, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	t.Remark = nullStrPtr(remark)
	t.Settings = rawBytesOf(settings)
	t.StreamSettings = rawBytesOf(stream)
	t.Sniffing = rawBytesOf(sniffing)
	t.Enabled = enabled != 0
	t.CreatedAt = nullTimePtr(createdAt)
	t.UpdatedAt = nullTimePtr(updatedAt)
	return &t, nil
}

func (s *xrayTemplateStore) templateOutboundByID(ctx context.Context, id uint64) (*XrayTemplateOutbound, error) {
	var (
		t                    XrayTemplateOutbound
		settings, stream     sql.NullString
		enabled              int
		createdAt, updatedAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, "SELECT "+xrayTemplateOutboundColumns+
		" FROM xray_template_outbounds WHERE id = ?", id).
		Scan(&t.ID, &t.Name, &t.Sort, &t.Tag, &t.Protocol, &settings, &stream,
			&enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	t.Settings = rawBytesOf(settings)
	t.StreamSettings = rawBytesOf(stream)
	t.Enabled = enabled != 0
	t.CreatedAt = nullTimePtr(createdAt)
	t.UpdatedAt = nullTimePtr(updatedAt)
	return &t, nil
}

func (s *xrayTemplateStore) templateRoutingByID(ctx context.Context, id uint64) (*XrayTemplateRouting, error) {
	var (
		t                       XrayTemplateRouting
		domains, ips            sql.NullString
		port, network, protocol sql.NullString
		inboundTag, remark      sql.NullString
		enabled                 int
		createdAt, updatedAt    sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, "SELECT "+xrayTemplateRoutingColumns+
		" FROM xray_template_routing WHERE id = ?", id).
		Scan(&t.ID, &t.Name, &t.Sort, &domains, &ips, &port, &network, &protocol,
			&inboundTag, &t.OutboundTag, &remark, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	t.Domains = rawBytesOf(domains)
	t.IPs = rawBytesOf(ips)
	t.Port = nullStrPtr(port)
	t.Network = nullStrPtr(network)
	t.Protocol = nullStrPtr(protocol)
	t.InboundTag = nullStrPtr(inboundTag)
	t.Remark = nullStrPtr(remark)
	t.Enabled = enabled != 0
	t.CreatedAt = nullTimePtr(createdAt)
	t.UpdatedAt = nullTimePtr(updatedAt)
	return &t, nil
}

// ---- 入站模板 CRUD ----

func (s *xrayTemplateStore) CreateTemplateInbound(ctx context.Context, in XrayTemplateInboundInput) (*XrayTemplateInbound, error) {
	now := s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_template_inbounds (name, sort, tag, remark, protocol, port, listen, "+
			"settings, stream_settings, sniffing, enabled, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.Name, in.Sort, in.Tag, nullStrValue(in.Remark), in.Protocol, in.Port, in.Listen,
		jsonTextValue(in.Settings), jsonTextValue(in.StreamSettings), jsonTextValue(in.Sniffing),
		templateEnabledValue(in.Enabled), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.templateInboundByID(ctx, uint64(id))
}

func (s *xrayTemplateStore) UpdateTemplateInbound(ctx context.Context, id uint64, in XrayTemplateInboundInput) (*XrayTemplateInbound, error) {
	_, err := s.db.ExecContext(ctx,
		"UPDATE xray_template_inbounds SET name = ?, sort = ?, tag = ?, remark = ?, protocol = ?, "+
			"port = ?, listen = ?, settings = ?, stream_settings = ?, sniffing = ?, enabled = ?, updated_at = ? "+
			"WHERE id = ?",
		in.Name, in.Sort, in.Tag, nullStrValue(in.Remark), in.Protocol, in.Port, in.Listen,
		jsonTextValue(in.Settings), jsonTextValue(in.StreamSettings), jsonTextValue(in.Sniffing),
		templateEnabledValue(in.Enabled), s.now().UTC().Truncate(time.Second), id)
	if err != nil {
		return nil, err
	}
	rec, err := s.templateInboundByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return rec, err
}

func (s *xrayTemplateStore) DeleteTemplateInbound(ctx context.Context, id uint64) error {
	return deleteTemplateRow(ctx, s.db, "xray_template_inbounds", id)
}

// ---- 出站模板 CRUD ----

func (s *xrayTemplateStore) CreateTemplateOutbound(ctx context.Context, in XrayTemplateOutboundInput) (*XrayTemplateOutbound, error) {
	now := s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_template_outbounds (name, sort, tag, protocol, settings, stream_settings, "+
			"enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.Name, in.Sort, in.Tag, in.Protocol, jsonTextValue(in.Settings),
		jsonTextValue(in.StreamSettings), templateEnabledValue(in.Enabled), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.templateOutboundByID(ctx, uint64(id))
}

func (s *xrayTemplateStore) UpdateTemplateOutbound(ctx context.Context, id uint64, in XrayTemplateOutboundInput) (*XrayTemplateOutbound, error) {
	_, err := s.db.ExecContext(ctx,
		"UPDATE xray_template_outbounds SET name = ?, sort = ?, tag = ?, protocol = ?, settings = ?, "+
			"stream_settings = ?, enabled = ?, updated_at = ? WHERE id = ?",
		in.Name, in.Sort, in.Tag, in.Protocol, jsonTextValue(in.Settings),
		jsonTextValue(in.StreamSettings), templateEnabledValue(in.Enabled),
		s.now().UTC().Truncate(time.Second), id)
	if err != nil {
		return nil, err
	}
	rec, err := s.templateOutboundByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return rec, err
}

func (s *xrayTemplateStore) DeleteTemplateOutbound(ctx context.Context, id uint64) error {
	return deleteTemplateRow(ctx, s.db, "xray_template_outbounds", id)
}

// ---- 路由模板 CRUD ----

func (s *xrayTemplateStore) CreateTemplateRouting(ctx context.Context, in XrayTemplateRoutingInput) (*XrayTemplateRouting, error) {
	now := s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO xray_template_routing (name, sort, domains, ips, port, network, protocol, "+
			"inbound_tag, outbound_tag, remark, enabled, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.Name, in.Sort, jsonTextValue(in.Domains), jsonTextValue(in.IPs), nullStrValue(in.Port),
		nullStrValue(in.Network), nullStrValue(in.Protocol), nullStrValue(in.InboundTag),
		in.OutboundTag, nullStrValue(in.Remark), templateEnabledValue(in.Enabled), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.templateRoutingByID(ctx, uint64(id))
}

func (s *xrayTemplateStore) UpdateTemplateRouting(ctx context.Context, id uint64, in XrayTemplateRoutingInput) (*XrayTemplateRouting, error) {
	_, err := s.db.ExecContext(ctx,
		"UPDATE xray_template_routing SET name = ?, sort = ?, domains = ?, ips = ?, port = ?, "+
			"network = ?, protocol = ?, inbound_tag = ?, outbound_tag = ?, remark = ?, enabled = ?, "+
			"updated_at = ? WHERE id = ?",
		in.Name, in.Sort, jsonTextValue(in.Domains), jsonTextValue(in.IPs), nullStrValue(in.Port),
		nullStrValue(in.Network), nullStrValue(in.Protocol), nullStrValue(in.InboundTag),
		in.OutboundTag, nullStrValue(in.Remark), templateEnabledValue(in.Enabled),
		s.now().UTC().Truncate(time.Second), id)
	if err != nil {
		return nil, err
	}
	rec, err := s.templateRoutingByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return rec, err
}

func (s *xrayTemplateStore) DeleteTemplateRouting(ctx context.Context, id uint64) error {
	return deleteTemplateRow(ctx, s.db, "xray_template_routing", id)
}

// deleteTemplateRow 删一行模板；不存在返回 sql.ErrNoRows（handler 翻 404）。
func deleteTemplateRow(ctx context.Context, db *sql.DB, table string, id uint64) error {
	res, err := db.ExecContext(ctx, "DELETE FROM `"+table+"` WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// templateEnabledValue 把「可选 bool」转成列值（缺省启用，与建表默认值 1 一致）。
func templateEnabledValue(v *bool) int {
	if v == nil {
		return 1
	}
	return boolToInt(*v)
}

// ---- 套用 ----

// ApplyToNode 把整份模板写进一台节点，返回该节点的处理结果。
//
// 跳过规则（不改任何数据）：
//   - driver_type != xray  → 「3x-ui 节点不支持模板」
//   - is_custom_config=1 且 overwriteCustom=false → 「单独配置过，未覆盖」
//
// 返回的 error 只用于「非预期故障」（数据库连不上等），业务性跳过用 Status 表达。
func (s *xrayTemplateStore) ApplyToNode(ctx context.Context, nodeID uint64, tpl *XrayTemplateSet, overwriteCustom bool) (XrayTemplateApplyResult, error) {
	res := XrayTemplateApplyResult{NodeID: nodeID, Status: xrayTemplateApplySkipped}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		nodeName     string
		driverType   string
		customConfig int
		driverConfig sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		"SELECT name, driver_type, is_custom_config, driver_config FROM nodes WHERE id = ? FOR UPDATE", nodeID).
		Scan(&nodeName, &driverType, &customConfig, &driverConfig)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			res.Status = xrayTemplateApplyFailed
			res.Message = "节点不存在"
			return res, nil
		}
		return res, err
	}
	res.NodeName = nodeName

	if driverType != "xray" {
		res.Message = "3x-ui 节点不支持模板"
		return res, nil
	}
	if customConfig != 0 && !overwriteCustom {
		res.Message = "该节点已单独配置过，未覆盖"
		return res, nil
	}

	now := s.now().UTC().Truncate(time.Second)
	warnings, err := s.applyNodeConfig(ctx, tx, nodeID, tpl, now)
	if err != nil {
		return res, err
	}

	// 恢复跟随模板 + 配置版本 +1（版本不涨节点永远拉不到新配置，等于没套用）。
	version := driverConfigInt(rawBytesOf(driverConfig), "config_version") + 1
	current := ordObj{}
	if driverConfig.Valid && driverConfig.String != "" {
		if parsed, perr := ordParse([]byte(driverConfig.String)); perr == nil {
			if obj, ok := parsed.(ordObj); ok {
				current = obj
			}
		}
	}
	current.Set("config_version", version)
	current.Set("config_changed_at", now.Format("2006-01-02T15:04:05+00:00"))
	body, err := ordEncode(current)
	if err != nil {
		return res, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE nodes SET is_custom_config = 0, driver_config = ?, updated_at = ? WHERE id = ?",
		string(body), now, nodeID); err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}

	res.Status = xrayTemplateApplyApplied
	res.Inbounds = len(tpl.Inbounds)
	res.Outbounds = len(tpl.Outbounds)
	res.Rules = len(tpl.Routing)
	res.Message = fmt.Sprintf("已套用：入站 %d 条、出站 %d 条、路由 %d 条",
		len(tpl.Inbounds), len(tpl.Outbounds), len(tpl.Routing))
	if len(warnings) > 0 {
		res.Message += "；" + strings.Join(warnings, "；")
	}
	return res, nil
}

// applyNodeConfig 在事务内重写一台节点的入站/出站/路由。
//
// 返回 warnings 是「不影响成功、但管理员要知道」的提示（目前只有端口试探失败）。
func (s *xrayTemplateStore) applyNodeConfig(ctx context.Context, tx *sql.Tx, nodeID uint64, tpl *XrayTemplateSet, now time.Time) ([]string, error) {
	warnings := []string{}

	// ── 入站：模板里没有的 tag 删掉，模板里的 tag 更新或插入 ──
	keep := make(map[string]bool, len(tpl.Inbounds))
	for _, t := range tpl.Inbounds {
		keep[t.Tag] = true
	}

	existingID := map[string]uint64{}
	rows, err := tx.QueryContext(ctx,
		"SELECT id, tag FROM xray_inbounds WHERE node_id = ?", nodeID)
	if err != nil {
		return nil, err
	}
	var dropIDs []uint64
	for rows.Next() {
		var (
			id  uint64
			tag string
		)
		if err := rows.Scan(&id, &tag); err != nil {
			rows.Close()
			return nil, err
		}
		if !keep[tag] {
			// 模板里没有的 tag：整条删掉（连同它的中转凭据，FK ON DELETE CASCADE）。
			dropIDs = append(dropIDs, id)
			continue
		}
		existingID[tag] = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if len(dropIDs) > 0 {
		args := make([]any, len(dropIDs))
		for i, id := range dropIDs {
			args[i] = id
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM xray_inbounds WHERE id IN ("+placeholdersOf(len(dropIDs))+")", args...); err != nil {
			return nil, err
		}
	}

	// 端口占用只需在「模板内部」去重：删除 + 重写之后，节点上的入站就是模板这一套，
	// 已经不在模板里的行刚被删掉，模板里的行本轮全部按模板端口重写。
	claimed := map[int64]map[string]bool{}

	for _, t := range tpl.Inbounds {
		families := inboundFamilies(t.Protocol, string(t.Settings))
		port := t.Port
		tries := 0
		for templatePortTaken(claimed, port, families) && tries < xrayTemplatePortTries {
			port++
			tries++
		}
		if port > 65535 {
			port = t.Port
		}
		if templatePortTaken(claimed, port, families) {
			warnings = append(warnings, fmt.Sprintf("入站 %s 的端口 %d 起 %d 个都被占用，沿用模板端口 %d",
				t.Tag, t.Port, xrayTemplatePortTries, port))
		}
		claimPort(claimed, port, families)

		stream := t.StreamSettings
		var privateKey *string
		if templateNeedsRealityKey(stream) {
			keypair, kerr := xray.GenerateKeypair()
			if kerr != nil {
				return nil, fmt.Errorf("生成 reality 私钥失败: %w", kerr)
			}
			stream = templateWithRealityPublicKey(stream, keypair[1])
			key := keypair[0]
			privateKey = &key
		}
		encryptedKey, err := s.encryptOptional(privateKey)
		if err != nil {
			return nil, err
		}

		if id, ok := existingID[t.Tag]; ok {
			if _, err := tx.ExecContext(ctx,
				"UPDATE xray_inbounds SET remark = ?, protocol = ?, port = ?, listen = ?, settings = ?, "+
					"stream_settings = ?, sniffing = ?, reality_private_key = ?, enabled = ?, sort = ?, "+
					"updated_at = ? WHERE id = ?",
				nullStrValue(t.Remark), t.Protocol, port, t.Listen, jsonTextValue(t.Settings),
				jsonTextValue(stream), jsonTextValue(t.Sniffing), nullStrValue(encryptedKey),
				boolToInt(t.Enabled), t.Sort, now, id); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO xray_inbounds (node_id, tag, remark, protocol, port, listen, settings, "+
				"stream_settings, sniffing, reality_private_key, enabled, sort, created_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			nodeID, t.Tag, nullStrValue(t.Remark), t.Protocol, port, t.Listen,
			jsonTextValue(t.Settings), jsonTextValue(stream), jsonTextValue(t.Sniffing),
			nullStrValue(encryptedKey), boolToInt(t.Enabled), t.Sort, now, now); err != nil {
			return nil, err
		}
	}

	// ── 出站：全量替换 ──
	if _, err := tx.ExecContext(ctx, "DELETE FROM xray_outbounds WHERE node_id = ?", nodeID); err != nil {
		return nil, err
	}
	for _, t := range tpl.Outbounds {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO xray_outbounds (node_id, tag, protocol, settings, stream_settings, enabled, "+
				"sort, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			nodeID, t.Tag, t.Protocol, jsonTextValue(t.Settings), jsonTextValue(t.StreamSettings),
			boolToInt(t.Enabled), t.Sort, now, now); err != nil {
			return nil, err
		}
	}

	// ── 路由：全量替换 ──
	if _, err := tx.ExecContext(ctx, "DELETE FROM xray_routing_rules WHERE node_id = ?", nodeID); err != nil {
		return nil, err
	}
	for _, t := range tpl.Routing {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO xray_routing_rules (node_id, domains, ips, port, network, protocol, "+
				"inbound_tag, outbound_tag, remark, enabled, sort, created_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			nodeID, jsonTextValue(t.Domains), jsonTextValue(t.IPs), nullStrValue(t.Port),
			nullStrValue(t.Network), nullStrValue(t.Protocol), nullStrValue(t.InboundTag),
			t.OutboundTag, nullStrValue(t.Remark), boolToInt(t.Enabled), t.Sort, now, now); err != nil {
			return nil, err
		}
	}
	return warnings, nil
}

// xrayTemplatePortTries 是端口冲突时的最大试探次数（模板端口, +1, ..., +10）。
const xrayTemplatePortTries = 10

// claimPort 记下某端口已被哪些传输族占用。
func claimPort(claimed map[int64]map[string]bool, port int64, families []string) {
	set := claimed[port]
	if set == nil {
		set = map[string]bool{}
		claimed[port] = set
	}
	for _, f := range families {
		set[f] = true
	}
}

// templatePortTaken 判断端口在给定传输族上是否已被占用。
// 同端口不同族可以并存（vless:443/tcp 与 hysteria:443/udp 不冲突）。
func templatePortTaken(claimed map[int64]map[string]bool, port int64, families []string) bool {
	set := claimed[port]
	if set == nil {
		return false
	}
	for _, f := range families {
		if set[f] {
			return true
		}
	}
	return false
}

// placeholdersOf 生成 n 个逗号分隔的 ?（IN (...) 用）。
func placeholdersOf(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// templateNeedsRealityKey 判断入站模板的 stream_settings 是否走 reality 安全层。
func templateNeedsRealityKey(stream []byte) bool {
	parsed, err := parseOrderedJS(stream)
	if err != nil {
		return false
	}
	obj, ok := parsed.(*jsObj)
	if !ok {
		return false
	}
	security, _ := obj.Get("security")
	s, _ := security.(string)
	return strings.EqualFold(strings.TrimSpace(s), "reality")
}

// templateWithRealityPublicKey 把刚生成的公钥写回 stream_settings.realitySettings.publicKey。
//
// 为什么必须写：驱动渲染节点配置时只用私钥（privateKey 由加密列解密注入），
// 而订阅链接的 pbk 参数读的是这里存的公钥 —— 不写就发出一条连不上的链接。
func templateWithRealityPublicKey(stream []byte, publicKey string) []byte {
	parsed, err := parseOrderedJS(stream)
	if err != nil || parsed == nil {
		return phpEncode(jo("security", "reality", "realitySettings", jo("publicKey", publicKey)))
	}
	obj, ok := parsed.(*jsObj)
	if !ok {
		return phpEncode(jo("security", "reality", "realitySettings", jo("publicKey", publicKey)))
	}
	reality, ok := mustRealitySettings(obj)
	if !ok {
		obj.Put("realitySettings", jo("publicKey", publicKey))
		return phpEncode(obj)
	}
	reality.Put("publicKey", publicKey)
	return phpEncode(obj)
}

// mustRealitySettings 取（或补建）realitySettings 子对象。
func mustRealitySettings(obj *jsObj) (*jsObj, bool) {
	raw, ok := obj.Get("realitySettings")
	if !ok || raw == nil {
		return nil, false
	}
	reality, ok := raw.(*jsObj)
	if !ok {
		return nil, false
	}
	return reality, true
}

// ---- 加密 ----

// encryptOptional 与节点域同口径：空值 → nil（SQL NULL），否则 Laravel 兼容密文。
func (s *xrayTemplateStore) encryptOptional(v *string) (*string, error) {
	if v == nil || *v == "" {
		return nil, nil
	}
	enc, err := crypto.EncryptString(*v, s.appKey)
	if err != nil {
		return nil, err
	}
	return &enc, nil
}

// ---- 节点标记（单独配置过） ----

// MarkNodeCustomConfig 把节点标成「入站已单独改过」，模板套用时默认跳过它。
//
// 定义在 nodeSQLStore 上（而不是 NodeStore 接口）是刻意的：写接口用的
// NodeStore 接口一旦加方法，仓库里几个嵌入式 fake store 会被动实现成
// 「零值调用即 panic」。NodeDomain 侧用可选接口断言调用，见 xray_template.go。
func (s *nodeSQLStore) MarkNodeCustomConfig(ctx context.Context, nodeID uint64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE nodes SET is_custom_config = 1 WHERE id = ? AND is_custom_config = 0", nodeID)
	return err
}
