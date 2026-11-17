package admin

// 代理订单开通（对齐 Services\ProxyStoreService::completeOrder / doCompleteOrder）。
//
// 口径（与站长确认过的产品规则）：
//   - 流量不归平台管：webshare 建号时把 GB 传给上游 proxy_limit，平台只读回用量展示
//   - webshare 独占：一人一个子账号；续费只改额度/到期，不新开号（省上游子账号额度）
//   - socks 共享：多人共用同一条 socks，不新建任何账号，只挂权益
//   - 库存：新开通占一份（reserve→consume），续费不占；开通失败要把库存还回去
//
// 本文件只承载管理端会用到的「重试开通」路径（ProxyOpsController::retryOrder）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"controlhub-server/internal/sockpool"
)

// errUpstreamMissing 是上游客户端未配置时的错误（管理端 → 502）。
var errUpstreamMissing = errors.New("上游客户端未配置")

// completeOrder 对齐 ProxyStoreService::completeOrder（幂等 + 互斥）。
//
// 为什么要锁：开号要打上游，最长 21 秒（连接 6s + 总 15s）。网关等不到响应会重推回调，
// 两条回调同时进来各自查「有没有权益」都是「没有」，就会各开一个上游子账号 ——
// 钱只收一次，却白占一个子账号。拿不到锁说明另一条正在处理，直接返回，那条会负责开通。
func (h *Handler) completeOrder(ctx context.Context, order *Order) {
	owner := randomProxyPassword()
	acquired, err := h.store.LockAcquire(ctx, "proxy:provision:"+itoaUint(order.ID), 120, owner)
	if err != nil {
		h.logger.Printf("代理订单开通取锁失败 order=%d: %v", order.ID, err)
		return
	}
	if !acquired {
		h.logger.Printf("代理订单开通已在处理中，跳过重复回调 order_no=%s", order.OrderNo)
		return
	}
	defer func() {
		if err := h.store.LockRelease(ctx, "proxy:provision:"+itoaUint(order.ID), owner); err != nil {
			h.logger.Printf("代理订单开通放锁失败 order=%d: %v", order.ID, err)
		}
	}()

	h.doCompleteOrder(ctx, order)
}

