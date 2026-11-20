package scheduler

// Backfill —— 老用户批量入组（地区功能补数据）。
//
// 背景：面板升级出「地区」后，老用户 user_nodes 是空表 —— 容量统计永远是 0，
// 均衡 / 迁移也搬不到他们。这里给「一条绑定都没有」的合格用户，按地区各补一条绑定。
//
// 与 Assign 的边界：
//   - Assign 是「这个用户还缺哪个地区」按需分配（购买 / 换套餐后调用）；
//   - Backfill 是管理端显式触发的一次批量补齐，按用户 + 地区批量推进，可 dry 预览。
//   - 选择节点的口径完全复用 pickNode（负载率升序、NULL 最后、平局小 id），
//     保证「批量补」与「正常分配」落到同一台节点上。
//
// 合格用户：enabled=1、未软删、有套餐且未过期、user_nodes 一条绑定都没有。
// 每用户每地区最多一条（INSERT IGNORE，重复键直接忽略）；套餐限制地区时只在套餐允许的地区里补。
//
// 幂等：真实执行只写 user_nodes，重复调用不会产生重复绑定（主键 + INSERT IGNORE）；
// dry=true 只按同一套选择逻辑在内存里推演，不写一行。
//
// 关于「模拟计数」：dry 与真实执行都以「数据库当前绑定数」为起点，在同一批用户上
// 按 user_id 升序、地区按 sort/id 升序推进，每选中一台就在内存里 +1 —— 这样 dry 的
// 预览结果与紧接着的真实执行一致；真实执行下并发写入只会让实际负载更高（偏保守），
// 不会出现 dry 少算、实际灌爆的情况。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"controlhub-server/internal/planregion"
)

const (
	// BackfillDefaultLimit 是单次批量的默认用户数。
	BackfillDefaultLimit = 500
	// BackfillMaxLimit 是单次批量的硬上限（管理员可以传更小，传更大按此夹住）。
	BackfillMaxLimit = 5000
)

// BackfillOptions 是一次批量入组的入参。
type BackfillOptions struct {
	// Dry=true 只预览、不写库（管理端默认）。
	Dry bool
	// Limit 是本批处理的候选用户数；<=0 用 BackfillDefaultLimit，超过 BackfillMaxLimit 夹住。
	Limit int
}

// BackfillNodeCount 是一个节点在本批里新增的绑定数。
type BackfillNodeCount struct {
	NodeID int64
	Count  int
}

// BackfillSkip 是一类「用户 → 地区」跳过统计（同一地区同一原因聚合成一行）。
type BackfillSkip struct {
	RegionID   int64
	RegionName string
	Reason     string
	Count      int
}

// BackfillResult 是一次批量入组的结果（dry 时为「将会发生」的推演值）。
type BackfillResult struct {
	Dry          bool
	Candidates   int // 本批开始时数据库里符合条件的候选用户总数（可能 > limit）
	Assigned     int // 实际写入（dry：计划写入）的绑定条数
	UsersDone    int // 至少入组成功一个地区的用户数
	UsersSkipped int // 本批处理了、但一条都没写成的用户数
	Remaining    int // 本批之后仍未处理到的候选用户数
	ByNode       []BackfillNodeCount
	Skipped      []BackfillSkip
}

// backfillSkipReasonNoCapacity 是某个地区没有可分配节点时的统一说明。
const backfillSkipReasonNoCapacity = "该地区暂无可分配节点"

// backfillUser 是候选用户（本批要处理的人）。
type backfillUser struct {
	id     uint64
	planID uint64
}

// backfillRegion 是地区字典一行（顺序即处理顺序：sort 升序、id 升序）。
type backfillRegion struct {
	id   int64
	name string
}

// backfillScope 是该用户套餐允许的地区范围。
//
// restricted=false 表示不限地区（plans.region_ids 为 NULL / 空数组 / 解析失败）；
// restricted=true 时只处理 allowed 里的地区。
type backfillScope struct {
	restricted bool
	allowed    map[int64]bool
}

