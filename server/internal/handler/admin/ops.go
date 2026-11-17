package admin

// 代理运营（对齐 Admin\ProxyOpsController）。
//
// 把「谁买了代理、还剩多少、什么时候到期、该不该停」集中到一页。分成三类数据：
//   - 住宅账号（webshare 独占）：回采 / 停用 / 重置密码 / 延期 / 删除
//   - 共享 socks：看「谁在用这条线」，线路坏掉时整条迁移
//   - 待补开：钱已付、但上游开号失败没发出权益的订单
//
// 只读写商城自己的数据（proxy_accounts / proxy_entitlements / orders），
// 不会碰节点套餐与订阅。

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"controlhub-server/internal/sockpool"
)

// opsOverview 对齐 GET /admin-api/proxy-ops/overview（轻量，30 秒刷新用）。
func (h *Handler) opsOverview(c *gin.Context) {
	stats, err := h.store.OpsStats(ctxOf(c), h.now())
	if err != nil {
		h.failServer(c, "统计代理运营概览", err)
		return
	}
	options, err := h.store.ActiveSocksOptions(ctxOf(c))
	if err != nil {
		h.failServer(c, "查询 socks 线路", err)
		return
	}

	socksArr := make(jsArr, 0, len(options))
	for _, o := range options {
		label := o.Host + ":" + strconv.FormatInt(o.Port, 10)
		if o.Country != nil && *o.Country != "" {
			label += " · " + *o.Country
		}
		socksArr = append(socksArr, jo("id", o.ID, "label", label))
	}

	h.success(c, jo(
		"stats", jo(
			"active", stats.Active,
			"expiring", stats.Expiring,
			"expired", stats.Expired,
			"pending_orders", stats.PendingOrders,
			"accounts", stats.Accounts,
			"socks_lines", stats.SocksLines,
		),
		"socks_options", socksArr,
	))
}

// opsAccounts 对齐 GET /admin-api/proxy-ops/accounts（分页，默认 50/页、上限 100、下限 10）。
func (h *Handler) opsAccounts(c *gin.Context) {
	perPage := clampInt(queryInt(c, "per_page", 50), 10, 100)
	page := queryInt(c, "page", 1)
	if page < 1 {
		page = 1
	}
	status := ""
	if raw := queryString(c, "status"); raw != nil {
		status = phpTrimSpaceOnly(*raw)
	}
	search := ""
	if raw := queryString(c, "search"); raw != nil {
		search = phpTrimSpaceOnly(*raw)
	}

	rows, total, err := h.store.OpsAccountsPage(ctxOf(c), status, search, page, perPage)
	if err != nil {
		h.failServer(c, "查询代理账号", err)
		return
	}

	items := make(jsArr, 0, len(rows))
	for i := range rows {
		a := &rows[i]
		items = append(items, jo(
			"id", a.ID,
			"entitlement_id", nullableUint(a.EntitlementID),
			"order_id", nullableUint(a.OrderID),
			"label", a.Label,
			"username", a.Username,
			"user", optionalUser(a.UserID, a.UserEmail),
			"provider_name", nullableStr(a.ProviderName),
			"sku_name", nullableStr(a.SKUName),
			"proxy_limit", phpFloatOf(a.ProxyLimit),
			"used_bytes", a.UsedBytes,
			"limit_bytes", nullableInt(a.TrafficLimitBytes),
			"status", a.Status,
			"expires_at", nullableISO(a.ExpiresAt),
			"last_synced_at", nullableISO(a.LastSyncedAt),
		))
	}
	h.successPage(c, items, page, perPage, total)
}

