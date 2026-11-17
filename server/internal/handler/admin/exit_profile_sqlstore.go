package admin

// exitProfileSQLStore 是 ExitProfileStore 的 database/sql 实现。
//
// 三条硬约束：
//  1. 分类行与关联表（exit_profile_nodes）必须同事务：分类建好了却没有节点、
//     或者修改时把旧关联删了却没插上新的，对运营都是「数据丢了却看不出来」。
//  2. 列表不逐行查关联表：一次 IN 查询把全部分类的 node_ids 取回来在 Go 侧分组，
//     否则 50 个分类就是 50 条子查询（N+1）。
//  3. 删除时先数套餐绑定，有绑定就返回 ErrExitProfileInUse（带套餐名），不删一行。

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
)

// NewExitProfileSQLStore 用已有连接池构造出口分类域 Store。
func NewExitProfileSQLStore(db *sql.DB) ExitProfileStore {
	return &exitProfileSQLStore{db: db}
}

type exitProfileSQLStore struct {
	db *sql.DB
}

// exitProfileColumns 是分类自身的列（关联表与账号名单独查，见 ExitProfileList）。
//
// LEFT JOIN proxy_providers 而不是 INNER：账号被删了分类也得能读出来
// （否则管理端整页 500，连「把这个分类改成别的账号」都做不到）。
const exitProfileColumns = `ep.id, ep.name, ep.provider_id, ep.traffic_gb, ep.duration_days,
	ep.max_thread_count, ep.socks_host, ep.socks_port, ep.enabled,
	COALESCE(pp.name, ''), ep.created_at, ep.updated_at
	FROM exit_profiles ep LEFT JOIN proxy_providers pp ON pp.id = ep.provider_id`