// Backfill 执行一次老用户批量入组。
func (s *Scheduler) Backfill(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = BackfillDefaultLimit
	}
	if limit > BackfillMaxLimit {
		limit = BackfillMaxLimit
	}
	res := BackfillResult{Dry: opts.Dry}

	total, err := s.backfillCandidateCount(ctx)
	if err != nil {
		return res, err
	}
	res.Candidates = total
	res.Remaining = total
	if total == 0 {
		res.ByNode = []BackfillNodeCount{}
		res.Skipped = []BackfillSkip{}
		return res, nil
	}

	users, err := s.backfillCandidates(ctx, limit)
	if err != nil {
		return res, err
	}
	regions, err := s.backfillRegions(ctx)
	if err != nil {
		return res, err
	}
	scopes, err := s.backfillPlanScopes(ctx, users)
	if err != nil {
		return res, err
	}

	done := make(map[uint64]bool, len(users))
	byNode := make(map[int64]int, len(regions))
	skippedByRegion := make(map[int64]int, len(regions))

	for _, region := range regions {
		// 每地区只读一次候选与负载，之后在内存里按 pickNode 的口径推演（dry 与真实执行同一套）。
		cands, err := s.candidates(ctx, region.id)
		if err != nil {
			return res, err
		}

		for i := range users {
			u := users[i]
			if scope := scopes[u.planID]; scope.restricted && !scope.allowed[region.id] {
				continue // 套餐不允许这个地区，安静跳过（不算失败）
			}
			nodeID, ok := pickAndUse(cands)
			if !ok {
				skippedByRegion[region.id]++
				continue
			}
			inserted := 1
			if !opts.Dry {
				inserted, err = s.insertBackfillBinding(ctx, u.id, nodeID)
				if err != nil {
					return res, err
				}
			}
			if inserted == 0 {
				// 并发下该用户已经被别的请求绑定过了：不计入本批。
				// 内存计数里为这台节点多算的 1 只会让后续选择更保守，不会重复写库。
				continue
			}
			res.Assigned++
			byNode[nodeID]++
			done[u.id] = true
		}
	}

	res.UsersDone = len(done)
	res.UsersSkipped = len(users) - res.UsersDone
	res.Remaining = total - len(users)
	if res.Remaining < 0 {
		res.Remaining = 0
	}
	res.ByNode = presentBackfillNodeCounts(byNode)
	res.Skipped = presentBackfillSkips(regions, skippedByRegion)
	return res, nil
}

// insertBackfillBinding 写一条绑定并返回实际插入行数（重复键 = 0，不算错误）。
func (s *Scheduler) insertBackfillBinding(ctx context.Context, userID uint64, nodeID int64) (int, error) {
	r, err := s.db.ExecContext(ctx,
		"INSERT IGNORE INTO user_nodes (user_id, node_id) VALUES (?, ?)", userID, nodeID)
	if err != nil {
		return 0, fmt.Errorf("写入用户节点绑定失败: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取绑定写入结果失败: %w", err)
	}
	return int(n), nil
}

// pickAndUse 选一台节点并在模拟计数里 +1（dry 与真实执行共用）。
func pickAndUse(cands []candidate) (int64, bool) {
	nodeID, err := pickNode(cands)
	if err != nil {
		return 0, false
	}
	for i := range cands {
		if cands[i].id == nodeID {
			cands[i].bound++
			break
		}
	}
	return nodeID, true
}

// backfillCandidateWhere 是「合格老用户」的统一过滤条件（users 别名必须是 u）。
const backfillCandidateWhere = `
	u.enabled = 1
	AND u.deleted_at IS NULL
	AND u.plan_id IS NOT NULL
	AND (u.expired_at IS NULL OR u.expired_at > NOW())
	AND NOT EXISTS (SELECT 1 FROM user_nodes un WHERE un.user_id = u.id)`

