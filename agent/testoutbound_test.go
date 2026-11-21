package main

import (
	"encoding/json"
	"net"
	"testing"
)

// TestRunTestOutboundTargets 面板下发 targets 数组时，agent 要按 tag 回数组结果。
// 这是 #147 之后真机发现的契约错位：面板发 {targets:[{tag,server,port}]}，
// 旧 agent 只认 {host,port} → 永远「host/port 缺失」。
func TestRunTestOutboundTargets(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起测试监听失败: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	payload, _ := json.Marshal(map[string]any{
		"targets": []map[string]any{
			{"tag": "ok", "server": "127.0.0.1", "port": port},
			{"tag": "bad", "server": "127.0.0.1", "port": 1},
			{"tag": "nohost", "server": "", "port": 0},
		},
	})
	res := runTestOutbound(Paths{}, Command{ID: 1, Action: "test_outbound", Payload: payload})
	if !res.Success {
		t.Fatalf("整体应成功（逐条报 ok）: %s", res.Output)
	}

	var got []struct {
		Tag       string `json:"tag"`
		OK        bool   `json:"ok"`
		LatencyMS int64  `json:"latency_ms"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Output), &got); err != nil {
		t.Fatalf("回执不是 JSON 数组: %v (%s)", err, res.Output)
	}
	if len(got) != 3 {
		t.Fatalf("回执条数 = %d, 期望 3", len(got))
	}
	if got[0].Tag != "ok" || !got[0].OK || got[0].LatencyMS <= 0 {
		t.Fatalf("可达目标结果不对: %+v", got[0])
	}
	if got[1].OK || got[1].Error == "" {
		t.Fatalf("不可达目标应 ok=false 且带 error: %+v", got[1])
	}
	if got[2].OK || got[2].Error == "" {
		t.Fatalf("缺 host/port 应报错: %+v", got[2])
	}
}

// TestRunTestOutboundLegacyHostPort 兼容老面板的单目标载荷。
func TestRunTestOutboundLegacyHostPort(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"host": "127.0.0.1", "port": 1})
	res := runTestOutbound(Paths{}, Command{ID: 2, Payload: payload})
	if !res.Success {
		t.Fatalf("整体应成功: %s", res.Output)
	}
	var got []struct {
		Tag string `json:"tag"`
		OK  bool   `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res.Output), &got); err != nil || len(got) != 1 || got[0].OK {
		t.Fatalf("旧载荷回执不对: %v %s", err, res.Output)
	}
}

// TestRunTestOutboundMissingTargets 两个字段都没有 → 明确失败，不空转。
func TestRunTestOutboundMissingTargets(t *testing.T) {
	res := runTestOutbound(Paths{}, Command{ID: 3, Payload: []byte(`{}`)})
	if res.Success || res.Output != "targets 缺失" {
		t.Fatalf("空载荷应失败: success=%v output=%s", res.Success, res.Output)
	}
}
