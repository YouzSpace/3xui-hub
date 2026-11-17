package admin

// 代理商城：总开关 / 商品 / 规格（对齐 Admin\ProxyProductController）。
//
// 与节点套餐（plans）完全分开：这里只读写 proxy_products / proxy_skus，
// 不影响节点与订阅。库存三态（total/locked/sold）由 ProxySku 维护，
// 后台可直接改 total（站长在 Webshare 加购子用户后把可售数量调大）。

import (
	"github.com/gin-gonic/gin"

	"controlhub-server/internal/sockpool"
)

const proxyStoreEnabledKey = "proxy_store_enabled"

// proxyStoreSettings 对齐 GET /admin-api/proxy-store/settings → {enabled}。
//
// 默认关闭：代理是可选产品线，商品/价格/上游没配好之前用户端不该看到入口。
func (h *Handler) proxyStoreSettings(c *gin.Context) {
	values, err := h.store.SiteConfigMany(ctxOf(c), []string{proxyStoreEnabledKey})
	if err != nil {
		h.failServer(c, "读取代理商城开关", err)
		return
	}
	h.success(c, jo("enabled", values[proxyStoreEnabledKey] == "1"))
}

// updateProxyStoreSettings 对齐 POST /admin-api/proxy-store/settings。
func (h *Handler) updateProxyStoreSettings(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("enabled", "required", "boolean")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	enabled := inputBool(input["enabled"])
	value := "0"
	if enabled {
		value = "1"
	}
	if err := h.store.SiteConfigSetMany(ctxOf(c), []KV{{Key: proxyStoreEnabledKey, Value: &value}}); err != nil {
		h.failServer(c, "保存代理商城开关", err)
		return
	}

	msg := "代理商城已关闭，用户端不再显示"
	if enabled {
		msg = "代理商城已开启，用户端可见"
	}
	h.success(c, jo("enabled", enabled), msg)
}

// listProducts 对齐 GET /admin-api/proxy-store/products → {items: [...]}。
//
// 排序：商品 sort_order 升序、id 降序；规格 sort_order 升序、id 升序（与 with() 闭包一致）。
func (h *Handler) listProducts(c *gin.Context) {
	products, err := h.store.ProductsWithSKUs(ctxOf(c))
	if err != nil {
		h.failServer(c, "查询代理商品", err)
		return
	}
	items := make(jsArr, 0, len(products))
	for i := range products {
		items = append(items, presentProduct(&products[i]))
	}
	h.success(c, jo("items", items))
}

// createProduct 对齐 POST /admin-api/proxy-store/products。
func (h *Handler) createProduct(c *gin.Context) {
	input := jsonInput(c)
	in, ok := h.validateProduct(c, input)
	if !ok {
		return
	}
	p, err := h.store.CreateProduct(ctxOf(c), in)
	if err != nil {
		h.failServer(c, "创建代理商品", err)
		return
	}
	h.success(c, presentProduct(p), "已创建")
}

// updateProduct 对齐 PUT /admin-api/proxy-store/products/{product}。
func (h *Handler) updateProduct(c *gin.Context) {
	id, ok := h.paramID(c, "product")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	// 先确认存在：Laravel 的隐式绑定在记录不存在时同样回「接口不存在」，
	// 且**早于**表单校验 —— 所以 404 必须排在 validate 之前。
	if _, err := h.store.ProductByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询代理商品", err)
		return
	}

	in, ok := h.validateProduct(c, jsonInput(c))
	if !ok {
		return
	}
	p, err := h.store.UpdateProduct(ctxOf(c), id, in)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "保存代理商品", err)
		return
	}
	h.success(c, presentProduct(p), "已保存")
}

// deleteProduct 对齐 DELETE /admin-api/proxy-store/products/{product}。
//
// 已售出的规格不允许连带删除（订单/账号还引用它的额度与时长）。
func (h *Handler) deleteProduct(c *gin.Context) {
	id, ok := h.paramID(c, "product")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.ProductByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询代理商品", err)
		return
	}
	sold, err := h.store.ProductHasSoldSKU(ctxOf(c), id)
	if err != nil {
		h.failServer(c, "检查代理规格售出", err)
		return
	}
	if sold {
		h.fail(c, "该商品已有售出的规格，不能删除；如需下架请关掉「上架」开关", 400)
		return
	}
	if err := h.store.DeleteProduct(ctxOf(c), id); err != nil {
		h.failServer(c, "删除代理商品", err)
		return
	}
	h.success(c, nil, "已删除")
}

// createSKU 对齐 POST /admin-api/proxy-store/products/{product}/skus。
func (h *Handler) createSKU(c *gin.Context) {
	productID, ok := h.paramID(c, "product")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.ProductByID(ctxOf(c), productID); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询代理商品", err)
		return
	}
	in, ok := h.validateSKU(c, jsonInput(c))
	if !ok {
		return
	}
	sku, err := h.store.CreateSKU(ctxOf(c), productID, in)
	if err != nil {
		h.failServer(c, "创建代理规格", err)
		return
	}
	h.success(c, presentSKU(sku), "已添加规格")
}

