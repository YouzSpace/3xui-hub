// Package xray 是 xray 裸内核节点的驱动层。
//
// 逐条对齐 backend/app/Drivers/Xray/：
//
//	XrayDriver.php          面板驱动契约（用户级操作走指令队列）
//	XrayConfigService.php   全量 config 渲染（入站/出站/路由/WARP/观测器）+ 订阅链接
//	XrayRelayService.php    中转自动化（源节点出站 + 目标节点承接凭据）
//	RealityKeyService.php   X25519 密钥对 / shortId / 私钥推公钥
//	RealityTargetCatalog.php Reality 候选目标目录
//	WarpService.php         Cloudflare WARP 注册与 wireguard 出站
//
// 与 PHP 的分工差异：xray 节点没有可请求的面板 API —— 用户级增删走 node_api_commands
// 指令队列（由 nodeapi 包负责投递），本包只负责「渲染」与「凭据/密钥」这类纯计算。
// 因此 XrayDriver 不依赖数据库，全部输入通过结构体传入（Data 类型），
// 便于单测与逐字节对拍。
package xray

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
	"controlhub-server/internal/driver/internal/secrets"
)

// API_PORT 是 xray 内核 API 的本地回环端口（v26 CLI 必须显式 --server 指向这里）。
const API_PORT = 10085

// Data 是 render() 的全部输入（一次查询集齐，避免渲染期间数据漂移）。
//
// 切片顺序就是数据库返回顺序，必须与 PHP 侧一致：
//   - Users           eligibleUserRows() 无 orderBy → 由 MySQL 决定（两级查询同源即同序）
//   - Inbounds/Outbounds/Balancers/Rules  enabledFor() 的 sort, id
//   - Subscriptions   enabledFor() 的 priority, id
//   - RelayClients    clientsForInbound() 的 id
type Data struct {
	Node          Node
	Users         []User
	Inbounds      []Inbound
	Outbounds     []Outbound
	Balancers     []Balancer
	RoutingRules  []RoutingRule
	Subscriptions []OutboundSubscription
	RelayClients  []RelayClient
	// Certificates 是 TLS 证书库（id → 已解密材料），供入站按 tlsCertId 引用。
	// 私钥只在这里出现，绝不落进 xray_inbounds.stream_settings。
	Certificates map[int64]Certificate
}

// Certificate 是 xray_certificates 的一行（私钥保持密文，渲染时解密）。
type Certificate struct {
	ID      int64
	CertPEM string
	KeyEnc  string
}

// Node 是渲染用到的节点字段。
type Node struct {
	ID          int64
	Name        string
	Host        string
	DriverType  string
	ConfigRaw   []byte // driver_config 原始 JSON 文本（nil = NULL）
	ConfigVer   int64
	RoutingOpts RoutingOptions
}

// RoutingOptions 是 driver_config.xray_routing 的口径（缺省 AsIs / 空）。
type RoutingOptions struct {
	DomainStrategy string
	DomainMatcher  string
}

// User 是 config 实际注入的用户列（与 eligibleUserRows() 完全同源）。
type User struct {
	ID           int64
	UUID         string
	Protocol     string
	TrafficLimit int64
	ExpiredAt    *time.Time
}

// Inbound 是启用入站的一行。
//
// ID 只用于把中转凭据（xray_relay_clients.inbound_id）挂回对应入站，不参与渲染输出。
type Inbound struct {
	ID                int64
	Tag               string
	Remark            *string
	Protocol          string
	Port              int64
	Listen            string
	Settings          json.RawMessage
	StreamSettings    json.RawMessage
	Sniffing          json.RawMessage
	RealityPrivateKey *string // Laravel 密文（渲染时解密）
}

// Outbound 是启用出站的一行。
type Outbound struct {
	Tag            string
	Protocol       string
	Settings       json.RawMessage
	StreamSettings json.RawMessage
	SecretKey      *string // Laravel 密文（wireguard 私钥）
}

// Balancer 是启用负载均衡器。
type Balancer struct {
	Tag         string
	Selectors   json.RawMessage
	Strategy    string
	FallbackTag *string
}

// RoutingRule 是启用路由规则。
type RoutingRule struct {
	Domains     json.RawMessage
	IPs         json.RawMessage
	Port        *string
	Network     *string
	Protocol    *string
	InboundTag  *string
	OutboundTag string
}

