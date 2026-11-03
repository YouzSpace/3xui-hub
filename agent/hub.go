package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HubClient 面板 HTTP API 客户端（兜底通道 + 配置拉取）。
//
// 双地址（issue #88）：bases 按优先级排列（v4 在前、v6 在后）。只有**传输层**
// 失败（拨号/超时/TLS）才换下一个地址；HTTP 4xx/5xx 说明地址是通的，直接上抛，
// 不拿 v6 去重试一个业务错误。成功的那条会粘住，后续请求不再重新试错。
type HubClient struct {
	mu     sync.Mutex
	bases  []string // 无尾斜杠；至少 1 条
	active int      // 当前粘住的地址下标
	secret string
}

func newHubClient(base, secret string) *HubClient {
	return newHubClientMulti([]string{base}, secret)
}

// newHubClientMulti 构造多地址客户端：空串跳过、去重、保留给定顺序。
func newHubClientMulti(bases []string, secret string) *HubClient {
	cleaned := make([]string, 0, len(bases))
	for _, b := range bases {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		if b == "" {
			continue
		}
		dup := false
		for _, existing := range cleaned {
			if existing == b {
				dup = true
				break
			}
		}
		if !dup {
			cleaned = append(cleaned, b)
		}
	}
	return &HubClient{bases: cleaned, secret: secret}
}

// Base 返回当前选中的面板地址（日志用）。
func (h *HubClient) Base() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bases) == 0 {
		return ""
	}
	return h.bases[h.active]
}

// basesInOrder 返回「从当前粘住的地址开始」的尝试顺序（v4 优先，失败才 v6）。
func (h *HubClient) basesInOrder() ([]string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bases) == 0 {
		return nil, 0
	}
	order := make([]string, 0, len(h.bases))
	for i := 0; i < len(h.bases); i++ {
		order = append(order, h.bases[(h.active+i)%len(h.bases)])
	}
	return order, h.active
}

