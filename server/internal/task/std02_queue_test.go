package task

// std02_queue_test.go 锁死队列的四件事（规范化-02 场景 3/4）：
//
//	1. 并发：10 个单元并发跑，同一个子项绝不重复执行；
//	2. 重试：失败后按 backoff 递增重放，次数用尽即收终态，退避不占 worker；
//	3. 单元时限：handler 不理会 ctx 也压不住 worker（到点脱身 + 记超时）；
//	4. 水位：queued/running/failed 三个计数在跑完后归零/累加正确。
//
// 这些行为线上都出过事故（见 queue.go 的注释：4 个卡住的单元把 worker 占满，
// 其它任务饿死 8 小时），必须有用例兜底。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestQueue 造一个「退避不真睡」的队列：sleep 记录到返回的切片里。
func newTestQueue(t *testing.T, workers, retries int, backoff []time.Duration) (*Queue, *[]time.Duration, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	delays := []time.Duration{}
	q := NewQueue(nil, QueueOptions{
		Workers: workers,
		Retries: retries,
		Backoff: backoff,
		Sleep: func(d time.Duration) {
			mu.Lock()
			delays = append(delays, d)
			mu.Unlock()
		},
		Logf: func(string, ...any) {},
	})
	t.Cleanup(q.Stop)
	return q, &delays, &mu
}

// waitFor 轮询等待条件成立（不用固定 sleep 以免偶发红）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// TestStd02QueueNoDuplicateExecution：10 个单元并发跑，每个子项只执行一次。
//
// 变异靶子：worker 里把 inflight/deadline 逻辑改坏、或 execute 重放不该重放的单元，
// 本用例的「执行次数」断言就会红。
func TestStd02QueueNoDuplicateExecution(t *testing.T) {
	q, _, _ := newTestQueue(t, 4, 4, []time.Duration{time.Millisecond})

	const n = 10
	var mu sync.Mutex
	runs := map[string]int{}
	var running, peak int64
	var wg sync.WaitGroup
	wg.Add(n)

	q.Register(JobSyncNodeTraffic, func(ctx context.Context, job JobPayload) error {
		cur := atomic.AddInt64(&running, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond) // 制造并发重叠
		atomic.AddInt64(&running, -1)

		mu.Lock()
		runs[job.ItemKey]++
		mu.Unlock()
		wg.Done()
		return nil
	})
	q.Start()

	for i := 0; i < n; i++ {
		q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic, TaskID: int64(100 + i), ItemKey: fmt.Sprintf("node:%d", i)})
	}
	wg.Wait()
	q.Stop()

	if len(runs) != n {
		t.Fatalf("应执行 %d 个子项，实际 %d 个：%v", n, len(runs), runs)
	}
	for key, times := range runs {
		if times != 1 {
			t.Errorf("子项 %s 执行了 %d 次，必须恰好 1 次", key, times)
		}
	}
	if got := atomic.LoadInt64(&peak); got > int64(4) {
		t.Errorf("并发峰值 %d 超过 worker 数 4", got)
	}
	if got := q.Stats().Running; got != 0 {
		t.Errorf("跑完后运行中应为 0，实际 %d", got)
	}
	if got := q.Stats().Pending; got != 0 {
		t.Errorf("跑完后排队中应为 0，实际 %d", got)
	}
}

// TestStd02QueueRetryBackoffIncreases：失败 N 次后放弃，退避按 backoff 序列递增。
//
// 断言三件事：① 每次重试用的是 backoff 里对应的档（递增，不是恒定 60s）；
// ② 重试次数 = Retries；③ 最终进入 failed 计数（failItem 被调用一次）。
func TestStd02QueueRetryBackoffIncreases(t *testing.T) {
	backoff := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	q, delays, mu := newTestQueue(t, 1, 2, backoff)

	var attempts int64
	q.Register(JobSyncNodeTraffic, func(ctx context.Context, job JobPayload) error {
		atomic.AddInt64(&attempts, 1)
		return errors.New("面板 502")
	})
	q.Start()

	q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic})

	waitFor(t, "队列失败计数变成 1", func() bool { return q.Stats().Failed == 1 })
	q.Stop()

	if got := atomic.LoadInt64(&attempts); got != 3 { // 首次 + 2 次重试
		t.Fatalf("执行次数 = %d，期望 3（1 次首发 + 2 次重试）", got)
	}
	mu.Lock()
	got := append([]time.Duration(nil), (*delays)...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("退避次数 = %d（%v），期望 2", len(got), got)
	}
	if got[0] != backoff[0] || got[1] != backoff[1] {
		t.Fatalf("退避序列 = %v，期望 %v（必须递增）", got, backoff)
	}
}

// TestStd02QueueSuccessStopsRetrying：第一次成功就不再重放（避免重复计费）。
func TestStd02QueueSuccessStopsRetrying(t *testing.T) {
	q, delays, mu := newTestQueue(t, 1, 3, []time.Duration{time.Millisecond})
	var attempts int64
	q.Register(JobSyncNodeTraffic, func(ctx context.Context, job JobPayload) error {
		atomic.AddInt64(&attempts, 1)
		return nil
	})
	q.Start()
	q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic})
	waitFor(t, "执行完成", func() bool { return atomic.LoadInt64(&attempts) == 1 })
	time.Sleep(20 * time.Millisecond)
	q.Stop()

	if got := atomic.LoadInt64(&attempts); got != 1 {
		t.Fatalf("成功单元执行了 %d 次，必须 1 次", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*delays) != 0 {
		t.Fatalf("成功单元不应产生退避，实际 %v", *delays)
	}
	if q.Stats().Failed != 0 {
		t.Fatalf("成功单元不应计入 failed，实际 %d", q.Stats().Failed)
	}
}

