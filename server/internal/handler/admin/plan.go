package admin

// 管理端套餐（/admin-api/plans）：CRUD + deactivate/activate。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/PlanController.php。

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// PlanRow 是 plans 表的一行（present 的输入）。
//
// 金额按 data-contract §3 以字符串持有（decimal(10,2)），输出时按 PHP 的 (float) 走 phpFloat。
type PlanRow struct {
	ID               uint64
	Name             string
	Type             string // 'period' | 'total'
	Months           *int64
	MonthlyTraffic   *int64
	PeriodTraffic    *int64
	TotalTraffic     *int64
	Price            string // decimal(10,2) 字面量
	ResetPrice       string
	IsActive         bool
	IncludeLocal     bool
	ThirdPartySubIDs []int64
	// RegionIDs 是套餐可用地区（plans.region_ids，JSON 数组）。nil = 不限地区（老套餐/未勾选）。
	RegionIDs []int64
	CreatedAt       *time.Time
}

// PlanInput 是 validatePlan 之后、落库之前的完整字段。
//
// Set 标志区分「未传」与「显式 null」（PHP 的 sometimes / nullable 两套语义）。
type PlanInput struct {
	Name             string
	Type             string
	Price            *string
	PriceSet         bool
	ResetPrice       *string
	ResetPriceSet    bool
	Months           *int64
	MonthsSet        bool
	MonthlyTraffic   *int64
	MonthlySet       bool
	PeriodTraffic    *int64
	PeriodSet        bool
	TotalTraffic     *int64
	TotalSet         bool
	IncludeLocal     bool
	ThirdPartySubIDs []int64
	// RegionIDs 是勾选的可用地区（#地区功能-15）。空 = 不限地区。
	// RegionIDsSet 区分「未传」（改别的字段时保持原值）与「传了空数组」（显式改成不限）。
	RegionIDs    []int64
	RegionIDsSet bool
}

// PlanStore 是套餐域对数据库的依赖（SQL 实现见 plan_sqlstore.go）。
type PlanStore interface {
	PlanPage(ctx context.Context, page, perPage int64) ([]PlanRow, int64, error)
	PlanAll(ctx context.Context) ([]PlanRow, error)
	PlanByID(ctx context.Context, id uint64) (*PlanRow, error)
	PlanExists(ctx context.Context, id uint64) (bool, error)
	CreatePlan(ctx context.Context, in PlanInput, now time.Time) (*PlanRow, error)
	UpdatePlan(ctx context.Context, id uint64, in PlanInput, now time.Time) error
	SetPlanActive(ctx context.Context, id uint64, active bool, now time.Time) (*PlanRow, error)
	// ThirdPartySubExists 校验 third_party_sub_ids.* → exists:third_party_subs,id。
	ThirdPartySubExists(ctx context.Context, id int64) (bool, error)
	// RegionExists 校验 region_ids.* → exists:regions,id（#地区功能-15）。
	RegionExists(ctx context.Context, id int64) (bool, error)
}

// present 对齐 PlanController::present。
func presentPlan(p *PlanRow) *jsObj {
	ids := make([]int64, 0, len(p.ThirdPartySubIDs))
	for _, v := range p.ThirdPartySubIDs {
		ids = append(ids, v)
	}
	// (float) $p->price —— PHP decimal cast 读出字符串再转 float。
	price := phpFloatOf(p.Price)
	reset := phpFloatOf(p.ResetPrice)
	return jo(
		"id", p.ID,
		"name", p.Name,
		"price", price,
		"reset_price", reset,
		"type", p.Type,
		"months", nullableInt(p.Months),
		"monthly_traffic", nullableInt(p.MonthlyTraffic),
		"period_traffic", nullableInt(p.PeriodTraffic),
		"total_traffic", nullableInt(p.TotalTraffic),
		"is_active", p.IsActive,
		"include_local", p.IncludeLocal,
		"third_party_sub_ids", int64List(ids),
		// 空数组 = 不限地区（DB 里存的是 NULL，前端按「一个都没勾 = 不限」渲染）。
		"region_ids", int64List(p.RegionIDs),
		"created_at", nullableISO(p.CreatedAt),
	)
}

// int64List 输出 JSON 整数数组（array_values(array_map('intval', ...))）。
func int64List(ids []int64) jsArr {
	out := make(jsArr, 0, len(ids))
	for _, v := range ids {
		out = append(out, v)
	}
	return out
}