// updateSKU 对齐 PUT /admin-api/proxy-store/skus/{sku}。
func (h *Handler) updateSKU(c *gin.Context) {
	id, ok := h.paramID(c, "sku")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.SKUByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询代理规格", err)
		return
	}
	in, ok := h.validateSKU(c, jsonInput(c))
	if !ok {
		return
	}
	sku, err := h.store.UpdateSKU(ctxOf(c), id, in)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "保存代理规格", err)
		return
	}
	h.success(c, presentSKU(sku), "已保存")
}

// deleteSKU 对齐 DELETE /admin-api/proxy-store/skus/{sku}。
func (h *Handler) deleteSKU(c *gin.Context) {
	id, ok := h.paramID(c, "sku")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	sku, err := h.store.SKUByID(ctxOf(c), id)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询代理规格", err)
		return
	}
	if sku.StockSold > 0 {
		h.fail(c, "该规格已有售出记录，不能删除；请关闭「启用」来下架", 400)
		return
	}
	if sku.StockLocked > 0 {
		h.fail(c, "该规格有未支付订单占用着库存，请等订单超时后再删", 400)
		return
	}
	if err := h.store.DeleteSKU(ctxOf(c), id); err != nil {
		h.failServer(c, "删除代理规格", err)
		return
	}
	h.success(c, nil, "已删除")
}

// ---- 校验 ----

// validateProduct 对齐 validateProduct：name 必填 ≤100，其余可空。
func (h *Handler) validateProduct(c *gin.Context, input map[string]any) (ProductInput, bool) {
	v := newValidator(ctxOf(c), input)
	v.field("name", "required", "string", "max:100")
	v.field("subtitle", "nullable", "string", "max:200")
	v.field("description", "nullable", "string", "max:5000")
	v.field("image_url", "nullable", "string", "max:500")
	v.field("is_active", "sometimes", "boolean")
	v.field("sort_order", "sometimes", "integer", "min:0")
	if !v.passes() {
		h.failValidation(c, v)
		return ProductInput{}, false
	}
	return productInputOf(v), true
}

