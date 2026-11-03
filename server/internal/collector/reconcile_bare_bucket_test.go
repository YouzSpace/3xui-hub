package collector

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// TestBareBucketAlgorithmGolden 钉死面板侧的分桶协议（issue #128 B），
// 用的是与 agent 侧 clientlist_bucket_test.go **同一组** golden 值
// （独立 Python 实现算出，不是拿本包代码反推）：
//
//	bucket = fnv32a(email) % 32，桶 hash = 桶内 email 排序后 join "\n" 的 md5。
//
// 两侧任何一边改算法/排序/分隔符，都会有一边红 —— 不一致的后果是「32 个桶每轮全判
// 变化」→ 优化彻底失效。
func TestBareBucketAlgorithmGolden(t *testing.T) {
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
		// 同一桶 3 人，入参顺序故意不是字典序：hash 必须与顺序无关。
		{[]string{"ch_user_153", "ch_user_14", "ch_user_122"}, 0, "3048fe222b001ddee46ef70e16f19809"},
	}
	for _, tc := range cases {
		for _, email := range tc.emails {
			if got := bareBucketID(email); got != tc.bucket {
				t.Errorf("%s 应落在桶 %d，实际 %d", email, tc.bucket, got)
			}
		}
		if got := bareBucketSum(append([]string(nil), tc.emails...)); got != tc.sum {
			t.Errorf("%v 的桶 hash 期望 %s，实际 %s", tc.emails, tc.sum, got)
		}
	}
	if got := bareBucketSum(nil); got != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("空桶 hash 应为 md5 空串，实际 %s", got)
	}
	if bareBucketCount != 32 {
		t.Fatalf("bareBucketCount 应为 32（agent 侧 clientBucketCount 同值），实际 %d", bareBucketCount)
	}
	// 入参切片不能被改写（调用方可能复用）。
	in := []string{"ch_user_3", "ch_user_1"}
	_ = bareBucketSum(in)
	if in[0] != "ch_user_3" {
		t.Fatalf("bareBucketSum 改写了入参: %v", in)
	}
}

// TestBareBucketsOf 守住心跳解析：形状对才认，任何异常一律 nil（= 按老 agent 走全量读）。
func TestBareBucketsOf(t *testing.T) {
	good := make([]any, bareBucketCount)
	for i := range good {
		good[i] = strings.Repeat("a", 32)
	}
	ok := bareBucketsOf(good)
	if len(ok) != bareBucketCount {
		t.Fatalf("32 个合法 hash 应全部接受，实际 %d 个", len(ok))
	}

	short := make([]any, bareBucketCount-1)
	for i := range short {
		short[i] = strings.Repeat("a", 32)
	}
	withEmpty := append([]any(nil), good...)
	withEmpty[7] = ""

	cases := []struct {
		name string
		in   any
		want bool // true = 认（可用）
	}{
		{"正常 32 个", good, true},
		{"只有 31 个", short, false},
		{"有一个空串", withEmpty, false},
		{"老 agent 没有这个键", nil, false},
		{"类型不对", "not-a-list", false},
		{"空对象", map[string]any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bareBucketsOf(tc.in)
			if (got != nil) != tc.want {
				t.Fatalf("期望可用=%v，实际 %v", tc.want, got != nil)
			}
		})
	}
}

// TestReadBareNodeStateBuckets 验证心跳里的 buckets 被读出来；
// 老 agent（只带 n/sum）→ buckets 为 nil、bucketOK=false（面板必须走全量读）。
func TestReadBareNodeStateBuckets(t *testing.T) {
	buckets := make([]string, bareBucketCount)
	for i := range buckets {
		buckets[i] = strings.Repeat("b", 32)
	}
	newAgent, _ := json.Marshal(map[string]any{
		"config_version": 7,
		"agent": map[string]any{
			"clients_fp": map[string]any{"n": 3, "sum": "sum123", "buckets": buckets},
		},
	})
	oldAgent, _ := json.Marshal(map[string]any{
		"config_version": 7,
		"agent": map[string]any{
			"clients_fp": map[string]any{"n": 3, "sum": "sum123"},
		},
	})

	for _, tc := range []struct {
		name       string
		cfg        []byte
		wantBucket bool
	}{
		{"新 agent 带 buckets", newAgent, true},
		{"老 agent 不带 buckets", oldAgent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := nodeRow{ID: 1, Name: "n1", DriverConfig: sql.NullString{String: string(tc.cfg), Valid: true}}
			cur, base, ver := readBareNodeState(node)
			if ver != 7 {
				t.Fatalf("config_version 应为 7，实际 %d", ver)
			}
			if cur.sum != "sum123" || cur.n != 3 {
				t.Fatalf("指纹 n/sum 读错: %+v", cur)
			}
			if cur.bucketOK() != tc.wantBucket {
				t.Fatalf("bucketOK 期望 %v，实际 %v", tc.wantBucket, cur.bucketOK())
			}
			if tc.wantBucket && len(cur.buckets) != bareBucketCount {
				t.Fatalf("buckets 应有 %d 个，实际 %d", bareBucketCount, len(cur.buckets))
			}
			if base.executedAt != "" {
				t.Fatalf("没有基线时不应有 executed_at: %+v", base)
			}
		})
	}
}

