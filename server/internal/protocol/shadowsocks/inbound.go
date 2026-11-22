package shadowsocks

import (
	"strings"

	p "controlhub-server/internal/protocol"
	"controlhub-server/internal/protocol/shadowsocks/ss2022"
)

// buildInbound shadowsocks 入站：method + 服务端 PSK（2022 走密钥链）。
//
// 落库形状：{method, password}。password 是服务端 PSK：
//   - SS-2022：必须是 base64 且解码后正好 16/32 字节，长度不对内核会 "bad key"
//     拒载整份节点配置；留空/非法就现场生成一把合法密钥（管理员可在回显里看到/复制）；
//   - 传统 AEAD：自由格式，只要求非空。
func buildInbound(c *p.InboundCtx) (*p.Object, bool) {
	method := c.DefaultStr("method")
	if s := c.Input("ss_method"); s != "" {
		method = s
	}
	settings := p.New("method", method)
	pw := strings.TrimSpace(c.Input("ss_password"))
	if pw == "" && c.Prev != nil && c.PrevProtocol == "shadowsocks" {
		// 编辑时留空 = 沿用原值（与 socks/http「不改不用重填」同口径）。
		pw = strings.TrimSpace(c.PrevStr("password"))
	}
	if ss2022.KeyLen(method) > 0 {
		if !ss2022.ValidKey(method, pw) {
			gen, err := ss2022.GenerateKey(method)
			if err != nil {
				c.FailServer("生成 Shadowsocks-2022 密钥", err)
				return nil, false
			}
			pw = gen
		}
	} else if pw == "" {
		c.Fail("Shadowsocks 必须设置密码")
		return nil, false
	}
	settings.Put("password", pw)
	return settings, true
}
