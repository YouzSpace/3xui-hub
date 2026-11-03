package admin

// 管理端邀请返佣域（只读）：
//
//	GET /admin-api/affiliate/rebates?page=&per_page=   返佣流水（分页）
//
// data = {list, total, page, per_page}，与兑换码/优惠码列表同形（前端复用同一套分页）。
// 每行含邀请人/被邀请人的 id + 邮箱、订单号、返佣金额（分）、发放时的比例、时间。
//
// 返佣的**发放**在用户端 / 支付链路（internal/handler/api/affiliate_*.go），本域不写数据：
// 管理员只能看流水；要停返佣就把 site_configs.affiliate_rebate_rate 改成 0。

import (
	"context"
	"log"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// 返佣流水列表的分页参数（默认 20 条，最多 100）。
const (
	affiliateRebatePerPage    = 20
	affiliateRebateMaxPerPage = 100
)

// AffiliateRebate 是 affiliate_rebates 一行（带双方邮箱，列表直接展示）。
type AffiliateRebate struct {
	ID        uint64
	InviterID uint64
	// InviterEmail 是被删号/无邮箱时的空值（LEFT JOIN users）。
	InviterEmail *string
	InviteeID    uint64
	InviteeEmail *string
	OrderID      uint64
	OrderNo      string
	// Amount 是返佣金额（分），恒为正数。
	Amount int64
	// Rate 是发放当时的比例 decimal(5,4) 文本（"0.1000"），留痕用。
	Rate      string
	CreatedAt *time.Time
}

// AffiliateStore 是返佣域对数据库的全部依赖（SQL 见 affiliate_sqlstore.go）。
type AffiliateStore interface {
	// RebatePage 返回当前页返佣流水（id 倒序）与总数。
	RebatePage(ctx context.Context, page, perPage int64) ([]AffiliateRebate, int64, error)
}

// AffiliateDeps 是返佣域的依赖清单。
type AffiliateDeps struct {
	Store  AffiliateStore
	Logger loggerLike
}

// AffiliateDomain 是管理端返佣域的 HTTP 入口。
type AffiliateDomain struct {
	store  AffiliateStore
	logger loggerLike
}

// NewAffiliateDomain 组装返佣域。
func NewAffiliateDomain(d AffiliateDeps) *AffiliateDomain {
	if d.Store == nil {
		panic("admin: AffiliateStore 不能为空")
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &AffiliateDomain{store: d.Store, logger: logger}
}

// Register 把返佣域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
func (d *AffiliateDomain) Register(r gin.IRouter) {
	r.GET("/affiliate/rebates", d.rebateIndex)
}

// ---- 响应封装（PHP ApiResponse 口径，与余额域一致）----

func (d *AffiliateDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *AffiliateDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *AffiliateDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// ---- 返佣流水 ----

// rebateIndex 对齐 GET /admin-api/affiliate/rebates?page=&per_page=。
//
// 页码越界不夹取，只回空 list（与其它列表同口径）。
func (d *AffiliateDomain) rebateIndex(c *gin.Context) {
	page, perPage := pageParams(c, affiliateRebatePerPage, affiliateRebateMaxPerPage)

	rows, total, err := d.store.RebatePage(ctxOf(c), page, perPage)
	if err != nil {
		d.failServer(c, "查询返佣流水", err)
		return
	}
	list := make(jsArr, 0, len(rows))
	for i := range rows {
		list = append(list, presentAffiliateRebate(&rows[i]))
	}
	d.success(c, jo(
		"list", list,
		"total", total,
		"page", page,
		"per_page", perPage,
	))
}

// presentAffiliateRebate 输出返佣流水一行（字段顺序固定，便于前端与文档对照）。
func presentAffiliateRebate(r *AffiliateRebate) *jsObj {
	return jo(
		"id", r.ID,
		"inviter_id", r.InviterID,
		"inviter_email", nullableStr(r.InviterEmail),
		"invitee_id", r.InviteeID,
		"invitee_email", nullableStr(r.InviteeEmail),
		"order_id", r.OrderID,
		"order_no", r.OrderNo,
		"amount", r.Amount,
		"rate", phpFloatOf(r.Rate),
		"created_at", nullableDateTime(r.CreatedAt),
	)
}
