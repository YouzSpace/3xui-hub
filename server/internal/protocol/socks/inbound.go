package socks

import (
	"strings"

	p "controlhub-server/internal/protocol"
)

// buildInbound socks 入站：单组账号密码。
// 落库形状（xray 26.3.27 实测可跑通真实流量）：
//
//	socks = {auth: "password", accounts: [{user, pass}], udp: false}
//
// udp=false：只占 TCP，与端口族判定（inboundFamiliesOf）同口径。
func buildInbound(c *p.InboundCtx) (*p.Object, bool) {
	account, ok := inboundAccount(c)
	if !ok {
		return nil, false
	}
	return p.New("auth", "password", "accounts", p.Array{account}, "udp", false), true
}

// inboundAccount 读取并校验单组用户名/密码（socks 与 http 入站共用口径）。
// 密码不回显：编辑时留空 = 沿用原值（重填才改）。
func inboundAccount(c *p.InboundCtx) (*p.Object, bool) {
	user := strings.TrimSpace(c.Input("auth_user"))
	pass := c.Input("auth_pass")
	if pass == "" && c.Prev != nil {
		pass = c.PrevStr("accounts", "0", "pass")
	}
	if user == "" || pass == "" {
		c.Fail("SOCKS / HTTP 入站必须填写用户名与密码（内核默认 noauth，公网裸代理会被扫端口滥用）")
		return nil, false
	}
	return p.New("user", user, "pass", pass), true
}
