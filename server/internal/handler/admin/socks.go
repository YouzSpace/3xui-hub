package admin

// 手工 socks 货源（对齐 Admin\ProxySocksController）。
//
// 站长手上有一些 socks（IP:端口 + 账号密码），没有 API，只能手工录入；
// 一条记录可以挂到一个「共享型 SKU」上，卖给多个用户共用同一套凭据。
//
// 支持两种批量粘贴格式（一行一条，空行/# 注释忽略）：
//  1. host:port:username:password[:country]
//  2. socks5://username:password@host:port
//
// 密码加密存储，列表/详情接口一律不回（只回 has_password）。

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// socksSchemeRe 对齐 Controller 里的正则：
//
//	#^(?:socks5h?|http)://(?:([^:@/]+):([^@/]*)@)?([^:/]+):(\d{1,5})$#i
//
// Go 的 regexp 不支持 PCRE 的 (?i) 之外的写法，这里用 (?i) 前缀等价表达。
var socksSchemeRe = regexp.MustCompile(
	`(?i)^(?:socks5h?|http)://(?:([^:@/]+):([^@/]*)@)?([^:/]+):([0-9]{1,5})$`)

// listSocks 对齐 GET /admin-api/proxy-store/socks → {items:[...]}（id 倒序）。
func (h *Handler) listSocks(c *gin.Context) {
	list, err := h.store.SocksList(ctxOf(c))
	if err != nil {
		h.failServer(c, "查询 socks 列表", err)
		return
	}
	items := make(jsArr, 0, len(list))
	for i := range list {
		items = append(items, presentSock(&list[i]))
	}
	h.success(c, jo("items", items))
}

