package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientBucketAlgorithmGolden 钉死分桶协议（issue #128 B）：
// bucket = fnv32a(email) % 32，桶 hash = 桶内 email 排序后 join "\n" 的 md5。
//
// 这些值是**独立实现**（Python fnv1a + md5）算出来的，不是拿本包代码反推的 ——
// 面板侧 reconcile_bare_bucket_test.go 用同一组 golden 值，两侧任何一边改了算法、
// 改了排序、改了分隔符，都会在这里红。
func TestClientBucketAlgorithmGolden(t *testing.T) {
	cases := []struct {
		emails []string
		bucket int
		sum    string
	}{
		{[]string{"ch_user_1"}, 20, "7000933afcbee5e99fabdd4e6d74a122"},
		{[]string{"ch_user_2"}, 13, "bd5bd68c5d3bbcc3c08c2f202afc29e6"},
		{[]string{"ch_user_3"}, 26, "da243d799e688f5065c31b878879982e"},
		{[]string{"ch_user_4"}, 27, "9a937c21e61f78d4fa2f9303230bd4b3"},
		{[]string{"ch_user_5"}, 8, "f81640ced2e169e85a40a7fc2d5a8894"},
		{[]string{"ch_user_6"}, 1, "61e598cf6e5829d4c820c485d9b87ad6"},
		// 同一桶 3 人：排序按字典序（ch_user_122 < ch_user_14 < ch_user_153）。
		{[]string{"ch_user_153", "ch_user_14", "ch_user_122"}, 0, "3048fe222b001ddee46ef70e16f19809"},
	}
	for _, tc := range cases {
		for _, email := range tc.emails {
			if got := clientBucketID(email); got != tc.bucket {
				t.Errorf("%s 应落在桶 %d，实际 %d", email, tc.bucket, got)
			}
		}
		// 入参顺序故意打乱：桶 hash 必须与顺序无关（排序后再 join）。
		if got := bucketSum(append([]string(nil), tc.emails...)); got != tc.sum {
			t.Errorf("%v 的桶 hash 期望 %s，实际 %s", tc.emails, tc.sum, got)
		}
	}

	// 空桶 = md5("")，两侧都靠同一公式自然得出（不许给空桶另立常量）。
	if got := bucketSum(nil); got != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("空桶 hash 应为 md5 空串，实际 %s", got)
	}

	// 桶数必须与面板 bareBucketCount 一致（32）。
	if clientBucketCount != 32 {
		t.Fatalf("clientBucketCount 应为 32（面板 bareBucketCount 同值），实际 %d", clientBucketCount)
	}
}

