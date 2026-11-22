package http

import (
	"strings"

	p "controlhub-server/internal/protocol"
)

// buildInbound http 入站：单组账号密码。
// 落库形状（xray 26.3.27 实测可跑通真实流量）：
//
//	http = {accounts: [{user, pass}], allowTransparent: false}
func buildInbound(c *p.InboundCtx) (*p.Object, bool) {
	user := strings.TrimSpace(c.Input("auth_user"))
	pass := c.Input("auth_pass")
	if pass == "" && c.Prev != nil {
		// 密码不回显：编辑时留空 = 沿用原值（重填才改）。
		pass = c.PrevStr("accounts", "0", "pass")
	}
	if user == "" || pass == "" {
		c.Fail("SOCKS / HTTP 入站必须填写用户名与密码（内核默认 noauth，公网裸代理会被扫端口滥用）")
		return nil, false
	}
	account := p.New("user", user, "pass", pass)
	return p.New("accounts", p.Array{account}, "allowTransparent", false), true
}
