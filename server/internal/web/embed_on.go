//go:build embed

package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// distFS 是编进二进制的前端产物：生产环境一个可执行文件就能对外服务，不依赖磁盘上的 dist。
// all: 前缀连点开头的文件一起收（vite 产物里通常没有，但避免 dist/.gitkeep 之外的隐藏文件被漏掉）。
//
//go:embed all:dist
var distFS embed.FS

// FrontendServer 返回内嵌前端服务；dist 里没有 index.html（前端没构建，只有 .gitkeep 占位）时返回 nil，
// 调用方据此退回 API 口径 404。
func FrontendServer() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	return Handler(sub)
}
