//go:build !embed

package web

import (
	"net/http"
	"os"
)

// distDir 是本地开发时前端产物的落盘目录，相对 server/ 工作目录
// （vite.config 的 outDir 指向 ../server/internal/web/dist，构建即就位）。
// 直接读磁盘：vite 重新构建后刷新页面就能看到，不用重启 server。
const distDir = "internal/web/dist"

// FrontendServer 返回磁盘版前端服务；目录不存在或没有 index.html 时返回 nil，
// 调用方据此退回 API 口径 404（本地起后端调试接口、前端还跑在 vite dev server 上时就是这种情况）。
func FrontendServer() http.Handler {
	return Handler(os.DirFS(distDir))
}
