package httpx

// 安全响应头中间件：HSTS / X-Frame-Options / X-Content-Type-Options / Referrer-Policy
// 强制下发，CSP 只走 Report-Only。
//
// 为什么不直接上强制 CSP：前端 index.html 里有内联主题脚本（读 localStorage 预置
// .light/.dark 防闪烁）、运行时大量 localStorage + Vite 产物的哈希资源，收紧了要
// 逐个发 nonce 才能不白屏。先用 Report-Only 收集违规，确认没有误伤后再考虑转强制。
//
// 挂在 gin.Engine 全局（registerRoutes 里 r.Use 最早的一批），这样 NoRoute 的
// index.html / 真实 404 / API JSON 都能带上头，不需要每个分支各写一遍。

import (
	"github.com/gin-gonic/gin"
)

// 强制下发的四个头。
const (
	// HSTS 只带 max-age，**不带 includeSubDomains / preload**：面板经常和节点域名、
	// 订阅域名共用主域（node1.example.com、sub.example.com），一旦带 includeSubDomains
	// 而某个子域暂时没证书，浏览器会直接拒绝访问且用户无法点「继续」，回滚要等 max-age
	// 过期。想开子域时由运维在确认全部子域都有 HTTPS 后自行加，代码不替用户做这个决定。
	headerHSTS        = "Strict-Transport-Security"
	valueHSTS         = "max-age=31536000"
	headerFrameOption = "X-Frame-Options"
	valueFrameOption  = "DENY"
	headerContentType = "X-Content-Type-Options"
	valueContentType  = "nosniff"
	headerReferrer    = "Referrer-Policy"
	valueReferrer     = "strict-origin-when-cross-origin"
)

// headerCSPReportOnly 是 CSP 的 Report-Only 头名（浏览器只在控制台报违规，不拦截）。
const headerCSPReportOnly = "Content-Security-Policy-Report-Only"

// CSPReportOnly 是 Report-Only 策略。
//
// 逐条说明（宽到「现有功能一条都不能断」，Report-Only 的意义只是先看见违规）：
//   - default-src 'self'：兜底只允许同源。
//   - script-src 'self' 'unsafe-inline' 'unsafe-eval'：index.html 有内联防闪烁脚本；
//     面板主页自定义 HTML 走 DOMPurify 清洗后插入，仍可能有内联脚本残留；
//     Vite 运行时/eval 场景一并放开，避免 Report 噪音淹没真正的违规。
//   - style-src 'self' 'unsafe-inline' + fonts.googleapis.com：主题变量、内联 style
//     属性、以及 index.html 直接引用的 Google Fonts 样式表。
//   - font-src 'self' data: fonts.gstatic.com：字体文件来自 gstatic，data: 用于内联字体。
//   - img-src 'self' data: blob: https:：data:/blob: 是前端导出二维码/图表用，https: 放
//     外部图床与 admin 设置页的 api.qrserver.com 二维码；img 本身不执行代码，放开风险低。
//   - connect-src 'self'：接口全同源，支付跳转是顶层导航（见 navigate-to/超链接，不受 connect-src 约束）。
//   - frame-src 'none'：站内没有 iframe 用途（自定义主页是 DOMPurify 清洗后直插 DOM，不是 iframe）。
//   - worker-src 'self' blob:、child-src 'self' blob:：兼容老浏览器对 worker 的取源。
//   - frame-ancestors 'none'：与 X-Frame-Options: DENY 同义（CSP 侧覆盖老浏览器不认 XFO 的场景）。
//   - base-uri 'self'、form-action 'self'：防 base 标签劫持与表单外发。
//
// 注意 frame-src 与 frame-ancestors 是相反方向：前者管「本页能嵌谁」，后者管「谁能嵌本页」。
const CSPReportOnly = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' data: https://fonts.gstatic.com; " +
	"img-src 'self' data: blob: https:; " +
	"connect-src 'self'; " +
	"frame-src 'none'; " +
	"worker-src 'self' blob:; " +
	"child-src 'self' blob:; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// SecurityHeaders 返回统一下发安全响应头的中间件。
//
// 放在 gin 全局 Use 里，所有响应（含 NoRoute 的 index.html、真实 404、API JSON、
// WebSocket 升级前的 101 响应）都会带上。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		// 只在写响应前设置一次；用 Set 而非 Add，避免上游中间件/代理重复追加出多值头。
		h.Set(headerHSTS, valueHSTS)
		h.Set(headerFrameOption, valueFrameOption)
		h.Set(headerContentType, valueContentType)
		h.Set(headerReferrer, valueReferrer)
		h.Set(headerCSPReportOnly, CSPReportOnly)
		c.Next()
	}
}

// FixedHeaders 是上面中间件写入的头（导出给单测/验收脚本断言用）。
// 顺序固定，便于测试逐条比对。
var FixedHeaders = [][2]string{
	{headerHSTS, valueHSTS},
	{headerFrameOption, valueFrameOption},
	{headerContentType, valueContentType},
	{headerReferrer, valueReferrer},
}
