package hysteria

import p "controlhub-server/internal/protocol"

// buildInbound hysteria 入站：内核只接受 version=2；写 1 会让**整份节点配置**
// 被拒（不是只挂这个入站）。默认值来自 Spec.Defaults。
func buildInbound(c *p.InboundCtx) (*p.Object, bool) {
	return p.New("version", c.DefaultInt("version")), true
}
