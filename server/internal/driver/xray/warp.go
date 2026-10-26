package xray

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
)

// 逐条对齐 backend/app/Drivers/Xray/WarpService.php。
//
// 异机架构正好复用现有通道：注册/换 IP 是纯 HTTP（调 CF API），
// 生成的 wireguard 出站由「面板渲染 config → 节点拉取」链路下发生效。
//
// 流程（对齐 3x-ui warp.go 的 CF API 口径）：
//
//	register → 生成 wireguard keypair → POST /reg → 存 device/token/peer/reserved
//	apply    → 组装 wireguard 出站（tag=warp）→ 写库 + bump config（落库由业务层做）
//	rotate   → 重新注册设备（换 IP）→ 申请回原 license → 重组出站 → bump
//
// Go 侧只做「注册 / 组装出站 / 设置 license」这三件纯 HTTP 与纯计算的事；
// 落库（node_warp_accounts、xray_outbounds、bump config_version）在业务层。

// CF WARP 注册 API（与 3x-ui 同源同版本头）。
const (
	warpAPIBase   = "https://api.cloudflareclient.com/v0a4005"
	warpClientVer = "a-6.30-3596"

	warpDefaultMTU = 1280
)

// WarpAccount 是 node_warp_accounts 的一行（解密后的明文形态）。
//
// AccessToken / PrivateKey 在库里是 Laravel 密文，由业务层用 secrets 解好后传入。
type WarpAccount struct {
	DeviceID      string
	AccessToken   string
	PrivateKey    string
	PublicKey     string
	PeerPublicKey string
	PeerEndpoint  string
	Addresses     []string
	Reserved      []int64
	ClientID      string
	LicenseKey    string
	Enabled       bool
}

// WarpService 是 CF WARP 的注册器。
type WarpService struct {
	// HTTPClient 可注入（测试用）。
	HTTPClient *http.Client
	// Now 可注入（测试固定 tos 时间戳用）。
	Now func() time.Time
	// APIBase 可注入（测试指向本地 fixture server）。
	APIBase string
}

// NewWarpService 构造服务（全部依赖有默认值）。
func NewWarpService() *WarpService {
	return &WarpService{}
}

func (s *WarpService) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (s *WarpService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *WarpService) apiBase() string {
	if s.APIBase != "" {
		return strings.TrimRight(s.APIBase, "/")
	}
	return warpAPIBase
}

