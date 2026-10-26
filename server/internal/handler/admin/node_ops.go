package admin

// RealNodeOps 是 NodeOps 的真实实现：对齐 PHP HealthCheckService / ThreeXUiClient /
// NodeWarpController / OutboundSubscriptionService / ThirdPartyService / AgentUpgradeService。
//
// 复用 P5 driver（threexui.Client / xray.WarpService），不重写取数逻辑。

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/driver/threexui"
	"controlhub-server/internal/driver/xray"
)

// RealNodeOps 实现 NodeOps 接口。
type RealNodeOps struct {
	cfg    NodeConfig
	store  CommandStore
	logger loggerLike
	client *http.Client
	warp   *xray.WarpService
}

// CommandStore 是 RelayTestTarget 需要的指令存取接缝。
type CommandStore interface {
	CreateCommand(ctx context.Context, nodeID uint64, action string, payload any) (*CommandRecord, error)
	CommandByID(ctx context.Context, id uint64) (*CommandRecord, error)
}

// NewRealNodeOps 构造真实 NodeOps 实现。
func NewRealNodeOps(cfg NodeConfig, store CommandStore, logger loggerLike) *RealNodeOps {
	cfg = nodeConfigDefaults(cfg)
	if logger == nil {
		logger = defaultLogger()
	}
	return &RealNodeOps{
		cfg:    cfg,
		store:  store,
		logger: logger,
		client: &http.Client{Timeout: 30 * time.Second},
		warp:   xray.NewWarpService(),
	}
}

// defaultLogger 返回默认 logger。
func defaultLogger() loggerLike {
	return loggerFunc(func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})
}

// loggerFunc 是 loggerLike 的函数实现。
type loggerFunc func(format string, args ...any)

func (f loggerFunc) Printf(format string, args ...any) { f(format, args...) }

// threeXClient 从 NodeRecord 构造 threexui.Client。
func (o *RealNodeOps) threeXClient(n *NodeRecord) *threexui.Client {
	return threexui.New(threexui.Config{
		Scheme:      n.Scheme,
		Host:        n.Host,
		Port:        int(n.Port),
		WebBasePath: n.WebBasePath,
		Username:    n.Username,
		Password:    opsDerefStr(n.Password),
		APIKey:      opsDerefStr(n.APIKey),
		VerifySSL:   n.VerifySSL,
	})
}

// opsDerefStr 解引用字符串指针。
func opsDerefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// HealthCheck 对齐 HealthCheckService。
func (o *RealNodeOps) HealthCheck(ctx context.Context, n *NodeRecord) (HealthResult, error) {
	if n.IsXray() {
		return o.healthCheckXray(ctx, n)
	}
	client := o.threeXClient(n)
	h, err := client.HealthCheck(ctx)
	if err != nil {
		return HealthResult{Error: err.Error()}, nil
	}
	return HealthResult{
		OK:        h.OK,
		LatencyMs: h.LatencyMs,
		CPU:       h.CPU,
		Mem:       h.Mem,
		XrayState: h.XrayState,
		Error:     h.Error,
	}, nil
}

// healthCheckXray 对齐 XrayDriver::healthCheck（心跳超时判定 + TCP 延迟）。
func (o *RealNodeOps) healthCheckXray(ctx context.Context, n *NodeRecord) (HealthResult, error) {
	if n.LastCheckAt == nil {
		return HealthResult{Error: "节点尚未注册（无心跳）"}, nil
	}
	age := time.Since(*n.LastCheckAt)
	if age > 120*time.Second {
		return HealthResult{Error: fmt.Sprintf("心跳超时（%.0fs 未上报）", age.Seconds())}, nil
	}
	return HealthResult{OK: true}, nil
}

// ProbeInbounds 用表单参数临时连 3x-ui 拉入站清单。
func (o *RealNodeOps) ProbeInbounds(ctx context.Context, conn ProbeConn) ([]map[string]any, error) {
	client := threexui.New(threexui.Config{
		Scheme:      conn.Scheme,
		Host:        conn.Host,
		Port:        int(conn.Port),
		WebBasePath: conn.WebBasePath,
		APIKey:      opsDerefStr(conn.APIKey),
		Username:    conn.Username,
		Password:    conn.Password,
		VerifySSL:   conn.VerifySSL,
	})
	return client.ListInbounds(ctx)
}

// ThreeXInbounds 读某 3x-ui 节点的入站清单。
func (o *RealNodeOps) ThreeXInbounds(ctx context.Context, n *NodeRecord) ([]map[string]any, error) {
	return o.threeXClient(n).ListInbounds(ctx)
}

