package admin

// 迁移历史 / 批次撤销的 SQL 层（地区功能 #10）。
//
// 表口径（schema.go 里已验收）：
//
//	user_node_migrations(id, user_id, from_node_id, to_node_id, reason, operator, created_at)
//	  + 本任务唯一新增的列 batch_id varchar(36) DEFAULT NULL
//
// 与 internal/scheduler（region-08）的衔接说明：调度引擎写迁移记录时**不带 batch_id**
// （那个任务的任务书里没有这一列），所以批次号是「事后打标」——
// 迁移任务开始时记下 user_node_migrations 的 MAX(id)，结束时把 id 大于该值的行
// （再按 reason 与节点对收窄）打上本任务的 batch_id。这样不需要改 scheduler，
// 撤销也能按批次精确回滚。详见 stampBatch。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// migrationUndoWindow 是撤销的时间窗：只允许撤销最近 24 小时内的批次。
// 越久越危险 —— 期间用户可能已经被别的批次搬过、续费换过套餐，
// 拿旧记录反向 UPDATE 会把节点绑定改成错的。
const migrationUndoWindow = 24 * time.Hour

// migrationReject 是「业务规则拒绝」（HTTP code 422），与库/环境故障（500）区分。
type migrationReject struct{ msg string }

func (e migrationReject) Error() string { return e.msg }

func migrationRejectf(format string, args ...any) error {
	return migrationReject{msg: fmt.Sprintf(format, args...)}
}

// migrationRecord 是 user_node_migrations 的一行。
type migrationRecord struct {
	ID         int64
	UserID     int64
	FromNodeID *int64
	ToNodeID   int64
	Reason     string
	Operator   *string
	BatchID    *string
	CreatedAt  *time.Time
}

// migrationFilter 是历史列表的过滤条件（0 / 空串 = 不过滤）。
type migrationFilter struct {
	nodeID  int64
	userID  int64
	reason  string
	page    int
	perPage int
}

// listMigrations 返回一页历史 + 总条数。
//
// node_id 的语义是「这个节点参与过这次迁移」—— from 或 to 命中都算：
// 管理员排查「这台节点上的人被搬走过」时，两个方向都要看得到，
// 只匹配 to_node_id 会把「谁被搬走了」漏掉。
func (d *MigrationDomain) listMigrations(ctx context.Context, f migrationFilter) ([]migrationRecord, int, error) {
	where := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if f.nodeID > 0 {
		where = append(where, "(from_node_id = ? OR to_node_id = ?)")
		args = append(args, f.nodeID, f.nodeID)
	}
	if f.userID > 0 {
		where = append(where, "user_id = ?")
		args = append(args, f.userID)
	}
	if f.reason != "" {
		where = append(where, "reason = ?")
		args = append(args, f.reason)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := d.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_node_migrations"+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计迁移历史失败: %w", err)
	}

	offset := (f.page - 1) * f.perPage
	if offset < 0 {
		offset = 0
	}
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, user_id, from_node_id, to_node_id, reason, operator, batch_id, created_at "+
			"FROM user_node_migrations"+clause+" ORDER BY id DESC LIMIT ? OFFSET ?",
		append(args, f.perPage, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询迁移历史失败: %w", err)
	}
	defer rows.Close()

	items := make([]migrationRecord, 0, f.perPage)
	for rows.Next() {
		rec, err := scanMigrationRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("读取迁移历史失败: %w", err)
	}
	return items, total, nil
}

// rowScanner 让 scanMigrationRecord 同时适配 *sql.Row 与 *sql.Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMigrationRecord(s rowScanner) (migrationRecord, error) {
	var (
		rec       migrationRecord
		fromNode  sql.NullInt64
		operator  sql.NullString
		batchID   sql.NullString
		createdAt sql.NullTime
	)
	if err := s.Scan(&rec.ID, &rec.UserID, &fromNode, &rec.ToNodeID, &rec.Reason,
		&operator, &batchID, &createdAt); err != nil {
		return rec, fmt.Errorf("解析迁移记录失败: %w", err)
	}
	if fromNode.Valid {
		v := fromNode.Int64
		rec.FromNodeID = &v
	}
	if operator.Valid {
		v := operator.String
		rec.Operator = &v
	}
	if batchID.Valid {
		v := batchID.String
		rec.BatchID = &v
	}
	if createdAt.Valid {
		v := createdAt.Time
		rec.CreatedAt = &v
	}
	return rec, nil
}

