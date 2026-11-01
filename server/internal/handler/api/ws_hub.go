package api

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// UserHub 是用户端 WebSocket 连接注册表 + 下行推送口。
//
// 设计成「通用通道」而不是单一用途：后期接入订单状态、工单、公告等功能时，
// 只需要在业务处调 Push(userID, WSMessage{Type: "xxx", ...})，通道本身不用动。
//
// 并发模型：一个用户可能多端登录（手机 + 电脑），所以 map 的值是连接集合；
// 推送对同一用户的每条连接都发一份。写操作都在锁外做（慢客户端不能拖住别人）。
type UserHub struct {
	mu    sync.RWMutex
	conns map[uint64]map[*UserConn]struct{}
}

// UserConn 是一条用户 WS 连接。
type UserConn struct {
	UserID uint64
	ws     *websocket.Conn
	// send 缓冲下行消息；满了说明客户端太慢，直接断开（不阻塞业务推送）。
	send chan []byte
	hub  *UserHub
	once sync.Once
}

// NewUserHub 构造空 hub。
func NewUserHub() *UserHub {
	return &UserHub{conns: map[uint64]map[*UserConn]struct{}{}}
}

// register 登记连接。
func (h *UserHub) register(c *UserConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.conns[c.UserID]
	if !ok {
		set = map[*UserConn]struct{}{}
		h.conns[c.UserID] = set
	}
	set[c] = struct{}{}
}

// unregister 移除连接（幂等）。
func (h *UserHub) unregister(c *UserConn) {
	h.mu.Lock()
	set, ok := h.conns[c.UserID]
	if ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.conns, c.UserID)
		}
	}
	h.mu.Unlock()
	c.close()
}

// Push 给某用户的所有在线连接推送一条消息。
//
// 返回实际送达的连接数。用户不在线不算错误（0 是完全正常的结果——
// 前端下次轮询 /api/me 仍能拿到最新数据，WS 只是让它不必等）。
func (h *UserHub) Push(userID uint64, msg WSMessage) int {
	if msg.TS == 0 {
		msg.TS = time.Now().UnixMilli()
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return 0
	}

	h.mu.RLock()
	targets := make([]*UserConn, 0, len(h.conns[userID]))
	for c := range h.conns[userID] {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	sent := 0
	for _, c := range targets {
		select {
		case c.send <- payload:
			sent++
		default:
			// 发送缓冲已满：客户端读得太慢，断开它（重连后自会补状态）。
			h.unregister(c)
		}
	}
	return sent
}

// Online 是当前在线连接数（监控/测试用）。
func (h *UserHub) Online() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, set := range h.conns {
		n += len(set)
	}
	return n
}

// close 关闭连接（幂等）。c.ws 可能为空：单测里只构造注册表、或握手失败前的半成品
// 连接都会走到这里，不能让清理路径自己 panic。
func (c *UserConn) close() {
	c.once.Do(func() {
		close(c.send)
		if c.ws != nil {
			_ = c.ws.Close()
		}
	})
}
