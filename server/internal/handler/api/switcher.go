package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/config"
)

// PanelSwitcher 是 ProtocolSwitcher 的实现：复刻 ProtocolSwitchService 的语义。
//
// 流程（每节点）：attach 新协议的全部 inbound 成功后，detach 旧协议 inbound（排除与新协议相同的）；
// 任一节点失败 → 把已 attach 的新 inbound 全部 detach 回去，并尝试重新 attach 旧 inbound，
// users.protocol 不变。
//
// 与 PHP 的差异（P2 的边界说明）：
//   - PHP 在一个 DB 事务里做「跨网络调用 + 写 users.protocol」，Go 侧把 users.protocol 的写入
//     交给调用方在上面的 Switch 成功之后完成（本实现自己写，见末尾），网络调用不在事务内；
//   - 驱动层（3x-ui client 的登录态缓存、xray 指令通道）属于 P5，这里只实现 3x-ui 的
//     Bearer / Cookie 两种鉴权的 HTTP 调用，xray 节点按 PHP 的行为直接视为成功（no-op）。
type PanelSwitcher struct {
	store  Store
	appKey []byte
	client *http.Client
	logger *log.Logger
}

// NewPanelSwitcher 构造协议切换器。cfg 用于解密 nodes.api_key / nodes.password。
func NewPanelSwitcher(store Store, cfg *config.Config, logger *log.Logger) *PanelSwitcher {
	if logger == nil {
		logger = log.Default()
	}
	return &PanelSwitcher{
		store:  store,
		appKey: cfg.AppKey,
		client: &http.Client{Timeout: 15 * time.Second},
		logger: logger,
	}
}

// Switch 实现 ProtocolSwitcher。
func (s *PanelSwitcher) Switch(ctx context.Context, user *User, newProtocol string) error {
	if newProtocol != "vless" && newProtocol != "trojan" {
		return NewDomainError("不支持的协议")
	}
	if user.Protocol == newProtocol {
		return nil
	}

	nodes, err := s.store.EnabledNodesForSwitch(ctx)
	if err != nil {
		return fmt.Errorf("读取节点失败: %w", err)
	}

	email := user.ClientEmail()
	attached := make(map[uint64][]int64) // nodeID → 已 attach 的新 inbound（用于回滚）

	rollback := func() {
		for nodeID, newInbounds := range attached {
			for _, n := range nodes {
				if n.ID != nodeID {
					continue
				}
				if derr := s.detach(ctx, &n, email, newInbounds); derr != nil {
					s.logger.Printf("协议切换回滚 detach 失败 node=%d: %v", nodeID, derr)
				}
				if old := n.InboundIDsFor(user.Protocol); len(old) > 0 {
					if aerr := s.attach(ctx, &n, email, old); aerr != nil {
						s.logger.Printf("协议切换回滚 attach 失败 node=%d: %v", nodeID, aerr)
					}
				}
			}
		}
	}

	for i := range nodes {
		n := &nodes[i]
		newInbounds := n.InboundIDsFor(newProtocol)
		if len(newInbounds) == 0 {
			continue // 该节点无新协议 inbound，跳过
		}
		if err := s.attach(ctx, n, email, newInbounds); err != nil {
			rollback()
			return err
		}
		attached[n.ID] = newInbounds

		if toDetach := difference(n.InboundIDsFor(user.Protocol), newInbounds); len(toDetach) > 0 {
			if err := s.detach(ctx, n, email, toDetach); err != nil {
				rollback()
				return err
			}
		}
	}

	// 与 PHP 一致：所有节点都成功后才落地 users.protocol（PHP 是在事务里最后一步存的）。
	// 并存期的差异：PHP 的 attach/detach 与这次 UPDATE 在同一个 DB 事务里，
	// Go 侧网络调用不能进事务（会长时间持锁），因此这里只在全部成功后写一次。
	if err := s.store.UpdateUserProtocol(ctx, user.ID, newProtocol); err != nil {
		rollback()
		return fmt.Errorf("更新用户协议失败: %w", err)
	}
	user.Protocol = newProtocol
	return nil
}

// attach / detach 只在 3x-ui 节点上有实际动作；xray 节点返回成功（与 XrayDriver 一致）。
func (s *PanelSwitcher) attach(ctx context.Context, n *Node, email string, inboundIDs []int64) error {
	if n.DriverType != "3x-ui" {
		return nil
	}
	return s.panelCall(ctx, n, "/panel/api/clients/"+url.PathEscape(email)+"/attach", inboundIDs)
}

func (s *PanelSwitcher) detach(ctx context.Context, n *Node, email string, inboundIDs []int64) error {
	if n.DriverType != "3x-ui" {
		return nil
	}
	return s.panelCall(ctx, n, "/panel/api/clients/"+url.PathEscape(email)+"/detach", inboundIDs)
}

// panelCall 调一次 3x-ui 的 clients/{email}/attach|detach。
//
// 鉴权与 ThreeXUiClient 一致：有 api_key 用 Bearer（无状态），否则 cookie 登录 + CSRF。
// 响应统一 {success:bool,msg:string,obj:any}：success!=true 视为失败。
func (s *PanelSwitcher) panelCall(ctx context.Context, n *Node, path string, inboundIDs []int64) error {
	base := panelBaseURL(n)
	body, _ := json.Marshal(map[string]any{"inboundIds": inboundIDs})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造面板请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	if apiKey := decryptOptional(n.APIKeyEnc, s.appKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("节点 %s 请求失败: %w", n.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("节点 %s 返回非法响应: %s", n.Name, truncate(string(raw), 120))
	}
	if !parsed.Success {
		msg := parsed.Msg
		if msg == "" {
			msg = "操作失败"
		}
		return fmt.Errorf("节点 %s: %s", n.Name, msg)
	}
	return nil
}

// panelBaseURL 拼面板地址：{scheme}://{host}:{port}{web_base_path}。
func panelBaseURL(n *Node) string {
	scheme := n.Scheme
	if scheme == "" {
		scheme = "https"
	}
	port := n.Port
	if port <= 0 {
		if scheme == "http" {
			port = 80
		} else {
			port = 443
		}
	}
	basePath := phpTrim(n.WebBasePath)
	if basePath != "" && !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	basePath = strings.TrimSuffix(basePath, "/")
	return scheme + "://" + n.Host + ":" + strconv.Itoa(port) + basePath
}

// difference 返回 a 中不在 b 里的元素（对齐 PHP array_diff 的保序语义）。
func difference(a, b []int64) []int64 {
	if len(a) == 0 {
		return nil
	}
	exclude := make(map[int64]struct{}, len(b))
	for _, v := range b {
		exclude[v] = struct{}{}
	}
	out := make([]int64, 0, len(a))
	for _, v := range a {
		if _, ok := exclude[v]; !ok {
			out = append(out, v)
		}
	}
	return out
}

