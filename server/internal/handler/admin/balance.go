package admin

// 管理端余额域的 HTTP 入口（路由清单见 balance_models.go 顶部注释）。
//
// 响应口径与其它管理端一致：HTTP 200 + {code, msg, data}（PHP ApiResponse 形态）。
// 本域是 Go 侧新增功能（没有 PHP 控制器可对拍），文案用中文直给管理员。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

const (
	// giftcardMaxCount 是单次批量生成的上限（规划 §3.2.1：count 1-100）。
	giftcardMaxCount = 100
	// giftcardBytes 是码体的随机字节数：8 字节 → 16 位 hex → XXXX-XXXX-XXXX-XXXX
	// （19 字符，落在 giftcards.code varchar(32) 内）。
	giftcardBytes = 8
	// balanceLogPerPage 是流水默认每页条数（最多 balanceLogMaxPerPage）。
	balanceLogPerPage    = 20
	balanceLogMaxPerPage = 100
	// giftcardPerPage 是兑换码列表默认每页条数。
	giftcardPerPage    = 20
	giftcardMaxPerPage = 100
)

// Register 把余额域路由挂到给定分组（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
//
// 路径参数名统一用 :user / :giftcard —— Gin 同一层级只允许一种参数名，
// 而 /users/:user/... 已经被用户域占用，这里必须与之一致。
func (d *BalanceDomain) Register(r gin.IRouter) {
	r.GET("/giftcards", d.giftcardIndex)
	r.POST("/giftcards", d.giftcardStore)
	r.DELETE("/giftcards/:giftcard/delete", d.giftcardDelete)
	r.POST("/giftcards/batch-delete", d.giftcardBatchDelete)

	// 兑换码文件夹（对齐优惠码 discount-code-folders 的语义与交互）
	r.POST("/giftcard-folders", d.giftcardFolderCreate)
	r.PUT("/giftcard-folders/:folder", d.giftcardFolderUpdate)
	r.DELETE("/giftcard-folders/:folder", d.giftcardFolderDelete)

	r.GET("/users/:user/balance", d.userBalanceShow)
	r.POST("/users/:user/balance", d.userBalanceAdjust)
}

// ---- 响应封装（PHP ApiResponse 口径）----

