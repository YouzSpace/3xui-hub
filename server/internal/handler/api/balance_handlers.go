package api

// 本文件是用户端的余额端点（对齐 docs/balance/implementation-plan.md §3.1.1 / §3.1.2）：
//
//	GET  /api/balance   查余额 + 最近流水
//	POST /api/redeem    兑换码兑换
//
// 路由挂在 UserExtras.Register 里（与 sync-traffic/invite 同一段 api.auth 分组），
// 这样不需要改动 cmd/server/routes.go 的组装。
//
// 余额支付与充值下单走支付端点（POST /api/payment/create，见 payment.go 的分支），
// 本文件只管「查」与「兑换」。

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
)

// balanceMaxPerPage 是流水一页的上限（默认 20 条，规划 §3.1.1）。
const (
	balanceDefaultPerPage = 20
	balanceMaxPerPage     = 100
)

// handleBalance 对齐 GET /api/balance：
// {"code":0,"data":{"balance":5000,"logs":[{"amount":5000,"type":"redeem","note":"兑换码",
// "balance_after":5000,"created_at":"..."}]}}
// 金额单位一律分（前端 /100 显示为元）。
func (e *UserExtras) handleBalance(c *gin.Context) {
	if e.balance == nil {
		e.handler.logger.Printf("余额端点不可用：BalanceStore 未接入")
		fail(c, "服务器错误", 500)
		return
	}
	u := currentUser(c)
	limit, offset := balancePageParams(c)

	balance, logs, err := e.balance.Current(c.Request.Context(), u.ID, limit, offset)
	if err != nil {
		e.handler.logger.Printf("balance 查询失败 user=%d: %v", u.ID, err)
		fail(c, "服务器错误", 500)
		return
	}

	items := make(jsArr, 0, len(logs))
	for i := range logs {
		l := &logs[i]
		items = append(items, jo(
			"id", l.ID,
			"amount", l.Amount,
			"type", l.Type,
			"note", nullString(l.Note),
			"balance_after", l.BalanceAfter,
			"created_at", nullableISOTime(l.CreatedAt),
		))
	}
	success(c, jo("balance", balance, "logs", items))
}

// handleRedeem 对齐 POST /api/redeem：{"code":"XXXX-XXXX-XXXX-XXXX"} → {"added":5000,"balance":5000}。
//
// 顺序不能换：**先查限流再查库**（被判定的请求一个字节的码表都不碰，也就没有枚举反馈），
// 放行后无论最终成功失败都要记账（成功清计数、失败累计失败次数）。
func (e *UserExtras) handleRedeem(c *gin.Context) {
	if e.balance == nil {
		e.handler.logger.Printf("兑换端点不可用：BalanceStore 未接入")
		fail(c, "服务器错误", 500)
		return
	}
	u := currentUser(c)
	ctx := c.Request.Context()

	input := jsonInput(c)
	v := newPaymentValidator(input).field("code", "required", "string", "max:32")
	if !v.passes() {
		respondPaymentValidation(c, v)
		return
	}

	ip := clientIP(c)
	limits := e.handler.loadRateLimits(ctx)
	blocked, berr := e.handler.redeemBlocked(ctx, ip, limits)
	if berr != nil {
		e.handler.logger.Printf("redeem 限流查询失败 ip=%s: %v", ip, berr)
	} else if blocked {
		fail(c, rateTooOftenMsg, 429)
		return
	}

	result, err := e.balance.Redeem(ctx, u.ID, inputStringOrEmpty(input["code"]))
	if err != nil {
		if msg, ok := redeemErrorText(err); ok {
			// 码不对（不存在/已用/过期/作废）计入 IP 失败次数，防爆破。
			if rerr := e.handler.recordRedeemFailure(ctx, ip, limits); rerr != nil {
				e.handler.logger.Printf("redeem 记录失败次数失败 ip=%s: %v", ip, rerr)
			}
			fail(c, msg, 400)
			return
		}
		e.handler.logger.Printf("redeem 失败 user=%d: %v", u.ID, err)
		fail(c, "兑换失败", 500)
		return
	}

	if cerr := e.handler.clearRedeemFailures(ctx, ip); cerr != nil {
		e.handler.logger.Printf("redeem 清理失败次数失败 ip=%s: %v", ip, cerr)
	}
	success(c, jo("added", result.Added, "balance", result.Balance), "兑换成功")
}

// redeemErrorText 把兑换的可辨失败映射成给用户看的中文文案（规划 §3.1.2 的四句文案）。
func redeemErrorText(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrGiftcardNotFound):
		return "兑换码不存在", true
	case errors.Is(err, ErrGiftcardUsed):
		return "兑换码已使用", true
	case errors.Is(err, ErrGiftcardExpired):
		return "兑换码已过期", true
	case errors.Is(err, ErrGiftcardDisabled):
		return "兑换码已作废", true
	}
	return "", false
}

// balancePageParams 解析流水分页参数：per_page（默认 20，上限 100）+ page（1 起）。
func balancePageParams(c *gin.Context) (limit, offset int) {
	limit = balanceDefaultPerPage
	if raw := c.Query("per_page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > balanceMaxPerPage {
		limit = balanceMaxPerPage
	}
	page := 1
	if raw := c.Query("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 1 {
			page = n
		}
	}
	return limit, (page - 1) * limit
}
