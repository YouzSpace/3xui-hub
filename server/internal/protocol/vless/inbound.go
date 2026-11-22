package vless

import p "controlhub-server/internal/protocol"

// buildInbound vless 入站：只有 decryption 一项（缺省 none）。
// 客户端凭据在用户维度（RelayKey=id），不在入站 settings 里。
func buildInbound(c *p.InboundCtx) (*p.Object, bool) {
	return p.New("decryption", c.DefaultStr("decryption")), true
}
