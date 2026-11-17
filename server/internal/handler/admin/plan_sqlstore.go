package admin

// PlanStore 的 database/sql 实现（套餐域）。

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// NewPlanSQLStore 用已有连接池构造套餐域的 PlanStore。
func NewPlanSQLStore(db *sql.DB) PlanStore {
	return &planSQLStore{db: db}
}

type planSQLStore struct {
	db *sql.DB
}

// planSelectColumns 是套餐自身的列。带 p. 前缀是因为读路径统一 LEFT JOIN exit_profiles
// （分类名要跟套餐一起给前端，否则套餐列表每行还要再查一次分类）。
//
// LEFT JOIN 而不是 INNER：exit_profile_id 为空（direct 套餐）必须照常读出，
// 分类被删的残留绑定也不该让整个套餐列表变空。
const planSelectColumns = `p.id, p.name, p.type, p.months, p.monthly_traffic, p.period_traffic, p.total_traffic,
	p.price, p.reset_price, p.is_active, p.include_local, p.third_party_sub_ids, p.region_ids,
	p.exit_type, p.exit_profile_id, COALESCE(ep.name, ''), p.created_at`

// planFrom 是读路径的 FROM 子句（与 planSelectColumns 成对使用）。
const planFrom = ` FROM plans p LEFT JOIN exit_profiles ep ON ep.id = p.exit_profile_id`

func scanPlanRow(row interface{ Scan(...any) error }) (*PlanRow, error) {
	var (
		p          PlanRow
		months     sql.NullInt64
		monthly    sql.NullInt64
		period     sql.NullInt64
		total      sql.NullInt64
		price      sql.NullString
		resetPrice sql.NullString
		isActive   int
		includeLcl int
		subIDs     sql.NullString
		regionIDs  sql.NullString
		// exit_type 是 NOT NULL DEFAULT 'direct'：空字符串只可能来自测试库建表时
		// 漏了默认值，按 direct 处理比回 "0"/"" 更安全（前端按 direct 渲染）。
		exitType   sql.NullString
		exitProfID sql.NullInt64
		createdAt  sql.NullTime
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Type, &months, &monthly, &period, &total,
		&price, &resetPrice, &isActive, &includeLcl, &subIDs, &regionIDs,
		&exitType, &exitProfID, &p.ExitProfileName, &createdAt); err != nil {
		return nil, err
	}
	p.Months = nullIntPtr(months)
	p.MonthlyTraffic = nullIntPtr(monthly)
	p.PeriodTraffic = nullIntPtr(period)
	p.TotalTraffic = nullIntPtr(total)
	if price.Valid {
		p.Price = price.String
	} else {
		p.Price = "0.00"
	}
	if resetPrice.Valid {
		p.ResetPrice = resetPrice.String
	} else {
		p.ResetPrice = "0.00"
	}
	p.IsActive = isActive != 0
	p.IncludeLocal = includeLcl != 0
	p.ThirdPartySubIDs = jsonIntList(nullStrPtr(subIDs))
	if p.ThirdPartySubIDs == nil {
		p.ThirdPartySubIDs = []int64{}
	}
	// 地区（#地区功能-15）：NULL = 不限地区，读出来就是 nil（空数组也是 nil）。
	p.RegionIDs = jsonIntList(nullStrPtr(regionIDs))
	// 出口类型（#静态IP）：老行升级后是 'direct'；空值也归一成 direct（见上面的说明）。
	p.ExitType = "direct"
	if exitType.Valid && exitType.String != "" {
		p.ExitType = exitType.String
	}
	p.ExitProfileID = nullUintPtr(exitProfID)
	p.CreatedAt = nullTimePtr(createdAt)
	return &p, nil
}

func (s *planSQLStore) PlanPage(ctx context.Context, page, perPage int64) ([]PlanRow, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans`).Scan(&total); err != nil {
		return nil, 0, err
	}
	if perPage < 1 {
		perPage = 50
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+planSelectColumns+planFrom+` ORDER BY p.id DESC LIMIT ? OFFSET ?`, perPage, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PlanRow{}
	for rows.Next() {
		p, err := scanPlanRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *p)
	}
	return out, total, rows.Err()
}

func (s *planSQLStore) PlanAll(ctx context.Context) ([]PlanRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+planSelectColumns+planFrom+` ORDER BY p.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanRow{}
	for rows.Next() {
		p, err := scanPlanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *planSQLStore) PlanByID(ctx context.Context, id uint64) (*PlanRow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+planSelectColumns+planFrom+` WHERE p.id = ?`, id)
	return scanPlanRow(row)
}

