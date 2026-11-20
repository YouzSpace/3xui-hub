package admin

// std03-split: 由 node_sqlstore.go 按职责拆出（纯结构移动，未改 SQL / 签名 / 错误信息）。
// 职责：异步任务（node_task / node_task_items）。

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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