// doCompleteOrder 对齐 doCompleteOrder：失败只记日志、订单保持 paid，由管理员补开。
func (h *Handler) doCompleteOrder(ctx context.Context, order *Order) {
	if order.SKUID == nil {
		h.logger.Printf("代理订单开通失败：规格缺失 order_no=%s", order.OrderNo)
		return
	}
	sku, err := h.store.SKUByID(ctx, *order.SKUID)
	if err != nil || sku == nil {
		h.logger.Printf("代理订单开通失败：规格缺失 order_no=%s", order.OrderNo)
		return
	}
	email, err := h.store.UserEmail(ctx, order.UserID)
	if err != nil && !NoRows(err) {
		h.logger.Printf("代理订单开通失败：查询用户出错 order_no=%s: %v", order.OrderNo, err)
		return
	}
	if email == nil {
		h.logger.Printf("代理订单开通失败：规格或用户缺失 order_no=%s", order.OrderNo)
		return
	}

	// 幂等：同一订单只开通一次。
	exists, err := h.store.EntitlementExistsForOrder(ctx, order.ID)
	if err != nil {
		h.logger.Printf("代理订单开通失败：查询权益出错 order_no=%s: %v", order.OrderNo, err)
		return
	}
	if exists {
		return
	}

	now := h.now().UTC().Truncate(time.Second)
	kind := KindWebshare
	if sku.SourceType != nil && *sku.SourceType == KindSocks {
		kind = KindSocks
	}

	existing, err := h.store.FindRenewableEntitlement(ctx, order.UserID, kind, true, now)
	if err != nil && !NoRows(err) {
		h.logger.Printf("代理订单开通失败：查询可续费权益出错 order_no=%s: %v", order.OrderNo, err)
		return
	}

	days := sku.DurationDays
	if days < 1 {
		days = 1
	}

	var accountID, socksID *uint64
	if kind == KindSocks {
		if sockpool.IsExclusive(sku.Allocation) {
			// 独占：一条线路只卖一个人。续费把旧线路传进去 —— 还归他就沿用，
			// 已经被释放/转卖就重新从池子里挑一条（池空即报「独占 socks 池已空」）。
			var renewSockID *uint64
			if existing != nil {
				renewSockID = existing.SocksID
			}
			id, allocErr := h.store.AcquireExclusiveSock(ctx, renewSockID, sku.SourceID, order.UserID, now)
			if allocErr != nil {
				h.logger.Printf("代理订单开通失败：%v order_no=%s sku_id=%d user_id=%d",
					allocErr, order.OrderNo, sku.ID, order.UserID)
				return
			}
			socksID = &id
		} else {
			// 共享（现状）：所有买家共用 sku.source_id 那一条。
			if sku.SourceID == nil {
				h.logger.Printf("代理订单开通失败：共享 socks 货源不可用 order_no=%s", order.OrderNo)
				return
			}
			sock, sockErr := h.store.SockByID(ctx, *sku.SourceID)
			if sockErr != nil || sock == nil || !sock.IsActive {
				h.logger.Printf("代理订单开通失败：共享 socks 货源不可用 order_no=%s", order.OrderNo)
				return
			}
			socksID = &sock.ID
		}
	} else {
		acc, accErr := h.openOrRenewWebshareAccount(ctx, order, sku, existing, now)
		if accErr != nil {
			// 上游失败：把库存还回去（新开通才占过），订单保持 paid，留日志给管理员补开。
			if !order.ProxyRenewal {
				if relErr := h.store.ReleaseStock(ctx, sku.ID, 1); relErr != nil {
					h.logger.Printf("代理订单开通失败后归还库存出错 order_no=%s: %v", order.OrderNo, relErr)
				}
			}
			h.logger.Printf("代理订单开通失败（用户已付款，需人工补开）order_no=%s user_id=%d sku_id=%d error=%v",
				order.OrderNo, order.UserID, sku.ID, accErr)
			return
		}
		accountID = &acc.ID
	}

	if err := h.upsertEntitlement(ctx, order, sku, kind, accountID, socksID, existing, days, now); err != nil {
		h.logger.Printf("代理订单开通失败：写权益出错 order_no=%s: %v", order.OrderNo, err)
		return
	}

	// 新开通成功：库存占用转已售（续费不占库存）。
	if !order.ProxyRenewal {
		if err := h.store.ConsumeStock(ctx, sku.ID, 1); err != nil {
			h.logger.Printf("代理订单开通后转库存出错 order_no=%s: %v", order.OrderNo, err)
		}
	}
}

// openOrRenewWebshareAccount 对齐 doCompleteOrder 的 webshare 分支。
func (h *Handler) openOrRenewWebshareAccount(
	ctx context.Context, order *Order, sku *SKU, existing *Entitlement, now time.Time,
) (*Account, error) {
	var account *Account
	if existing != nil && existing.AccountID != nil {
		acc, err := h.store.AccountByID(ctx, *existing.AccountID)
		if err != nil && !NoRows(err) {
			return nil, err
		}
		account = acc
	}

	if account != nil {
		// 续费：把额度重置回规格额度、恢复启用、延长到期。
		renewed, err := h.renewAccountForSKU(ctx, account, sku, now)
		if err != nil {
			return nil, err
		}
		return renewed, nil
	}
	return h.openWebshareAccount(ctx, order, sku, now)
}

// providerCandidate 是 pickProviders 的排序中间结果（账号 + 它已有的子账号数）。
type providerCandidate struct {
	provider   *Provider
	count      int
	countKnown bool
}

