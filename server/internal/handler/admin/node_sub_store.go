package admin

// std03-split: 由 node_sqlstore.go 按职责拆出（纯结构移动，未改 SQL / 签名 / 错误信息）。
// 职责：第三方订阅 + 出站订阅。

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

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
