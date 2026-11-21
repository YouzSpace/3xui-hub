package admin

// 渲染数据装配：把库里的节点/入站/出站/路由/订阅数据转成驱动层 xray.Data。
//
// 只服务管理端两处「预览」用途：
//   - GET  xray-routing → render_preview.rules（下发预览，含渲染层自动注入的护栏）
//   - POST xray-routing/test → 按真实渲染口径逐条模拟匹配
//
// 这不是 /node-api/config 那条生产通道（P4 已有独立实现，带完整的 ETag/指纹链路）；
// 这里刻意只取「规则链」需要的字段，避免管理端预览把 10 万用户也一起查出来。

import (
	"encoding/json"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

// renderData 装配一份渲染用 Data（用户列表为空：预览只看结构，不需要 client 明细）。
func (d *NodeDomain) renderData(c *gin.Context, node *NodeRecord) (*xray.Data, error) {
	ctx := ctxOf(c)

	inbounds, err := d.store.XrayInboundsByNode(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	outbounds, err := d.store.XrayOutboundsByNode(ctx, node.ID, true)
	if err != nil {
		return nil, err
	}
	rules, err := d.store.RoutingRulesByNode(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	balancers, err := d.store.BalancersByNode(ctx, node.ID, true)
	if err != nil {
		return nil, err
	}
	subs, err := d.store.OutboundSubsByNode(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	relayClients, err := d.store.RelayClientsByNode(ctx, node.ID)
	if err != nil {
		return nil, err
	}

	configRaw := node.DriverConfigRaw
	if len(configRaw) == 0 {
		configRaw = nil
	}

	data := &xray.Data{
		Node: xray.Node{
			ID:         int64(node.ID),
			Name:       node.Name,
			Host:       node.Host,
			DriverType: node.DriverType,
			ConfigRaw:  configRaw,
			ConfigVer:  node.ConfigVersion,
			RoutingOpts: xray.RoutingOptions{
				DomainStrategy: jsMustString(routingSettings(node), "domainStrategy"),
				DomainMatcher:  jsMustString(routingSettings(node), "domainMatcher"),
			},
		},
		Inbounds:     make([]xray.Inbound, 0, len(inbounds)),
		Outbounds:    make([]xray.Outbound, 0, len(outbounds)),
		Balancers:    make([]xray.Balancer, 0, len(balancers)),
		RoutingRules: make([]xray.RoutingRule, 0, len(rules)),
	}

	for _, ib := range inbounds {
		if !ib.Enabled {
			continue
		}
		data.Inbounds = append(data.Inbounds, xray.Inbound{
			ID:                int64(ib.ID),
			Tag:               ib.Tag,
			Remark:            ib.Remark,
			Protocol:          ib.Protocol,
			Port:              ib.Port,
			Listen:            ib.Listen,
			Settings:          json.RawMessage(ib.Settings),
			StreamSettings:    json.RawMessage(ib.StreamSettings),
			Sniffing:          json.RawMessage(ib.Sniffing),
			RealityPrivateKey: ib.RealityPrivateKey,
		})
	}

	for _, ob := range outbounds {
		data.Outbounds = append(data.Outbounds, xray.Outbound{
			Tag:            ob.Tag,
			Protocol:       ob.Protocol,
			Settings:       json.RawMessage(ob.Settings),
			StreamSettings: json.RawMessage(ob.StreamSettings),
			SecretKey:      ob.SecretKey,
		})
	}

	for _, b := range balancers {
		if !b.Enabled {
			continue
		}
		data.Balancers = append(data.Balancers, xray.Balancer{
			Tag:         b.Tag,
			Selectors:   json.RawMessage(b.Selectors),
			Strategy:    b.Strategy,
			FallbackTag: b.FallbackTag,
		})
	}

	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		data.RoutingRules = append(data.RoutingRules, xray.RoutingRule{
			Domains:     json.RawMessage(r.Domains),
			IPs:         json.RawMessage(r.IPs),
			Port:        r.Port,
			Network:     r.Network,
			Protocol:    r.Protocol,
			InboundTag:  r.InboundTag,
			OutboundTag: r.OutboundTag,
		})
	}

	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		var fetched json.RawMessage
		if sub.Fetched != nil && *sub.Fetched != "" {
			fetched = json.RawMessage(*sub.Fetched)
		}
		data.Subscriptions = append(data.Subscriptions, xray.OutboundSubscription{
			Prepend: sub.Prepend,
			Fetched: fetched,
		})
	}

	for _, rc := range relayClients {
		data.RelayClients = append(data.RelayClients, xray.RelayClient{
			InboundID: int64(rc.InboundID),
			ClientID:  rc.ClientID,
		})
	}

	certs, err := d.store.XrayCertificates(ctx)
	if err != nil {
		return nil, err
	}
	data.Certificates = make(map[int64]xray.Certificate, len(certs))
	for _, c := range certs {
		data.Certificates[int64(c.ID)] = xray.Certificate{ID: int64(c.ID), CertPEM: c.CertPEM, KeyEnc: c.KeyEnc}
	}

	return data, nil
}

// mustString 从 jsObj 取字符串（用于 routingSettings 的字段取值）。
// 改为包级函数（jsObj 现是 phpjson.Object 的别名，不能再在本包为其挂方法）。
func jsMustString(o *jsObj, key string) string {
	v, ok := o.Get(key)
	if !ok {
		return ""
	}
	return nodeScalarString(v)
}