// OutboundSubscription 是启用出站订阅（只需要 prepend 与最近一次拉取结果）。
type OutboundSubscription struct {
	Prepend bool
	Fetched json.RawMessage
}

// RelayClient 是中转凭据（按 id 排序，只用于向对应入站注入 client）。
type RelayClient struct {
	InboundID int64
	ClientID  string
}

// streamEmptyObjectKeys 是必须输出 {} 的结构体字段。
//
// PHP 空数组会被编码成 []，xray 对 tlsSettings 这类结构体字段报
// "cannot unmarshal array into ..." 并拒绝加载整份配置。
var streamEmptyObjectKeys = []string{
	"tlsSettings", "realitySettings", "wsSettings", "xhttpSettings",
	"httpupgradeSettings", "grpcSettings", "tcpSettings", "sockopt",
}

// realityAdvDefaults 是 Reality 高级字段的缺省语（3x-ui reality.ts 字段集，内核默认值）。
var realityAdvDefaults = phpjson.Object{
	{Key: "xver", Value: 0},
	{Key: "show", Value: false},
	{Key: "minClientVer", Value: ""},
	{Key: "maxClientVer", Value: ""},
	{Key: "maxTimediff", Value: 0},
	{Key: "mldsa65Seed", Value: ""},
}

// Render 复刻 XrayConfigService::render()（数组形态，保序）。
//
// key 是 APP_KEY 解出的 32 字节 AES 密钥（用于解密 reality_private_key / wireguard secret_key）；
// 传 nil 时保持密文原样（与 PHP 解密失败退回原值的口径一致）。
func Render(d *Data, key []byte) (phpjson.Object, error) {
	outbounds, err := renderOutbounds(d, key)
	if err != nil {
		return nil, err
	}
	routing, err := renderRouting(d)
	if err != nil {
		return nil, err
	}
	inbounds, err := renderInbounds(d, key)
	if err != nil {
		return nil, err
	}

	out := phpjson.Object{
		{Key: "log", Value: phpjson.Object{{Key: "loglevel", Value: "warning"}}},
		{Key: "api", Value: phpjson.Object{
			{Key: "tag", Value: "api"},
			{Key: "listen", Value: "127.0.0.1:" + phpval.Itoa(API_PORT)},
			{Key: "services", Value: phpjson.Array{"HandlerService", "StatsService"}},
		}},
		{Key: "stats", Value: phpjson.Object{}},
		{Key: "policy", Value: PolicyConfig()},
		{Key: "outbounds", Value: outbounds},
		{Key: "routing", Value: routing},
		{Key: "inbounds", Value: inbounds},
	}
	for _, m := range renderObservatory(d) {
		out = append(out, m)
	}
	return out, nil
}

// RenderJSON 复刻 renderJson()：JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE。
func RenderJSON(d *Data, key []byte) ([]byte, error) {
	o, err := Render(d, key)
	if err != nil {
		return nil, err
	}
	return phpjson.EncodeUnescaped(o)
}

// ETag 复刻 etag()：`"` + sha1(renderJson()) + `"`（agent 用 If-None-Match 对比）。
func ETag(d *Data, key []byte) (string, error) {
	body, err := RenderJSON(d, key)
	if err != nil {
		return "", err
	}
	return `"` + sha1Hex(string(body)) + `"`, nil
}

// PolicyConfig 复刻 policyConfig()：用户级统计（levels.0）+ 入/出站级统计（system）。
//
// system 段是「出站流量」功能的数据源。
func PolicyConfig() phpjson.Object {
	return phpjson.Object{
		{Key: "levels", Value: phpjson.Object{{Key: "0", Value: phpjson.Object{
			{Key: "statsUserUplink", Value: true},
			{Key: "statsUserDownlink", Value: true},
		}}}},
		{Key: "system", Value: phpjson.Object{
			{Key: "statsInboundUplink", Value: true},
			{Key: "statsInboundDownlink", Value: true},
			{Key: "statsOutboundUplink", Value: true},
			{Key: "statsOutboundDownlink", Value: true},
		}},
	}
}

// ── 入站 ────────────────────────────────────────────────────

func renderInbounds(d *Data, key []byte) (phpjson.Array, error) {
	out := phpjson.Array{}
	for _, ib := range d.Inbounds {
		frag, err := renderInbound(d, ib, key, true)
		if err != nil {
			return nil, err
		}
		out = append(out, frag)
	}
	return out, nil
}

