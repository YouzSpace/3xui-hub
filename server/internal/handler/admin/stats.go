package admin

// 管理端仪表盘统计（/admin-api/stats/revenue）：成交额（今日 / 累计）。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/StatsController.php。
//
// 只统计 status = paid 的订单，金额取 orders.amount（**实付金额**，已扣优惠码），
// 不是 original_amount —— 否则优惠码花掉的钱会被算进成交额。

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// StatsRevenue 是 revenue 的聚合结果。
type StatsRevenue struct {
	Timezone    string
	TodayAmount string // decimal 两位，输出前按 round(..., 2) 规范
	TodayOrders int64
	TotalAmount string
	TotalOrders int64
}

// StatsStore 是统计域对数据库的依赖（SQL 实现见 stats_sqlstore.go）。
type StatsStore interface {
	// RevenueToday / RevenueTotal 按 paid_at 阈值统计 status='paid'。
	// start 为 nil 时统计全部。
	RevenueToday(ctx context.Context, start *time.Time) (amount string, orders int64, err error)
	RevenueTotal(ctx context.Context) (amount string, orders int64, err error)
}

// statsRevenue 对齐 StatsController::revenue。
//
// 「今天」的边界：先在业务时区取当天 00:00，再换算到 app.timezone（UTC）——
// 直接用 UTC 日界会把北京时间 0:00–8:00 的订单算到前一天。
func (d *UserDomain) statsRevenue(c *gin.Context) {
	tzName := d.cfg.BusinessTimezone
	if tzName == "" {
		tzName = d.cfg.AppTimezone
	}
	if tzName == "" {
		tzName = "UTC"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		loc = time.UTC
	}
	now := d.now().In(loc)
	// 「今天 0 点」必须按业务时区构造再换算 UTC：直接用 time.UTC 拼年月日
	// 等于北京 08:00 才算当天开始，0:00–7:59 的订单全被漏出「今日」。
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).In(time.UTC)

	todayAmount, todayOrders, err := d.stats.RevenueToday(ctxOf(c), &startOfToday)
	if err != nil {
		d.failServer(c, "统计今日成交额", err)
		return
	}
	totalAmount, totalOrders, err := d.stats.RevenueTotal(ctxOf(c))
	if err != nil {
		d.failServer(c, "统计累计成交额", err)
		return
	}

	d.success(c, jo(
		"timezone", tzName,
		"today", jo(
			"amount", phpFloatOf(round2(todayAmount)),
			"orders", todayOrders,
		),
		"total", jo(
			"amount", phpFloatOf(round2(totalAmount)),
			"orders", totalOrders,
		),
	))
}

// round2 对齐 PHP round($x, 2)（四舍五入远离零）。
func round2(decimal string) string {
	f := parseFloatSafe(decimal)
	neg := f < 0
	if neg {
		f = -f
	}
	v := math.Floor(f*100+0.5) / 100
	if neg {
		v = -v
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}