func (d *BalanceDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *BalanceDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *BalanceDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// ---- 兑换码 ----

// giftcardIndex 对齐 GET /admin-api/giftcards?status=&folder=&value_order=&page=&per_page=。
//
// data = {list, total, page, per_page, folders}（folders 始终返回全部文件夹，供前端渲染文件夹行）。
//
// 归档口径与优惠码一致：
//   - 不传 folder → 只出未归档码（folder_id = null）
//   - 传 folder=id → 只出该文件夹内的码
//
// value_order = asc | desc 控制「面值」列排序；不传 = 按 id 倒序（恢复默认）。
func (d *BalanceDomain) giftcardIndex(c *gin.Context) {
	var filter GiftcardFilter
	if status := queryString(c, "status"); status != nil {
		if !isGiftcardStatus(*status) {
			d.fail(c, "状态筛选不合法", 400)
			return
		}
		filter.Status = status
	}
	if raw := queryString(c, "folder"); raw != nil && *raw != "" {
		fid, ok := parseUint(*raw)
		if !ok {
			d.fail(c, "文件夹不存在", 400)
			return
		}
		filter.FolderID = &fid
	}
	if raw := queryString(c, "value_order"); raw != nil {
		v := strings.ToLower(strings.TrimSpace(*raw))
		if v != "" && v != "asc" && v != "desc" {
			d.fail(c, "面值排序不合法", 400)
			return
		}
		filter.ValueOrder = v
	}
	page, perPage := pageParams(c, giftcardPerPage, giftcardMaxPerPage)

	rows, total, err := d.store.GiftcardPage(ctxOf(c), filter, page, perPage)
	if err != nil {
		d.failServer(c, "查询兑换码", err)
		return
	}
	list := make(jsArr, 0, len(rows))
	for i := range rows {
		list = append(list, presentGiftcard(&rows[i]))
	}
	// 文件夹行始终返回全部（与是否传 folder 无关），前端用它渲染上方文件夹列表。
	folderCounts, err := d.store.GiftcardFolderCounts(ctxOf(c), filter)
	if err != nil {
		d.failServer(c, "查询兑换码文件夹", err)
		return
	}
	folders := make(jsArr, 0, len(folderCounts))
	for _, f := range folderCounts {
		folders = append(folders, jo(
			"id", f.ID,
			"name", f.Name,
			"code_count", f.CodeCount,
		))
	}
	d.success(c, jo(
		"list", list,
		"total", total,
		"page", page,
		"per_page", perPage,
		"folders", folders,
	))
}

// giftcardStore 对齐 POST /admin-api/giftcards。
//
// 请求：{count, value, name?, note?, expires_at?}；value 单位分。
// 响应：{count, codes:[...]}，codes 是**本次真正落库**的码（撞车重生成后也一致）。
func (d *BalanceDomain) giftcardStore(c *gin.Context) {
	input := jsonInput(c)

	count := int64(1)
	if raw, ok := input["count"]; ok && raw != nil {
		n, ok := balanceIntOf(raw)
		if !ok {
			d.fail(c, "生成数量必须是整数", 400)
			return
		}
		count = n
	}
	if count < 1 || count > giftcardMaxCount {
		d.fail(c, "生成数量需在 1-"+strconv.FormatInt(giftcardMaxCount, 10)+" 之间", 400)
		return
	}

	rawValue, ok := input["value"]
	if !ok || rawValue == nil {
		d.fail(c, "请填写面值", 400)
		return
	}
	value, ok := balanceIntOf(rawValue)
	if !ok {
		d.fail(c, "面值必须是整数（单位：分）", 400)
		return
	}
	if value <= 0 {
		d.fail(c, "面值必须大于 0", 400)
		return
	}

	name := ""
	if raw, ok := input["name"]; ok && raw != nil {
		s, _ := inputString(raw)
		if utf8.RuneCountInString(s) > 255 {
			d.fail(c, "名称不能超过 255 个字符", 400)
			return
		}
		name = s
	}
	var note *string
	if raw, ok := input["note"]; ok && raw != nil {
		s, _ := inputString(raw)
		if utf8.RuneCountInString(s) > 255 {
			d.fail(c, "备注不能超过 255 个字符", 400)
			return
		}
		note = &s
	}

	now := d.now().UTC().Truncate(time.Second)
	var expiresAt *time.Time
	if raw, ok := input["expires_at"]; ok && raw != nil {
		s, _ := inputString(raw)
		parsed := parseDateTimeLenient(s)
		if parsed == nil {
			d.fail(c, "过期时间格式不正确（示例：2026-12-31 23:59:59）", 400)
			return
		}
		if !parsed.After(now) {
			d.fail(c, "过期时间必须晚于当前时间", 400)
			return
		}
		expiresAt = parsed
	}

	operatorID := adminIDOf(c)
	// 归档文件夹：folder_id 优先；否则按 folder_name 找，不存在就建（与优惠码同一口径）。
	folderID, err := d.resolveGiftcardFolderID(ctxOf(c), input)
	if err != nil {
		d.failServer(c, "解析兑换码文件夹", err)
		return
	}
	var codes []string
	var lastErr error
	// 撞唯一键就整批重生成：8 字节随机撞车概率极低，但仍不允许把「生成了 5 个、
	// 只写进 3 个」的半批数据返回给管理员，所以每轮都是「生成 → 整批落库」。
	for attempt := 0; attempt < 5; attempt++ {
		codes = generateGiftcardCodes(count)
		rows := make([]NewGiftcard, 0, len(codes))
		for _, code := range codes {
			rows = append(rows, NewGiftcard{
				Code:      code,
				Name:      name,
				Value:     value,
				ExpiresAt: expiresAt,
				CreatedBy: operatorID,
				Note:      note,
				FolderID:  folderID,
				CreatedAt: now,
			})
		}
		lastErr = d.store.InsertGiftcards(ctxOf(c), rows)
		if lastErr == nil {
			break
		}
		if !errors.Is(lastErr, ErrGiftcardCodeTaken) {
			break
		}
	}
	if lastErr != nil {
		d.failServer(c, "写入兑换码", lastErr)
		return
	}

	codeArr := make(jsArr, 0, len(codes))
	for _, code := range codes {
		codeArr = append(codeArr, code)
	}
	d.success(c, jo("count", len(codes), "codes", codeArr),
		"已生成 "+strconv.Itoa(len(codes))+" 个兑换码")
}

// giftcardDelete 对齐 DELETE /admin-api/giftcards/{giftcard}/delete：物理删除兑换码。
// 与「作废」分开：作废是失效留档，删除是彻底移除（含已使用/已作废的码），
// 用于管理员清理误发的码或测试数据；删除不留余额流水（余额已按兑换时入账）。
func (d *BalanceDomain) giftcardDelete(c *gin.Context) {
	id, ok := parseUint(c.Param("giftcard"))
	if !ok {
		d.fail(c, "兑换码不存在", 404)
		return
	}
	deleted, err := d.store.DeleteGiftcard(ctxOf(c), id)
	if err != nil {
		d.failServer(c, "删除兑换码", err)
		return
	}
	if !deleted {
		d.fail(c, "兑换码不存在", 404)
		return
	}
	d.success(c, nil, "已删除")
}

// giftcardBatchDelete 对齐 POST /admin-api/giftcards/batch-delete（body: {ids}）。
// 与折扣码批量删除同口径：逐条物理删除，不存在的归 failed(not_found)，单条失败不影响其余。
func (d *BalanceDomain) giftcardBatchDelete(c *gin.Context) {
	input := jsonInput(c)
	raw, ok := input["ids"].([]any)
	if !ok || len(raw) == 0 {
		d.fail(c, "请选择要删除的兑换码", 400)
		return
	}
	if len(raw) > 500 {
		d.fail(c, "单次最多删除 500 个", 400)
		return
	}
	ids := make([]uint64, 0, len(raw))
	seen := map[uint64]struct{}{}
	for _, item := range raw {
		id := uint64(inputInt(item))
		if id < 1 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	deleted := make(jsArr, 0, len(ids))
	failed := make(jsArr, 0)
	for _, id := range ids {
		okDel, err := d.store.DeleteGiftcard(ctxOf(c), id)
		if err != nil {
			failed = append(failed, jo("id", id, "reason", err.Error()))
			continue
		}
		if !okDel {
			failed = append(failed, jo("id", id, "reason", "not_found"))
			continue
		}
		deleted = append(deleted, id)
	}
	d.success(c, jo("deleted", deleted, "failed", failed),
		"已删除 "+strconv.Itoa(len(deleted))+" 个兑换码")
}

// ---- 兑换码文件夹 ----

// giftcardFolderCreate 对齐 POST /admin-api/giftcard-folders（body: {name}）。
func (d *BalanceDomain) giftcardFolderCreate(c *gin.Context) {
	input := jsonInput(c)
	name, ok := inputString(input["name"])
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		d.fail(c, "请填写文件夹名", 400)
		return
	}
	if utf8.RuneCountInString(name) > 60 {
		d.fail(c, "文件夹名不能超过 60 个字符", 400)
		return
	}
	f, err := d.store.GiftcardFolderCreate(ctxOf(c), name)
	if err != nil {
		d.failServer(c, "创建兑换码文件夹", err)
		return
	}
	d.success(c, jo("id", f.ID, "name", f.Name, "code_count", 0), "已创建")
}

// giftcardFolderUpdate 对齐 PUT /admin-api/giftcard-folders/{folder}（改名）。
func (d *BalanceDomain) giftcardFolderUpdate(c *gin.Context) {
	folder, ok := d.loadGiftcardFolder(c)
	if !ok {
		return
	}
	input := jsonInput(c)
	name, okName := inputString(input["name"])
	name = strings.TrimSpace(name)
	if !okName || name == "" {
		d.fail(c, "请填写文件夹名", 400)
		return
	}
	if utf8.RuneCountInString(name) > 60 {
		d.fail(c, "文件夹名不能超过 60 个字符", 400)
		return
	}
	if err := d.store.GiftcardFolderUpdate(ctxOf(c), folder.ID, name); err != nil {
		d.failServer(c, "保存兑换码文件夹", err)
		return
	}
	d.success(c, nil, "保存成功")
}

// giftcardFolderDelete 对齐 DELETE /admin-api/giftcard-folders/{folder}[?with_codes=1]。
//
// 默认（对齐优惠码）：里面所有码退回「未归档」，码一条都不删。
// with_codes=1：连同文件夹内的码一起删除（需二次确认，前端负责）。
func (d *BalanceDomain) giftcardFolderDelete(c *gin.Context) {
	folder, ok := d.loadGiftcardFolder(c)
	if !ok {
		return
	}

	if inputBool(queryRaw(c, "with_codes")) {
		ids, err := d.store.GiftcardIDsInFolder(ctxOf(c), folder.ID)
		if err != nil {
			d.failServer(c, "查询文件夹内兑换码", err)
			return
		}
		deleted := 0
		failed := make(jsArr, 0)
		for _, id := range ids {
			if _, err := d.store.DeleteGiftcard(ctxOf(c), id); err != nil {
				failed = append(failed, jo("id", id, "reason", err.Error()))
				continue
			}
			deleted++
		}
		// 没删掉的不能留成孤儿 folder_id：退回未归档。
		if len(failed) > 0 {
			if err := d.store.DetachGiftcardsFromFolder(ctxOf(c), folder.ID); err != nil {
				d.failServer(c, "摘除文件夹内兑换码", err)
				return
			}
		}
		if err := d.store.GiftcardFolderDelete(ctxOf(c), folder.ID); err != nil {
			d.failServer(c, "删除兑换码文件夹", err)
			return
		}
		var msg string
		if len(failed) == 0 {
			msg = "已删除文件夹及其中 " + strconv.Itoa(deleted) + " 个兑换码"
		} else {
			msg = "已删除文件夹，" + strconv.Itoa(deleted) + " 个码已删，" +
				strconv.Itoa(len(failed)) + " 个删除失败已转为未归档"
		}
		d.success(c, jo("deleted_codes", deleted, "failed", failed), msg)
		return
	}

	// 先摘码再删文件夹：删到一半失败时，码只会多出一批「未归档」，
	// 不会出现指向已删文件夹的孤儿 folder_id。
	if err := d.store.DetachGiftcardsFromFolder(ctxOf(c), folder.ID); err != nil {
		d.failServer(c, "摘除文件夹内兑换码", err)
		return
	}
	if err := d.store.GiftcardFolderDelete(ctxOf(c), folder.ID); err != nil {
		d.failServer(c, "删除兑换码文件夹", err)
		return
	}
	d.success(c, nil, "已删除文件夹，其中兑换码已转为未归档")
}

// loadGiftcardFolder 解析路径参数 :folder，不存在时回 404。
func (d *BalanceDomain) loadGiftcardFolder(c *gin.Context) (*GiftcardFolder, bool) {
	id, ok := parseUint(c.Param("folder"))
	if !ok {
		d.fail(c, "文件夹不存在", 404)
		return nil, false
	}
	f, err := d.store.GiftcardFolderByID(ctxOf(c), id)
	if err != nil {
		if NoRows(err) {
			d.fail(c, "文件夹不存在", 404)
			return nil, false
		}
		d.failServer(c, "查询兑换码文件夹", err)
		return nil, false
	}
	return f, true
}

// resolveGiftcardFolderID 解析生成时指定的归档文件夹：folder_id 优先；
// 否则按 folder_name 找，不存在就建（与优惠码 resolveFolderID 同一口径）。
func (d *BalanceDomain) resolveGiftcardFolderID(ctx context.Context, input map[string]any) (*uint64, error) {
	if val, ok := input["folder_id"]; ok && val != nil {
		id := uint64(inputInt(val))
		if id > 0 {
			return &id, nil
		}
	}
	name := ""
	if val, ok := input["folder_name"]; ok && val != nil {
		s, _ := inputString(val)
		name = strings.TrimSpace(s)
	}
	if name == "" {
		return nil, nil
	}
	f, err := d.store.GiftcardFolderByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if f != nil {
		return &f.ID, nil
	}
	created, err := d.store.GiftcardFolderCreate(ctx, name)
	if err != nil {
		return nil, err
	}
	return &created.ID, nil
}

// ---- 用户余额 ----

// userBalanceShow 对齐 GET /admin-api/users/{user}/balance。
//
// data = {user_id, balance, logs, total, page, per_page}（金额单位分）。
func (d *BalanceDomain) userBalanceShow(c *gin.Context) {
	userID, ok := parseUint(c.Param("user"))
	if !ok {
		d.fail(c, "用户不存在", 404)
		return
	}
	page, perPage := pageParams(c, balanceLogPerPage, balanceLogMaxPerPage)

	balance, err := d.store.UserBalance(ctxOf(c), userID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) || NoRows(err) {
			d.fail(c, "用户不存在", 404)
			return
		}
		d.failServer(c, "查询用户余额", err)
		return
	}
	logs, total, err := d.store.UserBalanceLogs(ctxOf(c), userID, page, perPage)
	if err != nil {
		d.failServer(c, "查询余额流水", err)
		return
	}
	list := make(jsArr, 0, len(logs))
	for i := range logs {
		list = append(list, presentBalanceLog(&logs[i]))
	}
	d.success(c, jo(
		"user_id", userID,
		"balance", balance,
		"logs", list,
		"total", total,
		"page", page,
		"per_page", perPage,
	))
}

