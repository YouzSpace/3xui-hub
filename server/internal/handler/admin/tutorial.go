package admin

// 教程管理（对齐 Admin\TutorialController）。
//
// 输出形态与其它管理端接口不同：控制器是手写 response()->json(['code'=>0,'data'=>...])，
//   - 列表/创建/修改/删除：{"code":0,"data":...}（**没有 msg**）
//   - 列表/修改输出模型属性（含 created_at/updated_at 的 Eloquent toJSON 口径）
//   - 删除返回 {"code":0,"data":{"message":"ok"}}
// 所以这里全走 successNoMsg，不能套 success()。

import (
	"github.com/gin-gonic/gin"
)

// listTutorials 对齐 GET /admin-api/tutorials：sort 升序、id 降序，输出全部（不过滤 enabled）。
func (h *Handler) listTutorials(c *gin.Context) {
	list, err := h.store.TutorialsAll(ctxOf(c))
	if err != nil {
		h.failServer(c, "查询教程", err)
		return
	}
	items := make(jsArr, 0, len(list))
	for i := range list {
		items = append(items, presentTutorial(&list[i]))
	}
	h.successNoMsg(c, items)
}

// createTutorial 对齐 POST /admin-api/tutorials。
func (h *Handler) createTutorial(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("title", "required", "string", "max:255")
	v.field("category", "nullable", "string", "max:100")
	v.field("content", "required", "string")
	v.field("sort", "nullable", "integer")
	v.field("enabled", "nullable", "boolean")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	in := TutorialInput{}
	in.Title, _ = inputString(input["title"])
	in.Content, _ = inputString(input["content"])
	if val, ok := v.valueOf("category"); ok {
		in.Category = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("sort"); ok && val != nil {
		n := inputInt(val)
		in.Sort = &n
	}
	if val, ok := v.valueOf("enabled"); ok && val != nil {
		b := inputBool(val)
		in.Enabled = &b
	}

	t, err := h.store.CreateTutorial(ctxOf(c), in)
	if err != nil {
		// PHP 在这里会把 SQL 异常原文回给前端（bootstrap 的 render 回调），
		// Go 侧只回通用文案并记日志 —— 文案差异见交付风险。
		h.failServer(c, "创建教程", err)
		return
	}
	// 新建时输出的是「刚 fill 的模型属性」：只包含请求里出现过的键，
	// 且 updated_at 在 created_at 之前（updateTimestamps 先设 updated_at），id 最后。
	h.successNoMsg(c, presentTutorialCreated(t, v))
}

// updateTutorial 对齐 PUT /admin-api/tutorials/{tutorial}。
func (h *Handler) updateTutorial(c *gin.Context) {
	id, ok := h.paramID(c, "tutorial")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.TutorialByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询教程", err)
		return
	}

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("title", "sometimes", "required", "string", "max:255")
	v.field("category", "nullable", "string", "max:100")
	v.field("content", "sometimes", "required", "string")
	v.field("sort", "nullable", "integer")
	v.field("enabled", "nullable", "boolean")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	upd := TutorialUpdate{}
	if val, ok := v.valueOf("title"); ok && val != nil {
		s, _ := inputString(val)
		upd.Title = &s
	}
	if val, ok := v.valueOf("category"); ok {
		upd.Category = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("content"); ok && val != nil {
		s, _ := inputString(val)
		upd.Content = &s
	}
	if val, ok := v.valueOf("sort"); ok {
		if val == nil {
			zero := int64(0)
			upd.Sort = &zero // 显式 null → 0（字段 NOT NULL）
		} else {
			n := inputInt(val)
			upd.Sort = &n
		}
	}
	if val, ok := v.valueOf("enabled"); ok {
		if val == nil {
			t := true
			upd.Enabled = &t
		} else {
			b := inputBool(val)
			upd.Enabled = &b
		}
	}

	t, err := h.store.UpdateTutorial(ctxOf(c), id, upd)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "保存教程", err)
		return
	}
	h.successNoMsg(c, presentTutorial(t))
}

// presentTutorialCreated 复刻「POST 后直接输出 $tutorial」的属性集合与顺序：
// 只含请求里出现过的列（未传的用模型默认值，但不会出现在属性里）+ 两个时间戳 + id。
//
// 实测（金样本 w_tut_create / y_tut_order）：
//
//	{"title":…,"category":…,"content":…,"sort":…,"enabled":…,"updated_at":…,"created_at":…,"id":6}
func presentTutorialCreated(t *Tutorial, v *validator) *jsObj {
	o := jo()
	for _, name := range []string{"title", "category", "content", "sort", "enabled"} {
		if _, ok := v.valueOf(name); !ok {
			continue
		}
		switch name {
		case "title":
			o.put("title", t.Title)
		case "category":
			o.put("category", t.Category)
		case "content":
			o.put("content", t.Content)
		case "sort":
			o.put("sort", t.Sort)
		case "enabled":
			o.put("enabled", t.Enabled)
		}
	}
	o.put("updated_at", nullableEloq(t.UpdatedAt))
	o.put("created_at", nullableEloq(t.CreatedAt))
	o.put("id", t.ID)
	return o
}

// deleteTutorial 对齐 DELETE /admin-api/tutorials/{tutorial}：{"code":0,"data":{"message":"ok"}}。
func (h *Handler) deleteTutorial(c *gin.Context) {
	id, ok := h.paramID(c, "tutorial")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.TutorialByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询教程", err)
		return
	}
	if err := h.store.DeleteTutorial(ctxOf(c), id); err != nil {
		h.failServer(c, "删除教程", err)
		return
	}
	h.successNoMsg(c, jo("message", "ok"))
}

// presentTutorial 输出模型属性（Eloquent toJSON：created_at/updated_at 带微秒 + Z）。
//
// 注意 PHP 的模型属性顺序：数据库列顺序在前，随后是哪个先被 touch 哪个在后 ——
// 新建时 updated_at 先于 created_at（updateTimestamps 先设 updated_at），
// 读取时按列顺序（created_at 在前）。这里用两个构造器区分这两种情形。
func presentTutorial(t *Tutorial) *jsObj {
	return jo(
		"id", t.ID,
		"title", t.Title,
		"category", t.Category,
		"content", t.Content,
		"sort", t.Sort,
		"enabled", t.Enabled,
		"created_at", nullableEloq(t.CreatedAt),
		"updated_at", nullableEloq(t.UpdatedAt),
	)
}