// markActive 记住当前可用的地址（下标相对 bases 原顺序）。
func (h *HubClient) markActive(base string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.bases {
		if b == base {
			h.active = i
			return
		}
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type RegisterInfo struct {
	NodeName      string `json:"node_name"`
	ConfigVersion int64  `json:"config_version"`
	PullInterval  int    `json:"pull_interval"`
	Ws            struct {
		Enabled bool   `json:"enabled"`
		URL     string `json:"url"`
	} `json:"ws"`
}

type RegisterBody struct {
	XrayVersion  string `json:"xray_version"`
	AgentVersion string `json:"agent_version"`
	// AgentSHA 是当前 agent 二进制自身的 sha256：面板存进 driver_config，
	// 同版本号重新编译过（manifest version 忘了 bump）时靠它发现「节点还是旧二进制」。
	AgentSHA   string `json:"agent_sha,omitempty"`
	KernelArch string `json:"kernel_arch"`
	Hostname   string `json:"hostname"`
}

// ConfigResponse 全量配置拉取结果。
type ConfigResponse struct {
	NotModified  bool
	Config       json.RawMessage
	Version      int64
	PullInterval int
	ETag         string
}

// Command 面板下发的指令（adu / rmu / test_outbound）。
type Command struct {
	ID      int64           `json:"id"`
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

// AckResult 指令执行回执。
type AckResult struct {
	ID      int64  `json:"id"`
	Success bool   `json:"success"`
	Output  string `json:"output,omitempty"`
}

func (h *HubClient) newRequest(ctx context.Context, base, method, path string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Node-Secret", h.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// errTransport 标记「传输层失败」——唯一会触发换地址的错误类别。
type errTransport struct{ err error }

func (e *errTransport) Error() string { return e.err.Error() }
func (e *errTransport) Unwrap() error { return e.err }

// postJSON 发起 POST，解析 {code,data} 信封；返回 data 原始 JSON。
//
// 地址按 v4 → v6 顺序尝试，只有传输层失败才换下一个（见 HubClient 注释）。
func (h *HubClient) postJSON(ctx context.Context, client *http.Client, path string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	order, _ := h.basesInOrder()
	if len(order) == 0 {
		return nil, fmt.Errorf("缺少面板地址")
	}

	var lastErr error
	for _, base := range order {
		data, err := h.postJSONAt(ctx, client, base, path, raw)
		if err == nil {
			h.markActive(base)
			return data, nil
		}
		te, isTransport := err.(*errTransport)
		if !isTransport {
			return nil, err
		}
		lastErr = te.err
		logf("面板 %s 不可达：%v", base, te.err)
	}
	return nil, lastErr
}

// postJSONAt 对单个面板地址发一次 POST。
func (h *HubClient) postJSONAt(ctx context.Context, client *http.Client, base, path string, raw []byte) (json.RawMessage, error) {
	req, err := h.newRequest(ctx, base, http.MethodPost, path, raw)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &errTransport{err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &errTransport{err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var env apiEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("面板返回 code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, nil
}

func (h *HubClient) client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// Register 注册（只负责"连上"；reality 等业务材料由面板侧管理）。
func (h *HubClient) Register(ctx context.Context, body RegisterBody) (*RegisterInfo, error) {
	data, err := h.postJSON(ctx, h.client(20*time.Second), "/api/node-api/register", body)
	if err != nil {
		return nil, err
	}
	var info RegisterInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("register 响应解析失败: %w", err)
	}
	if info.PullInterval < 5 {
		info.PullInterval = 30
	}
	return &info, nil
}

// FetchConfig 全量配置拉取（If-None-Match ETag；304 视为未变化）。
//
// 地址回退口径同 postJSON：只有传输层失败才换 v6 地址重试。
func (h *HubClient) FetchConfig(ctx context.Context, etag string) (*ConfigResponse, error) {
	order, _ := h.basesInOrder()
	if len(order) == 0 {
		return nil, fmt.Errorf("缺少面板地址")
	}

	var lastErr error
	for _, base := range order {
		out, err := h.fetchConfigAt(ctx, base, etag)
		if err == nil {
			h.markActive(base)
			return out, nil
		}
		te, isTransport := err.(*errTransport)
		if !isTransport {
			return nil, err
		}
		lastErr = te.err
		logf("面板 %s 不可达：%v", base, te.err)
	}
	return nil, lastErr
}

// fetchConfigAt 对单个面板地址拉一次配置。
func (h *HubClient) fetchConfigAt(ctx context.Context, base, etag string) (*ConfigResponse, error) {
	req, err := h.newRequest(ctx, base, http.MethodGet, "/api/node-api/config", nil)
	if err != nil {
		return nil, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := h.client(20 * time.Second).Do(req)
	if err != nil {
		return nil, &errTransport{err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return &ConfigResponse{NotModified: true}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, &errTransport{err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Config        json.RawMessage `json:"config"`
			ConfigVersion int64           `json:"config_version"`
			PullInterval  int             `json:"pull_interval"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("config 响应解析失败: %w", err)
	}
	if env.Code != 0 || env.Data.Config == nil {
		return nil, fmt.Errorf("config 响应异常（code=%d）", env.Code)
	}
	return &ConfigResponse{
		Config:       env.Data.Config,
		Version:      env.Data.ConfigVersion,
		PullInterval: env.Data.PullInterval,
		ETag:         resp.Header.Get("ETag"),
	}, nil
}

// Push 流量推送（HTTP 降级通道；批量绝对值）。
func (h *HubClient) Push(ctx context.Context, stats map[string]StatsPair) error {
	_, err := h.postJSON(ctx, h.client(20*time.Second), "/api/node-api/push", struct {
		Stats map[string]StatsPair `json:"stats"`
	}{Stats: stats})
	return err
}

// AliveBody 心跳载荷（HTTP 降级通道）。
type AliveBody struct {
	XrayVersion   string   `json:"xray_version,omitempty"`
	ConfigVersion int      `json:"config_version"`
	CPU           *float64 `json:"cpu,omitempty"`
	Mem           *float64 `json:"mem,omitempty"`
	Uptime        int64    `json:"uptime,omitempty"`
	Conns         int      `json:"conns,omitempty"`
	ActiveUsers   int      `json:"active_users,omitempty"`
	UpSpeed       float64  `json:"up_speed,omitempty"`
	DownSpeed     float64  `json:"down_speed,omitempty"`
	NetTotalUp    *uint64  `json:"net_total_up,omitempty"`
	NetTotalDown  *uint64  `json:"net_total_down,omitempty"`
	// BBR 是 sysctl/BBR 运行态（issue #94 R5），面板节点页直接展示。
	// 字段块级缺省：老面板忽略未知键，不影响存库。
	BBR *bbrStatus `json:"bbr,omitempty"`
	// ClientsFP 是节点配置里的 client 名单指纹（issue #115 3.5）：面板据它短路掉
	// 「名单没变」时的 client_list 全量读回。读不到 config.json 时为 nil（不带该键）。
	ClientsFP *clientsFingerprintBody `json:"clients_fp,omitempty"`
}

type AliveInfo struct {
	ConfigVersion int64 `json:"config_version"`
	PullInterval  int   `json:"pull_interval"`
}

// Alive 心跳（HTTP 降级通道）。
func (h *HubClient) Alive(ctx context.Context, body AliveBody) (*AliveInfo, error) {
	data, err := h.postJSON(ctx, h.client(15*time.Second), "/api/node-api/alive", body)
	if err != nil {
		return nil, err
	}
	var info AliveInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("alive 响应解析失败: %w", err)
	}
	return &info, nil
}

// Execute 指令长轮询（HTTP 降级通道；服务端挂起 ≤20s）。
func (h *HubClient) Execute(ctx context.Context) ([]Command, error) {
	data, err := h.postJSON(ctx, h.client(35*time.Second), "/api/node-api/execute", struct{}{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("execute 响应解析失败: %w", err)
	}
	return out.Commands, nil
}

// Ack 指令回执（HTTP 降级通道）。
func (h *HubClient) Ack(ctx context.Context, results []AckResult) error {
	_, err := h.postJSON(ctx, h.client(20*time.Second), "/api/node-api/ack", struct {
		Results []AckResult `json:"results"`
	}{Results: results})
	return err
}

func hostname() string {
	hn, err := os.Hostname()
	if err != nil {
		return ""
	}
	return hn
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