// ThreeXEnsureRelayClient 在 3x-ui 目标节点上确保 relay client 并返回其 uuid/密码。
func (o *RealNodeOps) ThreeXEnsureRelayClient(ctx context.Context, target *NodeRecord, inboundID int64, protocol, email string) (string, error) {
	client := o.threeXClient(target)

	// 1. 查已有 client
	existing, err := client.GetClient(ctx, email)
	if err == nil && existing != nil {
		// 已存在 → attach 到指定入站（幂等）
		_ = client.AttachClient(ctx, email, []int64{inboundID})
		return extractClientCredential(existing, protocol), nil
	}

	// 2. 创建 client
	clientData := map[string]any{
		"email":      email,
		"enable":     true,
		"totalGB":    0,
		"expiryTime": 0,
		"limitIp":    0,
	}
	addPayload := map[string]any{
		"client":     clientData,
		"inboundIds": []int64{inboundID},
	}
	if _, err := client.AddClient(ctx, addPayload, []int64{inboundID}); err != nil {
		return "", fmt.Errorf("创建中转凭据失败: %w", err)
	}

	// 3. attach
	_ = client.AttachClient(ctx, email, []int64{inboundID})

	// 4. 回读凭据
	created, err := client.GetClient(ctx, email)
	if err != nil || created == nil {
		return "", nil
	}
	return extractClientCredential(created, protocol), nil
}

// extractClientCredential 从3x-ui client 对象提取凭据。
func extractClientCredential(client map[string]any, protocol string) string {
	//3x-ui 可能返回 {client: {...}} 或直接是 client 对象
	if inner, ok := client["client"].(map[string]any); ok {
		client = inner
	}
	if protocol == "trojan" {
		if pw, ok := client["password"].(string); ok {
			return pw
		}
	}
	if uuid, ok := client["uuid"].(string); ok {
		return uuid
	}
	if pw, ok := client["password"].(string); ok {
		return pw
	}
	return ""
}

// ThreeXDeleteClient 删除 3x-ui 上的 relay client。
func (o *RealNodeOps) ThreeXDeleteClient(ctx context.Context, target *NodeRecord, email string) error {
	return o.threeXClient(target).DeleteClient(ctx, email, false)
}

// ThreeXClientLinks 读 3x-ui 面板为某 email 生成的分享链接。
func (o *RealNodeOps) ThreeXClientLinks(ctx context.Context, target *NodeRecord, email string) ([]string, error) {
	return o.threeXClient(target).GetClientLinks(ctx, email)
}

// RelayTestTarget 下发 test_outbound 指令并同步等待回执（≤8s）。
func (o *RealNodeOps) RelayTestTarget(ctx context.Context, nodeID uint64, host string, port int64) (string, string, bool) {
	payload := map[string]any{"host": host, "port": port}
	cmd, err := o.store.CreateCommand(ctx, nodeID, "test_outbound", payload)
	if err != nil {
		return "error", err.Error(), false
	}

	deadline := time.Now().Add(time.Duration(o.cfg.OutboundTestWait) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		fresh, err := o.store.CommandByID(ctx, cmd.ID)
		if err != nil {
			continue
		}
		if fresh.Status == "success" {
			output := ""
			if fresh.Result != nil {
				output = *fresh.Result
			}
			return "success", output, false
		}
		if fresh.Status == "failed" {
			output := ""
			if fresh.Result != nil {
				output = *fresh.Result
			}
			return "failed", output, false
		}
	}
	return "", "", true
}

// WarpRegister 注册 WARP 设备（保留已有 license）。
func (o *RealNodeOps) WarpRegister(ctx context.Context, nodeID uint64, nodeName string, existing *WarpAccountRecord) (*WarpAccountRecord, error) {
	var warpExisting *xray.WarpAccount
	if existing != nil {
		warpExisting = &xray.WarpAccount{
			DeviceID:      existing.DeviceID,
			AccessToken:   existing.AccessToken,
			PrivateKey:    existing.PrivateKey,
			PublicKey:     existing.PublicKey,
			PeerPublicKey: existing.PeerPublicKey,
			PeerEndpoint:  existing.PeerEndpoint,
			Addresses:     existing.Addresses,
			Reserved:      existing.Reserved,
			LicenseKey:    existing.LicenseKey,
			ClientID:      existing.ClientID,
		}
	}
	warpResult, err := o.warp.Register(ctx, nodeName, warpExisting)
	if err != nil {
		return nil, err
	}
	result := &WarpAccountRecord{
		NodeID:        nodeID,
		DeviceID:      warpResult.DeviceID,
		AccessToken:   warpResult.AccessToken,
		PrivateKey:    warpResult.PrivateKey,
		PublicKey:     warpResult.PublicKey,
		PeerPublicKey: warpResult.PeerPublicKey,
		PeerEndpoint:  warpResult.PeerEndpoint,
		Addresses:     warpResult.Addresses,
		Reserved:      warpResult.Reserved,
		LicenseKey:    warpResult.LicenseKey,
		ClientID:      warpResult.ClientID,
	}
	if existing != nil {
		result.NodeID = existing.NodeID
		result.AutoRotateHours = existing.AutoRotateHours
		result.LastRotateAt = existing.LastRotateAt
		result.Enabled = existing.Enabled
	}
	return result, nil
}

