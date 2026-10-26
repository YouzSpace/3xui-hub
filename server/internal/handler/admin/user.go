package admin

// 管理端用户（/admin-api/users…）：CRUD + reset-traffic/reset-password/protocol/renew。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/UserController.php 与
// backend/app/Services/UserAdminService.php。
//
// 面板动作（3x-ui / xray）走 UserPanelOps 接缝：未注入时用 NoopUserPanelOps，
// 数据库侧行为与 PHP 一致（PHP 对面板失败也只 report()，不影响接口成功）。

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- present ----

// presentUser 对齐 UserController::present。
//
// token/uuid/proxies 只在 full=true（详情与写响应）时输出；列表里 proxies 为 null。
func presentUser(u *UserRow, full bool, proxies jsArr) *jsObj {
	token := any(nil)
	uuid := any(nil)
	if full {
		token = u.Token
		uuid = u.UUID
	}
	proxiesOut := any(nil)
	if full {
		proxiesOut = proxies
	}
	return jo(
		"id", u.ID,
		"email", nullableStr(u.Email),
		"token", token,
		"uuid", uuid,
		"protocol", u.Protocol,
		"plan_id", nullableUint(u.PlanID),
		"plan_type", nullableStr(u.PlanType),
		"plan_name", nullableStr(u.PlanName),
		"plan_months", nullableInt(u.PlanMonths),
		"traffic_limit", u.TrafficLimit,
		"traffic_used", u.TrafficUsed,
		"monthly_traffic_used", u.MonthlyTrafficUsed,
		"monthly_traffic_limit", u.MonthlyTrafficLimit,
		"next_traffic_reset_at", nullableISO(u.NextTrafficResetAt),
		"expired_at", nullableISO(u.ExpiredAt),
		"enabled", u.Enabled,
		"created_at", nullableISO(u.CreatedAt),
		"proxy_active_count", u.ProxyActiveCount,
		"proxies", proxiesOut,
	)
}

// presentProxies 对齐 UserController::presentProxies（按 id 降序）。
func presentProxies(list []UserProxy, now time.Time) jsArr {
	sorted := make([]UserProxy, len(list))
	copy(sorted, list)
	// sortByDesc('id')
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].ID > sorted[i].ID {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	out := make(jsArr, 0, len(sorted))
	for i := range sorted {
		p := &sorted[i]
		kindLabel := "独享账号"
		if p.Kind == "socks" {
			kindLabel = "共享 socks"
		}
		status := p.Status
		if p.IsActive(now) {
			status = "active"
		}
		out = append(out, jo(
			"id", p.ID,
			"kind", p.Kind,
			"kind_label", kindLabel,
			"sku_name", nullableStr(p.SKUName),
			"status", status,
			"expires_at", nullableISO(p.ExpiresAt),
		))
	}
	return out
}

// ---- 校验 ----

// validateUser 对齐 UserController::validateUser。
//
// email 走 Laravel `email` 规则（共享 validate.go 未实现该规则，这里单独校验并按
// 声明顺序排在最前，保证 summary 的第一条文案与 PHP 一致）。
func (d *UserDomain) validateUser(c *gin.Context) (map[string]any, string, bool) {
	data := jsonInput(c)
	errs := []string{}

	if raw, ok := data["email"]; ok && raw != nil {
		if s, ok2 := inputString(raw); ok2 && !emailValid(s) {
			errs = append(errs, "The email must be a valid email address.")
		}
	}

	v := newValidator(ctxOf(c), data).existsChecker(d.userExistsChecker())
	v.field("email", "nullable") // 占位：email 语义在上面单独校验
	v.field("protocol", "sometimes", "in:vless,trojan")
	v.field("plan_id", "nullable", "integer", "exists:plans,id")
	v.field("traffic_limit", "sometimes", "integer", "min:0")
	v.field("traffic_used", "sometimes", "integer", "min:0")
	v.field("monthly_traffic_used", "sometimes", "integer", "min:0")
	v.field("monthly_traffic_limit", "sometimes", "integer", "min:0")
	v.field("expired_at", "sometimes", "nullable", "date")
	v.field("enabled", "sometimes", "boolean")

	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.logger.Printf("admin 用户校验时查库失败: %v", err)
			return nil, "服务器错误", false
		}
		errs = append(errs, v.summarySplit()...)
	}
	if len(errs) > 0 {
		return nil, validateSummary(errs), false
	}
	return data, "", true
}

