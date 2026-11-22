package admin

// std03-split: 由 node_sqlstore.go 按职责拆出（纯结构移动，未改 SQL / 签名 / 错误信息）。
// 职责：用户口径查询 + Xray 入站 / 证书。

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"controlhub-server/internal/crypto"
)

// ---- 用户口径 ----

// EligibleProtocolCounts 复刻 XrayConfigService::eligibleUsers() 的协议分布
// （present 里的 client_count：有套餐 + 启用 + 未被关闭流量）。
//
// nodeID > 0 时按节点统计，判据与 nodeapi.eligibleUsers 一致（看节点有没有归属地区）：
//   - 节点未归属地区（region_id IS NULL）→ 全量（不看绑定；移出地区即全量，保留旧绑定）；
//   - 节点已归属地区 → 只算绑到本节点的用户。
//
// nodeID <= 0 → 不过滤（全局面板口径）。
func (s *nodeSQLStore) EligibleProtocolCounts(ctx context.Context, nodeID int64) (map[string]int64, error) {
	query := "SELECT u.protocol, COUNT(*) FROM users u WHERE u.plan_id IS NOT NULL AND u.enabled = 1 " +
		"AND u.traffic_disabled_at IS NULL "
	var args []any
	if nodeID > 0 {
		var regionID sql.NullInt64
		if err := s.db.QueryRowContext(ctx, "SELECT region_id FROM nodes WHERE id = ?", nodeID).Scan(&regionID); err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("节点 %d 不存在", nodeID)
			}
			return nil, err
		}
		if regionID.Valid {
			query += "AND EXISTS (SELECT 1 FROM user_nodes un WHERE un.node_id = ? AND un.user_id = u.id) "
			args = append(args, nodeID)
		}
	}
	query += "GROUP BY u.protocol"

	rows, err := s.db.QueryContext(ctx, query, args...)
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

// ---- xray 入站 ----

const xrayInboundColumns = `id, node_id, region_id, tag, remark, protocol, port, listen, client_host, settings, stream_settings,
	sniffing, reality_private_key, salamander_password, enabled, sort, created_at, updated_at`

func (s *nodeSQLStore) scanXrayInbound(row interface{ Scan(...any) error }) (*XrayInboundRecord, error) {
	var ib XrayInboundRecord
	var remark, clientHost, settings, stream, sniffing, privateKey, salamander sql.NullString
	var regionID sql.NullInt64
	var enabled int
	var createdAt, updatedAt sql.NullTime
	if err := row.Scan(&ib.ID, &ib.NodeID, &regionID, &ib.Tag, &remark, &ib.Protocol, &ib.Port, &ib.Listen,
		&clientHost, &settings, &stream, &sniffing, &privateKey, &salamander, &enabled, &ib.Sort, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if regionID.Valid {
		v := uint64(regionID.Int64)
		ib.RegionID = &v
	}
	ib.Remark = nullStrPtr(remark)
	ib.ClientHost = nullStrPtr(clientHost)
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

// RegionExists 判断入站归属的地区是否存在（入站表单的 region_id 校验）。
// 与地区域 Store 的同名方法口径一致：只查 regions 表，不存在返回 false。
func (s *nodeSQLStore) RegionExists(ctx context.Context, id uint64) (bool, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM regions WHERE id = ?", id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
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

// XrayCertificatePEM 读一条证书的解密 PEM（cert_pem 原样 + key_enc 解密成私钥明文）。
//
// 私钥解密口径与节点密码 accessor 一致：解密失败时返回原文（兼容早期手工写库的明文行）。
// 找不到返回 sql.ErrNoRows，由 handler 翻成 404。
func (s *nodeSQLStore) XrayCertificatePEM(ctx context.Context, id uint64) (*XrayCertificatePEM, error) {
	var (
		out      XrayCertificatePEM
		domain   sql.NullString
		certPEM  sql.NullString
		keyEnc   sql.NullString
		notAfter sql.NullTime
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name, domain, cert_pem, key_enc, not_after FROM xray_certificates WHERE id = ?", id).
		Scan(&out.ID, &out.Name, &domain, &certPEM, &keyEnc, &notAfter)
	if err != nil {
		return nil, err
	}
	out.Domain = nullStrPtr(domain)
	out.CertPEM = certPEM.String
	if keyEnc.Valid && keyEnc.String != "" {
		if plain, derr := crypto.DecryptString(keyEnc.String, s.appKey); derr == nil {
			out.KeyPEM = plain
		} else {
			out.KeyPEM = keyEnc.String
		}
	}
	if notAfter.Valid {
		t := notAfter.Time
		out.NotAfter = &t
	}
	return &out, nil
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
		"INSERT INTO xray_inbounds (node_id, region_id, tag, remark, protocol, port, listen, client_host, settings, stream_settings, "+
			"sniffing, reality_private_key, salamander_password, enabled, sort, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		in.NodeID, nullableUint(in.RegionID), in.Tag, nullStrValue(in.Remark), in.Protocol, in.Port, in.Listen,
		nullStrValue(in.ClientHost),
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
		"client_host = ?", "settings = ?", "stream_settings = ?", "sniffing = ?"}
	args := []any{in.Tag, nullStrValue(in.Remark), in.Protocol, in.Port, in.Listen,
		nullStrValue(in.ClientHost),
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
	// 入站归属地区（#入站级分组）：没带字段 = 保持原值；带了 null = 清空（跟随节点）。
	if in.RegionIDSet {
		sets = append(sets, "region_id = ?")
		args = append(args, nullableUint(in.RegionID))
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
