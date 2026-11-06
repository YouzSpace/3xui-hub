package httpx

// 前端路由判定：NoRoute 只对「真正的前端路由」回 index.html，其余（静态资源类路径、
// 扫描器乱猜的 /.env / /.git/config / /backup.zip 等）一律真实 404。
//
// 为什么需要白名单而不是「没有扩展名就回 index.html」：Vue Router 的路径没有扩展名，
// 但扫描器请求的 /wp-admin、/phpmyadmin 之类同样没有扩展名，靠扩展名判不出来；
// 白名单是唯一既能保住深链、又能让胡说八道路径 404 的判据。
//
// 白名单与 frontend/src/router/index.js 对齐，只列到「第一段」：Vue Router 的子路由
// 都挂在同一段下面（/dashboard/*、/admin/*），新增子页不需要回来改后端。

import (
	"path"
	"path/filepath"
	"strings"
)

// frontendRouteRoots 是前端路由允许出现的第一段（区分大小写，与 Vue Router 默认一致）。
//
//   - 用户端：/、/login、/register、/forgot-password、/dashboard/*
//   - 管理端：/admin、/admin/login 以及 /admin/* 下的全部子页
//
// "dashboard" 与 "admin" 放在这里意味着「两段以下任意子路径都算前端路由」——
// 换取的是前端加了新页面不需要同步改后端；代价是 /admin/xxx.txt 这类路径只有在
// 带扩展名时才会被下面的 IsStaticAssetPath 拦下。
var frontendRouteRoots = map[string]bool{
	"login":           true,
	"register":        true,
	"forgot-password": true,
	"dashboard":       true,
	"admin":           true,
}

// IsFrontendPath 报告 p 是否是应当回 index.html 的前端路由。
//
// 判定顺序（先紧后松）：
//  1. 规范化路径（去掉 ../、重复斜杠、末尾斜杠）。
//  2. "/" 是首页，直接放行。
//  3. 形如静态文件/隐藏路径的一律不是前端路由（见 IsStaticAssetPath）—— 前端路由
//     不含点号，带点号的是浏览器/扫描器在要一个文件，不存在就该 404，不该回 HTML。
//  4. 第一段必须命中 frontendRouteRoots。
//
// 大小写：Vue Router 默认（未设 sensitive:false）按**大小写敏感**匹配，所以这里也不转
// 小写 —— 否则 /Admin/Users 会拿到 index.html、前端再跳登录页或 404，等于用 200 掩盖了
// 「这个路径不存在」，正是本卡要修的毛病。查询串不参与判定（只看 URL.Path）。
func IsFrontendPath(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	if clean == "/" {
		return true
	}

	rest := strings.TrimPrefix(clean, "/")
	segs := strings.Split(rest, "/")
	if segs[0] == "" {
		return false
	}
	if IsStaticAssetPath(clean) {
		return false
	}
	return frontendRouteRoots[segs[0]]
}

// IsStaticAssetPath 报告 p 看起来是不是「一次文件请求」——是的话就不该回 index.html。
//
// 两个判据：
//  1. 末段带扩展名：/backup.zip、/robots.txt、/assets/index-xxxx.js。
//     只看末段，/images/clash.png 里的 images 不影响判定。
//  2. 任意一段以点开头：/.env、/.git/config —— 这是部署泄漏/源码泄漏扫描器的典型目标。
//     filepath.Ext(".env") 虽然返回 ".env"，但 filepath.Ext(".git") 返回 ""，
//     所以隐藏目录（.git/.svn/.aws）得靠这条单独拦住。
//
// 前端路由本身不含点号，所以这两个判据不会误伤 /admin/users、/dashboard/tutorial/3 这类深链。
func IsStaticAssetPath(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	// path.Clean 之后 ".." 只会出现在根以下（/../x → /x），这里的 ".." 判断只是防御。
	segs := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	for _, seg := range segs {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
			return true
		}
	}
	last := segs[len(segs)-1]
	return filepath.Ext(last) != ""
}
