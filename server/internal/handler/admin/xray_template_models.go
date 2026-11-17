package admin

// 配置模板（#地区功能-19）的数据模型。
//
// 模板 = 一套「标准入站 / 出站 / 路由」的原始 config 片段，与具体节点无关，
// 同一个模板可以套到任意多台 xray 节点上。三条硬约束：
//  1. reality 私钥**不在模板里**：套用时每个节点、每条入站现场生成一把，
//     模板里带私钥等于全网共用一把钥匙；
//  2. 3x-ui 节点没有这套表的对应概念，套用时整台跳过（不报错、不改它的数据）；
//  3. 三张 xray_template_* 表都是字典表，由 ensureSchema 建 —— 升级后保持为空，
//     不套用模板的节点行为与升级前完全一致。

import "time"

// 模板种类（API 的 :kind 段）。
const (
	xrayTemplateKindInbound  = "inbound"
	xrayTemplateKindOutbound = "outbound"
	xrayTemplateKindRouting  = "routing"
)

// 套用结果状态。
const (
	// applied：已套用（入站/出站/路由都按模板重写，节点恢复「跟随模板」）。
	xrayTemplateApplyApplied = "applied"
	// skipped：没动这个节点（3x-ui 节点 / 单独配置过且没要求覆盖）。
	xrayTemplateApplySkipped = "skipped"
	// failed：该节点的事务失败已回滚（其它节点不受影响）。
	xrayTemplateApplyFailed = "failed"
)

// XrayTemplateInbound 是 xray_template_inbounds 一行（原始 config 片段）。
//
// 刻意不含 reality_private_key / salamander_password / client_host：
// 前两个是每节点独立的密钥材料，套用时现场生成；client_host 与节点地址相关。
type XrayTemplateInbound struct {
	ID             uint64
	Name           string
	Sort           int64
	Tag            string
	Remark         *string
	Protocol       string
	Port           int64
	Listen         string
	Settings       []byte
	StreamSettings []byte
	Sniffing       []byte
	Enabled        bool
	CreatedAt      *time.Time
	UpdatedAt      *time.Time
}

// XrayTemplateOutbound 是 xray_template_outbounds 一行。
type XrayTemplateOutbound struct {
	ID             uint64
	Name           string
	Sort           int64
	Tag            string
	Protocol       string
	Settings       []byte
	StreamSettings []byte
	Enabled        bool
	CreatedAt      *time.Time
	UpdatedAt      *time.Time
}

// XrayTemplateRouting 是 xray_template_routing 一行。
type XrayTemplateRouting struct {
	ID          uint64
	Name        string
	Sort        int64
	Domains     []byte
	IPs         []byte
	Port        *string
	Network     *string
	Protocol    *string
	InboundTag  *string
	OutboundTag string
	Remark      *string
	Enabled     bool
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
}

// XrayTemplateSet 是三类模板的合集（一次查询取齐，套用时整份用同一快照）。
type XrayTemplateSet struct {
	Inbounds  []XrayTemplateInbound
	Outbounds []XrayTemplateOutbound
	Routing   []XrayTemplateRouting
}

// IsEmpty 判断整份模板是否什么都没有（套用空模板会把节点配置删光，必须拦住）。
func (s *XrayTemplateSet) IsEmpty() bool {
	return s == nil || (len(s.Inbounds) == 0 && len(s.Outbounds) == 0 && len(s.Routing) == 0)
}

// XrayTemplateApplyResult 是「套用到某个节点」的结果。
type XrayTemplateApplyResult struct {
	NodeID    uint64
	NodeName  string
	Status    string
	Message   string
	Inbounds  int
	Outbounds int
	Rules     int
}

// ---- 写入入参 ----

// XrayTemplateInboundInput 是新增/修改一条入站模板的入参（PUT 为整行替换）。
type XrayTemplateInboundInput struct {
	Name           string
	Sort           int64
	Tag            string
	Remark         *string
	Protocol       string
	Port           int64
	Listen         string
	Settings       []byte
	StreamSettings []byte
	Sniffing       []byte
	Enabled        *bool
}

// XrayTemplateOutboundInput 是新增/修改一条出站模板的入参。
type XrayTemplateOutboundInput struct {
	Name           string
	Sort           int64
	Tag            string
	Protocol       string
	Settings       []byte
	StreamSettings []byte
	Enabled        *bool
}

// XrayTemplateRoutingInput 是新增/修改一条路由模板的入参。
type XrayTemplateRoutingInput struct {
	Name        string
	Sort        int64
	Domains     []byte
	IPs         []byte
	Port        *string
	Network     *string
	Protocol    *string
	InboundTag  *string
	OutboundTag string
	Remark      *string
	Enabled     *bool
}
