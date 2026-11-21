// Package planregion 是「套餐绑定地区」的公共口径（#地区功能-15）。
//
// 契约：
//   - plans.region_ids 为 NULL（或空数组）= 套餐不限地区，老套餐行为完全不变；
//   - 非空 = 买了该套餐的用户只能落在这些地区的节点上，订阅/节点列表里也只看得到这些地区；
//   - 用户没有套餐（users.plan_id 为 NULL）或套餐行已不存在 → 同样按「不限」处理。
//
// 之所以单独成包：这条口径要被三处同时遵守 —— 分配（internal/scheduler）、
// 订阅渲染（internal/subscription）、用户端节点列表（internal/handler/api）。
// SQL 各写一遍迟早走样，尤其是「region_id IS NULL 的老节点不受限」这条兼容分支。
//
// 入站级地区（region_ids 下沉一层）：xray_inbounds.region_id 非 NULL 时以入站为准，
// 有效地区 = COALESCE(入站地区, 节点地区)。只影响订阅渲染（按入站过滤），
// 分配 / 均衡 / 迁移 / 容量仍按节点 —— 那些路径继续用 NodeFilter。
package planregion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Allowed 返回该用户当前套餐允许的地区 id 列表。
//
// restricted=false 表示不限地区，调用方应当原样使用升级前的查询 —— 一个条件都不加。
func Allowed(ctx context.Context, db *sql.DB, userID uint64) (ids []int64, restricted bool, err error) {
	var raw sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT p.region_ids
		FROM users u
		JOIN plans p ON p.id = u.plan_id
		WHERE u.id = ?`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("查询套餐地区失败: %w", err)
	}
	ids, restricted = Parse(raw)
	return ids, restricted, nil
}

// Parse 解析 plans.region_ids（JSON 数组文本）。
//
// NULL / 空串 / "null" / 空数组 / 解析失败一律按「不限」处理：宁可多给用户节点，
// 也不能因为一条脏数据让用户凭空看不到节点 —— 老套餐存的就是 NULL。
func Parse(raw sql.NullString) ([]int64, bool) {
	if !raw.Valid {
		return nil, false
	}
	text := strings.TrimSpace(raw.String)
	if text == "" || text == "null" {
		return nil, false
	}
	var ids []int64
	if err := json.Unmarshal([]byte(text), &ids); err != nil {
		return nil, false
	}
	if len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

// NodeFilter 给「节点查询」追加套餐地区过滤（nodes 的表别名必须叫 n）：
//
//	AND (n.region_id IS NULL OR n.region_id IN (?, ...))
//
// n.region_id IS NULL 这条兼容分支必须留：没归属地区的老节点不受套餐地区限制，
// 否则一升级、老用户给老套餐一设地区，他们订阅里就突然少了节点。
//
// ids 为空时返回空串（不影响老查询）。
func NodeFilter(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	list, args := In(ids)
	return " AND (n.region_id IS NULL OR n.region_id IN " + list + ")", args
}

// InboundNodeFilter 给「节点查询」追加「入站级地区」过滤（节点表别名必须叫 n，
// 且查询的 FROM 必须是 nodes n —— 入站表在子查询里用固定别名 xi）：
//
//	AND (
//	    (n.driver_type <> 'xray' AND (n.region_id IS NULL OR n.region_id IN (?, ...)))
//	    OR (n.driver_type = 'xray' AND EXISTS (
//	        SELECT 1 FROM xray_inbounds xi
//	        WHERE xi.node_id = n.id AND xi.enabled = 1
//	          AND (COALESCE(xi.region_id, n.region_id) IS NULL
//	               OR COALESCE(xi.region_id, n.region_id) IN (?, ...))
//	    ))
//	)
//
// 为什么分驱动：只有 xray 节点的入站落在 xray_inbounds（自建、带 region_id）；
// 3x-ui 节点的入站挂在面板上（node_inbounds 只是协议 → inbound_id 的映射，没有地区概念），
// 所以它们继续按节点地区过滤 —— 与升级前逐字节一致。
//
// 对 xray 节点，有效地区 = COALESCE(入站地区, 节点地区)：
//   - 入站没归属 → 跟随节点；节点也没归属 → 不受限（老数据兼容分支，必须留）；
//   - 节点归属了地区、但所有启用入站都归到别的地区 → 整台不输出（#入站级分组的核心语义）。
//
// ids 为空时返回空串（不影响老查询）。
func InboundNodeFilter(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	list, first := In(ids)
	// 两次 IN 用同一批占位符，参数要按顺序各带一份（第一处：非 xray 节点；
	// 第二处：xray 节点的入站子查询）。
	args := make([]any, 0, len(first)*2)
	args = append(args, first...)
	args = append(args, first...)
	return " AND ((n.driver_type <> 'xray'" +
		" AND (n.region_id IS NULL OR n.region_id IN " + list + "))" +
		" OR (n.driver_type = 'xray' AND EXISTS (SELECT 1 FROM xray_inbounds xi" +
		" WHERE xi.node_id = n.id AND xi.enabled = 1" +
		" AND (COALESCE(xi.region_id, n.region_id) IS NULL" +
		" OR COALESCE(xi.region_id, n.region_id) IN " + list + "))))", args
}

// RegionFilter 给「地区查询」追加过滤（regions 的表别名必须叫 r）：
//
//	AND r.id IN (?, ...)
func RegionFilter(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	list, args := In(ids)
	return " AND r.id IN " + list, args
}

// In 生成 SQL 的 IN 列表 "(?,?,...)" 与对应参数，供不走 NodeFilter/RegionFilter
// 的位置（例如按地区删绑定的 DELETE）复用同一套占位符写法。
func In(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return "(" + strings.TrimPrefix(strings.Repeat(",?", len(ids)), ",") + ")", args
}