// userBalanceAdjust 对齐 POST /admin-api/users/{user}/balance。
//
// 请求：{amount, note?}；amount 单位分，正=加 / 负=扣，不能为 0。
// 响应：{user_id, balance, log}，balance 是调整后的余额。
func (d *BalanceDomain) userBalanceAdjust(c *gin.Context) {
	userID, ok := parseUint(c.Param("user"))
	if !ok {
		d.fail(c, "用户不存在", 404)
		return
	}
	input := jsonInput(c)

	raw, ok := input["amount"]
	if !ok || raw == nil {
		d.fail(c, "请填写调整金额（单位：分，正=加 / 负=扣）", 400)
		return
	}
	amount, ok := balanceIntOf(raw)
	if !ok {
		d.fail(c, "调整金额必须是整数（单位：分）", 400)
		return
	}
	if amount == 0 {
		d.fail(c, "调整金额不能为 0", 400)
		return
	}
	var note *string
	if raw, ok := input["note"]; ok && raw != nil {
		s, _ := inputString(raw)
		if utf8.RuneCountInString(s) > 255 {
			d.fail(c, "备注不能超过 255 个字符", 400)
			return
		}
		note = &s
	}

	res, err := d.store.AdjustUserBalance(ctxOf(c), BalanceAdjust{
		UserID:     userID,
		Amount:     amount,
		OperatorID: adminIDOf(c),
		Note:       note,
		Now:        d.now().UTC().Truncate(time.Second),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrUserNotFound):
			d.fail(c, "用户不存在", 404)
		case errors.Is(err, ErrBalanceNotEnough):
			d.fail(c, "余额不足：扣减后余额不能为负数", 400)
		default:
			d.failServer(c, "调整用户余额", err)
		}
		return
	}

	d.success(c, jo(
		"user_id", userID,
		"amount", amount,
		"balance", res.Balance,
		"log", presentBalanceLog(res.Log),
	), "余额已调整")
}

