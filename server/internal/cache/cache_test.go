package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// newTestClient 连接真实 Redis；连不上就直接 Skip。
//
// 仓库的 CI 和开发机都不保证有 Redis，所以所有依赖真服务的用例都必须走这里：
// 默认连 127.0.0.1:6379，可用 REDIS_ADDR / REDIS_PASSWORD / REDIS_DB 覆盖。
// 没有 Redis 时 go test 输出是 SKIP 而不是 PASS，不会假装跑过。
func newTestClient(t *testing.T) *Client {
	t.Helper()
	addr := envOr("REDIS_ADDR", DefaultAddr)
	c, err := New(addr, os.Getenv("REDIS_PASSWORD"), envInt("REDIS_DB", 0))
	if err != nil {
		t.Fatalf("New(%q) 失败: %v", addr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()
		t.Skipf("无可用 Redis（%s），跳过真用例: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// testKey 给每个用例一个独立 key，避免同一库上并发跑测试时互相踩。
func testKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s:%s:%d", t.Name(), time.Now().Format("150405.000000"), os.Getpid())
}

func TestCacheSetGet(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	nodeID := time.Now().UnixNano()
	t.Cleanup(func() { _ = c.rdb.Del(context.Background(), NodeLoadKey(nodeID)).Err() })

	want := NodeLoad{Users: 12, MemUsed: 512, MemTotal: 4096, CPU: 37.5, Conns: 88, Ts: time.Now().Unix()}
	if err := c.SetNodeLoad(ctx, nodeID, want); err != nil {
		t.Fatalf("SetNodeLoad 失败: %v", err)
	}
	got, err := c.GetNodeLoad(ctx, nodeID)
	if err != nil {
		t.Fatalf("GetNodeLoad 失败: %v", err)
	}
	if got == nil {
		t.Fatal("GetNodeLoad 返回 nil，期望拿到刚写入的负载")
	}
	if *got != want {
		t.Fatalf("往返不一致：写入 %+v，读回 %+v", want, *got)
	}

	// 宿主持久化的 TTL 必须是 LoadTTL（5 分钟），不是永不过期。
	ttl, err := c.rdb.TTL(ctx, NodeLoadKey(nodeID)).Result()
	if err != nil {
		t.Fatalf("读取 TTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > LoadTTL {
		t.Fatalf("TTL = %v，期望 (0, %v]", ttl, LoadTTL)
	}
}

func TestCacheGetMissing(t *testing.T) {
	c := newTestClient(t)
	got, err := c.GetNodeLoad(context.Background(), time.Now().UnixNano())
	if err != nil {
		t.Fatalf("未命中的 key 不该返回 error，得到: %v", err)
	}
	if got != nil {
		t.Fatalf("未命中的 key 应返回 nil，得到 %+v", *got)
	}
}

func TestCacheExpiredLoad(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	nodeID := time.Now().UnixNano()
	key := NodeLoadKey(nodeID)
	t.Cleanup(func() { _ = c.rdb.Del(context.Background(), key).Err() })

	// 直接写一条 100ms TTL 的负载，模拟「5 分钟没更新」的过期状态，避免真等 300 秒。
	if err := c.rdb.Set(ctx, key, `{"users":1,"ts":1}`, 100*time.Millisecond).Err(); err != nil {
		t.Fatalf("写入短 TTL 负载失败: %v", err)
	}
	time.Sleep(250 * time.Millisecond)

	got, err := c.GetNodeLoad(ctx, nodeID)
	if err != nil {
		t.Fatalf("过期 key 不该返回 error，得到: %v", err)
	}
	if got != nil {
		t.Fatalf("过期后应视为未命中（nil），得到 %+v", *got)
	}
}

func TestCacheGetNodeLoads(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	base := time.Now().UnixNano()
	ids := []int64{base, base + 1, base + 2}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = c.rdb.Del(context.Background(), NodeLoadKey(id)).Err()
		}
	})

	want := map[int64]NodeLoad{
		ids[0]: {Users: 3, MemUsed: 128, MemTotal: 2048, CPU: 12.5, Conns: 9, Ts: 1700000000},
		ids[1]: {Users: 7, MemUsed: 256, MemTotal: 2048, CPU: 55, Conns: 21, Ts: 1700000001},
	}
	for id, load := range want {
		if err := c.SetNodeLoad(ctx, id, load); err != nil {
			t.Fatalf("SetNodeLoad(%d) 失败: %v", id, err)
		}
	}

	got, err := c.GetNodeLoads(ctx, ids)
	if err != nil {
		t.Fatalf("GetNodeLoads 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("批量读取应返回 2 个节点（第三个没上报），得到 %d 个: %+v", len(got), got)
	}
	for id, load := range want {
		if got[id] != load {
			t.Fatalf("节点 %d 往返不一致：写入 %+v，读回 %+v", id, load, got[id])
		}
	}
	if _, exists := got[ids[2]]; exists {
		t.Fatalf("未上报的节点 %d 不该出现在结果里", ids[2])
	}

	// 空入参是合法调用，直接返回空 map，不该打 Redis。
	if empty, err := c.GetNodeLoads(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("空列表应返回空 map 且无 error，得到 len=%d err=%v", len(empty), err)
	}
}

func TestCacheCorruptLoadSkipped(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	nodeID := time.Now().UnixNano()
	key := NodeLoadKey(nodeID)
	t.Cleanup(func() { _ = c.rdb.Del(context.Background(), key).Err() })

	if err := c.rdb.Set(ctx, key, "not-json", LoadTTL).Err(); err != nil {
		t.Fatalf("写入脏数据失败: %v", err)
	}
	got, err := c.GetNodeLoads(ctx, []int64{nodeID})
	if err != nil {
		t.Fatalf("单条脏数据不该让整批失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("脏数据应按未命中跳过，得到 %+v", got)
	}
}

func TestCacheLockExclusive(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	key := testKey(t)
	t.Cleanup(func() { _ = c.rdb.Del(context.Background(), LockPrefix+key).Err() })

	const workers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		unlocks []func()
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, ok, err := c.Lock(ctx, key, 5*time.Second)
			if err != nil {
				t.Errorf("并发抢锁报错: %v", err)
				return
			}
			if ok {
				mu.Lock()
				unlocks = append(unlocks, unlock) // 先不释放，保证同一时刻只有一个持有者
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(unlocks) != 1 {
		t.Fatalf("%d 个并发抢锁里应只有 1 个成功，实际 %d 个", workers, len(unlocks))
	}
	unlocks[0]()
	if n, err := c.rdb.Exists(ctx, LockPrefix+key).Result(); err != nil || n != 0 {
		t.Fatalf("释放后锁应消失，exists=%d err=%v", n, err)
	}
}

func TestCacheLockAfterUnlock(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	key := testKey(t)
	t.Cleanup(func() { _ = c.rdb.Del(context.Background(), LockPrefix+key).Err() })

	unlock, ok, err := c.Lock(ctx, key, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("第一次抢锁失败: ok=%v err=%v", ok, err)
	}

	// 别人此时抢不到。
	if _, ok2, err := c.Lock(ctx, key, 5*time.Second); err != nil || ok2 {
		t.Fatalf("锁被持有时第二个进程不该拿到: ok=%v err=%v", ok2, err)
	}

	unlock()
	unlock() // 幂等：重复释放不该影响后面的人

	unlock2, ok, err := c.Lock(ctx, key, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("释放后应能重新抢到: ok=%v err=%v", ok, err)
	}
	unlock2()
}

func TestCacheLockTTLExpires(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	key := testKey(t)
	t.Cleanup(func() { _ = c.rdb.Del(context.Background(), LockPrefix+key).Err() })

	if _, ok, err := c.Lock(ctx, key, 200*time.Millisecond); err != nil || !ok {
		t.Fatalf("短 TTL 抢锁失败: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := c.Lock(ctx, key, time.Second); ok {
		t.Fatal("锁还没到期时不该被抢到")
	}
	time.Sleep(350 * time.Millisecond)
	unlock, ok, err := c.Lock(ctx, key, time.Second)
	if err != nil || !ok {
		t.Fatalf("TTL 到期后应能抢到，持锁进程崩溃也不会死锁: ok=%v err=%v", ok, err)
	}
	unlock()
}

// TestCacheUnlockStaleToken 是「unlock 必须比对 token」的守门用例：
// A 的锁过期后 B 拿到了同名锁，A 迟到的 unlock 绝不能把 B 的锁删掉。
// 若把 unlockScript 改成裸 DEL，这个用例会红。
func TestCacheUnlockStaleToken(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	key := testKey(t)
	t.Cleanup(func() { _ = c.rdb.Del(context.Background(), LockPrefix+key).Err() })

	unlockA, ok, err := c.Lock(ctx, key, 150*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("A 抢锁失败: ok=%v err=%v", ok, err)
	}
	time.Sleep(300 * time.Millisecond) // A 的锁自然过期（模拟 A 卡住超过 ttl）

	unlockB, ok, err := c.Lock(ctx, key, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("A 的锁过期后 B 应能抢到: ok=%v err=%v", ok, err)
	}
	t.Cleanup(unlockB)

	unlockA() // A 迟到的释放：token 已不匹配，必须是 no-op

	n, err := c.rdb.Exists(ctx, LockPrefix+key).Result()
	if err != nil {
		t.Fatalf("查锁是否存在失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("A 的过期锁释放误删了 B 的锁：unlock 必须比对 token 再 DEL（exists=%d）", n)
	}

	unlockB()
	if n, err := c.rdb.Exists(ctx, LockPrefix+key).Result(); err != nil || n != 0 {
		t.Fatalf("B 释放后锁应消失，exists=%d err=%v", n, err)
	}
}

func TestCacheNewFromEnv(t *testing.T) {
	newTestClient(t) // 没有 Redis 时整条用例 Skip

	t.Setenv("REDIS_ADDR", envOr("REDIS_ADDR", DefaultAddr))
	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("Redis 可用时 NewFromEnv 不该报错: %v", err)
	}
	if c == nil {
		t.Fatal("Redis 可用时 NewFromEnv 不该降级成 nil")
	}
	defer func() { _ = c.Close() }()
	if c.Disabled() {
		t.Fatal("Redis 可用时 Disabled 应为 false")
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("NewFromEnv 返回的 client Ping 失败: %v", err)
	}
}

// ---- 以下用例不需要 Redis，任何环境都应 PASS ----

func TestNodeLoadKeyFormat(t *testing.T) {
	if got, want := NodeLoadKey(42), "ch:node:42:load"; got != want {
		t.Fatalf("NodeLoadKey(42) = %q，期望 %q", got, want)
	}
}

func TestNodeLoadJSONFields(t *testing.T) {
	load := NodeLoad{Users: 1, MemUsed: 2, MemTotal: 3, CPU: 4, Conns: 5, Ts: 6}
	raw, err := json.Marshal(load)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	// 字段名是对外契约（key 里的 JSON 结构），改名等于让旧数据读不出来。
	want := `{"users":1,"mem_used":2,"mem_total":3,"cpu":4,"conns":5,"ts":6}`
	if string(raw) != want {
		t.Fatalf("JSON 结构变了:\n got %s\nwant %s", raw, want)
	}
}

func TestNewRejectsBadArgs(t *testing.T) {
	if _, err := New("", "", 0); err == nil {
		t.Fatal("空地址应报错")
	}
	if _, err := New("127.0.0.1:6379", "", -1); err == nil {
		t.Fatal("REDIS_DB 为负应报错")
	}
}

func TestDisabledClientSafe(t *testing.T) {
	var c *Client // 模拟 NewFromEnv 探测失败后的返回值
	if !c.Disabled() {
		t.Fatal("nil Client 应报告 Disabled")
	}
	ctx := context.Background()
	if err := c.Ping(ctx); !errors.Is(err, ErrDisabled) {
		t.Fatalf("nil Client Ping 应返回 ErrDisabled，得到 %v", err)
	}
	if err := c.SetNodeLoad(ctx, 1, NodeLoad{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("nil Client SetNodeLoad 应返回 ErrDisabled，得到 %v", err)
	}
	if load, err := c.GetNodeLoad(ctx, 1); !errors.Is(err, ErrDisabled) || load != nil {
		t.Fatalf("nil Client GetNodeLoad 应返回 (nil, ErrDisabled)，得到 (%+v, %v)", load, err)
	}
	if loads, err := c.GetNodeLoads(ctx, []int64{1}); !errors.Is(err, ErrDisabled) || len(loads) != 0 {
		t.Fatalf("nil Client GetNodeLoads 应返回 (空 map, ErrDisabled)，得到 (%+v, %v)", loads, err)
	}
	if unlock, ok, err := c.Lock(ctx, "k", time.Second); !errors.Is(err, ErrDisabled) || ok || unlock != nil {
		t.Fatalf("nil Client Lock 应返回 (nil,false,ErrDisabled)，得到 ok=%v err=%v", ok, err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("nil Client Close 应是无害 no-op，得到 %v", err)
	}
}

func TestNewFromEnvWithoutRedis(t *testing.T) {
	// 指向一个不会有人监听的端口：NewFromEnv 必须降级（nil, nil）而不是报错。
	t.Setenv("REDIS_ADDR", "127.0.0.1:1")
	t.Setenv("REDIS_DB", "0")
	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("连不上 Redis 不该返回 error（上层要能降级），得到: %v", err)
	}
	if c != nil {
		_ = c.Close()
		t.Fatal("连不上 Redis 应返回 nil Client")
	}
}