// productInputOf 从已校验的输入取值（只取出现过的键，与 validated() 一致）。
func productInputOf(v *validator) ProductInput {
	in := ProductInput{}
	if val, ok := v.valueOf("name"); ok {
		in.Name, _ = inputString(val)
	}
	if val, ok := v.valueOf("subtitle"); ok {
		in.Subtitle = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("description"); ok {
		in.Description = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("image_url"); ok {
		in.ImageURL = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("is_active"); ok && val != nil {
		b := inputBool(val)
		in.IsActive = &b
	}
	if val, ok := v.valueOf("sort_order"); ok && val != nil {
		n := inputInt(val)
		in.SortOrder = &n
	}
	return in
}

// validateSKU 对齐 validateSku：含 socks 货源的两条业务规则。
func (h *Handler) validateSKU(c *gin.Context, input map[string]any) (SKUInput, bool) {
	v := newValidator(ctxOf(c), input).
		existsChecker(h.existsChecker())
	v.field("name", "required", "string", "max:100")
	v.field("source_type", "sometimes", "in:webshare,socks")
	v.field("allocation", "sometimes", "in:exclusive,shared")
	v.field("source_id", "nullable", "integer", "min:1")
	v.field("region", "nullable", "string", "max:32")
	v.field("traffic_gb", "sometimes", "integer", "min:0")
	v.field("duration_days", "required", "integer", "min:1")
	v.field("max_thread_count", "sometimes", "integer", "min:0")
	v.field("price", "required", "numeric", "min:0")
	v.field("cost_price", "sometimes", "numeric", "min:0")
	v.field("stock_total", "sometimes", "integer", "min:0")
	v.field("is_active", "sometimes", "boolean")
	v.field("sort_order", "sometimes", "integer", "min:0")
	if !v.passes() {
		h.failValidation(c, v)
		return SKUInput{}, false
	}

	in := skuInputOf(v)

	// socks 货源的业务规则（#地区功能-17 起分两种分配方式）：
	//   - 共享：必须指定具体那一条 socks（多人共用，现状不变）；
	//   - 独占：source_id 可以为空，表示「下单时从整个独占池里挑未售出的」；
	//     非空则只在那一条里挑，且那条本身必须标成独占（否则永远挑不出货）。
	//
	// 注意文案与形态：PHP 在这里抛 ValidationException::withMessages(['source_id' => ...])，
	// 因此走的是与表单校验**同一条**渲染路径（HTTP 200 + code 500 + 单条文案）。
	sourceType := KindWebshare
	if in.SourceType != nil {
		sourceType = *in.SourceType
	}
	if sourceType == KindSocks {
		exclusive := sockpool.IsExclusive(in.Allocation)
		if exclusive {
			value := sockpool.Exclusive
			in.Allocation = &value
		} else {
			value := sockpool.Shared
			in.Allocation = &value
		}
		if in.SourceID == nil {
			if !exclusive {
				h.fail(c, "socks 货源必须选择一条已录入的 socks", 500)
				return SKUInput{}, false
			}
		} else {
			sock, err := h.store.SockByID(ctxOf(c), *in.SourceID)
			if err != nil && !NoRows(err) {
				h.failServer(c, "查询 socks 货源", err)
				return SKUInput{}, false
			}
			if sock == nil {
				h.fail(c, "选择的 socks 不存在", 500)
				return SKUInput{}, false
			}
			if exclusive && sock.Allocation != sockpool.Exclusive {
				h.fail(c, "这条 socks 是共享型，不能挂在独占规格上（请先在 socks 列表把它改成独占）", 500)
				return SKUInput{}, false
			}
		}
	}

	return in, true
}

// skuInputOf 从已校验输入取字段。
func skuInputOf(v *validator) SKUInput {
	in := SKUInput{}
	if val, ok := v.valueOf("name"); ok {
		in.Name, _ = inputString(val)
	}
	if val, ok := v.valueOf("source_type"); ok {
		in.SourceType = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("allocation"); ok {
		in.Allocation = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("source_id"); ok {
		if val == nil {
			// 显式传 null = 清空（独占池「不指定」/ webshare「自动挑账号」）。
			in.ClearSourceID = true
		} else {
			n := uint64(inputInt(val))
			in.SourceID = &n
		}
	}
	if val, ok := v.valueOf("region"); ok {
		in.Region = ptrStringOrNil(val)
	}
	if val, ok := v.valueOf("traffic_gb"); ok && val != nil {
		n := inputInt(val)
		in.TrafficGB = &n
	}
	if val, ok := v.valueOf("duration_days"); ok && val != nil {
		in.DurationDays = inputInt(val)
	}
	if val, ok := v.valueOf("max_thread_count"); ok && val != nil {
		n := inputInt(val)
		in.MaxThreadCount = &n
	}
	if val, ok := v.valueOf("price"); ok {
		in.Price, _ = decimalString(val, 2)
	}
	if val, ok := v.valueOf("cost_price"); ok && val != nil {
		s, _ := decimalString(val, 2)
		in.CostPrice = &s
	}
	if val, ok := v.valueOf("stock_total"); ok && val != nil {
		n := inputInt(val)
		in.StockTotal = &n
	}
	if val, ok := v.valueOf("is_active"); ok && val != nil {
		b := inputBool(val)
		in.IsActive = &b
	}
	if val, ok := v.valueOf("sort_order"); ok && val != nil {
		n := inputInt(val)
		in.SortOrder = &n
	}
	return in
}

// ---- 展示 ----

// presentProduct 对齐 presentProduct：字段顺序即 PHP 数组顺序。
func presentProduct(p *Product) *jsObj {
	skus := make(jsArr, 0, len(p.SKUs))
	for i := range p.SKUs {
		skus = append(skus, presentSKU(&p.SKUs[i]))
	}
	return jo(
		"id", p.ID,
		"name", p.Name,
		"subtitle", nullableStr(p.Subtitle),
		"description", nullableStr(p.Description),
		"image_url", nullableStr(p.ImageURL),
		"is_active", p.IsActive,
		"sort_order", p.SortOrder,
		"skus", skus,
		"created_at", nullableDateTime(p.CreatedAt),
	)
}

// presentSKU 对齐 presentSku：金额用 (float) 输出（decimal:2 → 0.01 / 1.5 / 0）。
func presentSKU(s *SKU) *jsObj {
	return jo(
		"id", s.ID,
		"product_id", s.ProductID,
		"name", s.Name,
		"source_type", nullableStr(s.SourceType),
		"allocation", nullableStr(s.Allocation),
		"source_id", nullableUint(s.SourceID),
		"region", nullableStr(s.Region),
		"traffic_gb", s.TrafficGB,
		"duration_days", s.DurationDays,
		"max_thread_count", s.MaxThreadCount,
		"price", phpFloatOf(s.Price),
		"cost_price", phpFloatOf(s.CostPrice),
		"stock_total", s.StockTotal,
		"stock_locked", s.StockLocked,
		"stock_sold", s.StockSold,
		"stock_available", s.AvailableStock(),
		"is_active", s.IsActive,
		"sort_order", s.SortOrder,
	)
}

// ---- 小工具 ----

// ptrStringOrNil 把输入值转成 *string（null → nil，其余按 PHP (string)）。
func ptrStringOrNil(v any) *string {
	if v == nil {
		return nil
	}
	s, ok := inputString(v)
	if !ok {
		return nil
	}
	return &s
}

// nullableUint 输出 uint64 指针（nil → null）。
func nullableUint(v *uint64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableInt 输出 int64 指针（nil → null）。
func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableBool 输出 bool 指针（nil → null）。
func nullableBool(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}

// phpFloatOf 把 decimal 字符串按 PHP 的 (float) 输出。
//
// decimal 只有 2 位小数，float64 表达无损；输出用最短往返表示，与 PHP 的
// json_encode((float)"10.00") → 10 一致。
func phpFloatOf(decimal string) phpFloat {
	return phpFloat(parseFloatSafe(decimal))
}