// TestBareExpectedBucketSumsEligibility 只把 renderEligible 的人算进「节点上该有的人」：
// 无套餐/未启用/带标记的人不会被渲染进节点配置，算进去会让含他们的桶每轮都白读。
func TestBareExpectedBucketSumsEligibility(t *testing.T) {
	eligible := reconcileUser{ID: 1, AccountEnabled: true, PlanIDValid: true}
	noPlan := reconcileUser{ID: 2, AccountEnabled: true}
	disabled := reconcileUser{ID: 3, AccountEnabled: false, PlanIDValid: true}
	flagged := reconcileUser{ID: 4, AccountEnabled: true, PlanIDValid: true, FlagSet: true}

	got := bareExpectedBucketSums([]reconcileUser{eligible, noPlan, disabled, flagged})
	want := bareExpectedBucketSums([]reconcileUser{eligible})
	for i := 0; i < bareBucketCount; i++ {
		if got[i] != want[i] {
			t.Fatalf("桶 %d：非渲染用户不该参与（got=%s want=%s）", i, got[i], want[i])
		}
	}

	// 换成「节点上只有 ch_user_1」的桶指纹，逐桶比对必须无差异。
	node := bareClientsFP{n: 1, sum: "s", buckets: want[:]}
	diff, ok := bareDiffBuckets(node, []reconcileUser{eligible})
	if !ok || len(diff) != 0 {
		t.Fatalf("面板预期与节点一致时应无差异桶，实际 diff=%v ok=%v", diff, ok)
	}
}

