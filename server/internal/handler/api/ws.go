package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// WebSocket 通道的时序常量。
const (
	wsWriteWait  = 10 * time.Second // 单条消息写超时
	wsPongWait   = 70 * time.Second // 两次心跳之间允许的最长静默
	wsPingPeriod = 30 * time.Second // 服务端主动 ping 周期
	wsSendBuffer = 16               // 每条连接的发送缓冲（满了就断开慢客户端）
)

// handleWS 是 GET /api/ws：用户端 WebSocket 长连接（订阅重置等下行通知的通道）。
//
// 鉴权：复用 access_token 口径（与 api.auth 一致），但浏览器 WS 不能设请求头，
// 因此额外允许 ?access_token= 查询参数；两者都给了就按 header 优先。
// 失效 token 一律 401 直接拒绝握手，不建立连接。
func (h *Handler) handleWS(c *gin.Context) {
	token := bearerToken(c)
	if token == "" {
		token = c.Query("access_token")
	}
	if token == "" {
		fail(c, "未认证", 401)
		return
	}

	ctx := c.Request.Context()
	access, err := h.store.AccessTokenByToken(ctx, token)
	if err != nil && !IsNoRows(err) {
		h.logger.Printf("api.ws 查询 access_token 失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}
	now := h.now()
	if access == nil || access.Expired(now) {
		fail(c, "未认证或 token 已过期", 401)
		return
	}
	user, uerr := h.store.UserByID(ctx, access.UserID)
	if uerr != nil && !IsNoRows(uerr) {
		h.logger.Printf("api.ws 查询用户失败: %v", uerr)
		fail(c, "服务器错误", 500)
		return
	}
	if user == nil || !user.Enabled {
		fail(c, "账号不可用", 403)
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		// 同源校验交给 access_token；这里放开 Origin（客户端与站点不同源也常见）。
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 失败时 gorilla 已写过响应，不要重复写。
		h.logger.Printf("api.ws 升级失败: %v", err)
		return
	}

	if h.userHub == nil {
		h.userHub = NewUserHub()
	}
	uc := &UserConn{UserID: user.ID,
		ws:   conn,
		send: make(chan []byte, wsSendBuffer),
		hub:  h.userHub,
	}
	h.userHub.register(uc)

	// 连接建立即推一次当前状态：前端刷新页面/重连后不必等下一次变更。
	h.userHub.Push(user.ID, WSMessage{
		Type: WSTypeSubscriptionReset,
		Data: SubscriptionResetData{Token: user.Token, SubPath: "/api/sub/" + user.Token},
	})

	go uc.writeLoop()
	uc.readLoop(c.Request.Context(), h)
}

// readLoop 处理上行消息（目前只有 ping），并在断开时清理注册。
func (c *UserConn) readLoop(ctx context.Context, h *Handler) {
	defer c.hub.unregister(c)
	_ = c.ws.SetReadDeadline(time.Now().Add(wsPongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(wsPongWait))

		var in struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &in) != nil {
			continue // 非 JSON 一律忽略，不因客户端乱发而断线
		}
		switch in.Type {
		case "ping":
			select {
			case c.send <- mustJSON(WSMessage{Type: WSTypePong, TS: time.Now().UnixMilli()}):
			default:
			}
		default:
			// 未知 type 忽略：给后期新增消息类型留出前向兼容空间。
		}
	}
}

// writeLoop 把 send 缓冲写到连接上，并定期发 ping 控制帧保活。
func (c *UserConn) writeLoop() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.ws.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// mustJSON 序列化信封（失败给空对象，绝不 panic）。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