// subuserCountTTL 是「账号下已开子账号数」的缓存时间。
//
// 一次开号要给每个候选账号排一次序，不缓存就是一次开号打 N 次上游；
// 60 秒足够让同一批订单用上同一份快照，又不至于把配额变化拖太久。
const subuserCountTTL = 60 * time.Second

// subuserCountEntry 是缓存项：条数 + 采集时间（TTL 用）。
type subuserCountEntry struct {
	count int
	at    time.Time
}

var (
	subuserCountMu    sync.Mutex
	subuserCountCache = map[uint64]subuserCountEntry{}
)

// providerLastOK 判断账号上次调用是否成功（表里成功写 success，文档里的 ok 同义）。
func providerLastOK(p *Provider) bool {
	return p.LastStatus == "success" || p.LastStatus == "ok"
}

// subuserCount 返回账号下已开子账号数，60 秒内复用上次结果（见 subuserCountTTL）。
//
// known=false 表示这次没读成（token 失效 / 网络失败 / 上游 429）：调用方把该账号
// 排到「已知条数」的后面，但仍然留着当候选 —— 读列表失败不等于开不了号。
func (h *Handler) subuserCount(ctx context.Context, p *Provider) (count int, known bool) {
	if h.upstream == nil {
		return 0, false
	}
	now := h.now()
	subuserCountMu.Lock()
	entry, ok := subuserCountCache[p.ID]
	subuserCountMu.Unlock()
	if ok && now.Sub(entry.at) < subuserCountTTL {
		return entry.count, true
	}

	subusers, err := h.upstream.ListSubusers(ctx, p.APIToken)
	if err != nil {
		h.logger.Printf("webshare 账号子用户数读取失败，本次排到后面 provider=%d(%s): %v", p.ID, p.Name, err)
		return 0, false
	}
	subuserCountMu.Lock()
	subuserCountCache[p.ID] = subuserCountEntry{count: len(subusers), at: now}
	subuserCountMu.Unlock()
	return len(subusers), true
}

// pickProviders 返回本次开号的候选账号（已按优先级排好）。
//
// 排序规则（sku 没显式指定账号时才比较）：
//  1. 上次调用成功的排前面（failed / 从没测过的 pending 排后面）
//  2. 账号下已开子账号少的排前面 —— 一个账号的子账号额度满了就再也开不出号，
//     先把新订单往空账号上引，才不会永远撞死在第一个账号上
//  3. 平局按 id 升序（老账号先用，行为可预测）
//
// sku.source_id 显式指定账号时只返回那一个：那是运营把规格钉死在某个账号上，
// 开不出来就报错给人看，不能偷偷换号。
func (h *Handler) pickProviders(ctx context.Context, sku *SKU) ([]*Provider, error) {
	if sku.SourceID != nil {
		provider, err := h.store.ProviderByID(ctx, *sku.SourceID)
		if err != nil {
			if NoRows(err) {
				return nil, fmt.Errorf("规格指定的 Webshare 账号不存在（id=%d）", *sku.SourceID)
			}
			return nil, err
		}
		return []*Provider{provider}, nil
	}

	providers, err := h.store.Providers(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]providerCandidate, 0, len(providers))
	for i := range providers {
		if !providers[i].Enabled {
			continue
		}
		count, known := h.subuserCount(ctx, &providers[i])
		candidates = append(candidates, providerCandidate{provider: &providers[i], count: count, countKnown: known})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if okA, okB := providerLastOK(a.provider), providerLastOK(b.provider); okA != okB {
			return okA
		}
		if a.countKnown != b.countKnown {
			return a.countKnown
		}
		if a.count != b.count {
			return a.count < b.count
		}
		return a.provider.ID < b.provider.ID
	})

	out := make([]*Provider, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.provider)
	}
	return out, nil
}

