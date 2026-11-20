package admin

// std03-split: 由 node_sqlstore.go 按职责拆出（纯结构移动，未改 SQL / 签名 / 错误信息）。
// 职责：WARP 账号（node_warp_accounts）。

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

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