// summarySplit 把 validator 的合并文案拆回逐条（便于与 email 错误合并后重新 summarize）。
func (v *validator) summarySplit() []string {
	out := make([]string, 0, len(v.errs))
	for _, e := range v.errs {
		out = append(out, e.Msg)
	}
	return out
}

// userExistsChecker 给校验器用。
func (d *UserDomain) userExistsChecker() func(table, column string, value any) (bool, error) {
	return func(table, column string, value any) (bool, error) {
		if table != "plans" {
			return false, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return d.users.PlanExists(ctx, uint64(inputInt(value)))
	}
}

// ---- handlers ----

// userIndex 对齐 UserController::index：50/页 + 全表搜索（ID / 邮箱 / Token）。
func (d *UserDomain) userIndex(c *gin.Context) {
	search := ""
	if s := queryString(c, "search"); s != nil {
		search = phpTrimSpaceOnly(*s)
	}
	page := pageParam(c)
	rows, total, err := d.users.UserPage(ctxOf(c), search, page, userPagePerPage, d.now())
	if err != nil {
		d.failServer(c, "查询用户", err)
		return
	}
	lastPage := (total + userPagePerPage - 1) / userPagePerPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page > lastPage {
		page = lastPage
		rows, total, err = d.users.UserPage(ctxOf(c), search, page, userPagePerPage, d.now())
		if err != nil {
			d.failServer(c, "查询用户", err)
			return
		}
	}
	items := make([]any, 0, len(rows))
	for i := range rows {
		items = append(items, presentUser(&rows[i], false, nil))
	}
	d.successPage(c, items, page, userPagePerPage, total)
}

// userShow 对齐 UserController::show（full=true，带 proxies 明细）。
func (d *UserDomain) userShow(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	proxies, err := d.users.UserProxies(ctxOf(c), u.ID)
	if err != nil {
		d.failServer(c, "查询代理权益", err)
		return
	}
	d.success(c, presentUser(u, true, presentProxies(proxies, d.now())))
}

// userStore 对齐 UserController::store + UserAdminService::create。
func (d *UserDomain) userStore(c *gin.Context) {
	data, summary, ok := d.validateUser(c)
	if !ok {
		d.failValidation(c, summary)
		return
	}
	now := d.now()

	in := UserCreateInput{
		Enabled: true,
		Now:     now,
	}
	if raw, has := data["email"]; has && raw != nil {
		if s, ok2 := inputString(raw); ok2 {
			in.Email = &s
		}
	}
	in.Token = "sub_" + randomToken(32)
	in.UUID = randomUUID()
	in.Protocol = "vless"
	if raw, has := data["protocol"]; has && raw != nil {
		if s, ok2 := inputString(raw); ok2 {
			in.Protocol = s
		}
	}
	if raw, has := data["plan_id"]; has && raw != nil {
		id := uint64(inputInt(raw))
		in.PlanID = &id
	}
	if raw, has := data["enabled"]; has && raw != nil {
		in.Enabled = inputBool(raw)
	}

	// 套餐联动：自动填充流量和到期时间（UserAdminService::create 的口径）。
	// 注意：PHP 侧 plan 分支会**覆盖**前面从 $data 取的 traffic_limit / expired_at，
	// 所以这里先写显式值，再在 plan 命中时整体替换。
	if raw, has := data["traffic_limit"]; has && raw != nil {
		in.TrafficLimit = inputInt(raw)
	}
	if raw, has := data["monthly_traffic_limit"]; has && raw != nil {
		in.MonthlyTrafficLimit = inputInt(raw)
	}
	if raw, has := data["expired_at"]; has {
		if raw == nil {
			in.ExpiredAt = nil
		} else if s, ok2 := inputString(raw); ok2 {
			in.ExpiredAt = parseDateTimeLenient(s)
		}
	}
	if in.PlanID != nil {
		plan, err := d.users.UserPlanByID(ctxOf(c), *in.PlanID)
		if err == nil && plan != nil {
			if plan.Type == "period" {
				in.TrafficLimit = derefInt64(plan.PeriodTraffic)
				in.MonthlyTrafficLimit = derefInt64(plan.MonthlyTraffic)
				exp := now.AddDate(0, int(derefInt64(plan.Months)), 0)
				if parsed := parseDateOnly(dateOnly(exp)); parsed != nil {
					in.ExpiredAt = parsed
				}
				next := now.AddDate(0, 0, 30)
				in.NextTrafficResetAt = &next
			} else {
				in.TrafficLimit = derefInt64(plan.TotalTraffic)
				in.MonthlyTrafficLimit = 0
				in.ExpiredAt = nil
			}
		}
	}

	created, err := d.users.CreateUser(ctxOf(c), in)
	if err != nil {
		d.failServer(c, "创建用户", err)
		return
	}
	// 建号后异步 provision（PHP 的 dispatchProvisionClient 失败只记日志）。
	if err := d.ops.ProvisionUser(ctxOf(c), created); err != nil {
		d.logger.Printf("admin 创建用户后 provision 失败（不影响建号）: %v", err)
	}
	full, err := d.users.UserByID(ctxOf(c), created.ID)
	if err != nil {
		full = created
	}
	proxies, _ := d.users.UserProxies(ctxOf(c), full.ID)
	d.success(c, presentUser(full, true, presentProxies(proxies, now)), "创建成功")
}

// userUpdate 对齐 UserController::update（含套餐切换的 applyPlan + 面板开关）。
func (d *UserDomain) userUpdate(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	data, summary, ok := d.validateUser(c)
	if !ok {
		d.failValidation(c, summary)
		return
	}
	now := d.now()

	oldPlanID := u.PlanID
	newPlanID := oldPlanID
	if raw, has := data["plan_id"]; has {
		if raw == nil {
			newPlanID = nil
		} else {
			id := uint64(inputInt(raw))
			newPlanID = &id
		}
	}

	patch := UserPatch{Now: now}
	if raw, has := data["email"]; has {
		if raw == nil {
			// PHP `$data['email'] ?? $user->email`：显式 null 也回落旧值。
			patch.Email = u.Email
			patch.EmailSet = true
		} else if s, ok2 := inputString(raw); ok2 {
			patch.Email = &s
			patch.EmailSet = true
		}
	}
	if raw, has := data["protocol"]; has {
		if raw == nil {
			p := u.Protocol
			patch.Protocol = &p
			patch.ProtocolSet = true
		} else if s, ok2 := inputString(raw); ok2 {
			patch.Protocol = &s
			patch.ProtocolSet = true
		}
	}
	patch.PlanID = newPlanID
	patch.PlanIDSet = true

	if raw, has := data["traffic_limit"]; has && raw != nil {
		n := inputInt(raw)
		patch.TrafficLimit = &n
		patch.TrafficLimitSet = true
	}
	if raw, has := data["traffic_used"]; has && raw != nil {
		n := inputInt(raw)
		patch.TrafficUsed = &n
		patch.TrafficUsedSet = true
	}
	if raw, has := data["monthly_traffic_used"]; has && raw != nil {
		n := inputInt(raw)
		patch.MonthlyTrafficUsed = &n
		patch.MonthlyUsedSet = true
	}
	if raw, has := data["monthly_traffic_limit"]; has && raw != nil {
		n := inputInt(raw)
		patch.MonthlyTrafficLimit = &n
		patch.MonthlyLimitSet = true
	}
	if raw, has := data["expired_at"]; has {
		if raw == nil {
			patch.ExpiredAt = nil
			patch.ExpiredAtSet = true
		} else if s, ok2 := inputString(raw); ok2 {
			patch.ExpiredAt = parseDateTimeLenient(s)
			patch.ExpiredAtSet = true
		}
	}
	enabledTouched := false
	if raw, has := data["enabled"]; has && raw != nil {
		b := inputBool(raw)
		patch.Enabled = &b
		patch.EnabledSet = true
		enabledTouched = true
	}

	if err := d.users.UpdateUser(ctxOf(c), u.ID, patch); err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "更新用户", err)
		return
	}

	full, err := d.users.UserByID(ctxOf(c), u.ID)
	if err != nil {
		d.failServer(c, "读取用户", err)
		return
	}

	planChanged := !samePlanID(oldPlanID, newPlanID)
	if planChanged {
		// 切换套餐：重算流量/到期（同步），节点侧 client 同步（PHP 走后台 Job，
		// 这里走 ops 接缝；失败只记日志）。
		if err := d.applyPlan(c, u.ID); err != nil {
			d.failServer(c, "切换套餐", err)
			return
		}
		after, err := d.users.UserByID(ctxOf(c), u.ID)
		if err == nil && after != nil {
			full = after
		}
		// ⚠️ 必须下发「重读出来的完整行」。这里以前手搓 UserRow{ID,Email,UUID,Protocol}，
		// PlanID=nil、Enabled=false、TrafficLimit=0 全是零值，provision 于是把每台
		// 3x-ui 节点上的 client 写成 enable=false + totalGB=0 —— 后台给人换套餐，
		// 反而把已经连不上的人写得更死（线上 ch_user_84 在 JP1/US1/US3/MYKL1 四台全中）。
		if err := d.ops.ProvisionUser(ctxOf(c), full); err != nil {
			d.logger.Printf("admin 切换套餐后 provision 失败: %v", err)
		}
		// 兜底：换套餐本身就是「让这个人重新可用」的明确动作，顺手按面板意图开一次。
		// collector 语义会做 3x-ui 整条替换前的归一化，并在全部节点确认后清掉
		// users.traffic_disabled_at —— 标记不清，xray 节点的配置渲染和对账任务
		// 都还把他当「已因流量关闭」，等于白开。
		if full.Enabled && full.PlanID != nil {
			_ = d.ops.SetClientEnabled(ctxOf(c), full, true)
		}
	}
	if enabledTouched {
		// 手动改 enabled 时同步到节点（BanService::toggleClient）。
		_ = d.ops.SetClientEnabled(ctxOf(c), full, *patch.Enabled)
	}

	proxies, _ := d.users.UserProxies(ctxOf(c), full.ID)
	d.success(c, presentUser(full, true, presentProxies(proxies, now)), "更新成功")
}