// openWebshareAccount 对齐 openWebshareAccount：开一个上游子账号并落库。
//
// 多账号容错：候选账号按 pickProviders 排好，前一个开号失败（子账号额度满 / token
// 失效 / 上游 5xx）就换下一个，全试完还失败才把错误抛给上层（订单保持 paid 等人工补开）。
func (h *Handler) openWebshareAccount(ctx context.Context, order *Order, sku *SKU, now time.Time) (*Account, error) {
	if h.upstream == nil {
		return nil, errUpstreamMissing
	}

	candidates, err := h.pickProviders(ctx, sku)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("没有可用的 Webshare 账号（请先在上游里添加并启用）")
	}

	label := "u" + itoaUint(order.UserID) + "-" + sku.Name
	var lastErr error
	for _, provider := range candidates {
		data, err := h.createUpstreamSubuser(ctx, provider, label, sku.TrafficGB, sku.MaxThreadCount)
		if err != nil {
			// 上游失败：记下是哪个账号、什么错，换下一个继续试。
			msg := err.Error()
			h.logger.Printf("webshare 开号失败，换下一个账号 provider=%d(%s) sku_id=%d: %v",
				provider.ID, provider.Name, sku.ID, err)
			h.markProviderResult(ctx, provider, false, &msg)
			lastErr = err
			continue
		}

		acc, err := h.persistOpenedAccount(ctx, provider, order, sku, now, label, data)
		if err != nil {
			// 上游已经建号成功、只是本地落库失败：不能换号重开，
			// 否则上游会多占一个子账号（钱只收了一次）。
			return nil, err
		}
		h.markProviderResult(ctx, provider, true, nil)
		return acc, nil
	}
	return nil, fmt.Errorf("所有 webshare 账号均不可用（试了 %d 个）：%w", len(candidates), lastErr)
}

// createUpstreamSubuser 在指定账号下开一个上游子账号（不含本地落库）。
//
// 参数按「额度 / 并发」而不是 *SKU：静态 IP 出口分类开号（OpenProfileAccount）
// 用的是 exit_profiles 的模板值，没有 SKU 行可传，但上游请求完全一致。
func (h *Handler) createUpstreamSubuser(
	ctx context.Context, provider *Provider, label string, trafficGB, maxThreadCount int64,
) (map[string]any, error) {
	data, err := h.upstream.CreateSubuser(ctx, provider.APIToken, map[string]any{
		"label":            label,
		"proxy_limit":      float64(trafficGB),
		"max_thread_count": maxThreadCount,
	})
	if err != nil {
		return nil, err
	}
	if jsonStringOf(data["id"]) == "" {
		return nil, fmt.Errorf("Webshare 建号响应缺少 id")
	}
	return data, nil
}

// persistOpenedAccount 把上游建号响应落成 proxy_accounts 一行（含商城字段）。
func (h *Handler) persistOpenedAccount(
	ctx context.Context, provider *Provider, order *Order, sku *SKU, now time.Time, label string, data map[string]any,
) (*Account, error) {
	skuID := sku.ID
	orderID := order.ID
	return h.persistAccountRow(ctx, provider, order.UserID, &orderID, &skuID, label,
		sku.TrafficGB, sku.MaxThreadCount, now.AddDate(0, 0, daysOf(sku)), now, data)
}