// Register 复刻 register()：注册（或重新注册）WARP 设备。
//
// existing 是「注册前」的账户（可能为 nil）；其 license_key 会被保留，
// 并在有 WARP+ 授权时尝试回挂到新设备（失败不阻断注册 —— 免费账户仍可用）。
// nodeName 用于 CF 侧的设备名（截 64 字符）。
func (s *WarpService) Register(ctx context.Context, nodeName string, existing *WarpAccount) (*WarpAccount, error) {
	existingLicense := ""
	if existing != nil {
		existingLicense = existing.LicenseKey
	}

	keypair, err := WireGuardKeypair()
	if err != nil {
		return nil, fmt.Errorf("生成 wireguard 密钥对失败: %w", err)
	}
	privateKey, publicKey := keypair[0], keypair[1]

	name := nodeName
	if name == "" {
		name = "controlhub"
	}
	name = runeTruncate(name, 64)

	payload := map[string]any{
		"key":   publicKey,
		"tos":   s.now().Format("2006-01-02T15:04:05.000Z"),
		"type":  "PC",
		"model": "controlhub",
		"name":  name,
	}

	resp, status, err := s.doJSON(ctx, http.MethodPost, "/reg", payload, "")
	if err != nil {
		return nil, fmt.Errorf("WARP 注册失败：%w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("WARP 注册失败：CF API 返回 %d", status)
	}

	var body map[string]any
	if err := json.Unmarshal(resp, &body); err != nil {
		return nil, fmt.Errorf("WARP 注册失败：响应不是 JSON（%w）", err)
	}

	deviceID := phpval.String(body["id"])
	token := phpval.String(body["token"])
	if deviceID == "" || token == "" {
		return nil, errors.New("WARP 注册失败：响应缺少 device id / token")
	}

	config, _ := body["config"].(map[string]any)
	peer := firstObject(config["peers"])
	iface, _ := config["interface"].(map[string]any)
	addrs, _ := iface["addresses"].(map[string]any)

	addresses := []string{}
	if v4 := phpval.String(addrs["v4"]); v4 != "" {
		addresses = append(addresses, v4+"/32")
	}
	if v6 := phpval.String(addrs["v6"]); v6 != "" {
		addresses = append(addresses, v6+"/128")
	}

	clientID := phpval.String(config["client_id"])
	reserved := []int64{}
	if clientID != "" {
		decoded, err := base64.StdEncoding.DecodeString(clientID)
		if err == nil {
			for _, b := range decoded {
				reserved = append(reserved, int64(b))
			}
		}
	}

	peerEndpoint := ""
	if ep, ok := peer["endpoint"].(map[string]any); ok {
		// 端点优先用 IPv4 字面量（对齐 PHP endpointOf）：写域名会让 xray 在 A/AAAA 间
		// 随机抽一条，IPv4-only 节点抽到 IPv6 就 network is unreachable。
		peerEndpoint = endpointOf(ep)
	}

	account := &WarpAccount{
		DeviceID:      deviceID,
		AccessToken:   token,
		PrivateKey:    privateKey,
		PublicKey:     publicKey,
		PeerPublicKey: phpval.String(peer["public_key"]),
		PeerEndpoint:  peerEndpoint,
		Addresses:     addresses,
		Reserved:      reserved,
		ClientID:      clientID,
		LicenseKey:    existingLicense,
		Enabled:       true,
	}

	// CF 回执里的 account.license 优先（新设备自带 license），否则沿用原 license。
	if acct, ok := body["account"].(map[string]any); ok {
		if license := phpval.String(acct["license"]); license != "" {
			account.LicenseKey = license
		}
	}

	// 有原 license（WARP+）则回挂新设备（失败不阻断注册）
	if existingLicense != "" && len(existingLicense) >= 26 {
		if err := s.SetLicense(ctx, account, existingLicense); err != nil {
			// 忽略：免费账户仍可用
			_ = err
		}
	}

	return account, nil
}

// Apply 复刻 apply()：组装 wireguard 出站（tag=warp）。
//
// 返回的出站片段可直接落进 xray_outbounds（settings 已含 secretKey；
// 密文落库由业务层负责 —— secret_key 列要单独 Crypt::encryptString）。
func (s *WarpService) Apply(account *WarpAccount) (Outbound, error) {
	if account == nil {
		return Outbound{}, errors.New("尚未注册 WARP，请先注册")
	}
	if account.PrivateKey == "" {
		return Outbound{}, errors.New("WARP 账户缺少私钥，请重新注册")
	}

	settings := phpjson.Object{}
	if len(account.Addresses) > 0 {
		settings = append(settings, phpjson.Member{Key: "address", Value: stringArrayValue(account.Addresses)})
	}
	settings = append(settings, phpjson.Member{Key: "peers", Value: phpjson.Array{phpjson.Object{
		{Key: "publicKey", Value: account.PeerPublicKey},
		{Key: "endpoint", Value: account.PeerEndpoint},
	}}})
	if len(account.Reserved) > 0 {
		settings = append(settings, phpjson.Member{Key: "reserved", Value: intArray(account.Reserved)})
	}
	settings = append(settings, phpjson.Member{Key: "mtu", Value: warpDefaultMTU})
	// 对齐 PHP 修复后的 apply()：不写 domainStrategy 即 FORCE_IP，v4/v6 同时启用，
	// 隧道内目标解析随机落两族之一（IPv4-only 节点约一半概率不通）。
	settings = append(settings, phpjson.Member{Key: "domainStrategy", Value: "ForceIPv4v6"})
	// 与面板连通性测试走同一条数据路径（测试固定 noKernelTun=true），否则「测试通过、实际流量不通」。
	settings = append(settings, phpjson.Member{Key: "noKernelTun", Value: true})

	settingsRaw, err := phpjson.EncodeUnescaped(settings)
	if err != nil {
		return Outbound{}, err
	}

	return Outbound{
		Tag:            "warp",
		Protocol:       "wireguard",
		Settings:       json.RawMessage(settingsRaw),
		StreamSettings: nil,
		// SecretKey 由调用方加密后落库（这里保持 nil，避免把明文当成密文写进库）。
		SecretKey: nil,
	}, nil
}

// WarpSettings 是 Apply 的 settings 片段（业务层落库时需要的形态）。
//
// 与 PHP 的差异：PHP 把 secretKey 也塞进 settings、落库时再抽出来加密；
// Go 侧把「settings（可公开） + secretKey（需加密）」分开返回，避免误把明文入库。
func WarpSettings(account *WarpAccount) (phpjson.Object, string, error) {
	out, err := NewWarpService().Apply(account)
	if err != nil {
		return nil, "", err
	}
	parsed, err := phpjson.ParseOrdered(out.Settings)
	if err != nil {
		return nil, "", err
	}
	obj, ok := parsed.(phpjson.Object)
	if !ok {
		return nil, "", errors.New("WARP settings 组装异常")
	}
	return obj, account.PrivateKey, nil
}

// SetLicense 复刻 setLicense()：PUT /reg/{device}/account（WARP+）。
func (s *WarpService) SetLicense(ctx context.Context, account *WarpAccount, license string) error {
	if account == nil {
		return errors.New("尚未注册 WARP，请先注册")
	}
	path := "/reg/" + account.DeviceID + "/account"
	resp, status, err := s.doJSON(ctx, http.MethodPut, path, map[string]any{"license": license}, account.AccessToken)
	if err != nil {
		return fmt.Errorf("license 设置失败：%w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("license 设置失败：CF API 返回 %d", status)
	}
	var body map[string]any
	if err := json.Unmarshal(resp, &body); err != nil {
		return fmt.Errorf("license 设置失败：响应不是 JSON（%w）", err)
	}
	if _, ok := body["id"].(string); !ok {
		return fmt.Errorf("license 设置失败：CF API 返回 %d", status)
	}
	account.LicenseKey = license
	return nil
}

// doJSON 发一次 CF API 请求（CF-Client-Version 头与 3x-ui 一致）。
func (s *WarpService) doJSON(ctx context.Context, method, path string, payload map[string]any, bearer string) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, s.apiBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("CF-Client-Version", warpClientVer)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	return raw, resp.StatusCode, nil
}

// firstObject 取数组第 0 个对象（CF 回执的 config.peers[0]）。
func firstObject(v any) map[string]any {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return map[string]any{}
	}
	obj, ok := arr[0].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return obj
}