// ---- 展示 ----

// presentGiftcard 输出兑换码一行（字段顺序固定，便于前端与文档对照）。
func presentGiftcard(g *Giftcard) *jsObj {
	return jo(
		"id", g.ID,
		"code", g.Code,
		"name", g.Name,
		"value", g.Value,
		"status", g.Status,
		"used_by", nullableUint(g.UsedBy),
		"used_at", nullableDateTime(g.UsedAt),
		"expires_at", nullableDateTime(g.ExpiresAt),
		"created_by", nullableUint(g.CreatedBy),
		"note", nullableStr(g.Note),
		"folder_id", nullableUint(g.FolderID),
		"created_at", nullableDateTime(g.CreatedAt),
	)
}

// presentBalanceLog 输出余额流水一行（金额单位分，正=加 / 负=扣）。
func presentBalanceLog(l *BalanceLog) *jsObj {
	if l == nil {
		return jo()
	}
	return jo(
		"id", l.ID,
		"user_id", l.UserID,
		"amount", l.Amount,
		"type", l.Type,
		"order_id", nullableUint(l.OrderID),
		"giftcard_id", nullableUint(l.GiftcardID),
		"operator_id", nullableUint(l.OperatorID),
		"balance_after", l.BalanceAfter,
		"note", nullableStr(l.Note),
		"created_at", nullableDateTime(l.CreatedAt),
	)
}

