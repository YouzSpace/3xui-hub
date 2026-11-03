package nodeapi

import "testing"

// TestClientsFingerprintPatchKeepsBuckets 验证分桶 Merkle 的关键链路：
// agent 心跳上报的 clients_fp.buckets 必须被保留进 driver_config，否则
// reconcile_bare 的 bareBucketOK 恒 false、分桶永不触发（真机踩坑）。
func TestClientsFingerprintPatchKeepsBuckets(t *testing.T) {
	// 新 agent：带 buckets（32 桶 hash）。
	data := map[string]any{
		"clients_fp": map[string]any{
			"n":   float64(300),
			"sum": "abc123",
			"buckets": func() []any {
				b := make([]any, 32)
				for i := range b {
					b[i] = "d41d8cd98f00b204e9800998ecf8427e"
				}
				return b
			}(),
		},
	}
	out := ClientsFingerprintPatch(data)
	obj, ok := out.(Object)
	if !ok {
		t.Fatalf("期望 Object，得到 %T", out)
	}
	bv, has := obj.Get("buckets")
	if !has {
		t.Fatal("丢了 buckets 字段（分桶 Merkle 会失效）")
	}
	if list, ok := bv.([]any); !ok || len(list) != 32 {
		t.Fatalf("buckets 形状不对：%v", bv)
	}

	// 老 agent：不带 buckets → 不应有 buckets 字段（行为与旧版一致）。
	data2 := map[string]any{
		"clients_fp": map[string]any{"n": float64(1), "sum": "x"},
	}
	out2 := ClientsFingerprintPatch(data2)
	obj2, _ := out2.(Object)
	if _, has := obj2.Get("buckets"); has {
		t.Fatal("老 agent 不应有 buckets 字段")
	}

	// buckets 显式 null → 不应有 buckets 字段。
	data3 := map[string]any{
		"clients_fp": map[string]any{"n": float64(1), "sum": "x", "buckets": nil},
	}
	out3 := ClientsFingerprintPatch(data3)
	obj3, _ := out3.(Object)
	if _, has := obj3.Get("buckets"); has {
		t.Fatal("buckets=null 不应有 buckets 字段")
	}
}
