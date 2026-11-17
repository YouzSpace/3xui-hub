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