// TestBareDiffBuckets 穷举逐桶比对的判据：
//   - 老 agent 没有 buckets → ok=false（走全量读）；
//   - 成员一致 → 无差异；
//   - 少一人 / 多一人（删号残留、陌生 email）→ 只有那个人的桶有差异；
//   - 只改 totalGB（成员不变）→ 无差异（桶 hash 只覆盖 email 集合，这是刻意的取舍）。
func TestBareDiffBuckets(t *testing.T) {
	users := []reconcileUser{
		{ID: 1, AccountEnabled: true, PlanIDValid: true, Limit: 100},
		{ID: 2, AccountEnabled: true, PlanIDValid: true, Limit: 100},
		{ID: 3, AccountEnabled: true, PlanIDValid: true, Limit: 100},
	}
	sums := bareExpectedBucketSums(users)
	nodeFP := func(buckets [bareBucketCount]string) bareClientsFP {
		return bareClientsFP{n: 3, sum: "overall", buckets: buckets[:]}
	}
	// 用独立算出的桶 hash 覆盖某一桶：模拟「节点上这个桶的成员与面板不同」。
	withEmails := func(base [bareBucketCount]string, emails ...string) [bareBucketCount]string {
		out := base
		for _, e := range emails {
			b := bareBucketID(e)
			// 把该桶的 hash 换成「只含这个 email」的 hash —— 与预期集合必然不同。
			out[b] = bareBucketSum([]string{e})
		}
		return out
	}

	t.Run("老 agent 无 buckets", func(t *testing.T) {
		diff, ok := bareDiffBuckets(bareClientsFP{n: 3, sum: "overall"}, users)
		if ok || diff != nil {
			t.Fatalf("无桶指纹应判不可用（走全量读），实际 ok=%v diff=%v", ok, diff)
		}
	})

	t.Run("完全一致", func(t *testing.T) {
		diff, ok := bareDiffBuckets(nodeFP(sums), users)
		if !ok || len(diff) != 0 {
			t.Fatalf("应无差异，实际 ok=%v diff=%v", ok, diff)
		}
	})

	t.Run("节点少一人", func(t *testing.T) {
		// 节点上没有 ch_user_2：把它所在桶的 hash 改成「该桶其余人」的 hash。
		rest := []string{}
		for _, u := range users {
			if u.ID != 2 {
				rest = append(rest, clientEmail(u.ID))
			}
		}
		buckets := sums
		buckets[bareBucketID("ch_user_2")] = bareBucketSum(rest)
		diff, ok := bareDiffBuckets(nodeFP(buckets), users)
		if !ok || len(diff) != 1 || diff[0] != bareBucketID("ch_user_2") {
			t.Fatalf("应只有 ch_user_2 的桶有差异（%d），实际 diff=%v ok=%v",
				bareBucketID("ch_user_2"), diff, ok)
		}
	})

	t.Run("节点多人（删号残留）", func(t *testing.T) {
		buckets := withEmails(sums, "ch_user_999")
		diff, ok := bareDiffBuckets(nodeFP(buckets), users)
		if !ok || len(diff) != 1 || diff[0] != bareBucketID("ch_user_999") {
			t.Fatalf("应只有 ch_user_999 的桶有差异（%d），实际 diff=%v ok=%v",
				bareBucketID("ch_user_999"), diff, ok)
		}
	})

	t.Run("只改 totalGB 不产生差异桶", func(t *testing.T) {
		// 桶 hash 只覆盖 email 集合 —— 字段漂移这一轮读不到（刻意的取舍：
		// 漂移只报告、从不触发重拉，见 bareCompareResult.needBump）。
		changed := []reconcileUser{
			{ID: 1, AccountEnabled: true, PlanIDValid: true, Limit: 999},
			{ID: 2, AccountEnabled: true, PlanIDValid: true, Limit: 100},
			{ID: 3, AccountEnabled: true, PlanIDValid: true, Limit: 100},
		}
		diff, ok := bareDiffBuckets(nodeFP(sums), changed)
		if !ok || len(diff) != 0 {
			t.Fatalf("只改限额不应产生差异桶，实际 diff=%v", diff)
		}
	})

	t.Run("节点上被停用也不产生差异桶（裸内核路径不可达）", func(t *testing.T) {
		// 桶 hash 只有成员：enable=false 不会让桶变。裸内核的渲染结果根本不写 enable
		// 字段，agent 恒报 true，所以 Disabled 在裸内核路径上不可达；这条用例把这个
		// 「已知取舍」钉住 —— 将来若渲染开始写 enable，必须改设计（见 bareFillVerifiedBuckets）。
		diff, ok := bareDiffBuckets(nodeFP(sums), users)
		if !ok || len(diff) != 0 {
			t.Fatalf("成员没变就不该有差异桶（enable 不在桶 hash 里），实际 diff=%v", diff)
		}
	})
}

// TestBareFillVerifiedBuckets 是「只读差异桶」不误删人的关键：
// 没读的桶按面板预期补齐，读过的桶一律不动（读回的实际值才是结论）。
func TestBareFillVerifiedBuckets(t *testing.T) {
	u1 := reconcileUser{ID: 1, AccountEnabled: true, PlanIDValid: true, Limit: 111, ExpiryMs: 111000}
	u2 := reconcileUser{ID: 2, AccountEnabled: true, PlanIDValid: true, Limit: 222, ExpiryMs: 222000}
	u3 := reconcileUser{ID: 3, AccountEnabled: true} // 无套餐 → 不渲染，不补
	b1, b2 := bareBucketID(clientEmail(u1.ID)), bareBucketID(clientEmail(u2.ID))
	if b1 == b2 {
		t.Fatalf("测试前提失败：u1/u2 落在同一个桶（%d）", b1)
	}

	// 本轮只读了 b1，且读回的结果是「u1 在节点上被停用」（必须保留，不能被补齐覆盖）。
	out := bareClientSnapshot{entries: map[string]nodeSnapshotEntry{
		clientEmail(u1.ID): {Enable: false},
	}}
	bareFillVerifiedBuckets(&out, map[int]bool{b1: true}, []reconcileUser{u1, u2, u3})

	if got := out.entries[clientEmail(u1.ID)]; got.Enable {
		t.Fatalf("读过的桶不能按面板预期覆盖（节点上 u1 是停用的）: %+v", got)
	}
	got, exists := out.entries[clientEmail(u2.ID)]
	if !exists {
		t.Fatal("没读的桶必须按面板预期补齐（否则会被判成节点上缺人）")
	}
	if !got.Enable || got.TotalGB != u2.Limit || got.ExpiryMs != u2.ExpiryMs {
		t.Fatalf("补齐的值应取面板预期: %+v", got)
	}
	if _, exists := out.entries[clientEmail(u3.ID)]; exists {
		t.Fatal("非渲染用户（无套餐）不该补进 entries")
	}
}