// persistAccountRow 按上游建号响应写 proxy_accounts（不含任何 SKU 概念）。
//
// 两条开号路径共用：代理商城（有 SKU/订单）与静态 IP 出口分类（只有分类模板与订单）。
// skuID 为 nil 时不写 sku_id（分类开号的列必须留 NULL，proxy_skus 里没有对应行）。
func (h *Handler) persistAccountRow(
	ctx context.Context, provider *Provider, userID uint64, orderID, skuID *uint64, label string,
	trafficGB, maxThreadCount int64, expiry, now time.Time, data map[string]any,
) (*Account, error) {
	upstreamID := jsonStringOf(data["id"])

	respLabel := label
	if v, ok := data["label"].(string); ok {
		respLabel = v
	}
	username := jsonStringOf(data["username"])
	password := jsonStringOf(data["password"])
	limit := float64(trafficGB)
	if v, ok := data["proxy_limit"]; ok {
		limit = phpNumberOf(v)
	}
	threads := maxThreadCount
	if v, ok := data["max_thread_count"]; ok {
		threads = int64(phpNumberOf(v))
	}

	acc, err := h.store.CreateAccount(ctx, AccountCreate{
		ProviderID:     provider.ID,
		UpstreamID:     upstreamID,
		UserID:         &userID,
		Label:          respLabel,
		Username:       username,
		Password:       strPtrOrNil(password),
		ProxyLimit:     floatToDecimal(limit, 2),
		MaxThreadCount: threads,
		ProxyCountries: proxyCountriesJSON(data["proxy_countries"]),
		UsedBytes:      0,
		Status:         "active",
		LastSyncedAt:   &now,
	})
	if err != nil {
		return nil, err
	}

	// 商城 / 分类字段：sku / 流量上限 / 首单 / 到期（续费不覆盖 order_id，见 schema 注释）。
	accountPatch := AccountPatch{ExpiresAt: &expiry}
	if skuID != nil {
		accountPatch.SKUID = skuID
	}
	if orderID != nil {
		accountPatch.OrderID = orderID
	}
	if err := h.store.PatchAccount(ctx, acc.ID, accountPatch); err != nil {
		return nil, err
	}
	if trafficGB > 0 {
		bytes := trafficGB * 1024 * 1024 * 1024
		if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{TrafficLimitBytes: &bytes}); err != nil {
			return nil, err
		}
	}
	return h.store.AccountByID(ctx, acc.ID)
}

// renewAccountForSKU 对齐 renewWebshareAccount：额度重置、恢复启用、延长到期。
func (h *Handler) renewAccountForSKU(ctx context.Context, acc *Account, sku *SKU, now time.Time) (*Account, error) {
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
		"proxy_limit":      float64(sku.TrafficGB),
		"max_thread_count": sku.MaxThreadCount,
	}); err != nil {
		return nil, err
	}

	base := now
	if acc.ExpiresAt != nil && acc.ExpiresAt.After(now) {
		base = acc.ExpiresAt.UTC()
	}
	expiry := base.AddDate(0, 0, daysOf(sku))

	skuID := sku.ID
	limit := floatToDecimal(float64(sku.TrafficGB), 2)
	status := "active"
	if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{
		Password:     &password,
		ProxyLimit:   &limit,
		SKUID:        &skuID,
		ExpiresAt:    &expiry,
		Status:       &status,
		LastSyncedAt: &now,
	}); err != nil {
		return nil, err
	}
	if sku.TrafficGB > 0 {
		bytes := sku.TrafficGB * 1024 * 1024 * 1024
		if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{TrafficLimitBytes: &bytes}); err != nil {
			return nil, err
		}
	}
	return h.store.AccountByID(ctx, acc.ID)
}

// upsertEntitlement 对齐 upsertEntitlement：续费时延长到期，不新增行。
func (h *Handler) upsertEntitlement(
	ctx context.Context, order *Order, sku *SKU, kind string,
	accountID, socksID *uint64, existing *Entitlement, days int64, now time.Time,
) error {
	if existing != nil {
		base := now
		if existing.ExpiresAt != nil && existing.ExpiresAt.After(now) {
			base = existing.ExpiresAt.UTC()
		}
		expiry := base.AddDate(0, 0, int(days))
		orderID := order.ID
		skuID := sku.ID
		status := KindStatusActive
		patch := EntitlementPatch{
			OrderID:   &orderID,
			SKUID:     &skuID,
			Status:    &status,
			ExpiresAt: &expiry,
		}
		if accountID != nil {
			patch.AccountID = accountID
		}
		if socksID != nil {
			patch.SocksID = socksID
		}
		return h.store.UpdateEntitlement(ctx, existing.ID, patch)
	}

	orderID := order.ID
	expiry := now.AddDate(0, 0, int(days))
	_, err := h.store.CreateEntitlement(ctx, EntitlementCreate{
		UserID:    order.UserID,
		OrderID:   &orderID,
		SKUID:     sku.ID,
		Kind:      kind,
		AccountID: accountID,
		SocksID:   socksID,
		ExpiresAt: &expiry,
		Status:    KindStatusActive,
	})
	return err
}

