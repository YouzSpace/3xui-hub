package collector

import (
	"strings"
	"testing"
)

// TestParseBareClientListClientsFP 验证回执里的 clients_fp 被解析出来：
// 有指纹 → n/sum 都记下；没有 → 不可用（按老 agent 处理，不做跨页校验）。
func TestParseBareClientListClientsFP(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantOK bool
		wantN  int
		want   string
	}{
		{
			name:   "带 clients_fp",
			raw:    `{"clients":[{"email":"ch_user_1","enable":true,"totalGB":0,"expiryTime":0}],"total":1,"clients_fp":{"n":1,"sum":"abc123"}}`,
			wantOK: true,
			wantN:  1,
			want:   "abc123",
		},
		{
			name:   "老 agent 不带 clients_fp",
			raw:    `{"clients":[{"email":"ch_user_1","enable":true,"totalGB":0,"expiryTime":0}],"total":1}`,
			wantOK: false,
		},
		{
			name:   "clients_fp 是 null（不能把整条回执判废）",
			raw:    `{"clients":[{"email":"ch_user_1","enable":true,"totalGB":0,"expiryTime":0}],"total":1,"clients_fp":null}`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := parseBareClientList(tc.raw)
			if !ok {
				t.Fatal("解析失败")
			}
			if parsed.clientsFP.ok() != tc.wantOK {
				t.Fatalf("指纹可用性期望 %v，得到 %v（%+v）", tc.wantOK, parsed.clientsFP.ok(), parsed.clientsFP)
			}
			if parsed.clientsFP.sum != tc.want {
				t.Fatalf("sum 期望 %q，得到 %q", tc.want, parsed.clientsFP.sum)
			}
			if parsed.clientsFP.n != tc.wantN {
				t.Fatalf("n 期望 %d，得到 %d", tc.wantN, parsed.clientsFP.n)
			}
			if len(parsed.entries) != 1 {
				t.Fatalf("应解析出 1 条 entry，得到 %d", len(parsed.entries))
			}
		})
	}
}

// TestBarePageFPMismatch 穷举跨页一致性判定的边界：
//   - 第一页没指纹（老 agent）→ 不校验，永远判没变；
//   - 两页相同 → 没变；
//   - 两页不同 / 本页缺指纹 → 判变了，调用方丢弃本轮。
func TestBarePageFPMismatch(t *testing.T) {
	cases := []struct {
		name  string
		first bareClientsFP
		cur   bareClientsFP
		want  bool // true = 判「配置变了」
	}{
		{"第一页无指纹（老 agent）", bareClientsFP{}, bareClientsFP{sum: "abc"}, false},
		{"两页都无指纹", bareClientsFP{}, bareClientsFP{}, false},
		{"两页相同", bareClientsFP{n: 3, sum: "abc123"}, bareClientsFP{n: 3, sum: "abc123"}, false},
		{"两页不同", bareClientsFP{n: 3, sum: "abc123"}, bareClientsFP{n: 4, sum: "def456"}, true},
		{"第一页有指纹本页没有", bareClientsFP{n: 3, sum: "abc123"}, bareClientsFP{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := barePageFPMismatch(tc.first, tc.cur)
			if got := err != nil; got != tc.want {
				t.Fatalf("first=%+v cur=%+v：期望判变=%v，实际 err=%v", tc.first, tc.cur, tc.want, err)
			}
			if err != nil {
				t.Logf("拦下本轮：%v", err)
			}
		})
	}
}

// TestBareClientListPayloadHasBrief 守住入参：每页都必须带 brief=1，
// 少了它 agent 就会把 inbounds 诊断字段加回来（每人多 30-40 字节）。
func TestBareClientListPayloadHasBrief(t *testing.T) {
	raw := string(bareClientListPayload(200))
	if !strings.Contains(raw, `"brief":1`) {
		t.Fatalf("分页入参缺 brief=1: %s", raw)
	}
	if !strings.Contains(raw, `"cursor":200`) {
		t.Fatalf("分页入参 cursor 错: %s", raw)
	}
	if !strings.Contains(raw, `"limit":200`) {
		t.Fatalf("分页入参 limit 错: %s", raw)
	}
}