// scanExitProfile 扫一行（列顺序与 exitProfileColumns 一致）。
func scanExitProfile(row interface{ Scan(...any) error }) (*ExitProfileRecord, error) {
	var (
		p                    ExitProfileRecord
		enabled              int
		createdAt, updatedAt sql.NullTime
	)
	if err := row.Scan(&p.ID, &p.Name, &p.ProviderID, &p.TrafficGB, &p.DurationDays,
		&p.MaxThreadCount, &p.SocksHost, &p.SocksPort, &enabled,
		&p.ProviderName, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = nullTimePtr(createdAt)
	p.UpdatedAt = nullTimePtr(updatedAt)
	p.NodeIDs = []uint64{}
	return &p, nil
}

// ExitProfileList 返回全部分类 + node_ids。
//
// 关联表一次取全（两列都很小，分类数量是「十几个」量级），在 Go 侧按 profile_id 分组：
// 比每个分类一条子查询少 N 次往返，也不会随分类数线性变慢。
func (s *exitProfileSQLStore) ExitProfileList(ctx context.Context) ([]ExitProfileRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+exitProfileColumns+" ORDER BY ep.id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ExitProfileRecord{}
	for rows.Next() {
		p, err := scanExitProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	nodes, err := s.profileNodeIDs(ctx, 0)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if ids, ok := nodes[out[i].ID]; ok {
			out[i].NodeIDs = ids
		}
	}
	return out, nil
}

// profileNodeIDs 读关联表并按 profile_id 分组（profileID=0 表示全部）。
func (s *exitProfileSQLStore) profileNodeIDs(ctx context.Context, profileID uint64) (map[uint64][]uint64, error) {
	query := "SELECT profile_id, node_id FROM exit_profile_nodes"
	args := []any{}
	if profileID != 0 {
		query += " WHERE profile_id = ?"
		args = append(args, profileID)
	}
	query += " ORDER BY profile_id, node_id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint64][]uint64{}
	for rows.Next() {
		var pid, nid uint64
		if err := rows.Scan(&pid, &nid); err != nil {
			return nil, err
		}
		out[pid] = append(out[pid], nid)
	}
	return out, rows.Err()
}

// ExitProfileByID 查单个分类；不存在返回 sql.ErrNoRows。
func (s *exitProfileSQLStore) ExitProfileByID(ctx context.Context, id uint64) (*ExitProfileRecord, error) {
	p, err := scanExitProfile(s.db.QueryRowContext(ctx,
		"SELECT "+exitProfileColumns+" WHERE ep.id = ?", id))
	if err != nil {
		return nil, err
	}
	nodes, err := s.profileNodeIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	if ids, ok := nodes[id]; ok {
		p.NodeIDs = ids
	}
	return p, nil
}

// ExitProfileCreate 新建分类 + 关联表，同事务。
func (s *exitProfileSQLStore) ExitProfileCreate(ctx context.Context, in ExitProfileInput) (*ExitProfileRecord, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后 Rollback 是 no-op

	res, err := tx.ExecContext(ctx, `INSERT INTO exit_profiles
		(name, provider_id, traffic_gb, duration_days, max_thread_count,
		 socks_host, socks_port, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Name, in.ProviderID, in.TrafficGB, in.DurationDays, in.MaxThreadCount,
		in.SocksHost, in.SocksPort, boolToInt(in.Enabled), now, now)
	if err != nil {
		if isDuplicateKey(err) {
			return nil, ErrExitProfileNameTaken
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := replaceProfileNodes(ctx, tx, uint64(id), in.NodeIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ExitProfileByID(ctx, uint64(id))
}

// ExitProfileUpdate 整行替换分类 + 同步关联表（旧的删掉、新的插上），同事务。
//
// 「旧的全删再全插」而不是算差集：一张分类最多十几个节点，删插两条语句；
// 算差集要读旧值、比两次集合，代码更长而收益为零。
func (s *exitProfileSQLStore) ExitProfileUpdate(ctx context.Context, id uint64, in ExitProfileInput) (*ExitProfileRecord, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后 Rollback 是 no-op

	// 先确认分类存在：MySQL 的 UPDATE 影响 0 行既可能是「值没变」也可能是「行不存在」，
	// 而生产 DSN 带 clientFoundRows=true（匹配行数而非变更行数），不能拿 RowsAffected 判断。
	var exists int
	switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM exit_profiles WHERE id = ?", id).Scan(&exists); {
	case err == sql.ErrNoRows:
		return nil, sql.ErrNoRows
	case err != nil:
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE exit_profiles SET
		name = ?, provider_id = ?, traffic_gb = ?, duration_days = ?, max_thread_count = ?,
		socks_host = ?, socks_port = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		in.Name, in.ProviderID, in.TrafficGB, in.DurationDays, in.MaxThreadCount,
		in.SocksHost, in.SocksPort, boolToInt(in.Enabled), now, id); err != nil {
		if isDuplicateKey(err) {
			return nil, ErrExitProfileNameTaken
		}
		return nil, err
	}
	if err := replaceProfileNodes(ctx, tx, id, in.NodeIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ExitProfileByID(ctx, id)
}

// replaceProfileNodes 清空某分类的关联再插入新的一批（同一事务内）。
func replaceProfileNodes(ctx context.Context, tx *sql.Tx, profileID uint64, nodeIDs []uint64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM exit_profile_nodes WHERE profile_id = ?", profileID); err != nil {
		return err
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	// 多值 INSERT：一条语句插完，节点数很少（最多几十），不必上占位符分批。
	placeholders := make([]string, 0, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs)*2)
	for _, nid := range nodeIDs {
		placeholders = append(placeholders, "(?, ?)")
		args = append(args, profileID, nid)
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO exit_profile_nodes (profile_id, node_id) VALUES "+strings.Join(placeholders, ", "), args...)
	return err
}

// ExitProfileDelete 删除分类 + 关联表，同事务；不存在返回 sql.ErrNoRows。
//
// 「有套餐绑着不能删」的守卫在 handler（先查绑定拿套餐名），
// 但这里再数一次是为了防并发：守卫通过之后、删除之前有人新建了绑这个分类的套餐，
// 事务里这次复查会把删除挡掉，不会留下悬空的 plans.exit_profile_id。
func (s *exitProfileSQLStore) ExitProfileDelete(ctx context.Context, id uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后 Rollback 是 no-op

	bindings, err := queryProfileBindings(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(bindings) > 0 {
		return &ErrExitProfileInUse{Count: int64(len(bindings)), Names: bindingNames(bindings)}
	}

	res, err := tx.ExecContext(ctx, "DELETE FROM exit_profiles WHERE id = ?", id)
	if err != nil {
		return err
	}
	// clientFoundRows=true 只改 UPDATE 的 RowsAffected 语义（匹配行数 vs 变更行数），
	// DELETE 仍然是「实际删掉的行数」，所以 0 行可以直接判「分类不存在」。
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM exit_profile_nodes WHERE profile_id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// ExitProfileNameTaken 判断名称是否被别的分类占用（excludeID=0 表示新建场景）。
//
// 大小写敏感性跟随列的 collation（utf8mb4_unicode_ci → 不分大小写），
// 与唯一键 exit_profiles_name_unique 的判定一致。
func (s *exitProfileSQLStore) ExitProfileNameTaken(ctx context.Context, name string, excludeID uint64) (bool, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM exit_profiles WHERE name = ? AND id <> ?", name, excludeID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// bindingQuery 是「绑定了某分类的套餐」查询（删除守卫与删除事务复查共用）。
const bindingQuery = `SELECT id, name FROM plans WHERE exit_profile_id = ? ORDER BY id`

type bindingsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryProfileBindings 执行绑定查询（*sql.DB 与 *sql.Tx 都满足 bindingsQuerier）。
func queryProfileBindings(ctx context.Context, q bindingsQuerier, profileID uint64) ([]ExitProfileBinding, error) {
	rows, err := q.QueryContext(ctx, bindingQuery, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ExitProfileBinding{}
	for rows.Next() {
		var b ExitProfileBinding
		if err := rows.Scan(&b.ID, &b.Name); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// bindingNames 取套餐名（文案用，顺序与 id 升序一致）。
func bindingNames(rows []ExitProfileBinding) []string {
	out := make([]string, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.Name)
	}
	return out
}

// ExitProfileBindings 返回绑定了该分类的套餐（handler 的删除守卫）。
func (s *exitProfileSQLStore) ExitProfileBindings(ctx context.Context, id uint64) ([]ExitProfileBinding, error) {
	return queryProfileBindings(ctx, s.db, id)
}

// ProviderExists 判断 provider_id 是否为真实账号。
//
// 不建外键（老库升级加不了），入参就得自己校验：存进去一个不存在的账号，
// 列表里那行会显示「—」，等真正开号（后续任务）才报「账号不存在」就太晚了。
func (s *exitProfileSQLStore) ProviderExists(ctx context.Context, id uint64) (bool, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_providers WHERE id = ?", id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// exitProfileNodeColumns 是「前置节点」的展示列（详情与存在性校验共用）。
const exitProfileNodeColumns = `id, name, host, driver_type, status, enabled FROM nodes`

// NodesByIDs 返回 ids 里真实存在的节点（顺序按 id 升序，与入参顺序无关）。
//
// 用 IN 一次查回来再比数量：比逐个 SELECT 1 少 N 次往返，也避免
// 「查到一个就算通过」放过其中不存在的那个。
func (s *exitProfileSQLStore) NodesByIDs(ctx context.Context, ids []uint64) ([]ExitProfileNode, error) {
	if len(ids) == 0 {
		return []ExitProfileNode{}, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+exitProfileNodeColumns+" WHERE id IN ("+strings.Join(placeholders, ",")+") ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	return scanExitProfileNodes(rows)
}

// ExitProfileNodes 返回某分类绑定的前置节点。
//
// INNER JOIN：节点被删后关联表的残留行不该出现在详情里（删除接口会清，
// 但历史数据可能已经脏了）。status/enabled 一并给出，前端不用再拉一遍 /nodes。
func (s *exitProfileSQLStore) ExitProfileNodes(ctx context.Context, id uint64) ([]ExitProfileNode, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.name, n.host, n.driver_type, n.status, n.enabled
		FROM exit_profile_nodes epn INNER JOIN nodes n ON n.id = epn.node_id
		WHERE epn.profile_id = ? ORDER BY n.id`, id)
	if err != nil {
		return nil, err
	}
	return scanExitProfileNodes(rows)
}

func scanExitProfileNodes(rows *sql.Rows) ([]ExitProfileNode, error) {
	defer rows.Close()
	out := []ExitProfileNode{}
	for rows.Next() {
		var (
			n       ExitProfileNode
			enabled int
		)
		if err := rows.Scan(&n.ID, &n.Name, &n.Host, &n.DriverType, &n.Status, &enabled); err != nil {
			return nil, err
		}
		n.Enabled = enabled != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// normalizeNodeIDs 去重并排序（同一个节点在前端勾两次、或顺序不同都不该落成两行）。
func normalizeNodeIDs(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return []uint64{}
	}
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