// userDestroy 对齐 UserController::destroy：先按 clientEmail 全量删面板 client，再删行。
func (d *UserDomain) userDestroy(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	// 3x-ui 按 email 全量删除 client（包含所有入站挂载）；节点离线忽略。
	if err := d.ops.DeleteUserClients(ctxOf(c), u); err != nil {
		d.logger.Printf("admin 删除用户 %d 的面板 client 失败（忽略）: %v", u.ID, err)
	}
	if err := d.users.DeleteUser(ctxOf(c), u.ID); err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "删除用户", err)
		return
	}
	d.success(c, nil, "已删除")
}

// userResetTraffic 对齐 UserController::resetTraffic + UserAdminService::resetTraffic。
//
// 铁律（behavior-contract §3）：只清零已用流量，不动 next_traffic_reset_at 和套餐时长。
func (d *UserDomain) userResetTraffic(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	if err := d.resetTraffic(c, u); err != nil {
		d.failServer(c, "重置流量", err)
		return
	}
	full, err := d.users.UserByID(ctxOf(c), u.ID)
	if err != nil {
		d.failServer(c, "读取用户", err)
		return
	}
	proxies, _ := d.users.UserProxies(ctxOf(c), full.ID)
	d.success(c, presentUser(full, true, presentProxies(proxies, d.now())), "流量已重置")
}

