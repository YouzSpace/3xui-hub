// Package web 把前端构建产物（internal/web/dist）当静态站点对外提供。
//
// 两套实现同签名、按构建 tag 二选一：
//   - embed_on.go（-tags=embed，生产）：dist 用 go:embed 编进二进制，单文件部署不依赖磁盘目录
//   - embed_off.go（默认 tag，本地开发）：直接读磁盘 internal/web/dist，vite 重建后刷新即可见
//
// 两边都返回 Handler(fs)，本文件是唯一的服务逻辑：命中文件发文件，其余非 API 路径回
// index.html，交给 Vue Router 做 SPA fallback。
package web

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// apiPrefixes 是后端专属路径前缀：这些路径不参与 SPA fallback，由调用方按 API 口径回 404。
// 前端路由里不会出现以它们开头的地址（/api、/admin-api 是接口，/node-* 是节点安装脚本与资产）。
var apiPrefixes = []string{"/api/", "/admin-api/", "/node-"}

// IsAPIPath 报告 p 是否属于后端路径（接口 / 节点资产），这类路径不该回 index.html。
func IsAPIPath(p string) bool {
	for _, prefix := range apiPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	// 裸前缀没有结尾斜杠，上面的 HasPrefix 匹配不到，单独判一下。
	return p == "/api" || p == "/admin-api"
}

// Handler 用指定文件系统构造前端服务，返回 nil 表示「这里没有前端」。
//
// 返回 nil 的两种情况，调用方应当退回 API 口径 404 而不是 5xx：
//   - fsys 为 nil
//   - 根下没有 index.html（仓库里只有 dist/.gitkeep 占位，前端还没构建）
func Handler(fsys fs.FS) http.Handler {
	if fsys == nil {
		return nil
	}
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return nil
	}

	files := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Clean 去掉 ../ 之类的穿越段；fs.FS 本身也拒绝越界路径，这里只是提前归一化。
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && isFile(fsys, name) {
			// vite 产出的 assets 文件名带内容哈希，可以放心长期缓存；
			// index.html 与业务路由走下面的 no-cache，保证升级后立刻拿到新引用。
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}

		// assets 下的缺失文件必须 404：升级后浏览器若还拿着旧 index.html，
		// 老哈希的 js/css 会请求到这里，回 index.html 只会变成莫名其妙的 JS 语法错误。
		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}

		// 每次都从 fsys 重读 index.html：本地开发时 vite 重建会改掉 asset 哈希，
		// 缓存启动时那份就会让新 index 引用到已不存在的旧文件（表现为整站白屏）。
		// 读失败（dist 正在重建/被清空）时退回启动时读到的那份，至少不是 500。
		body := index
		if fresh, err := fs.ReadFile(fsys, "index.html"); err == nil {
			body = fresh
		}
		serveIndex(w, r, body)
	})
}

// isFile 判断 fsys 里是否存在 name 且不是目录（目录走 index.html，不走文件服务）。
func isFile(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// serveIndex 输出 index.html。ServeContent 顺带处理 HEAD / Range / 条件请求；
// 零值 modtime 表示不生成 Last-Modified，配合 Cache-Control: no-cache 每次都会重新校验。
func serveIndex(w http.ResponseWriter, r *http.Request, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
}
