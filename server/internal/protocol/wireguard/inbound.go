package wireguard

import p "controlhub-server/internal/protocol"

// buildInbound wireguard 入站：内核自带的 WireGuard 服务端。
//
// secretKey 是服务端身份私钥（等同于节点密码），只在库里与下发给节点的配置里出现，
// 读接口只回 wg_public_key。密钥生成是带副作用的业务动作，经 GenWireGuardKey 注入。
func buildInbound(c *p.InboundCtx) (*p.Object, bool) {
	secret := ""
	if c.Prev != nil {
		secret = c.PrevStr("secretKey")
	}
	if secret == "" {
		gen, err := c.GenWireGuardKey()
		if err != nil {
			c.FailServer("生成 WireGuard 密钥", err)
			return nil, false
		}
		secret = gen
	}
	peers, ok := ParsePeers(c.Input("wg_peers"))
	if !ok {
		c.Fail("WireGuard 对端格式错误：每行「公钥 [允许IP,...]」，公钥为 44 字符 base64")
		return nil, false
	}
	mtu := c.DefaultInt("mtu")
	if v, present := c.Int("wg_mtu"); present && v > 0 {
		mtu = v
	}
	// noKernelTun=true：内核目前只有 gVisor TUN 支持 WireGuard 入站（自带告警），
	// 显式写上，免得以后内核默认值变化把入站搞挂。
	settings := p.New("secretKey", secret, "mtu", mtu, "noKernelTun", true)
	if len(peers) > 0 {
		settings.Put("peers", peers)
	}
	return settings, true
}
