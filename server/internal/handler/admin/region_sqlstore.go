package admin

// regionSQLStore 是 RegionStore 的 database/sql 实现。
//
// 统计全部在 SQL 里做（COUNT/SUM 子查询），不把整表拉回 Go 再算：
// 节点数是 O(节点)，绑定表可能上百万行，任何一次列表请求都不该扫它两遍。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"controlhub-server/internal/metrics"
)

// NewRegionSQLStore 用已有连接池构造地区域 Store。
func NewRegionSQLStore(db *sql.DB) RegionStore {
	return &regionSQLStore{db: db}
}

type regionSQLStore struct {
	db *sql.DB
}

// RegionList 返回全部地区 + 四项统计（sort 升序，同 sort 按 id 升序）。
//
// 两个子查询分别聚合「节点」「绑定」，再按 region_id LEFT JOIN 回地区：
// 没有节点的地区统计全是 0（COALESCE），不会因为 INNER JOIN 丢行。
func (s *regionSQLStore) RegionList(ctx context.Context) ([]RegionSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.name, r.sort,
		       COALESCE(n.node_count, 0), COALESCE(n.capacity_total, 0),
		       COALESCE(u.user_count, 0), COALESCE(u.bound_count, 0)
		FROM regions r
		LEFT JOIN (
			SELECT region_id, COUNT(*) AS node_count, SUM(capacity) AS capacity_total
			FROM nodes WHERE region_id IS NOT NULL GROUP BY region_id
		) n ON n.region_id = r.id
		LEFT JOIN (
			SELECT nd.region_id, COUNT(DISTINCT un.user_id) AS user_count, COUNT(*) AS bound_count
			FROM nodes nd INNER JOIN user_nodes un ON un.node_id = nd.id
			WHERE nd.region_id IS NOT NULL GROUP BY nd.region_id
		) u ON u.region_id = r.id
		ORDER BY r.sort ASC, r.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RegionSummary{}
	for rows.Next() {
		var (
			r                    RegionSummary
			capacityTotal, bound sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.Sort,
			&r.NodeCount, &capacityTotal, &r.UserCount, &bound); err != nil {
			return nil, err
		}
		r.CapacityTotal = capacityTotal.Int64
		r.CapacityUsed = bound.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegionByID 查单个地区；不存在返回 sql.ErrNoRows。
func (s *regionSQLStore) RegionByID(ctx context.Context, id uint64) (*RegionRecord, error) {
	var r RegionRecord
	err := s.db.QueryRowContext(ctx, "SELECT id, name, sort FROM regions WHERE id = ?", id).
		Scan(&r.ID, &r.Name, &r.Sort)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// RegionCreate 新建地区；重名翻译成 ErrRegionNameTaken（并发下靠唯一键兜底）。
func (s *regionSQLStore) RegionCreate(ctx context.Context, name string, sort int64) (*RegionRecord, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO regions (name, sort, created_at, updated_at) VALUES (?, ?, ?, ?)",
		name, sort, now, now)
	if err != nil {
		if isDuplicateKey(err) {
			return nil, ErrRegionNameTaken
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &RegionRecord{ID: uint64(id), Name: name, Sort: sort}, nil
}

// RegionUpdate 只改传入的字段（nil = 不动），返回改后的整行。
//
// 新旧值完全相同时 MySQL 的 RowsAffected 是 0（默认非 FOUND_ROWS 模式），
// 不能用它判断「地区不存在」—— 0 行时再查一次，查不到才回 sql.ErrNoRows。
func (s *regionSQLStore) RegionUpdate(ctx context.Context, id uint64, name *string, sort *int64) (*RegionRecord, error) {
	sets := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *name)
	}
	if sort != nil {
		sets = append(sets, "sort = ?")
		args = append(args, *sort)
	}
	if len(sets) == 0 {
		return s.RegionByID(ctx, id)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC(), id)

	if _, err := s.db.ExecContext(ctx,
		"UPDATE regions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		if isDuplicateKey(err) {
			return nil, ErrRegionNameTaken
		}
		return nil, err
	}
	return s.RegionByID(ctx, id)
}

// RegionDelete 删除地区；地区不存在返回 sql.ErrNoRows。
// 「有节点不能删」的守卫在 handler（先查 RegionNodeCount 拿明确文案）。
func (s *regionSQLStore) RegionDelete(ctx context.Context, id uint64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM regions WHERE id = ?", id)
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

// RegionExists 判断地区 id 是否存在。
func (s *regionSQLStore) RegionExists(ctx context.Context, id uint64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM regions WHERE id = ?", id).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RegionNames 返回「地区 id → 名称」（一次查全表；地区数量小，不做分页）。
func (s *regionSQLStore) RegionNames(ctx context.Context) (map[uint64]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name FROM regions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint64]string{}
	for rows.Next() {
		var (
			id   uint64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// RegionNameTaken 判断名称是否被别的地区占用（excludeID=0 表示新建场景）。
//
// 大小写敏感性跟随列的 collation（utf8mb4_unicode_ci → 不分大小写），
// 与唯一键 regions_name_unique 的判定一致。
func (s *regionSQLStore) RegionNameTaken(ctx context.Context, name string, excludeID uint64) (bool, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM regions WHERE name = ? AND id <> ?", name, excludeID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// RegionNodeCount 是地区下的节点数（删除守卫）。
func (s *regionSQLStore) RegionNodeCount(ctx context.Context, id uint64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE region_id = ?", id).Scan(&n)
	return n, err
}

// regionNodeQuery 是「地区节点 / 未归属节点」共用的列与统计口径。
const regionNodeQuery = `
	SELECT n.id, n.name, n.host, n.driver_type, n.status, n.capacity,
	       (SELECT COUNT(*) FROM user_nodes un WHERE un.node_id = n.id) AS bound_users
	FROM nodes n`

// RegionNodes 返回某地区的节点（id 升序），并带上每个节点上的入站及其归属。
func (s *regionSQLStore) RegionNodes(ctx context.Context, id uint64) ([]RegionNode, error) {
	nodes, err := s.queryRegionNodes(ctx, regionNodeQuery+" WHERE n.region_id = ? ORDER BY n.id ASC", id)
	if err != nil {
		return nil, err
	}
	if err := s.attachRegionInbounds(ctx, nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// regionInboundQuery 读指定节点的入站并把「有效归属」解析出来：入站自己归属了地区就用它，
// 否则显示节点地区（跟随）。region_name 用 LEFT JOIN 取；没有地区时为 NULL。
const regionInboundQuery = `
	SELECT xi.id, xi.node_id, xi.tag, xi.protocol, xi.port, xi.enabled,
	       COALESCE(xi.region_id, n.region_id) AS eff_region_id, r.name
	FROM xray_inbounds xi
	INNER JOIN nodes n ON n.id = xi.node_id
	LEFT JOIN regions r ON r.id = COALESCE(xi.region_id, n.region_id)
	WHERE xi.node_id IN (%s)
	ORDER BY xi.node_id ASC, xi.sort ASC, xi.id ASC`

// attachRegionInbounds 一次性把 nodes 上挂的入站读出来（避免 N+1）。
func (s *regionSQLStore) attachRegionInbounds(ctx context.Context, nodes []RegionNode) error {
	if len(nodes) == 0 {
		return nil
	}
	ids := make([]any, len(nodes))
	placeholders := make([]string, len(nodes))
	index := make(map[uint64]int, len(nodes))
	for i := range nodes {
		ids[i] = nodes[i].ID
		placeholders[i] = "?"
		index[nodes[i].ID] = i
	}
	query := fmt.Sprintf(regionInboundQuery, strings.Join(placeholders, ","))
	rows, err := s.db.QueryContext(ctx, query, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			in     RegionInbound
			nodeID uint64
			effID  sql.NullInt64
			name   sql.NullString
		)
		if err := rows.Scan(&in.ID, &nodeID, &in.Tag, &in.Protocol, &in.Port, &in.Enabled,
			&effID, &name); err != nil {
			return err
		}
		if effID.Valid {
			v := uint64(effID.Int64)
			in.RegionID = &v
		}
		if name.Valid {
			n := name.String
			in.RegionName = &n
		}
		if i, ok := index[nodeID]; ok {
			nodes[i].Inbounds = append(nodes[i].Inbounds, in)
		}
	}
	return rows.Err()
}

// UnassignedNodes 返回 region_id IS NULL 的节点（全量下发）。
func (s *regionSQLStore) UnassignedNodes(ctx context.Context) ([]RegionNode, error) {
	return s.queryRegionNodes(ctx, regionNodeQuery+" WHERE n.region_id IS NULL ORDER BY n.id ASC")
}

func (s *regionSQLStore) queryRegionNodes(ctx context.Context, query string, args ...any) ([]RegionNode, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RegionNode{}
	for rows.Next() {
		var (
			n        RegionNode
			capacity sql.NullInt64
		)
		if err := rows.Scan(&n.ID, &n.Name, &n.Host, &n.DriverType, &n.Status,
			&capacity, &n.BoundUsers); err != nil {
			return nil, err
		}
		if capacity.Valid {
			v := capacity.Int64
			n.Capacity = &v
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeTotals 返回全局合计：节点数、容量合计（NULL 不计）、绑定合计。
//
// 绑定合计只算「节点还在」的绑定（INNER JOIN nodes）：
// 节点删除后 user_nodes 的残留行不该进看板，否则总已绑永远降不下来。
func (s *regionSQLStore) NodeTotals(ctx context.Context) (nodes, capacityTotal, capacityUsed int64, err error) {
	var capacity sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM nodes),
		       (SELECT SUM(capacity) FROM nodes),
		       (SELECT COUNT(*) FROM user_nodes un INNER JOIN nodes n ON n.id = un.node_id)`).
		Scan(&nodes, &capacity, &capacityUsed)
	if err != nil {
		return 0, 0, 0, err
	}
	return nodes, capacity.Int64, capacityUsed, nil
}

// Alerts 计算当前容量告警（节点级 + 地区级，实时算，不落库）。
//
// 算法在 internal/metrics.ComputeAlerts：告警是「容量水位」的函数，不属于地区 CRUD 语义，
// 所以不往 RegionStore 接口里加方法 —— 这里只把连接池递过去，handler 按可选能力
// （alertSource）取用，fake store 不必跟着实现。
func (s *regionSQLStore) Alerts(ctx context.Context) ([]metrics.Alert, error) {
	return metrics.ComputeAlerts(ctx, s.db)
}

// NodeBinding 读节点当前归属；节点不存在返回 sql.ErrNoRows。
func (s *regionSQLStore) NodeBinding(ctx context.Context, nodeID uint64) (regionID *uint64, capacity *int64, err error) {
	var (
		region      sql.NullInt64
		capacityCol sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx,
		"SELECT region_id, capacity FROM nodes WHERE id = ?", nodeID).Scan(&region, &capacityCol); err != nil {
		return nil, nil, err
	}
	if region.Valid {
		v := uint64(region.Int64)
		regionID = &v
	}
	if capacityCol.Valid {
		v := capacityCol.Int64
		capacity = &v
	}
	return regionID, capacity, nil
}

// NodeSetRegion 设置节点归属（nil = 移出地区，回到全量下发）。
func (s *regionSQLStore) NodeSetRegion(ctx context.Context, nodeID uint64, regionID *uint64) error {
	return s.execNodeUpdate(ctx, "UPDATE nodes SET region_id = ? WHERE id = ?", nodeValue(regionID), nodeID)
}

// NodeSetCapacity 设置节点容量（nil = 不限）。
func (s *regionSQLStore) NodeSetCapacity(ctx context.Context, nodeID uint64, capacity *int64) error {
	return s.execNodeUpdate(ctx, "UPDATE nodes SET capacity = ? WHERE id = ?", nodeValue(capacity), nodeID)
}

// execNodeUpdate 执行单列更新并在 0 行时区分「改了但值相同」与「节点不存在」。
func (s *regionSQLStore) execNodeUpdate(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var one int
	switch err := s.db.QueryRowContext(ctx, "SELECT 1 FROM nodes WHERE id = ?", args[len(args)-1]).Scan(&one); {
	case err == sql.ErrNoRows:
		return sql.ErrNoRows
	case err != nil:
		return err
	}
	return nil
}

// nodeValue 把可空指针转成驱动能接受的参数（nil → SQL NULL）。
func nodeValue[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}