// userResetPassword 对齐 UserController::resetPassword：改密 + 作废全部登录态。
func (d *UserDomain) userResetPassword(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	data := jsonInput(c)
	errs := []string{}
	pass, hasPass := inputString(data["password"])
	if !hasPass || data["password"] == nil || pass == "" {
		errs = append(errs, "The password field is required.")
	} else if phpMbStrlen(pass) < 6 {
		errs = append(errs, "The password field must be at least 6 characters.")
	}
	if len(errs) > 0 {
		d.failValidation(c, validateSummary(errs))
		return
	}

	hash, err := bcryptHash(pass, d.cfg.BcryptRounds)
	if err != nil {
		d.failServer(c, "生成密码哈希", err)
		return
	}
	patch := UserPatch{Password: &hash, PasswordSet: true, Now: d.now()}
	if err := d.users.UpdateUser(ctxOf(c), u.ID, patch); err != nil {
		d.failServer(c, "重置密码", err)
		return
	}
	if err := d.users.DeleteAccessTokens(ctxOf(c), u.ID); err != nil {
		d.failServer(c, "作废登录态", err)
		return
	}
	d.success(c, nil, "密码已重置")
}

// userRenew 对齐 UserController::renew + UserAdminService::renew（仅总量套餐）。
func (d *UserDomain) userRenew(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	if !u.IsTotalPlan() {
		d.fail(c, "仅总量套餐支持续费", 400)
		return
	}
	if err := d.renew(c, u); err != nil {
		d.failServer(c, "续费", err)
		return
	}
	full, err := d.users.UserByID(ctxOf(c), u.ID)
	if err != nil {
		d.failServer(c, "读取用户", err)
		return
	}
	proxies, _ := d.users.UserProxies(ctxOf(c), full.ID)
	d.success(c, presentUser(full, true, presentProxies(proxies, d.now())), "续费成功")
}