// opsSocksGroups 对齐 GET /admin-api/proxy-ops/socks-groups（分页对象是「线路」不是「权益」）。
func (h *Handler) opsSocksGroups(c *gin.Context) {
	perPage := clampInt(queryInt(c, "per_page", 20), 5, 50)
	page := queryInt(c, "page", 1)
	if page < 1 {
		page = 1
	}

	socksIDs, totalLines, err := h.store.SocksGroupIDs(ctxOf(c), page, perPage)
	if err != nil {
		h.failServer(c, "查询 socks 线路", err)
		return
	}
	lastPage := (totalLines + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page > lastPage {
		page = lastPage
	}

	items := make(jsArr, 0, len(socksIDs))
	if len(socksIDs) > 0 {
		rows, err := h.store.SocksGroupRows(ctxOf(c), socksIDs)
		if err != nil {
			h.failServer(c, "查询 socks 线路明细", err)
			return
		}
		// 按 socksID 分组（rows 已按 id 倒序）。
		grouped := map[uint64][]SocksGroupRow{}
		for _, r := range rows {
			if r.SocksID == nil {
				continue
			}
			grouped[*r.SocksID] = append(grouped[*r.SocksID], r)
		}

		now := h.now()
		for _, socksID := range socksIDs {
			group := grouped[socksID]
			var host, country *string
			var port *int64
			socksActive := false
			if len(group) > 0 {
				host = group[0].SocksHost
				port = group[0].SocksPort
				country = group[0].SocksCountry
				if group[0].SocksActive != nil {
					socksActive = *group[0].SocksActive
				}
			}

			userCount, totalCount := 0, len(group)
			var nextExpiry *time.Time
			for i := range group {
				e := &Entitlement{
					Status:    group[i].Status,
					ExpiresAt: group[i].ExpiresAt,
				}
				if !e.IsActive(now) {
					continue
				}
				userCount++
				if group[i].ExpiresAt != nil {
					if nextExpiry == nil || group[i].ExpiresAt.Before(*nextExpiry) {
						nextExpiry = group[i].ExpiresAt
					}
				}
			}

			users := make(jsArr, 0, len(group))
			for i := range group {
				e := &Entitlement{Status: group[i].Status, ExpiresAt: group[i].ExpiresAt}
				status := group[i].Status
				if e.IsActive(now) {
					status = "active"
				}
				users = append(users, jo(
					"entitlement_id", group[i].EntitlementID,
					"user_id", group[i].UserID,
					"email", nullableStr(group[i].UserEmail),
					"sku_name", nullableStr(group[i].SKUName),
					"status", status,
					"expires_at", nullableISO(group[i].ExpiresAt),
				))
			}

			items = append(items, jo(
				"socks_id", socksID,
				"host", nullableStr(host),
				"port", nullableInt(port),
				"country", nullableStr(country),
				"socks_active", socksActive,
				"user_count", userCount,
				"total_count", totalCount,
				"next_expiry", nullableISO(nextExpiry),
				"users", users,
			))
		}
	}

	h.successPage(c, items, page, perPage, totalLines)
}

// opsPending 对齐 GET /admin-api/proxy-ops/pending（已支付但没发出权益的代理单）。
func (h *Handler) opsPending(c *gin.Context) {
	perPage := clampInt(queryInt(c, "per_page", 50), 10, 100)
	page := queryInt(c, "page", 1)
	if page < 1 {
		page = 1
	}

	rows, total, err := h.store.PendingOrdersPage(ctxOf(c), page, perPage)
	if err != nil {
		h.failServer(c, "查询待补开订单", err)
		return
	}
	items := make(jsArr, 0, len(rows))
	for i := range rows {
		o := &rows[i]
		items = append(items, jo(
			"id", o.ID,
			"order_no", o.OrderNo,
			"user", optionalUserIDEmail(o.UserID, o.UserEmail),
			"sku_name", nullableStr(o.SKUName),
			"amount", phpFloatOf(o.Amount),
			"paid_at", nullableISO(o.PaidAt),
			"renewal", o.Renewal,
		))
	}
	h.successPage(c, items, page, perPage, total)
}

// opsPull 对齐 POST /admin-api/proxy-ops/accounts/{account}/pull（回采用量）。
func (h *Handler) opsPull(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	updated, err := h.pullUsage(ctxOf(c), acc)
	if err != nil {
		h.fail(c, "回采失败："+err.Error(), 502)
		return
	}
	h.success(c, jo("used_bytes", updated.UsedBytes), "已回采")
}

// opsDisable 对齐 POST /admin-api/proxy-ops/accounts/{account}/disable。
func (h *Handler) opsDisable(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	if _, err := h.disableAccount(ctxOf(c), acc); err != nil {
		h.fail(c, "停用失败："+err.Error(), 502)
		return
	}
	h.success(c, nil, "已停用")
}

// opsResetPassword 对齐 POST /admin-api/proxy-ops/accounts/{account}/reset-password。
//
// 返回的明文只在这里出现一次，库里存密文；同时把额度恢复成权益规格的额度，
// 这样「停用过的号」也能靠这一步直接恢复可用。
func (h *Handler) opsResetPassword(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}

	ent, err := h.store.LatestEntitlementForAccount(ctxOf(c), acc.ID)
	if err != nil && !NoRows(err) {
		h.failServer(c, "查询代理权益", err)
		return
	}
	var sku *SKU
	if ent != nil {
		sku, err = h.store.SKUByID(ctxOf(c), ent.SKUID)
		if err != nil && !NoRows(err) {
			h.failServer(c, "查询代理规格", err)
			return
		}
	}

	limit := parseFloatSafe(acc.ProxyLimit)
	threads := acc.MaxThreadCount
	if sku != nil {
		if sku.TrafficGB > 0 {
			limit = float64(sku.TrafficGB)
		}
		if sku.MaxThreadCount > 0 {
			threads = sku.MaxThreadCount
		}
	}

	updated, err := h.renewAccount(ctxOf(c), acc, limit, threads)
	if err != nil {
		h.fail(c, "重置失败："+err.Error(), 502)
		return
	}
	h.success(c, jo(
		"username", updated.Username,
		"password", derefString(updated.Password),
	), "已重置，请把新凭据发给用户")
}

