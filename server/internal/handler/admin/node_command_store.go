package admin

// std03-split: 由 node_sqlstore.go 按职责拆出（纯结构移动，未改 SQL / 签名 / 错误信息）。
// 职责：节点指令（node_api_commands）+ 系统审计（system_audit）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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

// UpgradeAttemptStats 按「节点 + 目标版本」统计历史升级尝试。
//
// 为什么现算而不建表：每次升级都在 node_api_commands 留了 payload(version)/status/result，
// 按节点取最近若干条、用 payload.version 过滤即可，少一张表就少一处状态漂移。
//
// 口径（用户定稿）：Attempts 数的是**已下发过的指令条数** —— 节点离线恢复后下发的同样算一次，
// 离线恢复后的更新失败也不额外加次数，总机会就是 3 次。
func (s *nodeSQLStore) UpgradeAttemptStats(ctx context.Context, nodeID uint64, targetVersion string) (UpgradeAttemptStats, error) {
	var st UpgradeAttemptStats
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, payload, status, result, created_at FROM node_api_commands "+
			"WHERE node_id = ? AND action = 'upgrade' ORDER BY id DESC LIMIT 50", nodeID)
	if err != nil {
		return st, err
	}
	defer rows.Close()

	first := true
	for rows.Next() {
		var (
			id        uint64
			payload   sql.NullString
			status    string
			result    sql.NullString
			createdAt sql.NullTime
		)
		if err := rows.Scan(&id, &payload, &status, &result, &createdAt); err != nil {
			return st, err
		}
		if payloadVersion(payload.String) != targetVersion {
			continue
		}
		st.Attempts++
		if first {
			first = false
			st.LastStatus = status
			st.LastCommandID = id
			if createdAt.Valid {
				t := createdAt.Time
				st.LastAt = &t
			}
		}
		if st.LastError == "" && status == "failed" {
			st.LastError = trimReason(result.String)
		}
	}
	return st, rows.Err()
}

// payloadVersion 从指令 payload 里取 version（解析失败当空串，等价于「这条不属于该版本」）。
func payloadVersion(payload string) string {
	if strings.TrimSpace(payload) == "" {
		return ""
	}
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.Version)
}

// RecordSystemAuditOnce 幂等写系统审计：同一 (action, path) 已存在就直接返回 false。
//
// 后台任务每 5 分钟一轮，不去重会把「操作审计」页刷满（那页只留最近 500 条），
// 所以按 (action, path) 做永久幂等 —— 同一条失败指令、同一个「已放弃」结论只留一条。
func (s *nodeSQLStore) RecordSystemAuditOnce(ctx context.Context, action, actor, path, errMsg string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM operation_logs WHERE type = 'system' AND action = ? AND path = ? LIMIT 1",
		action, path).Scan(&exists)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, fmt.Errorf("查询系统审计失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO operation_logs (type, actor_name, action, method, path, status, error, created_at) "+
			"VALUES ('system', ?, ?, 'CLI', ?, 500, ?, ?)",
		actor, action, path, errMsg, time.Now().UTC()); err != nil {
		return false, fmt.Errorf("写入系统审计失败: %w", err)
	}
	return true, nil
}