// userSwitchProtocol 对齐 UserController::switchProtocol。
func (d *UserDomain) userSwitchProtocol(c *gin.Context) {
	u, ok := d.loadUser(c)
	if !ok {
		return
	}
	data := jsonInput(c)
	proto, has := inputString(data["protocol"])
	if !has || data["protocol"] == nil || (proto != "vless" && proto != "trojan") {
		if data["protocol"] == nil || proto == "" {
			d.failValidation(c, "The protocol field is required.")
			return
		}
		d.failValidation(c, "The selected protocol is invalid.")
		return
	}

	if err := d.ops.SwitchProtocol(ctxOf(c), u, proto); err != nil {
		var dom *userDomainError
		if asUserDomainError(err, &dom) {
			d.fail(c, dom.msg, 400)
			return
		}
		d.logger.Printf("admin 切换协议失败: %v", err)
		d.fail(c, "协议切换失败："+err.Error(), 500)
		return
	}
	// protocol 列由调用方在成功后落库（与 P2 PanelSwitcher 的分工一致）。
	p := proto
	patch := UserPatch{Protocol: &p, ProtocolSet: true, Now: d.now()}
	if err := d.users.UpdateUser(ctxOf(c), u.ID, patch); err != nil {
		d.failServer(c, "落库协议", err)
		return
	}
	full, err := d.users.UserByID(ctxOf(c), u.ID)
	if err != nil {
		d.failServer(c, "读取用户", err)
		return
	}
	proxies, _ := d.users.UserProxies(ctxOf(c), full.ID)
	d.success(c, presentUser(full, true, presentProxies(proxies, d.now())), "切换成功")
}

// ---- 业务（UserAdminService）----

// applyPlan 对齐 UserAdminService::applyPlan：重算流量限额与到期时间。
// 无套餐 → 清零限额、永不过期；周期套餐 → 30 天后重置；总量套餐 → 只给总量。
func (d *UserDomain) applyPlan(c *gin.Context, userID uint64) error {
	// MailNotifyService::restoreReminders 属于邮件域（P8c），这里通过 ops 之外的空实现跳过：
	// PHP 侧也只是清防重标记，不影响本次接口的落库结果。
	u, err := d.users.UserByID(ctxOf(c), userID)
	if err != nil {
		return err
	}
	now := d.now()
	patch := UserPatch{Now: now}

	if u.PlanID == nil {
		zero := int64(0)
		patch.TrafficLimit = &zero
		patch.TrafficLimitSet = true
		patch.TrafficUsed = &zero
		patch.TrafficUsedSet = true
		patch.MonthlyTrafficUsed = &zero
		patch.MonthlyUsedSet = true
		patch.MonthlyTrafficLimit = &zero
		patch.MonthlyLimitSet = true
		patch.NextTrafficResetAt = nil
		patch.NextResetSet = true
		patch.ExpiredAt = nil
		patch.ExpiredAtSet = true
		if err := d.users.UpdateUser(ctxOf(c), userID, patch); err != nil {
			return err
		}
		return d.ops.ResetClientTraffic(ctxOf(c), u)
	}

	plan, err := d.users.UserPlanByID(ctxOf(c), *u.PlanID)
	if err != nil {
		return err
	}
	if plan.Type == "period" {
		traffic := derefInt64(plan.PeriodTraffic)
		monthly := derefInt64(plan.MonthlyTraffic)
		months := derefInt64(plan.Months)
		exp := now.AddDate(0, int(months), 0)
		expStr := dateOnly(exp)
		next := now.AddDate(0, 0, 30)
		patch.TrafficLimit = &traffic
		patch.TrafficLimitSet = true
		zero := int64(0)
		patch.TrafficUsed = &zero
		patch.TrafficUsedSet = true
		patch.MonthlyTrafficUsed = &zero
		patch.MonthlyUsedSet = true
		patch.MonthlyTrafficLimit = &monthly
		patch.MonthlyLimitSet = true
		if parsed := parseDateOnly(expStr); parsed != nil {
			patch.ExpiredAt = parsed
			patch.ExpiredAtSet = true
		}
		patch.NextTrafficResetAt = &next
		patch.NextResetSet = true
	} else {
		traffic := derefInt64(plan.TotalTraffic)
		zero := int64(0)
		patch.TrafficLimit = &traffic
		patch.TrafficLimitSet = true
		patch.TrafficUsed = &zero
		patch.TrafficUsedSet = true
		patch.MonthlyTrafficUsed = &zero
		patch.MonthlyUsedSet = true
		patch.MonthlyTrafficLimit = &zero
		patch.MonthlyLimitSet = true
		patch.ExpiredAt = nil
		patch.ExpiredAtSet = true
	}
	if err := d.users.UpdateUser(ctxOf(c), userID, patch); err != nil {
		return err
	}
	// 重置 3x-ui 流量统计
	_ = u
	fresh, _ := d.users.UserByID(ctxOf(c), userID)
	if fresh == nil {
		fresh = u
	}
	return d.ops.ResetClientTraffic(ctxOf(c), fresh)
}