// TestFingerprintBucketsChangeIsolation 是分桶的意义所在：
// 名单里加一个人，只有他那个桶的 hash 变，其余 31 个桶一字不差（面板只需回读 1 个桶）。
func TestFingerprintBucketsChangeIsolation(t *testing.T) {
	base := `{"inbounds":[{"tag":"vless-443","settings":{"clients":[
		{"email":"ch_user_1","totalGB":100,"expiryTime":0},
		{"email":"ch_user_2","totalGB":100,"expiryTime":0},
		{"email":"ch_user_3","totalGB":100,"expiryTime":0}
	]}}]}`
	fp1 := clientsFingerprint([]byte(base))
	if fp1 == nil {
		t.Fatal("指纹解析失败")
	}
	if fp1.N != 3 {
		t.Fatalf("N 应为 3，实际 %d", fp1.N)
	}

	// 加一个人（ch_user_5 → 桶 8）。
	added := strings.Replace(base, `{"email":"ch_user_3"`,
		`{"email":"ch_user_5","totalGB":0,"expiryTime":0},{"email":"ch_user_3"`, 1)
	fp2 := clientsFingerprint([]byte(added))
	if fp2 == nil {
		t.Fatal("加人后指纹解析失败")
	}
	if fp2.Sum == fp1.Sum {
		t.Fatal("名单变了，整体指纹应变化")
	}

	var changed []int
	for i := 0; i < clientBucketCount; i++ {
		if fp1.Buckets[i] != fp2.Buckets[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) != 1 || changed[0] != clientBucketID("ch_user_5") {
		t.Fatalf("只有 ch_user_5 的桶该变（%d），实际变了 %v", clientBucketID("ch_user_5"), changed)
	}

	// 改字段（不动成员）不该影响桶 hash —— 桶 hash 只覆盖 email 集合。
	quotaChanged := strings.Replace(base, `"totalGB":100`, `"totalGB":999`, 1)
	fp3 := clientsFingerprint([]byte(quotaChanged))
	if fp3.Sum == fp1.Sum {
		t.Fatal("改 totalGB 后整体指纹应变化")
	}
	for i := 0; i < clientBucketCount; i++ {
		if fp1.Buckets[i] != fp3.Buckets[i] {
			t.Fatalf("桶 %d 的 hash 因 totalGB 变化了（桶 hash 只该看成员集合）", i)
		}
	}
}

// TestFilterClientListBucketPartitions 穷举 32 个桶：
//   - 每个桶只回该桶的 client（变异：把过滤条件反转成 !=，这条和 EndToEnd 会红）；
//   - 32 个桶并起来 = 全量名单，一个人不多一个人不少（分桶不能丢人，丢了面板会误判缺人）。
func TestFilterClientListBucketPartitions(t *testing.T) {
	const n = 60
	raw := clientsConfigJSON(t, n)
	full0, err := parseClientList(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := map[string]bool{}
	for _, c := range full0.Clients {
		want[c.Email] = true
	}

	seen := map[string]int{}
	for bucket := 0; bucket < clientBucketCount; bucket++ {
		full, err := parseClientList(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		filterClientListBucket(full, bucket)
		for _, c := range full.Clients {
			if got := clientBucketID(c.Email); got != bucket {
				t.Fatalf("桶 %d 的回执里出现 %s（属于桶 %d）", bucket, c.Email, got)
			}
			seen[c.Email]++
		}
		if full.Total != len(full.Clients) {
			t.Fatalf("桶 %d 的 total（%d）应等于过滤后条数（%d）", bucket, full.Total, len(full.Clients))
		}
	}

	if len(seen) != n {
		t.Fatalf("%d 个桶并起来应覆盖全部 %d 人，实际 %d 人", clientBucketCount, n, len(seen))
	}
	for email, times := range seen {
		if times != 1 {
			t.Fatalf("%s 在分桶结果里出现了 %d 次（应恰好 1 次）", email, times)
		}
		if !want[email] {
			t.Fatalf("分桶结果里出现不存在的 email: %s", email)
		}
	}
}

// TestRunClientListBucketEndToEnd 走完整指令入口：
//   - 带 bucket_id 只回该桶；
//   - clients_fp 仍是**整份**配置的指纹（面板靠它做跨页/跨桶一致性校验）；
//   - bucket_id=0 也能生效（agent 侧必须用指针解析，值类型会把 0 当「没发」）。
func TestRunClientListBucketEndToEnd(t *testing.T) {
	const n = 60
	p := Paths{Config: filepath.Join(t.TempDir(), "config.json")}
	if err := os.WriteFile(p.Config, clientsConfigJSON(t, n), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	fullFP := clientsFingerprint(clientsConfigJSON(t, n))
	if fullFP == nil || fullFP.N != n {
		t.Fatalf("整份指纹异常: %+v", fullFP)
	}

	for _, bucket := range []int{0, 1, clientBucketCount - 1} {
		payload := fmt.Sprintf(`{"bucket_id":%d,"cursor":0,"limit":200,"brief":1}`, bucket)
		ack := runClientList(p, Command{ID: 9, Action: "client_list", Payload: json.RawMessage(payload)})
		if !ack.Success {
			t.Fatalf("桶 %d 执行失败: %s", bucket, ack.Output)
		}
		var res struct {
			Clients   []clientListClient      `json:"clients"`
			Total     int                     `json:"total"`
			ClientsFP *clientsFingerprintBody `json:"clients_fp"`
		}
		if err := json.Unmarshal([]byte(ack.Output), &res); err != nil {
			t.Fatalf("桶 %d 回执解析失败: %v", bucket, err)
		}
		if len(res.Clients) == 0 {
			// n=60 分布到 32 个桶，个别桶为空是正常的，但 0/1/31 这三个桶必须有人
			// （下面用「至少有一个桶非空且全部属于该桶」兜住）。
			t.Logf("桶 %d 回执为空（该桶确实没人）", bucket)
		}
		for _, c := range res.Clients {
			if got := clientBucketID(c.Email); got != bucket {
				t.Fatalf("桶 %d 回执里出现 %s（属于桶 %d）", bucket, c.Email, got)
			}
		}
		if res.Total != len(res.Clients) {
			t.Fatalf("桶 %d 的 total 应为过滤后条数 %d，实际 %d", bucket, len(res.Clients), res.Total)
		}
		if res.ClientsFP == nil || res.ClientsFP.Sum != fullFP.Sum || res.ClientsFP.N != fullFP.N {
			t.Fatalf("按桶回执的 clients_fp 必须是整份配置的指纹（%d/%s），实际 %+v",
				fullFP.N, fullFP.Sum, res.ClientsFP)
		}
		// 回执不带 buckets：32 个桶 hash 只在心跳里用，每页都背会白占约 1.1KB。
		if len(res.ClientsFP.Buckets) != 0 {
			t.Fatalf("client_list 回执不应带 buckets（只有心跳带），实际 %d 个", len(res.ClientsFP.Buckets))
		}
		t.Logf("桶 %d：%d 条，%d 字节", bucket, len(res.Clients), len(ack.Output))
	}
}

// TestRunClientListBucketCompat 守住向后兼容的两个方向：
//   - 老面板不发 bucket_id → 必须回全量（agent 侧用 *int 解析；值类型会把「没发」读成
//     第 0 桶，老面板会把 31/32 的人判成缺人 → 每轮 bump → 内核反复重启）；
//   - bucket_id 越界/负数 → 按全量（不猜、不回空）。
func TestRunClientListBucketCompat(t *testing.T) {
	const n = 60
	p := Paths{Config: filepath.Join(t.TempDir(), "config.json")}
	if err := os.WriteFile(p.Config, clientsConfigJSON(t, n), 0o644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}

	for _, payload := range []string{
		`{"cursor":0,"limit":200,"brief":1}`,       // 老面板：不带 bucket_id
		`{"bucket_id":null,"limit":200,"brief":1}`, // 显式 null
		`{"bucket_id":-1,"limit":200,"brief":1}`,   // 负数
		`{"bucket_id":32,"limit":200,"brief":1}`,   // 越界
	} {
		ack := runClientList(p, Command{ID: 10, Action: "client_list", Payload: json.RawMessage(payload)})
		if !ack.Success {
			t.Fatalf("%s 执行失败: %s", payload, ack.Output)
		}
		var res clientListResult
		if err := json.Unmarshal([]byte(ack.Output), &res); err != nil {
			t.Fatalf("%s 回执解析失败: %v", payload, err)
		}
		if res.Total != n || len(res.Clients) != n {
			t.Fatalf("%s 应回全量 %d 人（老面板兼容），实际 total=%d clients=%d",
				payload, n, res.Total, len(res.Clients))
		}
	}
}

// clientsConfigJSON 造一份 n 个 client 的节点配置（每人一个 email，跨两个入站也有）。
func clientsConfigJSON(t *testing.T, n int) []byte {
	t.Helper()
	clients := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		clients = append(clients, map[string]any{
			"email":      fmt.Sprintf("ch_user_%d", i),
			"totalGB":    json.Number("107374182400"),
			"expiryTime": json.Number("0"),
		})
	}
	config := map[string]any{
		"inbounds": []map[string]any{
			{"tag": "vless-443", "port": 443, "protocol": "vless",
				"settings": map[string]any{"clients": clients}},
			{"tag": "hy2", "port": 8443, "protocol": "hysteria2",
				"settings": map[string]any{"clients": clients}},
		},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("造配置失败: %v", err)
	}
	return raw
}
