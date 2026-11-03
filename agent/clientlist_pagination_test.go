package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestClientListPagination 验证 issue #127 分页拉取：300 人分 2 页拉全，
// 每页 < 39000 字符、无截断。这是分页协议的核心正确性保障。
func TestClientListPagination(t *testing.T) {
	res := mustParseClients(t, 300)

	var all []clientListClient
	cursor, pages := int64(0), 0
	for {
		ack := marshalClientListAck(1, res, cursor, 200)
		if !ack.Success {
			t.Fatalf("第 %d 页失败", pages+1)
		}
		if len(ack.Output) >= clientListOutputMax {
			t.Fatalf("第 %d 页超 39000 上限：%d 字符", pages+1, len(ack.Output))
		}
		var page clientListResult
		if err := json.Unmarshal([]byte(ack.Output), &page); err != nil {
			t.Fatalf("第 %d 页解析失败: %v", pages+1, err)
		}
		if page.Truncated {
			t.Fatalf("第 %d 页被截断（分页模式不应截断）: %s", pages+1, page.Reason)
		}
		if len(page.Clients) == 0 {
			t.Fatalf("第 %d 页为空", pages+1)
		}
		all = append(all, page.Clients...)
		pages++
		if page.Next <= 0 {
			break
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatalf("疑似死循环，已拉 %d 页", pages)
		}
	}

	if len(all) != 300 {
		t.Fatalf("分页拉全后应 300 人，得到 %d", len(all))
	}
	if pages != 2 {
		t.Fatalf("300 人 / 每页 200 应 2 页，得到 %d 页", pages)
	}
}

// TestClientListFullLegacy 验证老面板（limit=0）行为不变：回全量，超 39000 截断 + truncated。
func TestClientListFullLegacy(t *testing.T) {
	res := mustParseClients(t, 500)

	ack := marshalClientListAck(1, res, 0, 0)
	if !ack.Success {
		t.Fatal("全量模式失败")
	}
	var full clientListResult
	if err := json.Unmarshal([]byte(ack.Output), &full); err != nil {
		t.Fatal(err)
	}
	if !full.Truncated {
		t.Fatal("500 人超 39000，全量模式应截断标 truncated")
	}
}

// mustParseClients 造 n 个 client 的配置并解析成 clientListResult。
func mustParseClients(t *testing.T, n int) *clientListResult {
	t.Helper()
	clients := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		clients = append(clients, map[string]any{
			"email":      fmt.Sprintf("ch_user_%d@test.com", i),
			"totalGB":    json.Number("107374182400"),
			"expiryTime": json.Number("0"),
		})
	}
	config := map[string]any{
		"inbounds": []map[string]any{{
			"tag":      "vless-443",
			"port":     443,
			"protocol": "vless",
			"settings": map[string]any{"clients": clients},
		}},
	}
	raw, _ := json.Marshal(config)
	res, err := parseClientList(raw)
	if err != nil {
		t.Fatalf("parseClientList: %v", err)
	}
	if len(res.Clients) != n {
		t.Fatalf("期望 %d client，得到 %d", n, len(res.Clients))
	}
	return res
}
