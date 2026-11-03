package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// briefMultiInboundConfig 造一份「3 个用户 × 2 个入站」的节点配置。
// 同一批 email 在两个入站里各出现一次 —— 非 brief 模式下 inbounds 应收齐两个 tag。
const briefMultiInboundConfig = `{
  "inbounds": [
    {"tag": "vless-443", "port": 443, "protocol": "vless",
     "settings": {"clients": [
       {"email": "ch_user_1", "totalGB": 107374182400, "expiryTime": 0},
       {"email": "ch_user_2", "totalGB": 0, "expiryTime": "2026-12-01T00:00:00Z"},
       {"email": "ch_user_3", "totalGB": 5368709120, "expiryTime": 0}
     ]}},
    {"tag": "hy2", "port": 8443, "protocol": "hysteria2",
     "settings": {"clients": [
       {"email": "ch_user_1", "totalGB": 107374182400, "expiryTime": 0},
       {"email": "ch_user_2", "totalGB": 0, "expiryTime": "2026-12-01T00:00:00Z"},
       {"email": "ch_user_3", "totalGB": 5368709120, "expiryTime": 0},
       {"email": "ch_user_3", "totalGB": 5368709120, "expiryTime": 0, "enable": false}
     ]}}
  ]
}`

// TestClientListBriefDropsInbounds 是 brief 模式的核心用例：
//   - brief=true 时 inbounds 一个都不收（变异：把 brief 判断反转，这条会红）；
//   - 对账字段（email/enable/totalGB/expiryTime）与非 brief 完全一致 —— 砍诊断信息
//     绝不能碰对账口径；
//   - 序列化后不含 inbounds 键、体积更小。
func TestClientListBriefDropsInbounds(t *testing.T) {
	full, err := parseClientListMode([]byte(briefMultiInboundConfig), false)
	if err != nil {
		t.Fatalf("非 brief 解析失败: %v", err)
	}
	brief, err := parseClientListMode([]byte(briefMultiInboundConfig), true)
	if err != nil {
		t.Fatalf("brief 解析失败: %v", err)
	}

	if full.Total != 3 || brief.Total != 3 {
		t.Fatalf("两种模式都应去重成 3 人，得到 full=%d brief=%d", full.Total, brief.Total)
	}

	// 非 brief 仍要收 tag（守住正常路径，别为了省字节把诊断信息砍成永久空）。
	if got := full.Clients[0].Inbounds; len(got) != 2 || got[0] != "vless-443" || got[1] != "hy2" {
		t.Fatalf("非 brief 模式 inbounds 应为 [vless-443 hy2]，得到 %v", got)
	}

	for i, c := range brief.Clients {
		if len(c.Inbounds) != 0 {
			t.Fatalf("brief 模式第 %d 条（%s）仍收了 inbounds=%v", i, c.Email, c.Inbounds)
		}
		// 对账字段必须与非 brief 一致（含 enable 取「任一处停用即停用」的合并结果）。
		f := full.Clients[i]
		if c.Email != f.Email || c.Enable != f.Enable ||
			fpValue(c.TotalGB) != fpValue(f.TotalGB) || fpValue(c.ExpiryTime) != fpValue(f.ExpiryTime) {
			t.Fatalf("brief 改了对账字段：brief=%+v full=%+v", c, f)
		}
	}
	// ch_user_3 在第二个入站被停用 → 两种模式都应报 enable=false。
	if brief.Clients[2].Enable {
		t.Fatal("ch_user_3 有一处 enable=false，brief 模式下也必须报停用")
	}

	fullRaw, _ := json.Marshal(full)
	briefRaw, _ := json.Marshal(brief)
	if strings.Contains(string(briefRaw), `"inbounds"`) {
		t.Fatalf("brief 回执不应出现 inbounds 键: %s", briefRaw)
	}
	if !strings.Contains(string(fullRaw), `"inbounds"`) {
		t.Fatalf("非 brief 回执应带 inbounds 键: %s", fullRaw)
	}
	if len(briefRaw) >= len(fullRaw) {
		t.Fatalf("brief 回执应更小：brief=%d full=%d 字节", len(briefRaw), len(fullRaw))
	}
	t.Logf("brief=%d 字节 / full=%d 字节，每条省 %.1f 字节",
		len(briefRaw), len(fullRaw), float64(len(fullRaw)-len(briefRaw))/float64(brief.Total))
}

// TestRunClientListBriefEndToEnd 走完整指令入口：面板发 brief:1（数字）时
//   - 载荷能解析（brief 用 flexBool 认数字 1，严格 bool 会整条失败）；
//   - 回执不带 inbounds；
//   - 回执带 clients_fp（面板分页跨页一致性校验靠它），n 与 total 同口径、sum 非空。
func TestRunClientListBriefEndToEnd(t *testing.T) {
	p := Paths{Config: filepath.Join(t.TempDir(), "config.json")}
	if err := os.WriteFile(p.Config, []byte(briefMultiInboundConfig), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}

	cases := []struct {
		name       string
		payload    string
		wantBrief  bool
		wantOutput string
	}{
		{"数字 1（面板实际发的）", `{"cursor":0,"limit":200,"brief":1}`, true, ""},
		{"JSON true", `{"cursor":0,"limit":200,"brief":true}`, true, ""},
		{"不带 brief（老面板）", `{"cursor":0,"limit":200}`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ack := runClientList(p, Command{ID: 7, Action: "client_list", Payload: json.RawMessage(tc.payload)})
			if !ack.Success {
				t.Fatalf("执行失败: %s", ack.Output)
			}
			hasInbounds := strings.Contains(ack.Output, `"inbounds"`)
			if tc.wantBrief && hasInbounds {
				t.Fatalf("brief 回执仍带 inbounds: %s", ack.Output)
			}
			if !tc.wantBrief && !hasInbounds {
				t.Fatalf("非 brief 回执丢了 inbounds: %s", ack.Output)
			}

			var res struct {
				Total     int `json:"total"`
				ClientsFP *struct {
					N   int    `json:"n"`
					Sum string `json:"sum"`
				} `json:"clients_fp"`
			}
			if err := json.Unmarshal([]byte(ack.Output), &res); err != nil {
				t.Fatalf("回执解析失败: %v", err)
			}
			if res.Total != 3 {
				t.Fatalf("total 应为 3，得到 %d", res.Total)
			}
			if res.ClientsFP == nil || res.ClientsFP.Sum == "" {
				t.Fatalf("回执应带 clients_fp: %s", ack.Output)
			}
			if res.ClientsFP.N != 3 {
				t.Fatalf("clients_fp.n 应为 3（与 total 同口径），得到 %d", res.ClientsFP.N)
			}
			t.Logf("%s：%d 字节，fp.n=%d fp.sum=%s", tc.name, len(ack.Output), res.ClientsFP.N, res.ClientsFP.Sum)
		})
	}
}