// daysOf 取规格天数（至少 1 天，对齐 max(1, duration_days)）。
func daysOf(sku *SKU) int {
	if sku.DurationDays < 1 {
		return 1
	}
	return int(sku.DurationDays)
}

// itoaUint 是 uint64 → 十进制字符串。
func itoaUint(v uint64) string { return fmt.Sprintf("%d", v) }

// ---------------------------------------------------------------------------
// 静态 IP 出口分类开号（#静态IP-02）
// ---------------------------------------------------------------------------

// ExitProfileSpec 是开号要用到的分类模板（exit_profiles 一行）。
//
// 刻意不复用 ExitProfileRecord：那个结构带 node_ids / provider_name 等详情接口字段，
// 购买流程只要开号所需的 4 个值，窄结构更好测。
type ExitProfileSpec struct {
	ID             uint64
	ProviderID     uint64
	TrafficGB      int64
	DurationDays   int64
	MaxThreadCount int64
}

// ErrExitProfileAccountExists 表示该用户在该分类下已经有 proxy_accounts 行。
//
// 购买流程据此区分「首开」与「续费」：已经有了就只改额度/到期，不再打上游开号。
var ErrExitProfileAccountExists = errors.New("admin: 该用户在该出口分类下已有账号")

// staticProfileAccountLabel 是分类开号的 label：`u<uid>-exit<profileID>`。
//
// 与商城的 `u<uid>-<sku名>` 区分开，管理员在上游子账号列表里一眼能看出是哪个来源。
func staticProfileAccountLabel(userID, profileID uint64) string {
	return "u" + itoaUint(userID) + "-exit" + itoaUint(profileID)
}

// OpenProfileAccount 为「买了 static_chain 套餐的用户」开一个 webshare 子账号。
//
// 与商城开号（openWebshareAccount）共用同一条落库链路，区别只有三点：
//   - 没有 SKU：额度/并发/天数来自 exit_profiles，sku_id 留 NULL；
//   - 不涉及库存（分类不卖子账号配额，库存属于 proxy_skus）；
//   - 用户在该分类下已有账号时返回 ErrExitProfileAccountExists，**绝不再开一个**
//     （一人一分类一个子账号，续费只改额度/到期）。
//
// 上游失败返回错误；调用方（支付流程）只记日志、订单保持 paid，由管理员补开。
func (h *Handler) OpenProfileAccount(
	ctx context.Context, userID, orderID uint64, spec ExitProfileSpec, now time.Time,
) (*Account, error) {
	if h.upstream == nil {
		return nil, errUpstreamMissing
	}
	if spec.ID == 0 {
		return nil, errors.New("出口分类缺失")
	}

	existing, err := h.findProfileAccount(ctx, userID, spec)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrExitProfileAccountExists
	}

	provider, err := h.store.ProviderByID(ctx, spec.ProviderID)
	if err != nil {
		if NoRows(err) {
			return nil, fmt.Errorf("出口分类指定的 Webshare 账号不存在（id=%d）", spec.ProviderID)
		}
		return nil, err
	}
	if !provider.Enabled {
		return nil, fmt.Errorf("出口分类指定的 Webshare 账号已停用（id=%d）", spec.ProviderID)
	}

	label := staticProfileAccountLabel(userID, spec.ID)
	data, err := h.createUpstreamSubuser(ctx, provider, label, spec.TrafficGB, spec.MaxThreadCount)
	if err != nil {
		msg := err.Error()
		h.logger.Printf("静态 IP 开号失败 provider=%d(%s) profile_id=%d user_id=%d: %v",
			provider.ID, provider.Name, spec.ID, userID, err)
		h.markProviderResult(ctx, provider, false, &msg)
		return nil, err
	}

	days := spec.DurationDays
	if days < 1 {
		days = 1
	}
	expiry := now.AddDate(0, 0, int(days))
	oid := orderID
	acc, err := h.persistAccountRow(ctx, provider, userID, &oid, nil, label,
		spec.TrafficGB, spec.MaxThreadCount, expiry, now, data)
	if err != nil {
		// 上游已建号、本地落库失败：不能重开（会白占一个上游子账号）。
		return nil, err
	}
	h.markProviderResult(ctx, provider, true, nil)
	h.patchProfileID(ctx, acc.ID, spec.ID)
	refreshed, err := h.store.AccountByID(ctx, acc.ID)
	if err != nil {
		return acc, nil
	}
	return refreshed, nil
}