// WarpSetLicense 设置 WARP+ license。
func (o *RealNodeOps) WarpSetLicense(ctx context.Context, account *WarpAccountRecord, license string) error {
	warpAccount := &xray.WarpAccount{
		DeviceID:    account.DeviceID,
		AccessToken: account.AccessToken,
		LicenseKey:  account.LicenseKey,
	}
	return o.warp.SetLicense(ctx, warpAccount, license)
}

// OutboundSubFetch 拉取并解析一条出站订阅。
func (o *RealNodeOps) OutboundSubFetch(ctx context.Context, rawURL string) ([]any, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "ControlHub-outbound-sub/1.0")

	timeout := time.Duration(o.cfg.OutboundSubFetchTimeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("拉取失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	decoded := strings.TrimSpace(string(body))
	if decoded == "" {
		return nil, nil, fmt.Errorf("拉取内容为空")
	}

	// 尝试 base64 解码
	if decodedBytes, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(decoded, "\n", "")); err == nil {
		decoded = string(decodedBytes)
	}

	var outbounds []any
	var identities []string
	for _, line := range strings.Split(decoded, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "://") {
			continue
		}
		outbounds = append(outbounds, map[string]any{"tag": fmt.Sprintf("out-%d", len(outbounds)), "uri": line})
		identities = append(identities, line)
	}
	if len(outbounds) == 0 {
		return nil, nil, fmt.Errorf("拉取成功但未解析到任何可用出站")
	}
	return outbounds, identities, nil
}

// SanitizeSubURL 校验订阅 URL（仅 http/https + 非内网）。
func (o *RealNodeOps) SanitizeSubURL(ctx context.Context, rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", fmt.Errorf("订阅地址不能为空")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("订阅地址格式不正确")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("订阅地址仅支持 http/https")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("订阅地址格式不正确")
	}
	if isPrivateOrLoopback(u.Hostname()) {
		return "", fmt.Errorf("禁止使用内网/回环地址")
	}
	return trimmed, nil
}

// isPrivateOrLoopback 判断 host 是否为内网/回环地址。
func isPrivateOrLoopback(host string) bool {
	// 去掉 IPv6 括号
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	// hostname: 检查 localhost / *.local
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	return false
}

// ThirdPartyFetch 拉取第三方机场订阅，返回节点链接。
func (o *RealNodeOps) ThirdPartyFetch(ctx context.Context, rawURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ControlHub/1.0)")

	timeout := time.Duration(o.cfg.ThirdPartyFetchTimeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	decoded := strings.TrimSpace(string(body))
	if decoded == "" {
		return nil, fmt.Errorf("拉取内容为空")
	}

	if decodedBytes, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(decoded, "\n", "")); err == nil {
		decoded = string(decodedBytes)
	}

	var links []string
	for _, line := range strings.Split(decoded, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "://") {
			links = append(links, line)
		}
	}
	return links, nil
}

// Manifest 读面板上的 agent manifest。
func (o *RealNodeOps) Manifest(ctx context.Context) (*ManifestAgent, error) {
	paths := []string{
		"storage/app/xray/manifest.json",
		"storage/app/node-bin/manifest.json",
		filepath.Join("storage", "app", "manifest.json"),
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var parsed struct {
			Agent struct {
				Version string `json:"version"`
				Assets  []struct {
					Arch   string `json:"arch"`
					File   string `json:"file"`
					SHA256 string `json:"sha256"`
				} `json:"assets"`
			} `json:"agent"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			continue
		}
		if parsed.Agent.Version == "" && len(parsed.Agent.Assets) == 0 {
			continue
		}
		assets := make([]ManifestAsset, 0, len(parsed.Agent.Assets))
		for _, a := range parsed.Agent.Assets {
			assets = append(assets, ManifestAsset{Arch: a.Arch, File: a.File, SHA256: a.SHA256})
		}
		return &ManifestAgent{Version: parsed.Agent.Version, Assets: assets}, nil
	}
	return nil, nil
}

// opsSHA256Hex 计算 SHA-256 十六进制。
func opsSHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// formatPort 端口转字符串。
func formatPort(port int64) string {
	return strconv.FormatInt(port, 10)
}
