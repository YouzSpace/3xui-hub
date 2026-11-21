package admin

// 中转自动化（「目标来源 = 本站服务器」）。
//
// 逐条对齐 backend/app/Drivers/Xray/XrayRelayService.php 的**落库部分**：
//   - 目标节点建承接凭据（xray：xray_relay_clients；3x-ui：面板 addClient + attach）
//   - 源节点 upsert 中转出站（幂等键 = 源节点 × 目标节点 × 目标入站）
//
// 纯计算部分（settings/streamSettings 组装、tag 决策）复用 internal/driver/xray 的
// EnsureRelayFor* —— 那是 P5 已对拍过的实现，这里只负责喂数据与落库。

import (
	"context"
	"errors"

	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
)

// relayFailure 是「目标不合法」的业务错误（对应 PHP 的 ValidationException）。
type relayFailure struct {
	Message string
}

func (e *relayFailure) Error() string { return e.Message }

func errRelay(msg string) error { return &relayFailure{Message: msg} }

// asRelayFailure 判断错误是否为业务失败。
func asRelayFailure(err error, target **relayFailure) bool {
	return errors.As(err, target)
}

// ensureRelay 对齐 XrayRelayService::ensureRelay()：返回源节点上的中转出站。
func (d *NodeDomain) ensureRelay(c *gin.Context, source, target *NodeRecord, targetInboundID uint64, customTag, customRemark string, existing *XrayOutboundRecord) (*XrayOutboundRecord, error) {
	ctx := ctxOf(c)

	tags, err := d.store.XrayOutboundTags(ctx, source.ID)
	if err != nil {
		return nil, err
	}

	opts := xray.RelayOptions{
		SourceName:   source.Name,
		SourceTags:   tags,
		TargetNodeID: int64(target.ID),
		TargetHost:   target.Host,
		TargetName:   target.Name,
		RelayExists:  existing != nil,
		CustomTag:    customTag,
		CustomRemark: customRemark,
	}

	var plan xray.RelayPlan
	if target.IsXray() {
		inbound, err := d.store.XrayInboundByID(ctx, target.ID, targetInboundID)
		if err != nil {
			return nil, errRelay("目标入站不存在")
		}
		if !inbound.Enabled {
			return nil, errRelay("目标入站未启用")
		}
		clientID, err := d.ensureXrayRelayClient(c, source, target, inbound)
		if err != nil {
			return nil, err
		}
		opts.TargetPort = inbound.Port
		plan, err = xray.EnsureRelayForXrayInbound(toXrayInbound(inbound), clientID, opts)
		if err != nil {
			return nil, wrapRelayErr(err)
		}
	} else {
		row, ok := findNodeInbound(target, int64(targetInboundID))
		if !ok {
			return nil, errRelay("目标节点的该入站不存在")
		}
		// 3x-ui 面板入站清单（端口 / tag / streamSettings 都在面板侧）。
		stream := map[string]any{}
		port := int64(0)
		inboundTag := ""
		inbounds, err := d.ops.ThreeXInbounds(ctx, target)
		if err == nil {
			for _, in := range inbounds {
				if jsonInt(in["id"]) != row.InboundID {
					continue
				}
				port = jsonInt(in["port"])
				inboundTag = nodeScalarString(in["tag"])
				if raw, ok := in["streamSettings"]; ok {
					stream = normalizeThreeXStream(raw)
				}
				break
			}
		}
		if port == 0 {
			port = 443
		}

		email := truncateRunes("relay_"+uInt64String(source.ID)+"_"+uInt64String(uint64(row.InboundID)), 128)
		clientID, err := d.ops.ThreeXEnsureRelayClient(ctx, target, row.InboundID, row.Protocol, email)
		if err != nil {
			return nil, err
		}
		opts.TargetPort = port
		plan, err = xray.EnsureRelayForThreeXInbound(row.Protocol, row.InboundID, port, inboundTag, stream, clientID, opts)
		if err != nil {
			return nil, wrapRelayErr(err)
		}
	}

	tag := plan.Tag
	if existing != nil {
		tag = existing.Tag
	}
	input := XrayOutboundInput{
		NodeID:               source.ID,
		Tag:                  tag,
		Protocol:             plan.Protocol,
		Settings:             phpEncode(plan.Settings),
		StreamSettings:       phpEncode(plan.StreamSettings),
		Enabled:              ptrBoolOf(true),
		RelayTargetNodeID:    ptrUintOf(target.ID),
		RelayTargetInboundID: ptrUintOf(targetInboundID),
	}
	if plan.Remark != "" {
		input.Remark = ptrStringOf(plan.Remark)
	} else if existing == nil {
		// PHP 的 ensureRelayOutbound 传 $remark = ''（非 null）→ 落库空串；
		// 唯一列/展示上等价于「无备注」，这里保持与 PHP 相同的空串语义。
		input.Remark = ptrStringOf("")
	}

	return d.store.UpsertRelayOutbound(ctx, input)
}