// renderInbound 渲染单个入站片段。
//
// withStream=false 是 adu 指令载荷的形态（入站已存在，不需要 streamSettings）。
func renderInbound(d *Data, ib Inbound, key []byte, withStream bool) (phpjson.Object, error) {
	settingsVal, err := phpjson.ParseOrdered(ib.Settings)
	if err != nil {
		return nil, err
	}
	settings := phpval.ToObject(settingsVal)

	clients, hasClients, err := clientsFor(d, ib)
	if err != nil {
		return nil, err
	}
	if hasClients {
		settings = settings.Clone()
		settings.Set("clients", clients)
	}

	frag := phpjson.Object{
		{Key: "tag", Value: ib.Tag},
		{Key: "listen", Value: listenOr(ib.Listen)},
		{Key: "port", Value: ib.Port},
		{Key: "protocol", Value: ib.Protocol},
	}
	if len(settings) == 0 {
		frag = append(frag, phpjson.Member{Key: "settings", Value: phpjson.Object{}})
	} else {
		frag = append(frag, phpjson.Member{Key: "settings", Value: settings})
	}

	if withStream {
		streamVal, err := phpjson.ParseOrdered(ib.StreamSettings)
		if err != nil {
			return nil, err
		}
		stream := phpval.ToObject(streamVal)
		stream = stream.Clone()

		if phpval.String(mustGet(stream, "security")) == "reality" {
			rs, err := RealitySettingsFor(ib, key)
			if err != nil {
				return nil, err
			}
			stream.Set("realitySettings", rs)
		}
		applyCertificateRef(stream, d.Certificates, key)
		for _, subKey := range streamEmptyObjectKeys {
			if v, ok := stream.Get(subKey); ok {
				if arr, isArr := v.(phpjson.Array); isArr && len(arr) == 0 {
					stream.Set(subKey, phpjson.Object{})
				}
			}
		}
		if len(stream) > 0 {
			frag = append(frag, phpjson.Member{Key: "streamSettings", Value: stream})
		}

		sniffVal, err := phpjson.ParseOrdered(ib.Sniffing)
		if err != nil {
			return nil, err
		}
		if phpval.Empty(sniffVal) {
			frag = append(frag, phpjson.Member{Key: "sniffing", Value: defaultSniffing()})
		} else {
			frag = append(frag, phpjson.Member{Key: "sniffing", Value: sniffVal})
		}
	}

	return frag, nil
}

// ADUInboundFragment 复刻 aduInboundFragment()：adu 指令用的入站片段（单用户，不含 streamSettings）。
//
// 格式沿用 P1 真机验证过的契约：{"inbounds":[{完整入站片段, clients:[单用户]}]}。
//
// relayClients 要传「该入站上已存在的全部中转凭据」：PHP 的 renderInbound 在注入
// clients 后**无条件**追加 XrayRelayClient::clientsForInbound($inbound) 的条目，
// 少传会让一次 adu 把中转凭据从内核里抹掉（中转链路当场断）。
func ADUInboundFragment(ib Inbound, user User, relayClients []RelayClient, key []byte) (phpjson.Object, error) {
	one := &Data{
		Users:        []User{user},
		RelayClients: relayClients,
	}
	return renderInbound(one, ib, key, false)
}

func defaultSniffing() phpjson.Object {
	return phpjson.Object{
		{Key: "enabled", Value: true},
		{Key: "destOverride", Value: phpjson.Array{"http", "tls", "quic"}},
	}
}

// mustGet 复刻 `$x['k'] ?? null` 里「键不存在」的语义。
func mustGet(o phpjson.Object, k string) any {
	v, ok := o.Get(k)
	if !ok {
		return nil
	}
	return v
}

// listenOr 复刻 `$inbound->listen ?: '0.0.0.0'`。
func listenOr(listen string) string {
	if phpval.Empty(listen) {
		return "0.0.0.0"
	}
	return listen
}

