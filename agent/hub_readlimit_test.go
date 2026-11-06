package main

import (
	"strings"
	"testing"
)

// 旧实现用裸 io.LimitReader：超过 32 MiB 时静默截断，上层 JSON 解析报
// "unexpected end of JSON input"，与真实原因（配置太大）无关，现场极难定位。
// 这里确保超限时明确报错。
func TestReadLimited(t *testing.T) {
	cases := []struct {
		name    string
		size    int
		limit   int64
		wantErr bool
	}{
		{"小于上限", 100, 1024, false},
		{"恰好等于上限", 1024, 1024, false},
		{"超过上限 1 字节", 1025, 1024, true},
		{"超过上限一倍", 2048, 1024, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, err := readLimited(strings.NewReader(strings.Repeat("x", c.size)), c.limit)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际成功读到 %d 字节", len(body))
				}
				if !strings.Contains(err.Error(), "超过") {
					t.Fatalf("错误信息应说明超限原因，实际: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if len(body) != c.size {
				t.Fatalf("读取长度 %d，期望 %d", len(body), c.size)
			}
		})
	}
}