// resetTraffic 对齐 UserAdminService::resetTraffic：只清零已用流量。
// 周期套餐：monthly_traffic_used 清零 + traffic_limit 加月流量额度；总量套餐：traffic_used 清零。
func (d *UserDomain) resetTraffic(c *gin.Context, u *UserRow) error {
	now := d.now()
	patch := UserPatch{Now: now}
	if u.IsPeriodPlan() {
		zero := int64(0)
		add := u.MonthlyTrafficLimit
		limit := u.TrafficLimit + add
		patch.MonthlyTrafficUsed = &zero
		patch.MonthlyUsedSet = true
		patch.TrafficLimit = &limit
		patch.TrafficLimitSet = true
	} else {
		zero := int64(0)
		patch.TrafficUsed = &zero
		patch.TrafficUsedSet = true
	}
	if err := d.users.UpdateUser(ctxOf(c), u.ID, patch); err != nil {
		return err
	}
	fresh, err := d.users.UserByID(ctxOf(c), u.ID)
	if err != nil {
		return err
	}
	// 同步新的流量限制到面板 + 重置流量计数 + 打开连接
	if err := d.ops.ProvisionUser(ctxOf(c), fresh); err != nil {
		d.logger.Printf("admin 重置流量 provision 失败: %v", err)
	}
	if err := d.ops.ResetClientTraffic(ctxOf(c), fresh); err != nil {
		d.logger.Printf("admin 重置流量 resetClient 失败: %v", err)
	}
	_ = d.ops.SetClientEnabled(ctxOf(c), fresh, true)
	return nil
}

// renew 对齐 UserAdminService::renew：重置总流量 + 重新启用。
func (d *UserDomain) renew(c *gin.Context, u *UserRow) error {
	now := d.now()
	zero := int64(0)
	en := true
	patch := UserPatch{
		TrafficUsed:        &zero,
		TrafficUsedSet:     true,
		MonthlyTrafficUsed: &zero,
		MonthlyUsedSet:     true,
		Enabled:            &en,
		EnabledSet:         true,
		Now:                now,
	}
	if err := d.users.UpdateUser(ctxOf(c), u.ID, patch); err != nil {
		return err
	}
	fresh, err := d.users.UserByID(ctxOf(c), u.ID)
	if err != nil {
		return err
	}
	if err := d.ops.ResetClientTraffic(ctxOf(c), fresh); err != nil {
		d.logger.Printf("admin 续费 resetClient 失败: %v", err)
	}
	// 重置计数不等于重新启用：统一再走一次开关（issue #51）。
	_ = d.ops.SetClientEnabled(ctxOf(c), fresh, true)
	return nil
}

// ---- 小工具 ----

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func samePlanID(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func asUserDomainError(err error, target **userDomainError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*userDomainError); ok {
		*target = e
		return true
	}
	return false
}