// ---- 内部小工具 ----

// generateGiftcardCodes 生成 n 个兑换码：8 字节随机 → 16 位 hex → XXXX-XXXX-XXXX-XXXX。
//
// 用 crypto/rand（与优惠码同一熵源）；批内去重，库内撞车由 InsertGiftcards 报
// ErrGiftcardCodeTaken 后整批重生成。
func generateGiftcardCodes(n int64) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	buf := make([]byte, giftcardBytes)
	for len(out) < int(n) {
		if _, err := randRead(buf); err != nil {
			panic("admin: 生成兑换码随机数失败: " + err.Error())
		}
		code := formatGiftcardCode(buf)
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

// formatGiftcardCode 把随机字节格式化成 XXXX-XXXX-XXXX-XXXX（大写 hex，每 2 字节一组）。
//
// 8 字节 → 16 位 hex → 4 组 × 4 位 = 19 字符，符合 giftcards.code varchar(32)。
func formatGiftcardCode(raw []byte) string {
	const hexDigits = "0123456789ABCDEF"
	buf := make([]byte, 0, len(raw)*2+len(raw)/2)
	for i, b := range raw {
		if i > 0 && i%2 == 0 {
			buf = append(buf, '-')
		}
		buf = append(buf, hexDigits[b>>4], hexDigits[b&0x0F])
	}
	return string(buf)
}

// balanceIntOf 取「整数分」输入：只接受整数值（拒绝 10.5 这种小数分）。
//
// 金额不做 float64 转换（与 data-contract §3 一致），非整数一律判为非法输入。
func balanceIntOf(v any) (int64, bool) {
	switch t := v.(type) {
	case jsonNumber:
		if !t.isIntLiteral() {
			return 0, false
		}
		n, err := strconv.ParseInt(string(t), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case string:
		n, err := strconv.ParseInt(phpTrimSpaceOnly(t), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case int:
		return int64(t), true
	case int64:
		return t, true
	}
	return 0, false
}

// pageParams 取 page/per_page：缺省用 defPerPage，越界夹回合法区间（1..maxPerPage）。
func pageParams(c *gin.Context, defPerPage, maxPerPage int64) (page, perPage int64) {
	page = queryInt(c, "page", 1)
	if page < 1 {
		page = 1
	}
	perPage = queryInt(c, "per_page", defPerPage)
	if perPage < 1 {
		perPage = defPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

// isGiftcardStatus 判断 status 筛选值是否合法。
func isGiftcardStatus(s string) bool {
	switch s {
	case GiftcardStatusActive, GiftcardStatusUsed, GiftcardStatusDisabled:
		return true
	}
	return false
}

// adminIDOf 取当前登录管理员 id（流水里的 operator_id）。取不到返回 nil。
func adminIDOf(c *gin.Context) *uint64 {
	if a := httpx.AdminFromContext(c); a != nil && a.ID > 0 {
		id := a.ID
		return &id
	}
	return nil
}