// loadBatch 取出某一批次的全部记录（按 id 升序，撤销按原顺序回放）。
func (d *MigrationDomain) loadBatch(ctx context.Context, batchID string) ([]migrationRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, user_id, from_node_id, to_node_id, reason, operator, batch_id, created_at "+
			"FROM user_node_migrations WHERE batch_id = ? ORDER BY id", batchID)
	if err != nil {
		return nil, fmt.Errorf("查询批次失败: %w", err)
	}
	defer rows.Close()

	out := make([]migrationRecord, 0, 64)
	for rows.Next() {
		rec, err := scanMigrationRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取批次失败: %w", err)
	}
	return out, nil
}

// migrationUndoResult 是撤销的结果。
type migrationUndoResult struct {
	BatchID string
	// Undone 是实际回滚的绑定条数。
	Undone int
	// Rebound 是「回到来源节点」的条数（from_node_id 为空的是首次分配，撤销只能解绑）。
	Rebound int
	// Unbound 是解绑条数（首次分配类记录，或来源节点已被删除）。
	Unbound int
	// OldestText 是批次里最早一条记录的时间（MySQL 口径的字符串，仅用于日志/排查）。
	OldestText string
}

// undoBatch 按批次反向回滚 user_nodes。
//
// 规则：
//   - 只允许 24 小时内的批次，超时拒绝并说明原因（migrationReject）；
//   - from_node_id 有值 → user_nodes 里把 (user, to) 删掉、(user, from) 补回来；
//   - from_node_id 为空（首次分配，没有来源）→ 只解绑 (user, to)；
//   - 每条都顺手 UPDATE users.updated_at：订阅指纹靠这个时间判断「要不要重拉配置」，
//     不更新的话用户节点虽然搬回去了，但下发到客户端的配置还是旧的（与 scheduler 的
//     Migrate 同一原因，见 region-08 任务书第 58 行）。
func (d *MigrationDomain) undoBatch(ctx context.Context, batchID string) (migrationUndoResult, error) {
	res := migrationUndoResult{BatchID: batchID}

	records, err := d.loadBatch(ctx, batchID)
	if err != nil {
		return res, err
	}
	if len(records) == 0 {
		return res, migrationRejectf("批次 %s 不存在（或记录已被清理），没有可撤销的内容", batchID)
	}

	// 24 小时判定必须在 SQL 里做：Go 侧 now() 的时区标签与 MySQL 会话时区不一致时，
	// time.Time.Sub() 会把「8 小时时差」算进时长里（实测 MySQL 会话 +08:00、DSN loc=UTC
	// 时，25 小时前的记录被算成 17 小时前，过期的批次反而能撤销）。
	// 让 MySQL 拿自己的两个值相减，与部署时区无关。
	oldest, hours, err := d.batchAge(ctx, batchID)
	if err != nil {
		return res, err
	}
	if oldest == "" {
		return res, migrationRejectf("批次 %s 的记录缺少创建时间，无法判断是否在 %d 小时撤销窗口内，已拒绝",
			batchID, int(migrationUndoWindow.Hours()))
	}
	res.OldestText = oldest
	if hours > int64(migrationUndoWindow.Hours()) {
		return res, migrationRejectf(
			"该批次最早一条记录在 %s（%d 小时前），已超过 %d 小时撤销窗口，已拒绝。"+
				"更早的批次期间用户可能已被其他批次迁移或续费变更，反向回滚会把节点绑定改错",
			oldest, hours, int(migrationUndoWindow.Hours()))
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("开启撤销事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, rec := range records {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM user_nodes WHERE user_id = ? AND node_id = ?", rec.UserID, rec.ToNodeID); err != nil {
			return res, fmt.Errorf("解绑 user=%d node=%d 失败: %w", rec.UserID, rec.ToNodeID, err)
		}
		if rec.FromNodeID != nil {
			if _, err := tx.ExecContext(ctx,
				"INSERT IGNORE INTO user_nodes (user_id, node_id) VALUES (?, ?)",
				rec.UserID, *rec.FromNodeID); err != nil {
				return res, fmt.Errorf("回绑 user=%d node=%d 失败: %w", rec.UserID, *rec.FromNodeID, err)
			}
			res.Rebound++
		} else {
			res.Unbound++
		}
		// updated_at 交给 MySQL 的 NOW()：与 scheduler 写入口径一致，不受 Go 侧时区影响。
		if _, err := tx.ExecContext(ctx,
			"UPDATE users SET updated_at = NOW() WHERE id = ?", rec.UserID); err != nil {
			// 用户行被删是正常情况（迁移后销号），不阻断撤销。
			if !isUnknownUserError(err) {
				return res, fmt.Errorf("刷新 user=%d 时间戳失败: %w", rec.UserID, err)
			}
		}
		res.Undone++
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("提交撤销事务失败: %w", err)
	}
	return res, nil
}

