package trojan

import p "controlhub-server/internal/protocol"

// buildInbound trojan 入站：settings 为空对象（凭据在用户维度）。
func buildInbound(*p.InboundCtx) (*p.Object, bool) { return p.New(), true }
