package collector

import "testing"

// 临时验证：parseBareClientList 解析分页回执的 next 游标。跑完即删。
func TestParseBareClientListNext(t *testing.T) {
	// 分页回执：next=200 表示还有下一页。
	raw := `{"clients":[{"email":"ch_user_1","enable":true,"totalGB":0,"expiryTime":0}],"total":300,"next":200}`
	parsed, ok := parseBareClientList(raw)
	if !ok {
		t.Fatal("解析失败")
	}
	if parsed.next != 200 {
		t.Fatalf("期望 next=200，得到 %d", parsed.next)
	}
	if parsed.truncated {
		t.Fatal("分页回执不应 truncated")
	}
	if len(parsed.entries) != 1 {
		t.Fatalf("期望 1 entry，得到 %d", len(parsed.entries))
	}
	t.Logf("✅ next 解析通过: next=%d, entries=%d", parsed.next, len(parsed.entries))
}

// 临时验证：无 next 字段（老 agent 全量回执）时 next=0。
func TestParseBareClientListNoNext(t *testing.T) {
	raw := `{"clients":[{"email":"ch_user_1","enable":true,"totalGB":0,"expiryTime":0}],"total":1}`
	parsed, ok := parseBareClientList(raw)
	if !ok {
		t.Fatal("解析失败")
	}
	if parsed.next != 0 {
		t.Fatalf("无 next 时应为 0，得到 %d", parsed.next)
	}
	t.Logf("✅ 无 next 时 next=0（老 agent 全量回执，循环只跑一次）")
}