// bareBucketsFromEmails 按协议算「一组 email」的 32 桶指纹（测试辅助，模拟节点上报）。
func bareBucketsFromEmails(emails []string) []string {
	byBucket := [bareBucketCount][]string{}
	for _, e := range emails {
		b := bareBucketID(e)
		byBucket[b] = append(byBucket[b], e)
	}
	out := make([]string, bareBucketCount)
	for i, list := range byBucket {
		out[i] = bareBucketSum(list)
	}
	return out
}

// TestPartialReadThenCompare 是端到端的核心断言：
// 只读差异桶 + 补齐之后走现有 compareBareClients，「没读的人」绝不能被算成 Missing
// （那会每轮 bump → 内核反复重启），真正的成员差异必须照旧被抓住。
func TestPartialReadThenCompare(t *testing.T) {
	users := []reconcileUser{
		{ID: 11, AccountEnabled: true, PlanIDValid: true, Limit: 1000, ExpiryMs: 1000},
		{ID: 12, AccountEnabled: true, PlanIDValid: true, Limit: 2000, ExpiryMs: 2000},
		{ID: 13, AccountEnabled: true, PlanIDValid: true, Limit: 3000, ExpiryMs: 3000},
		{ID: 14, AccountEnabled: true, PlanIDValid: true, Limit: 4000, ExpiryMs: 4000},
	}
	// 桶是 32 个，用户有 4 个 → 先挑出「独占一个桶」的那位当缺人样本，
	// 再造一个落在空桶里的删号残留（两个差异桶互不干扰，结论才干净）。
	byBucket := map[int][]int64{}
	for _, u := range users {
		b := bareBucketID(clientEmail(u.ID))
		byBucket[b] = append(byBucket[b], u.ID)
	}
	var missing reconcileUser
	for _, u := range users {
		if len(byBucket[bareBucketID(clientEmail(u.ID))]) == 1 {
			missing = u
			break
		}
	}
	if missing.ID == 0 {
		t.Fatal("测试前提失败：4 个用户挤在同一个桶，无法构造独占样本")
	}
	orphan := ""
	for i := 900; i < 1200; i++ {
		cand := clientEmail(int64(i))
		if _, taken := byBucket[bareBucketID(cand)]; !taken {
			orphan = cand
			break
		}
	}
	if orphan == "" {
		t.Fatal("测试前提失败：找不到空桶放删号残留")
	}

	// 节点实际状态：少了 missing，多了 orphan。
	emails := []string{orphan}
	for _, u := range users {
		if u.ID == missing.ID {
			continue
		}
		emails = append(emails, clientEmail(u.ID))
	}

	node := bareClientsFP{n: len(emails), sum: "overall", buckets: bareBucketsFromEmails(emails)}
	diff, ok := bareDiffBuckets(node, users)
	if !ok {
		t.Fatal("应该能逐桶比对")
	}
	if len(diff) != 2 {
		t.Fatalf("应恰好 2 个差异桶（missing 的 + orphan 的），实际 %v", diff)
	}

	// 模拟「只读了差异桶」：missing 的桶读回空、orphan 的桶读回它自己。
	snap := bareClientSnapshot{entries: map[string]nodeSnapshotEntry{
		orphan: {Enable: true},
	}}
	read := map[int]bool{}
	for _, b := range diff {
		read[b] = true
	}
	bareFillVerifiedBuckets(&snap, read, users)

	cmp := compareBareClients(snap.entries, users)
	if cmp.Missing != 1 {
		t.Fatalf("应只报 1 个缺人（missing），实际 %d（补齐逻辑漏了 → 会每轮 bump）", cmp.Missing)
	}
	if cmp.Extra != 1 {
		t.Fatalf("应报 1 个该删却在（orphan），实际 %d", cmp.Extra)
	}
	if cmp.Disabled != 0 || cmp.Stale != 0 || cmp.Incomplete != 0 || cmp.Drift != 0 {
		t.Fatalf("不该有其它结论: %+v", cmp)
	}
	if !cmp.needBump() {
		t.Fatal("缺人 + 该删却在必须触发重拉")
	}
}

// TestBareClientListBucketPayload 守住按桶入参：bucket_id 必须显式发（0 号桶也要发，
// agent 侧是 *int，省略 = 全量），并且仍带 brief=1 / cursor / limit。
func TestBareClientListBucketPayload(t *testing.T) {
	raw := string(bareClientListBucketPayload(0, 400))
	for _, want := range []string{`"bucket_id":0`, `"cursor":400`, `"limit":200`, `"brief":1`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("按桶入参缺 %s: %s", want, raw)
		}
	}
}