func (s *planSQLStore) PlanExists(ctx context.Context, id uint64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plans WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

func (s *planSQLStore) ThirdPartySubExists(ctx context.Context, id int64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM third_party_subs WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// RegionExists 是 exists:regions,id 的实现（套餐可用地区，见 plan.go 的 region_ids.*）。
func (s *planSQLStore) RegionExists(ctx context.Context, id int64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM regions WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// ExitProfileExists 是 exists:exit_profiles,id 的实现（#静态IP 的套餐出口分类）。
func (s *planSQLStore) ExitProfileExists(ctx context.Context, id int64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exit_profiles WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// regionIDsValue 把可用地区列表落成可绑定值（#地区功能-15）：
// 空列表 → NULL（不勾地区就是「不限」，与老套餐一致，不存 []）。
func regionIDsValue(ids []int64) any {
	if len(ids) == 0 {
		return nil
	}
	return intListToJSON(ids)
}

// planWriteValues 把 PlanInput 摊成列值（create/update 共用语义）。
//
// 未传的金额不写（create 用 DB 默认 0.00，update 保持旧值）；类型派生字段由
// validatePlan 全部填好，这里一律落库。
func planWriteValues(in PlanInput) (cols []string, args []any) {
	cols = append(cols, "name = ?", "type = ?", "include_local = ?", "third_party_sub_ids = ?")
	args = append(args, in.Name, in.Type, boolToInt(in.IncludeLocal), intListToJSON(in.ThirdPartySubIDs))

	if in.PriceSet {
		cols = append(cols, "price = ?")
		if in.Price != nil {
			args = append(args, *in.Price)
		} else {
			args = append(args, nil)
		}
	}
	if in.ResetPriceSet {
		cols = append(cols, "reset_price = ?")
		if in.ResetPrice != nil {
			args = append(args, *in.ResetPrice)
		} else {
			args = append(args, nil)
		}
	}
	cols = append(cols, "months = ?", "monthly_traffic = ?", "period_traffic = ?", "total_traffic = ?")
	args = append(args, nullInt64Value(in.Months), nullInt64Value(in.MonthlyTraffic),
		nullInt64Value(in.PeriodTraffic), nullInt64Value(in.TotalTraffic))

	// 出口类型（#静态IP）：update 未传则不动原值 —— 不能把已有的 static_chain
	// 静默改回 direct。（create 侧不走这里，见 CreatePlan 的 INSERT 列表。）
	if in.ExitTypeSet {
		cols = append(cols, "exit_type = ?")
		args = append(args, in.ExitType)
	}

	// 可用地区（#地区功能-15）：只有请求里带了 region_ids 才写（改价格不动地区）。
	if in.RegionIDsSet {
		cols = append(cols, "region_ids = ?")
		args = append(args, regionIDsValue(in.RegionIDs))
	}
	// 出口分类（#静态IP）：只在请求带了该字段时才写。
	// validatePlan 已经把「exit_type 改成非 static_chain」翻译成 ExitProfileSet=true +
	// ExitProfileID=nil，所以这里只管写值。
	if in.ExitProfileSet {
		cols = append(cols, "exit_profile_id = ?")
		args = append(args, nullUintValue(in.ExitProfileID))
	}
	return cols, args
}

// nullInt64Value 把 int64 指针变成可绑定值（nil → NULL）。
func nullInt64Value(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// planExitTypeValue 给 create 用的 exit_type 值（#静态IP）。
//
// INSERT 列表里写死了列名，MySQL 的建表默认值只在「语句完全没提这一列」时才生效，
// 所以没传 exit_type 时必须显式给 'direct'（列是 NOT NULL，严格模式下给 nil 会报错）。
func planExitTypeValue(in PlanInput) string {
	if in.ExitTypeSet && in.ExitType != "" {
		return in.ExitType
	}
	return "direct"
}

func (s *planSQLStore) CreatePlan(ctx context.Context, in PlanInput, now time.Time) (*PlanRow, error) {
	price := "0.00"
	if in.PriceSet && in.Price != nil {
		price = *in.Price
	}
	reset := "0.00"
	if in.ResetPriceSet && in.ResetPrice != nil {
		reset = *in.ResetPrice
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO plans
		(name, type, months, monthly_traffic, period_traffic, total_traffic,
		 price, reset_price, is_active, include_local, third_party_sub_ids, region_ids,
		 exit_type, exit_profile_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)`,
		in.Name, in.Type,
		nullInt64Value(in.Months), nullInt64Value(in.MonthlyTraffic),
		nullInt64Value(in.PeriodTraffic), nullInt64Value(in.TotalTraffic),
		price, reset,
		boolToInt(in.IncludeLocal), intListToJSON(in.ThirdPartySubIDs), regionIDsValue(in.RegionIDs),
		planExitTypeValue(in), nullUintValue(in.ExitProfileID), now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.PlanByID(ctx, uint64(id))
}

func (s *planSQLStore) UpdatePlan(ctx context.Context, id uint64, in PlanInput, now time.Time) error {
	cols, args := planWriteValues(in)
	cols = append(cols, "updated_at = ?")
	args = append(args, now, id)
	res, err := s.db.ExecContext(ctx, `UPDATE plans SET `+strings.Join(cols, ", ")+` WHERE id = ?`, args...)
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

func (s *planSQLStore) SetPlanActive(ctx context.Context, id uint64, active bool, now time.Time) (*PlanRow, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE plans SET is_active = ?, updated_at = ? WHERE id = ?`, boolToInt(active), now, id)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.PlanByID(ctx, id)
}