// opsExtend 对齐 POST /admin-api/proxy-ops/entitlements/{entitlement}/extend。
func (h *Handler) opsExtend(c *gin.Context) {
	id, ok := h.paramID(c, "entitlement")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	ent, err := h.store.EntitlementByID(ctxOf(c), id)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询代理权益", err)
		return
	}

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("days", "required", "integer", "min:1", "max:365")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}
	days := inputInt(input["days"])

	now := h.now().UTC().Truncate(time.Second)
	base := now
	if ent.ExpiresAt != nil && ent.ExpiresAt.After(now) {
		base = ent.ExpiresAt.UTC()
	}
	newExpiry := base.AddDate(0, 0, int(days))
	status := KindStatusActive
	patch := EntitlementPatch{ExpiresAt: &newExpiry, Status: &status}
	if err := h.store.UpdateEntitlement(ctxOf(c), ent.ID, patch); err != nil {
		h.failServer(c, "延长代理权益", err)
		return
	}

	// 权益带账号时同步账号到期时间；账号被停用过就顺手恢复一次可用凭据。
	if ent.AccountID != nil {
		acc, err := h.store.AccountByID(ctxOf(c), *ent.AccountID)
		if err == nil && acc != nil {
			if err := h.store.PatchAccount(ctxOf(c), acc.ID, AccountPatch{ExpiresAt: &newExpiry}); err != nil {
				h.failServer(c, "同步代理账号到期时间", err)
				return
			}
			if acc.Status != "active" {
				limit := parseFloatSafe(acc.ProxyLimit)
				threads := acc.MaxThreadCount
				if entSKU, skuErr := h.store.SKUByID(ctxOf(c), ent.SKUID); skuErr == nil && entSKU != nil {
					if entSKU.TrafficGB > 0 {
						limit = float64(entSKU.TrafficGB)
					}
					if entSKU.MaxThreadCount > 0 {
						threads = entSKU.MaxThreadCount
					}
				}
				if _, renewErr := h.renewAccount(ctxOf(c), acc, limit, threads); renewErr != nil {
					h.logger.Printf("延长权益时恢复账号失败 account=%d: %v", acc.ID, renewErr)
				}
			}
		}
	}

	h.success(c, jo(
		"id", ent.ID,
		"expires_at", isoTime(newExpiry),
	), "已延长 "+strconv.FormatInt(days, 10)+" 天")
}

// opsMoveSocks 对齐 POST /admin-api/proxy-ops/entitlements/{entitlement}/move-socks。
func (h *Handler) opsMoveSocks(c *gin.Context) {
	id, ok := h.paramID(c, "entitlement")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	ent, err := h.store.EntitlementByID(ctxOf(c), id)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询代理权益", err)
		return
	}

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("socks_id", "required", "integer")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}
	socksID := uint64(inputInt(input["socks_id"]))

	if ent.Kind != KindSocks {
		h.fail(c, "只有共享 socks 权益能换线路", 400)
		return
	}
	sock, err := h.store.SockByID(ctxOf(c), socksID)
	if err != nil || sock == nil || !sock.IsActive {
		// 对齐 moveSocksEntitlement：不存在或未启用都是同一条业务错误。
		h.fail(c, "目标线路不存在或已停用", 400)
		return
	}
	// 独占线路（#地区功能-17）不参与手工换线：它跟买下它的那个用户绑定，
	// 换过去会让这条线路被再卖一次，而老线路永远占着 sold_to 再也卖不出去。
	if sock.Allocation == sockpool.Exclusive {
		h.fail(c, "目标线路是独占型，不能给共享权益换过去", 400)
		return
	}
	if ent.SocksID != nil {
		if cur, curErr := h.store.SockByID(ctxOf(c), *ent.SocksID); curErr == nil && cur != nil &&
			cur.Allocation == sockpool.Exclusive {
			h.fail(c, "这条权益挂在独占线路上，不能手工换线路", 400)
			return
		}
	}

	if err := h.store.UpdateEntitlement(ctxOf(c), ent.ID, EntitlementPatch{SocksID: &socksID}); err != nil {
		h.failServer(c, "迁移 socks 权益", err)
		return
	}
	h.success(c, nil, "已换线路")
}

// opsDeleteAccount 对齐 DELETE /admin-api/proxy-ops/accounts/{account}（上游 + 本地）。
func (h *Handler) opsDeleteAccount(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	if err := h.deleteAccount(ctxOf(c), acc); err != nil {
		h.fail(c, "删除失败："+err.Error(), 502)
		return
	}
	h.success(c, nil, "已删除")
}