// RenewProfileAccount 是分类续费：只更新额度 / 到期 / 状态，不重新开号。
//
// 到期时间从「较晚的那个」起算（与商城续费同口径）：没过期就顺延，已过期就从现在算。
func (h *Handler) RenewProfileAccount(
	ctx context.Context, acc *Account, spec ExitProfileSpec, now time.Time,
) (*Account, error) {
	if acc == nil {
		return nil, errors.New("账号不存在")
	}
	days := spec.DurationDays
	if days < 1 {
		days = 1
	}
	base := now
	if acc.ExpiresAt != nil && acc.ExpiresAt.After(now) {
		base = acc.ExpiresAt.UTC()
	}
	expiry := base.AddDate(0, 0, int(days))

	limit := floatToDecimal(float64(spec.TrafficGB), 2)
	threads := spec.MaxThreadCount
	status := "active"
	if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{
		ProxyLimit:     &limit,
		MaxThreadCount: &threads,
		ExpiresAt:      &expiry,
		Status:         &status,
		LastSyncedAt:   &now,
	}); err != nil {
		return nil, err
	}
	if spec.TrafficGB > 0 {
		bytes := spec.TrafficGB * 1024 * 1024 * 1024
		if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{TrafficLimitBytes: &bytes}); err != nil {
			return nil, err
		}
	}
	h.patchProfileID(ctx, acc.ID, spec.ID)
	refreshed, err := h.store.AccountByID(ctx, acc.ID)
	if err != nil {
		return acc, nil
	}
	return refreshed, nil
}

// FindProfileAccount 返回该用户在该分类下已有的账号（没有则 nil）。
//
// 导出供支付流程复用：续费判断与「不重新开号」都建立在这个查询上。
func (h *Handler) FindProfileAccount(ctx context.Context, userID uint64, spec ExitProfileSpec) (*Account, error) {
	return h.findProfileAccount(ctx, userID, spec)
}

// findProfileAccount 查该用户在该分类下已有的账号（没有则 nil）。
//
// 优先按 proxy_accounts.profile_id 查（该列由并行任务「静态IP-32」加）；
// 列还不存在时退回「同 provider + 同 label」—— label 由 user_id 与 profile_id
// 共同决定（见 staticProfileAccountLabel），分类内唯一，两种判据等价。
func (h *Handler) findProfileAccount(ctx context.Context, userID uint64, spec ExitProfileSpec) (*Account, error) {
	if ps, ok := h.store.(accountProfileStore); ok {
		exists, err := ps.AccountProfileColumnExists(ctx)
		if err != nil {
			return nil, err
		}
		if exists {
			id, err := ps.AccountIDByProfile(ctx, userID, spec.ID)
			if err != nil {
				return nil, err
			}
			if id == 0 {
				return nil, nil
			}
			return h.store.AccountByID(ctx, id)
		}
	}

	label := staticProfileAccountLabel(userID, spec.ID)
	accounts, err := h.store.ProviderAccounts(ctx, spec.ProviderID)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		a := &accounts[i]
		if a.UserID != nil && *a.UserID == userID && a.Label == label {
			return a, nil
		}
	}
	return nil, nil
}