// backfillCandidateCount 数一遍合格候选用户（「remaining」的分母）。
func (s *Scheduler) backfillCandidateCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users u WHERE "+backfillCandidateWhere).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计待入组用户失败: %w", err)
	}
	return n, nil
}

// backfillCandidates 取本批要处理的候选用户（user_id 升序，保证确定性）。
func (s *Scheduler) backfillCandidates(ctx context.Context, limit int) ([]backfillUser, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT u.id, u.plan_id FROM users u WHERE "+backfillCandidateWhere+" ORDER BY u.id LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("查询待入组用户失败: %w", err)
	}
	defer rows.Close()

	out := make([]backfillUser, 0, limit)
	for rows.Next() {
		var u backfillUser
		if err := rows.Scan(&u.id, &u.planID); err != nil {
			return nil, fmt.Errorf("读取待入组用户失败: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历待入组用户失败: %w", err)
	}
	return out, nil
}

// backfillRegions 取地区字典（sort 升序、id 升序，与 unboundRegions 的顺序一致）。
func (s *Scheduler) backfillRegions(ctx context.Context) ([]backfillRegion, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT r.id, r.name FROM regions r ORDER BY r.sort, r.id")
	if err != nil {
		return nil, fmt.Errorf("查询地区列表失败: %w", err)
	}
	defer rows.Close()

	var out []backfillRegion
	for rows.Next() {
		var r backfillRegion
		if err := rows.Scan(&r.id, &r.name); err != nil {
			return nil, fmt.Errorf("读取地区失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历地区失败: %w", err)
	}
	return out, nil
}

// backfillPlanScopes 按套餐批量取地区范围（同一套餐只查一次，避免每用户一条 SQL）。
func (s *Scheduler) backfillPlanScopes(ctx context.Context, users []backfillUser) (map[uint64]backfillScope, error) {
	scopes := make(map[uint64]backfillScope)
	for _, u := range users {
		if _, ok := scopes[u.planID]; ok {
			continue
		}
		scope, err := s.backfillPlanScope(ctx, u.planID)
		if err != nil {
			return nil, err
		}
		scopes[u.planID] = scope
	}
	return scopes, nil
}

// backfillPlanScope 读一个套餐的 region_ids；套餐行不存在按「不限地区」处理
// （与 planregion.Allowed 的兼容口径一致：宁可多分，不能因脏数据让人看不到节点）。
func (s *Scheduler) backfillPlanScope(ctx context.Context, planID uint64) (backfillScope, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT region_ids FROM plans WHERE id = ?", planID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return backfillScope{}, nil
	}
	if err != nil {
		return backfillScope{}, fmt.Errorf("查询套餐地区失败: %w", err)
	}
	ids, restricted := planregion.Parse(raw)
	if !restricted {
		return backfillScope{}, nil
	}
	allowed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	return backfillScope{restricted: true, allowed: allowed}, nil
}

// presentBackfillNodeCounts 把节点分布压成 id 升序的数组（输出稳定，前端好对账）。
func presentBackfillNodeCounts(byNode map[int64]int) []BackfillNodeCount {
	out := make([]BackfillNodeCount, 0, len(byNode))
	for nodeID, count := range byNode {
		out = append(out, BackfillNodeCount{NodeID: nodeID, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// presentBackfillSkips 把「地区 → 跳过人数」压成按 region_id 升序的数组。
func presentBackfillSkips(regions []backfillRegion, skipped map[int64]int) []BackfillSkip {
	out := make([]BackfillSkip, 0, len(skipped))
	for _, region := range regions {
		count := skipped[region.id]
		if count == 0 {
			continue
		}
		out = append(out, BackfillSkip{
			RegionID:   region.id,
			RegionName: region.name,
			Reason:     backfillSkipReasonNoCapacity,
			Count:      count,
		})
	}
	return out
}