// opsProxies 对齐 GET /admin-api/proxy-ops/accounts/{account}/proxies（出口 IP 列表）。
func (h *Handler) opsProxies(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	if acc.ProviderID == 0 {
		h.fail(c, "账号缺少上游配置", 400)
		return
	}
	provider, err := h.store.ProviderByID(ctxOf(c), acc.ProviderID)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			h.fail(c, "账号缺少上游配置", 400)
			return
		}
		h.failServer(c, "查询上游账号", err)
		return
	}
	if h.upstream == nil {
		h.fail(c, "上游客户端未配置", 502)
		return
	}

	data, err := h.upstream.ListProxies(ctxOf(c), provider.APIToken, map[string]string{
		"mode":      "direct",
		"page_size": "100",
	})
	if err != nil {
		h.fail(c, "获取 IP 失败："+err.Error(), 502)
		return
	}

	results, _ := data["results"].([]any)
	items := make(jsArr, 0, len(results))
	for _, raw := range results {
		item, _ := raw.(map[string]any)
		items = append(items, jo(
			"id", item["id"],
			"address", mapString(item, "proxy_address"),
			"port", item["port"],
			"country", mapString(item, "country_code"),
			"city", mapString(item, "city_name"),
			"asn", mapString(item, "asn_name"),
			"valid", inputBool(item["valid"]),
			"last_verification", item["last_verification"],
		))
	}
	h.success(c, items)
}

// opsRetryOrder 对齐 POST /admin-api/proxy-ops/orders/{order}/retry（钱已付但上游开号失败）。
func (h *Handler) opsRetryOrder(c *gin.Context) {
	id, ok := h.paramID(c, "order")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	order, err := h.store.OrderByID(ctxOf(c), id)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询订单", err)
		return
	}
	if order.OrderType != OrderTypeProxy {
		h.fail(c, "不是代理订单", 400)
		return
	}
	if order.Status != OrderStatusPaid {
		h.fail(c, "只有已支付的订单能重试开通", 400)
		return
	}

	h.completeOrder(ctxOf(c), order)

	created, err := h.store.EntitlementExistsForOrder(ctxOf(c), order.ID)
	if err != nil {
		h.failServer(c, "查询代理权益", err)
		return
	}
	if !created {
		h.fail(c, "仍然开通失败，请查看日志或检查上游额度", 502)
		return
	}
	h.success(c, nil, "已开通")
}

// renewAccount 对齐 ProxyProviderService::renewAccount：换新密码 + 提回额度 + 恢复启用。
func (h *Handler) renewAccount(ctx context.Context, acc *Account, proxyLimit float64, threads int64) (*Account, error) {
	if h.upstream == nil {
		return nil, errUpstreamMissing
	}
	provider, err := h.store.ProviderByID(ctx, acc.ProviderID)
	if err != nil {
		return nil, err
	}
	password := randomProxyPassword()
	if _, err := h.upstream.UpdateSubuser(ctx, provider.APIToken, acc.UpstreamID, map[string]any{
		"password":         password,
		"proxy_limit":      proxyLimit,
		"max_thread_count": threads,
	}); err != nil {
		return nil, err
	}
	h.markProviderResult(ctx, provider, true, nil)

	now := h.now().UTC().Truncate(time.Second)
	limit := floatToDecimal(proxyLimit, 2)
	status := "active"
	username := acc.Username
	if username == "" {
		// 同步导入的号可能没有本地 username，补一个可读的（只影响展示与交付）。
		base := acc.UserID
		if base == nil {
			id := acc.ID
			base = &id
		}
		username = "u" + strconv.FormatUint(*base, 10)
	}
	if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{
		Password:       &password,
		Username:       &username,
		ProxyLimit:     &limit,
		MaxThreadCount: &threads,
		Status:         &status,
		LastSyncedAt:   &now,
	}); err != nil {
		return nil, err
	}
	return h.store.AccountByID(ctx, acc.ID)
}

// ---- 小工具 ----

// optionalUser 输出 {id, email} 或 null（对齐 PHP 的三元表达式）。
func optionalUser(id *uint64, email *string) any {
	if id == nil {
		return nil
	}
	return jo("id", *id, "email", nullableStr(email))
}

// optionalUserIDEmail 同 optionalUser，用于待补开列表（user_id 非空才输出）。
func optionalUserIDEmail(id uint64, email *string) any {
	if id == 0 {
		return nil
	}
	return jo("id", id, "email", nullableStr(email))
}

// clampInt 把 value 夹到 [min, max]。
func clampInt(v, min, max int64) int64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// isoTimeTime 取 isoTime 对应的时间（比较用）。
func isoTimeTime(v isoTime) time.Time { return time.Time(v) }
