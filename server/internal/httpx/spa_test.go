package httpx

import "testing"

func TestIsFrontendPath(t *testing.T) {
	yes := []string{
		"/",
		"/login",
		"/register",
		"/forgot-password",
		"/dashboard",
		"/dashboard/shop",
		"/dashboard/proxy/mine",
		"/dashboard/tutorial/3",
		"/admin",
		"/admin/users",
		"/admin/login",
		"/admin/nodes/xray",
		"/login/", // 末尾斜杠
	}
	for _, p := range yes {
		if !IsFrontendPath(p) {
			t.Errorf("IsFrontendPath(%q) = false, want true", p)
		}
	}

	no := []string{
		"/robots.txt",
		"/backup.zip",
		"/.env",
		"/.git/config",
		"/assets/index-cXzeYoHz.js",
		"/images/clash.png",
		"/admin/users.json",
		"/dashboard/../../etc/passwd",
		"/wp-admin",
		"/phpmyadmin",
		"/random",
		"/administer",
		"/Admin/Users", // 前端路由大小写敏感，/Admin 不是 /admin
		"/DASHBOARD",   // 同上
	}
	for _, p := range no {
		if IsFrontendPath(p) {
			t.Errorf("IsFrontendPath(%q) = true, want false", p)
		}
	}
}

func TestIsStaticAssetPath(t *testing.T) {
	asset := []string{
		"/robots.txt",
		"/backup.zip",
		"/.env",
		"/.git/config",
		"/.aws/credentials",
		"/assets/index-cXzeYoHz.js",
		"/images/world.svg",
	}
	for _, p := range asset {
		if !IsStaticAssetPath(p) {
			t.Errorf("IsStaticAssetPath(%q) = false, want true", p)
		}
	}

	page := []string{
		"/",
		"/login",
		"/admin/users",
		"/dashboard/tutorial/3",
	}
	for _, p := range page {
		if IsStaticAssetPath(p) {
			t.Errorf("IsStaticAssetPath(%q) = true, want false", p)
		}
	}
}