// validatePlan 对齐 PlanController::validatePlan（含纯第三方强制周期与流量归零）。
//
// 返回 (input, 校验失败文案, ok)。
func (d *UserDomain) validatePlan(c *gin.Context) (PlanInput, string, bool) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data).existsChecker(d.planExistsChecker())
	v.field("name", "required", "string", "max:64")
	v.field("price", "sometimes", "numeric", "min:0")
	v.field("reset_price", "sometimes", "numeric", "min:0")
	v.field("type", "required", "in:period,total")
	v.field("months", "nullable", "integer", "min:1")
	v.field("monthly_traffic", "nullable", "integer", "min:0")
	v.field("period_traffic", "nullable", "integer", "min:0")
	v.field("total_traffic", "nullable", "integer", "min:0")
	v.field("include_local", "sometimes", "boolean")
	v.field("third_party_sub_ids", "sometimes", "array")
	v.field("third_party_sub_ids.*", "integer", "exists:third_party_subs,id")
	// 可用地区（#地区功能-15）：不传 = 保持原值；传空数组 = 不限地区。
	v.field("region_ids", "sometimes", "array")
	v.field("region_ids.*", "integer", "exists:regions,id")

	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.logger.Printf("admin 套餐校验时查库失败: %v", err)
			return PlanInput{}, "服务器错误", false
		}
		return PlanInput{}, v.summary(), false
	}

	in := PlanInput{}
	if name, ok := v.valueOf("name"); ok {
		in.Name, _ = inputString(name)
	}
	typ, _ := v.valueOf("type")
	in.Type, _ = inputString(typ)

	if price, ok := v.valueOf("price"); ok {
		if s, ok2 := decimalString(price, 2); ok2 {
			in.Price = &s
			in.PriceSet = true
		}
	}
	if reset, ok := v.valueOf("reset_price"); ok {
		if s, ok2 := decimalString(reset, 2); ok2 {
			in.ResetPrice = &s
			in.ResetPriceSet = true
		}
	}
	if raw, ok := v.valueOf("months"); ok && raw != nil {
		n := inputInt(raw)
		in.Months = &n
		in.MonthsSet = true
	} else if _, ok := v.valueOf("months"); ok {
		in.MonthsSet = true // 显式 null
	}
	if raw, ok := v.valueOf("monthly_traffic"); ok && raw != nil {
		n := inputInt(raw)
		in.MonthlyTraffic = &n
		in.MonthlySet = true
	} else if _, ok := v.valueOf("monthly_traffic"); ok {
		in.MonthlySet = true
	}
	if raw, ok := v.valueOf("period_traffic"); ok && raw != nil {
		n := inputInt(raw)
		in.PeriodTraffic = &n
		in.PeriodSet = true
	} else if _, ok := v.valueOf("period_traffic"); ok {
		in.PeriodSet = true
	}
	if raw, ok := v.valueOf("total_traffic"); ok && raw != nil {
		n := inputInt(raw)
		in.TotalTraffic = &n
		in.TotalSet = true
	} else if _, ok := v.valueOf("total_traffic"); ok {
		in.TotalSet = true
	}

	in.IncludeLocal = true
	if raw, ok := v.valueOf("include_local"); ok {
		in.IncludeLocal = inputBool(raw)
	}
	in.ThirdPartySubIDs = []int64{}
	if raw, ok := v.valueOf("third_party_sub_ids"); ok && raw != nil {
		if items, ok2 := raw.([]any); ok2 {
			for _, item := range items {
				in.ThirdPartySubIDs = append(in.ThirdPartySubIDs, inputInt(item))
			}
		}
	}
	// 可用地区（#地区功能-15）：未传 → 不动原值；传空数组 → 落库成 NULL（= 不限地区）。
	in.RegionIDs = []int64{}
	if raw, ok := v.valueOf("region_ids"); ok {
		in.RegionIDsSet = true
		if items, ok2 := raw.([]any); ok2 {
			for _, item := range items {
				in.RegionIDs = append(in.RegionIDs, inputInt(item))
			}
		}
	}

	// 纯第三方套餐（不勾本地、勾了第三方）：第三方不管流量只管时间 → 强制周期套餐。
	// 必须在「按类型清理无关字段」之前改 type，否则 type=total 的清理分支会把 months 置 null。
	if !in.IncludeLocal && len(in.ThirdPartySubIDs) > 0 {
		in.Type = "period"
	}

	// 根据类型清理无关字段
	if in.Type == "period" {
		in.TotalTraffic = nil
		in.TotalSet = true
		if in.Months == nil {
			one := int64(1)
			in.Months = &one
			in.MonthsSet = true
		}
		monthly := int64(0)
		if in.MonthlyTraffic != nil {
			monthly = *in.MonthlyTraffic
		}
		period := monthly * *in.Months
		in.PeriodTraffic = &period
		in.PeriodSet = true
	} else {
		in.Months = nil
		in.MonthsSet = true
		in.MonthlyTraffic = nil
		in.MonthlySet = true
		in.PeriodTraffic = nil
		in.PeriodSet = true
	}

	// 纯第三方：流量字段全部归零（上面已把 type 锁成周期）。
	if !in.IncludeLocal && len(in.ThirdPartySubIDs) > 0 {
		zero := int64(0)
		in.MonthlyTraffic = &zero
		in.MonthlySet = true
		in.PeriodTraffic = &zero
		in.PeriodSet = true
		in.TotalTraffic = nil
		in.TotalSet = true
	}

	return in, "", true
}