// clientsFor 复刻 clientsFor()：shadowsocks 单凭据模式不注入 clients。
// 第二个返回值表示是否注入（对应 PHP 的 null）。
func clientsFor(d *Data, ib Inbound) (phpjson.Array, bool, error) {
	if ib.Protocol == "shadowsocks" {
		return nil, false, nil
	}

	clients := phpjson.Array{}
	for _, u := range d.Users {
		// 用户全协议通用：每个入站都给所有人注入 client，不按 users.protocol 过滤。
		var client phpjson.Object
		switch ib.Protocol {
		case "vless":
			client = phpjson.Object{
				{Key: "id", Value: u.UUID},
				{Key: "email", Value: ClientEmail(u.ID)},
				{Key: "totalGB", Value: u.TrafficLimit},
				{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
			}
			if FlowApplies(ib) {
				client = append(client, phpjson.Member{Key: "flow", Value: Flow})
			}
		case "vmess":
			client = phpjson.Object{
				{Key: "id", Value: u.UUID},
				{Key: "email", Value: ClientEmail(u.ID)},
				{Key: "totalGB", Value: u.TrafficLimit},
				{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
			}
		case "trojan":
			// trojan 以密码定位用户：统一用 uuid 作密码（面板侧保证唯一）
			client = phpjson.Object{
				{Key: "password", Value: u.UUID},
				{Key: "email", Value: ClientEmail(u.ID)},
				{Key: "totalGB", Value: u.TrafficLimit},
				{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
			}
		case "hysteria":
			// Hysteria2 以 auth 令牌定位用户（键名是 auth，不是 password/id）：
			// 沿用 uuid 作令牌，与订阅侧 hysteria2:// 链接的密码同源。
			client = phpjson.Object{
				{Key: "auth", Value: u.UUID},
				{Key: "email", Value: ClientEmail(u.ID)},
				{Key: "totalGB", Value: u.TrafficLimit},
				{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
			}
		default:
			// 未知协议：PHP 的 match 返回 null，不注入该用户。
			continue
		}
		clients = append(clients, client)
	}

	// 中转凭据：面板在目标入站上为来源节点自动生成的 client。
	for _, rc := range d.RelayClients {
		if rc.InboundID != ib.ID {
			continue
		}
		switch ib.Protocol {
		case "trojan":
			clients = append(clients, phpjson.Object{{Key: "password", Value: rc.ClientID}})
		case "hysteria":
			clients = append(clients, phpjson.Object{{Key: "auth", Value: rc.ClientID}})
		default:
			clients = append(clients, phpjson.Object{{Key: "id", Value: rc.ClientID}})
		}
	}

	return clients, true, nil
}

// ClientEmail 复刻 clientEmail(): 'ch_user_{id}'（data-contract.md §1）。
func ClientEmail(userID int64) string { return "ch_user_" + phpval.Itoa(userID) }

// expiryZulu 复刻 expiryZulu(): nil → 0，否则 toIso8601ZuluString。
func expiryZulu(t *time.Time) any {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// FlowApplies 复刻 flowApplies()：仅 vless + tcp + reality/tls。
func FlowApplies(ib Inbound) bool {
	if ib.Protocol != "vless" {
		return false
	}
	streamVal, err := phpjson.ParseOrdered(ib.StreamSettings)
	if err != nil {
		return false
	}
	stream := phpval.ToObject(streamVal)
	network := "tcp"
	if v, ok := stream.Get("network"); ok && phpval.String(v) != "" {
		network = phpval.String(v)
	}
	security := phpval.String(mustGet(stream, "security"))
	if security == "" {
		security = "none"
	}
	return network == "tcp" && (security == "reality" || security == "tls")
}

// CertRefKey 是 tlsSettings 里「引用证书库」的键（只存证书库 id，不存任何 PEM）。
const CertRefKey = "tlsCertId"

// applyCertificateRef 把 tlsSettings.tlsCertId 展开成内核要的 certificates 段。
//
// 入站表的 stream_settings 只存证书库 id（私钥加密在 xray_certificates.key_enc），
// 渲染时才把 PEM 拼进来 —— 这样 `SELECT stream_settings ... WHERE protocol='hysteria'`
// 里永远看不到私钥。tlsCertId 本身不是 xray 认识的键，无论是否命中都必须摘掉。
func applyCertificateRef(stream phpjson.Object, certs map[int64]Certificate, key []byte) {
	tls := phpval.ToObject(mustGet(stream, "tlsSettings"))
	raw, ok := tls.Get(CertRefKey)
	if !ok {
		return
	}
	id := phpval.Int(raw)
	out := tls.Clone()
	if idx := indexOfKey(out, CertRefKey); idx >= 0 {
		out = append(out[:idx], out[idx+1:]...)
	}
	cert, found := certs[id]
	keyPEM := ""
	if found && cert.KeyEnc != "" {
		// 与 reality_private_key 同口径：解不开就当存量明文原样用。
		keyPEM = secrets.DecryptOrRaw(cert.KeyEnc, key)
	}
	if cert.CertPEM != "" && keyPEM != "" {
		out.Set("certificates", phpjson.Array{phpjson.Object{
			{Key: "certificate", Value: phpjson.Array{cert.CertPEM}},
			{Key: "key", Value: phpjson.Array{keyPEM}},
		}})
	}
	stream.Set("tlsSettings", out)
}

// indexOfKey 返回对象里键的位置（不存在 → -1）。
func indexOfKey(o phpjson.Object, key string) int {
	for i, m := range o {
		if m.Key == key {
			return i
		}
	}
	return -1
}

// RealitySettingsFor 复刻 realitySettingsFor()：公开参数来自入站，私钥解密注入。
func RealitySettingsFor(ib Inbound, key []byte) (phpjson.Object, error) {
	streamVal, err := phpjson.ParseOrdered(ib.StreamSettings)
	if err != nil {
		return nil, err
	}
	stream := phpval.ToObject(streamVal)
	public := phpval.ToObject(mustGet(stream, "realitySettings"))

	dest := phpval.String(mustGet(public, "dest"))
	if dest == "" {
		dest = DefaultDest
	}

	serverNames := phpjson.Array{}
	if raw, ok := public.Get("serverNames"); ok {
		for _, v := range phpval.ToArrayValues(raw) {
			if s, isStr := v.(string); isStr {
				serverNames = append(serverNames, s)
			}
		}
	} else {
		serverNames = append(serverNames, DefaultSNI)
	}

	shortIDs := phpjson.Array{}
	if raw, ok := public.Get("shortIds"); ok {
		for _, v := range phpval.ToArrayValues(raw) {
			if s, isStr := v.(string); isStr {
				shortIDs = append(shortIDs, s)
			}
		}
	}

	out := phpjson.Object{
		{Key: "dest", Value: dest},
		{Key: "serverNames", Value: serverNames},
		{Key: "shortIds", Value: shortIDs},
	}
	if pk := realityPrivateKey(ib, key); pk != "" {
		out = append(out, phpjson.Member{Key: "privateKey", Value: pk})
	}

	// 高级字段：仅写「显式设置且非内核默认」的值。
	//
	// 空字符串（minClientVer/maxClientVer/mldsa65Seed 未设置时的形态）与 maxTimediff=0
	// 曾原样写入配置，实测会让内核把空版本窗当非法认证条件、reality 握手静默拒绝
	// （26.3.27 复现）—— 缺省即不写，与 3x-ui 落库行为对齐。
	for _, def := range realityAdvDefaults {
		v, ok := public.Get(def.Key)
		if !ok || v == nil || phpval.String(v) == "" || phpval.EqualScalar(v, def.Value) {
			continue
		}
		out = append(out, phpjson.Member{Key: def.Key, Value: v})
	}

	if mkl := trimSpace(phpval.String(mustGet(public, "masterKeyLog"))); mkl != "" {
		out = append(out, phpjson.Member{Key: "masterKeyLog", Value: mkl})
	}

	for _, k := range []string{"limitFallbackUpload", "limitFallbackDownload"} {
		blockRaw, ok := public.Get(k)
		if !ok {
			continue
		}
		block, isObj := blockRaw.(phpjson.Object)
		if !isObj {
			continue
		}
		out = append(out, phpjson.Member{Key: k, Value: phpjson.Object{
			{Key: "afterBytes", Value: phpval.Int(mustGet(block, "afterBytes"))},
			{Key: "bytesPerSec", Value: phpval.Int(mustGet(block, "bytesPerSec"))},
			{Key: "burstBytesPerSec", Value: phpval.Int(mustGet(block, "burstBytesPerSec"))},
		}})
	}

	return out, nil
}

// realityPrivateKey 复刻 realityPrivateKey()：解密失败时退回原始值（兼容明文存量）。
func realityPrivateKey(ib Inbound, key []byte) string {
	if ib.RealityPrivateKey == nil || *ib.RealityPrivateKey == "" {
		return ""
	}
	if len(key) != 32 {
		return *ib.RealityPrivateKey
	}
	return secrets.DecryptOrRaw(*ib.RealityPrivateKey, key)
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// trimSpace 是 strings.TrimSpace 的别名（保持本文件对 PHP trim() 的语义标注）。
func trimSpace(s string) string { return strings.TrimSpace(s) }