// batchAge 用 MySQL 自己算批次最早一条记录的时刻与「多少小时前」。
// 全为 NULL 时 oldest 为空串、hours 为 0。
func (d *MigrationDomain) batchAge(ctx context.Context, batchID string) (oldest string, hours int64, err error) {
	var (
		at   sql.NullString
		diff sql.NullInt64
	)
	err = d.db.QueryRowContext(ctx,
		"SELECT DATE_FORMAT(MIN(created_at), '%Y-%m-%d %H:%i:%s'), "+
			"TIMESTAMPDIFF(HOUR, MIN(created_at), NOW()) "+
			"FROM user_node_migrations WHERE batch_id = ?", batchID).Scan(&at, &diff)
	if err != nil {
		return "", 0, fmt.Errorf("查询批次时间失败: %w", err)
	}
	return at.String, diff.Int64, nil
}

// isUnknownUserError 判断 UPDATE users 是否撞上「表/行不存在」这类可忽略的错误。
// MySQL 1146 = 表不存在（极端：老库还没跑 ensureSchema），1054 = 列不存在。
func isUnknownUserError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "1146") || strings.Contains(msg, "1054")
}

// stampBatch 把「本任务期间新写入的迁移记录」打上批次号。
//
// 为什么是事后打标而不是写入时带：调度引擎（internal/scheduler）的 INSERT 没有 batch_id
// 这一列，本任务不允许改它。用 id 水位线（任务开始前的 MAX(id)）比用时间窗更稳：
// 引擎写 created_at 不一定填（列默认 NULL），而自增 id 一定连续递增。
//
// created_at 也一并补（COALESCE 只在为空时写）：NULL 的行没法判断 24 小时撤销窗口，
// 补成打标时刻（MySQL NOW()，与本仓其他写入口径一致）就能判断了。
func (d *MigrationDomain) stampBatch(ctx context.Context, batchID, reason string, afterID int64, from, to *int64) (int64, error) {
	q := "UPDATE user_node_migrations SET batch_id = ?, created_at = COALESCE(created_at, NOW()) " +
		"WHERE batch_id IS NULL AND id > ? AND reason = ?"
	args := []any{batchID, afterID, reason}
	if from != nil {
		q += " AND from_node_id = ?"
		args = append(args, *from)
	}
	if to != nil {
		q += " AND to_node_id = ?"
		args = append(args, *to)
	}
	res, err := d.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("打批次号失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取打标影响行数失败: %w", err)
	}
	return n, nil
}

// maxMigrationID 取当前迁移记录的最大 id（本任务的水位线）。
func (d *MigrationDomain) maxMigrationID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	if err := d.db.QueryRowContext(ctx, "SELECT MAX(id) FROM user_node_migrations").Scan(&id); err != nil {
		return 0, fmt.Errorf("读取迁移记录水位线失败: %w", err)
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// nodeExists 判断节点是否存在（迁移的源/目标都必须是真实节点）。
func (d *MigrationDomain) nodeExists(ctx context.Context, id int64) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE id = ?", id).Scan(&n)
	if err != nil {
		if isUnknownUserError(err) {
			// nodes 表不存在（库还没初始化）—— 当成「节点不存在」，不抛 500。
			return false, nil
		}
		return false, fmt.Errorf("查询节点失败: %w", err)
	}
	return n > 0, nil
}

// regionExists 判断地区是否存在（批量均衡的目标地区）。
func (d *MigrationDomain) regionExists(ctx context.Context, id int64) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM regions WHERE id = ?", id).Scan(&n)
	if err != nil {
		if isUnknownUserError(err) {
			return false, nil
		}
		return false, fmt.Errorf("查询地区失败: %w", err)
	}
	return n > 0, nil
}

// errNoEngine 是「调度引擎还没接线」时的统一文案。
// 本任务只交付接口层：internal/scheduler 由并行任务创建，合入后只需在 cmd/server 的
// 构造处把 adapter 传进 MigrationDeps.Engine，这里就会自动变成可执行。
const errNoEngine = "迁移引擎未就绪：internal/scheduler 尚未接入（接口与后台任务已就绪，接线后即可执行）"