// planExistsChecker 给校验器用（exists 规则同步接口）。
func (d *UserDomain) planExistsChecker() func(table, column string, value any) (bool, error) {
	return func(table, column string, value any) (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		switch table {
		case "plans":
			return d.plans.PlanExists(ctx, uint64(inputInt(value)))
		case "third_party_subs":
			return d.plans.ThirdPartySubExists(ctx, inputInt(value))
		case "regions":
			// 套餐可用地区（#地区功能-15）：勾的地区必须真实存在。
			return d.plans.RegionExists(ctx, inputInt(value))
		}
		return false, nil
	}
}

// ---- handlers ----

// planIndex 对齐 PlanController::index：?all=1 全量，否则 50/页。
func (d *UserDomain) planIndex(c *gin.Context) {
	if inputBool(c.Query("all")) {
		rows, err := d.plans.PlanAll(ctxOf(c))
		if err != nil {
			d.failServer(c, "查询套餐", err)
			return
		}
		out := make(jsArr, 0, len(rows))
		for i := range rows {
			out = append(out, presentPlan(&rows[i]))
		}
		d.success(c, out)
		return
	}

	page := pageParam(c)
	rows, total, err := d.plans.PlanPage(ctxOf(c), page, userPagePerPage)
	if err != nil {
		d.failServer(c, "查询套餐", err)
		return
	}
	// 越界夹回最后一页：重查一次（与 PHP 的 paginate 二次查询同口径）。
	lastPage := (total + userPagePerPage - 1) / userPagePerPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page > lastPage {
		page = lastPage
		rows, total, err = d.plans.PlanPage(ctxOf(c), page, userPagePerPage)
		if err != nil {
			d.failServer(c, "查询套餐", err)
			return
		}
	}
	items := make([]any, 0, len(rows))
	for i := range rows {
		items = append(items, presentPlan(&rows[i]))
	}
	d.successPage(c, items, page, userPagePerPage, total)
}

func (d *UserDomain) planShow(c *gin.Context) {
	p, ok := d.loadPlan(c)
	if !ok {
		return
	}
	d.success(c, presentPlan(p))
}

func (d *UserDomain) planStore(c *gin.Context) {
	in, summary, ok := d.validatePlan(c)
	if !ok {
		d.failValidation(c, summary)
		return
	}
	p, err := d.plans.CreatePlan(ctxOf(c), in, d.now())
	if err != nil {
		d.failServer(c, "创建套餐", err)
		return
	}
	d.success(c, presentPlan(p), "创建成功")
}

func (d *UserDomain) planUpdate(c *gin.Context) {
	p, ok := d.loadPlan(c)
	if !ok {
		return
	}
	in, summary, ok := d.validatePlan(c)
	if !ok {
		d.failValidation(c, summary)
		return
	}
	if err := d.plans.UpdatePlan(ctxOf(c), p.ID, in, d.now()); err != nil {
		d.failServer(c, "更新套餐", err)
		return
	}
	updated, err := d.plans.PlanByID(ctxOf(c), p.ID)
	if err != nil {
		d.failServer(c, "读取套餐", err)
		return
	}
	d.success(c, presentPlan(updated), "更新成功")
}

func (d *UserDomain) planDeactivate(c *gin.Context) {
	p, ok := d.loadPlan(c)
	if !ok {
		return
	}
	updated, err := d.plans.SetPlanActive(ctxOf(c), p.ID, false, d.now())
	if err != nil {
		d.failServer(c, "下架套餐", err)
		return
	}
	d.success(c, presentPlan(updated), "已下架（已购用户不受影响）")
}

func (d *UserDomain) planActivate(c *gin.Context) {
	p, ok := d.loadPlan(c)
	if !ok {
		return
	}
	updated, err := d.plans.SetPlanActive(ctxOf(c), p.ID, true, d.now())
	if err != nil {
		d.failServer(c, "上架套餐", err)
		return
	}
	d.success(c, presentPlan(updated), "已上架")
}
