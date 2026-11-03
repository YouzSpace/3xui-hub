package api

// 本文件是用户端「我的邀请返佣」端点：
//
//	GET /api/affiliate  → {rate, total_rebate(分), invited_count, invitees:[{user_id,email,rebate,invited_at}]}
//
// 路由挂在 UserExtras.Register（与 /api/balance、/api/invite/me 同一段 api.auth 分组）。
// 「我的邀请码/邀请开关」仍走 GET /api/invite/me，本端点只补返佣数据，不改那边。
//
// 隐私：被邀请人的邮箱一律打码（用户端不展示他人完整邮箱，管理端才看全量）。

import (
	"database/sql"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// handleAffiliate 对齐前端「我的邀请」页的返佣部分。
func (e *UserExtras) handleAffiliate(c *gin.Context) {
	if e.affiliate == nil {
		e.handler.logger.Printf("返佣端点不可用：AffiliateStore 未接入")
		fail(c, "服务器错误", 500)
		return
	}
	u := currentUser(c)

	sum, err := e.affiliate.Summary(c.Request.Context(), u.ID)
	if err != nil {
		e.handler.logger.Printf("affiliate 查询失败 user=%d: %v", u.ID, err)
		fail(c, "服务器错误", 500)
		return
	}

	invitees := make(jsArr, 0, len(sum.Invitees))
	for _, it := range sum.Invitees {
		invitees = append(invitees, jo(
			"user_id", it.UserID,
			"email", maskEmail(it.Email),
			"rebate", it.Rebate,
			"invited_at", nullableISOTime(it.InvitedAt),
		))
	}
	success(c, jo(
		"rate", phpFloat(sum.Rate),
		"total_rebate", sum.TotalRebate,
		"invited_count", len(sum.Invitees),
		"invitees", invitees,
	))
}

// maskEmail 给被邀请人的邮箱打码：local 前 2 个字符 + *** + @domain。
//
//	zhangsan@qq.com → zh***@qq.com；a@qq.com → a***@qq.com；无 @ 时按同样规则截断。
func maskEmail(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	email := strings.TrimSpace(v.String)
	if email == "" {
		return nil
	}
	local, domain := email, ""
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		local, domain = email[:i], email[i:]
	}
	keep := 2
	if utf8.RuneCountInString(local) < keep {
		keep = utf8.RuneCountInString(local)
	}
	runes := []rune(local)
	return string(runes[:keep]) + "***" + domain
}
