package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSClient 面板 WS 长连接客户端（主通道）。
//
// 消息协议（应用层 JSON，event/data 信封）：
//   上行：hello / report / ack / pong
//   下行：hello.ok / commands / config.changed / ping
type WSClient struct {
	url    string
	secret string

	mu   sync.Mutex
	conn *websocket.Conn
}

func newWSClient(url, secret string) *WSClient {
	return &WSClient{url: url, secret: secret}
}

type wsMessage struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// connect 建立连接并发送 hello（鉴权 + 上报元数据；等价 HTTP register）。
// helloData 由调用方构造（agent/xray 版本、config_version、主机信息）。
func (w *WSClient) connect(ctx context.Context, helloData map[string]any) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	header := http.Header{}
	header.Set("X-Node-Secret", w.secret)

	conn, _, err := dialer.DialContext(ctx, w.url, header)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.conn = conn
	w.mu.Unlock()

	// 服务端下发 hello.ok / commands；agent 先发 hello 自我介绍
	return w.Send("hello", helloData)
}

func (w *WSClient) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		_ = w.conn.Close()
		w.conn = nil
	}
}

// Send 发送一条应用层消息（并发安全）。
func (w *WSClient) Send(event string, data any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("ws 未连接")
	}
	msg, err := json.Marshal(wsMessage{Event: event, Data: mustJSON(data)})
	if err != nil {
		return err
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return w.conn.WriteMessage(websocket.TextMessage, msg)
}

// readLoop 读循环：直到连接出错/关闭返回。每条消息按 event 分发。
// serverPing 周期由服务端负责；agent 侧设置读超时（90s 无消息视为死连接）。
func (w *WSClient) readLoop(handler func(event string, data json.RawMessage)) error {
	w.mu.Lock()
	conn := w.conn
	w.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("ws 未连接")
	}

	conn.SetReadLimit(16 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	// gorilla 默认对服务端 Ping 帧自动回 Pong；这里再兜底恢复读超时
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var msg wsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		handler(msg.Event, msg.Data)
	}
}

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