// wrapRelayErr 把驱动的 RelayError 转成本域的业务失败。
func wrapRelayErr(err error) error {
	var relErr *xray.RelayError
	if errors.As(err, &relErr) {
		return errRelay(relErr.Message)
	}
	return err
}

// ensureXrayRelayClient 对齐 ensureXrayRelayClient()：幂等复用目标入站上的承接凭据。
func (d *NodeDomain) ensureXrayRelayClient(c *gin.Context, source, target *NodeRecord, inbound *XrayInboundRecord) (string, error) {
	ctx := ctxOf(c)

	existing, err := d.store.RelayClientBySource(ctx, target.ID, inbound.ID, source.ID)
	if err != nil && !d.noRowsToNotFoundSilent(err) {
		return "", err
	}
	if existing != nil {
		return existing.ClientID, nil
	}

	clientID := ""
	if inbound.Protocol == "trojan" {
		clientID = randomAlnum(32)
	} else {
		clientID = randomUUID()
	}
	email := truncateRunes("relay_"+uInt64String(source.ID)+"_"+uInt64String(target.ID)+"_"+uInt64String(inbound.ID), 128)

	if err := d.store.InsertRelayClient(ctx, RelayClientInput{
		NodeID:       target.ID,
		InboundID:    inbound.ID,
		Email:        email,
		ClientID:     clientID,
		SourceNodeID: source.ID,
	}); err != nil {
		return "", err
	}

	// 凭据同时影响两侧渲染：目标节点入站 + 源节点出站。
	if _, err := d.store.BumpConfigVersion(ctx, target.ID); err != nil {
		return "", err
	}
	if _, err := d.store.BumpConfigVersion(ctx, source.ID); err != nil {
		return "", err
	}
	return clientID, nil
}

// toXrayInbound 把记录转成驱动层的入站视图。
func toXrayInbound(ib *XrayInboundRecord) xray.Inbound {
	out := xray.Inbound{
		ID:                int64(ib.ID),
		Tag:               ib.Tag,
		Remark:            ib.Remark,
		Protocol:          ib.Protocol,
		Port:              ib.Port,
		Listen:            ib.Listen,
		Settings:          ib.Settings,
		StreamSettings:    ib.StreamSettings,
		Sniffing:          ib.Sniffing,
		RealityPrivateKey: ib.RealityPrivateKey,
	}
	return out
}

// findNodeInbound 在节点入站里找某 inbound_id（3x-ui 的 inbound_id 是面板侧 id）。
func findNodeInbound(n *NodeRecord, inboundID int64) (NodeInboundRecord, bool) {
	for _, row := range n.Inbounds {
		if row.InboundID == inboundID {
			return row, true
		}
	}
	return NodeInboundRecord{}, false
}

// normalizeThreeXStream 复刻 normalizeStream()：3x-ui 的 list 响应里 streamSettings
// 可能是 JSON 字符串（历史版本为对象），统一成 map 供驱动组装。
func normalizeThreeXStream(raw any) map[string]any {
	switch t := raw.(type) {
	case map[string]any:
		return t
	case string:
		parsed, err := parseOrderedJS([]byte(t))
		if err != nil {
			return map[string]any{}
		}
		obj, ok := parsed.(*jsObj)
		if !ok {
			return map[string]any{}
		}
		out := make(map[string]any, len(obj.Keys()))
		keys := obj.Keys()
		vals := obj.Vals()
		for i, k := range keys {
			out[k] = jsToNative(vals[i])
		}
		return out
	}
	return map[string]any{}
}

// jsToNative 把保序结构转成普通 map/slice（driver 侧按 map[string]any 读）。
func jsToNative(v any) any {
	switch t := v.(type) {
	case *jsObj:
		out := make(map[string]any, len(t.Keys()))
		keys := t.Keys()
		vals := t.Vals()
		for i, k := range keys {
			out[k] = jsToNative(vals[i])
		}
		return out
	case jsArr:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = jsToNative(item)
		}
		return out
	}
	return v
}

// randByte 由 math/rand 提供（与 randomAlnum 同源，测试不要求可复现）。
func randByte() int { return randIntn(256) }

// sanitizeRelayCustomTag 对齐 validateCustomTag()：长度 + 非空 + 非保留字。
func sanitizeRelayCustomTag(tag string) string {
	tag = phpTrimSpaceOnly(tag)
	if tag == "" || len([]rune(tag)) > 64 {
		return ""
	}
	if outboundReservedTags[tag] || tag == "warp" {
		return ""
	}
	return tag
}

// randomUUID 复刻 (string) Str::uuid()（v4 形态）。
func randomUUID() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(randByte())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexDigits[v>>4], hexDigits[v&0xf])
	}
	return string(out)
}

// 编译期断言：确保 context 包在本文件被使用（ctxOf 返回 context.Context）。
var _ = context.Background