// TestStd02QueueJobTimeoutReleasesWorker：handler 无视 ctx 也必须脱身。
//
// 场景：面板 HTTP 卡死，handler 永远不返回。旧实现会把这个 worker 永久占住；
// 现在到 jobTimeout 就要判超时、worker 回池继续吃下一个单元。
func TestStd02QueueJobTimeoutReleasesWorker(t *testing.T) {
	var mu sync.Mutex
	logs := []string{}
	q := NewQueue(nil, QueueOptions{
		Workers:    1,
		Retries:    -1, // 0 会被当成默认值 4（tries=5），这里明确不要重试
		Backoff:    []time.Duration{time.Millisecond},
		JobTimeout: 60 * time.Millisecond,
		Sleep:      func(time.Duration) {},
		Logf: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		},
	})
	t.Cleanup(q.Stop)

	block := make(chan struct{})
	defer close(block)
	var second int64

	q.Register(JobSyncNodeTraffic, func(ctx context.Context, job JobPayload) error {
		if job.ItemKey == "stuck" {
			<-block // 故意无视 ctx
			return nil
		}
		atomic.AddInt64(&second, 1)
		return nil
	})
	q.Start()
	q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic, TaskID: 1, ItemKey: "stuck"})
	q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic, TaskID: 2, ItemKey: "ok"})

	waitFor(t, "卡住的单元被判超时", func() bool { return q.Stats().Failed >= 1 })
	waitFor(t, "后续单元仍然跑得动", func() bool { return atomic.LoadInt64(&second) == 1 })

	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatal("超时判失败必须留日志，否则线上无从定位")
	}
}

// TestStd02QueueUnknownKindFails：未登记的任务类型收失败终态，不许永久 running。
func TestStd02QueueUnknownKindFails(t *testing.T) {
	q, _, _ := newTestQueue(t, 1, 0, []time.Duration{0})
	q.Start()
	q.Enqueue(JobPayload{Kind: JobKind("NotRegistered"), TaskID: 1, ItemKey: "x"})
	waitFor(t, "未知类型计入失败", func() bool { return q.Stats().Failed == 1 })
	q.Stop()
}

// TestStd02QueueInflightTracksBackoffWindow：等退避期间 inflight 保持置位。
//
// 这是「超时清扫不许把还在等重试的子项当僵尸清掉」的依据：
// 只要 inflight=true，TimeoutStale 就豁免它。
func TestStd02QueueInflightTracksBackoffWindow(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var delays []time.Duration
	q := NewQueue(nil, QueueOptions{
		Workers: 1,
		Retries: 1,
		Backoff: []time.Duration{50 * time.Millisecond},
		Sleep: func(d time.Duration) {
			mu.Lock()
			delays = append(delays, d)
			mu.Unlock()
			<-release // 把「退避中」的窗口拉长，便于观察 inflight
		},
		Logf: func(string, ...any) {},
	})
	t.Cleanup(q.Stop)

	var attempts int64
	q.Register(JobSyncNodeTraffic, func(ctx context.Context, job JobPayload) error {
		if atomic.AddInt64(&attempts, 1) == 1 {
			return errors.New("首次失败")
		}
		return nil
	})
	q.Start()
	q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic, TaskID: 77, ItemKey: "node:9"})

	waitFor(t, "进入退避", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delays) == 1
	})
	if !q.Inflight(77, "node:9") {
		t.Fatal("退避期间 inflight 必须为 true，否则超时清扫会误清")
	}
	close(release)
	waitFor(t, "重试成功后 inflight 释放", func() bool { return !q.Inflight(77, "node:9") })
}

// TestStd02QueueStopDropsEnqueue：队列停止后入队直接丢弃（PHP 进程退出同向）。
func TestStd02QueueStopDropsEnqueue(t *testing.T) {
	q, _, _ := newTestQueue(t, 1, 0, []time.Duration{0})
	q.Start()
	q.Stop()
	q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic, TaskID: 5, ItemKey: "node:1"})
	if got := q.Stats().Pending; got != 0 {
		t.Fatalf("停止后入队不应增加排队数，实际 %d", got)
	}
}

// TestStd02QueueBackoffCappedAtLast：重试档位超过序列长度时停在最后一档。
func TestStd02QueueBackoffCappedAtLast(t *testing.T) {
	backoff := []time.Duration{time.Millisecond, 2 * time.Millisecond}
	q, delays, mu := newTestQueue(t, 1, 3, backoff)
	var attempts int64
	q.Register(JobSyncNodeTraffic, func(ctx context.Context, job JobPayload) error {
		atomic.AddInt64(&attempts, 1)
		return errors.New("总是失败")
	})
	q.Start()
	q.Enqueue(JobPayload{Kind: JobSyncNodeTraffic})
	waitFor(t, "重试耗尽", func() bool { return q.Stats().Failed == 1 })
	q.Stop()

	mu.Lock()
	got := append([]time.Duration(nil), (*delays)...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("退避次数 = %d（%v），期望 3", len(got), got)
	}
	if got[2] != backoff[1] {
		t.Fatalf("第 3 次退避 = %v，应停在最后一档 %v", got[2], backoff[1])
	}
}