// endpointOf 复刻 PHP WarpService::endpointOf()：出站握手端点优先用 CF 返回的 IPv4 字面量，
// 避免端点域名在 A/AAAA 间解析（IPv4-only 节点约一半概率抽到 IPv6 导致 network is unreachable）。
// 没有 v4 才退 v6 字面量，最后才退 host 域名。入参是 peer["endpoint"]。
func endpointOf(endpoint map[string]any) string {
	port := 0
	if ports, ok := endpoint["ports"].([]any); ok && len(ports) > 0 {
		port = int(phpval.Int(ports[0]))
	}
	if port <= 0 {
		port = 2408
	}
	if lit := literalEndpoint(phpval.String(endpoint["v4"]), port); lit != "" {
		return lit
	}
	// 纯 IPv6 节点：写域名同样会抽到不可用的 A 记录，这里直接给 v6 字面量。
	if lit := literalEndpointV6(phpval.String(endpoint["v6"]), port); lit != "" {
		return lit
	}
	return phpval.String(endpoint["host"])
}

// literalEndpoint 把 CF 的 endpoint.v4（形如 "162.159.192.5:0"，冒号后是占位 0）转成 host:port。
func literalEndpoint(v4 string, port int) string {
	host := strings.TrimSpace(v4)
	if host == "" {
		return ""
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if ip := net.ParseIP(host); ip == nil || ip.To4() == nil {
		return ""
	}
	return host + ":" + phpval.Itoa(int64(port))
}

// literalEndpointV6 把 CF 的 endpoint.v6（形如 "[2606:4700:d0::a29f:c009]:0"）转成 [v6]:port。
// 只按方括号剥端口：裸 v6 的末段自己就可能是十六进制数字，按冒号硬切会切坏地址。
func literalEndpointV6(v6 string, port int) string {
	host := strings.TrimSpace(v6)
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return ""
		}
		host = host[1:end]
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil {
		return ""
	}
	return "[" + host + "]:" + phpval.Itoa(int64(port))
}

// stringArrayValue 把 []string 转成保序 Array（与 routing.go 的 stringArray 区分：
// 那个是从 json.RawMessage 里筛字符串，这个是直接构造）。
func stringArrayValue(in []string) phpjson.Array {
	out := make(phpjson.Array, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// intArray 把 []int64 转成保序 Array。
func intArray(in []int64) phpjson.Array {
	out := make(phpjson.Array, len(in))
	for i, n := range in {
		out[i] = n
	}
	return out
}

// runeTruncate 复刻 PHP mb_substr($s, 0, $limit)（按字符截断，不是字节）。
func runeTruncate(s string, limit int) string {
	if limit < 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