// importSocks 对齐 POST /admin-api/proxy-store/socks/import（body: {text}）。
//
// 返回里如实回传 created/skipped/invalid/errors（最多 5 条错误行），
// 管理员据此修格式重贴；msg 里带三项计数。
func (h *Handler) importSocks(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("text", "required", "string", "max:20000")
	v.field("allocation", "sometimes", "in:shared,exclusive")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}
	text, _ := inputString(input["text"])
	// 这批导入的分配方式（#地区功能-17）：不传 = 共享（老导入行为完全不变）。
	var allocation *string
	if val, ok := input["allocation"]; ok && val != nil {
		if s, isStr := inputString(val); isStr && s != "" {
			allocation = &s
		}
	}

	created, skipped, invalid := 0, 0, 0
	errs := make(jsArr, 0, 5)

	// PHP 用 preg_split('/\r\n|\r|\n/') 切行：先把 \r\n 归一成 \n，再按 \n 切。
	norm := strings.ReplaceAll(text, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	for index, raw := range strings.Split(norm, "\n") {
		line := phpTrimSpaceOnly(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parsed, ok := parseSockLine(line)
		if !ok {
			invalid++
			if len(errs) < 5 {
				errs = append(errs, "第 "+strconv.Itoa(index+1)+" 行无法识别："+line)
			}
			continue
		}

		// 去重：同 host + port + username 视为同一条。
		exists, err := h.store.SockExists(ctxOf(c), parsed.Host, parsed.Port, parsed.Username)
		if err != nil {
			h.failServer(c, "socks 去重查询", err)
			return
		}
		if exists {
			skipped++
			continue
		}
		parsed.Allocation = allocation
		if _, err := h.store.CreateSock(ctxOf(c), parsed); err != nil {
			h.failServer(c, "导入 socks", err)
			return
		}
		created++
	}

	msg := "导入完成：新增 " + strconv.Itoa(created) + "，重复跳过 " + strconv.Itoa(skipped)
	if invalid > 0 {
		msg += "，无法识别 " + strconv.Itoa(invalid)
	}
	h.success(c, jo(
		"created", created,
		"skipped", skipped,
		"invalid", invalid,
		"errors", errs,
	), msg)
}

// updateSock 对齐 PUT /admin-api/proxy-store/socks/{sock}。
//
// 密码留空 = 不修改（前端编辑表单不回显密码，空值代表「保持原密码」）。
func (h *Handler) updateSock(c *gin.Context) {
	id, ok := h.paramID(c, "sock")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.SockByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询 socks", err)
		return
	}

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("name", "sometimes", "string", "max:100")
	v.field("host", "sometimes", "string", "max:190")
	v.field("port", "sometimes", "integer", "min:1", "max:65535")
	v.field("username", "sometimes", "nullable", "string", "max:190")
	v.field("password", "sometimes", "nullable", "string", "max:190")
	v.field("country", "sometimes", "nullable", "string", "max:16")
	v.field("city", "sometimes", "nullable", "string", "max:64")
	v.field("note", "sometimes", "nullable", "string", "max:255")
	v.field("is_active", "sometimes", "boolean")
	v.field("allocation", "sometimes", "in:shared,exclusive")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	upd := SockUpdate{}
	if val, ok := v.valueOf("name"); ok && val != nil {
		s, _ := inputString(val)
		upd.Name = &s
	}
	if val, ok := v.valueOf("host"); ok && val != nil {
		s, _ := inputString(val)
		upd.Host = &s
	}
	if val, ok := v.valueOf("port"); ok && val != nil {
		n := inputInt(val)
		upd.Port = &n
	}
	if val, ok := v.valueOf("username"); ok {
		upd.Username = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("password"); ok {
		// 留空（null 或空串）→ 不修改。
		if s, isStr := inputString(val); isStr && s != "" {
			upd.Password = &s
		}
	}
	if val, ok := v.valueOf("country"); ok {
		upd.Country = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("city"); ok {
		upd.City = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("note"); ok {
		upd.Note = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("is_active"); ok && val != nil {
		b := inputBool(val)
		upd.IsActive = &b
	}
	if val, ok := v.valueOf("allocation"); ok && val != nil {
		s, _ := inputString(val)
		upd.Allocation = &s
	}

	s, err := h.store.UpdateSock(ctxOf(c), id, upd)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "保存 socks", err)
		return
	}
	h.success(c, presentSock(s), "已保存")
}

// deleteSock 对齐 DELETE /admin-api/proxy-store/socks/{sock}。
//
// 被 SKU 挂着的 socks 不能直接删（那个 SKU 已卖出/在售，删了用户凭据就成孤儿）。
func (h *Handler) deleteSock(c *gin.Context) {
	id, ok := h.paramID(c, "sock")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	sock, err := h.store.SockByID(ctxOf(c), id)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询 socks", err)
		return
	}
	used, err := h.store.SKUExistsUsingSocks(ctxOf(c), id)
	if err != nil {
		h.failServer(c, "检查 socks 引用", err)
		return
	}
	if used {
		h.fail(c, "这条 socks 已被某个代理规格使用，请先把规格下线或换货源", 400)
		return
	}
	// 已售出的独占线路（#地区功能-17）不能删：删了用户的凭据就成孤儿
	// （独占规格的 source_id 是空的，上面的「被规格引用」检查兜不住这种情况）。
	if sock.SoldTo != nil {
		h.fail(c, "这条线路已售给用户 #"+strconv.FormatUint(*sock.SoldTo, 10)+
			"，等它到期释放后才能删", 400)
		return
	}
	if err := h.store.DeleteSock(ctxOf(c), id); err != nil {
		h.failServer(c, "删除 socks", err)
		return
	}
	h.success(c, nil, "已删除")
}

// parseSockLine 对齐 parseLine：识别不了返回 ok=false。
func parseSockLine(line string) (SockCreate, bool) {
	if m := socksSchemeRe.FindStringSubmatch(line); m != nil {
		port, err := strconv.ParseInt(m[4], 10, 64)
		if err != nil {
			return SockCreate{}, false
		}
		out := SockCreate{
			Host: m[3],
			Port: port,
			Name: m[3] + ":" + strconv.FormatInt(port, 10),
		}
		if m[1] != "" {
			u := m[1]
			out.Username = &u
		}
		if m[2] != "" {
			p := m[2]
			out.Password = &p
		}
		return out, true
	}

	// 形式 2：host:port:user:pass[:country]
	parts := strings.Split(line, ":")
	if len(parts) >= 2 && isNumericParts(parts[1]) {
		port, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || port < 1 || port > 65535 || parts[0] == "" {
			return SockCreate{}, false
		}
		out := SockCreate{
			Host: parts[0],
			Port: port,
			Name: parts[0] + ":" + strconv.FormatInt(port, 10),
		}
		if len(parts) > 2 && parts[2] != "" {
			u := parts[2]
			out.Username = &u
		}
		if len(parts) > 3 && parts[3] != "" {
			p := parts[3]
			out.Password = &p
		}
		if len(parts) > 4 && parts[4] != "" {
			// PHP 用 strtoupper 转国家代码（只转 ASCII）。
			country := phpStrToUpper(parts[4])
			out.Country = &country
		}
		return out, true
	}

	return SockCreate{}, false
}

// isNumericParts 对齐 PHP 的 is_numeric($parts[1])。
func isNumericParts(s string) bool { return phpIsNumericString(s) }

// phpStrToUpper 对齐 PHP strtoupper()：只转 ASCII a-z。
func phpStrToUpper(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// presentSock 对齐 present：密码只回 has_password，明文/密文都不出接口。
// 独占模式（#地区功能-17）额外回 allocation / sold_to / sold_at，前端据此显示
// 「独占 · 已售给用户 #N」；共享线路这三项是 shared / null / null。
func presentSock(s *Sock) *jsObj {
	return jo(
		"id", s.ID,
		"name", s.Name,
		"host", s.Host,
		"port", s.Port,
		"username", nullableStr(s.Username),
		"has_password", s.HasPassword(),
		"country", nullableStr(s.Country),
		"city", nullableStr(s.City),
		"note", nullableStr(s.Note),
		"is_active", s.IsActive,
		"created_at", nullableDateTime(s.CreatedAt),
		"allocation", s.Allocation,
		"sold_to", nullableUint(s.SoldTo),
		"sold_at", nullableDateTime(s.SoldAt),
	)
}