// patchProfileID 把新开/续费的账号关联到出口分类（proxy_accounts.profile_id）。
//
// 列不存在时跳过并记日志：本任务不改 schema（该列由并行任务「静态IP-32」补），
// 缺列绝不能让开号失败 —— 但管理员要能从日志看出「关联待补」。
func (h *Handler) patchProfileID(ctx context.Context, accountID, profileID uint64) {
	ps, ok := h.store.(accountProfileStore)
	if !ok {
		return
	}
	exists, err := ps.AccountProfileColumnExists(ctx)
	if err != nil {
		h.logger.Printf("静态 IP 探测 proxy_accounts.profile_id 失败 account=%d: %v", accountID, err)
		return
	}
	if !exists {
		h.logger.Printf("proxy_accounts.profile_id 列不存在，暂不写分类关联 account=%d profile_id=%d"+
			"（等「静态IP-32」迁移落地后由重试补写）", accountID, profileID)
		return
	}
	if err := ps.PatchAccountProfile(ctx, accountID, profileID); err != nil {
		h.logger.Printf("静态 IP 写分类关联失败 account=%d profile_id=%d: %v", accountID, profileID, err)
	}
}

// accountProfileStore 是 proxy_accounts.profile_id（静态 IP 出口分类关联）的可选能力。
//
// 这一列由并行任务「静态IP-32」以幂等 ALTER 加进 ensureSchema；本任务不改 schema，
// 所以全部按「可选能力」处理：*sqlStore 在（同包，天然满足），窄 fake 不满足时
// 调用方退回 label 判定、且不写该列 —— 缺列不能让开号失败。
type accountProfileStore interface {
	AccountProfileColumnExists(ctx context.Context) (bool, error)
	AccountIDByProfile(ctx context.Context, userID, profileID uint64) (uint64, error)
	PatchAccountProfile(ctx context.Context, accountID, profileID uint64) error
}

// ---- proxy_accounts.profile_id（静态 IP 出口分类关联，可选列） ----
//
// 该列由并行任务「静态IP-32」的幂等 ALTER 落到库上；本任务只在运行时探测，不做 DDL。

// AccountProfileColumnExists 用 information_schema 探测 proxy_accounts.profile_id。
//
// 刻意不缓存：只在购买/续费时问一次（低频路径），缓存反而会让「先起服务、后跑迁移」
// 的部署一直拿到旧结论，也让不同库的测试互相污染。
func (s *sqlStore) AccountProfileColumnExists(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'proxy_accounts' AND COLUMN_NAME = 'profile_id'`).
		Scan(&n); err != nil {
		// 查不到 information_schema（权限受限）按「不存在」处理：只影响关联列，
		// 不能因此让开号失败。
		return false, nil
	}
	return n > 0, nil
}

// AccountIDByProfile 按 (user_id, profile_id) 查账号 id（0 = 没有）。
func (s *sqlStore) AccountIDByProfile(ctx context.Context, userID, profileID uint64) (uint64, error) {
	var id uint64
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM proxy_accounts WHERE user_id = ? AND profile_id = ? ORDER BY id LIMIT 1",
		userID, profileID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// PatchAccountProfile 把账号关联到分类。
func (s *sqlStore) PatchAccountProfile(ctx context.Context, accountID, profileID uint64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE proxy_accounts SET profile_id = ?, updated_at = ? WHERE id = ?",
		profileID, s.now().UTC().Truncate(time.Second), accountID)
	return err
}
