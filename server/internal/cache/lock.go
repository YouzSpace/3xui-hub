package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// LockPrefix 是所有分布式锁的 key 前缀，避免与负载等业务 key 混淆。
	LockPrefix = "ch:lock:"
	// DefaultLockTTL 是 Lock 收到非正 ttl 时使用的默认值。
	// 3 秒只够覆盖一次「选节点 + 写库」的最坏耗时，任务更久必须显式传更大 ttl。
	DefaultLockTTL = 3 * time.Second
	// unlockTimeout 是释放锁用的独立超时：调用方 ctx 可能已经超时/取消，
	// 但锁必须还回去，所以不能复用调用方 ctx。
	unlockTimeout = 2 * time.Second
)

// unlockScript 只在锁仍属于自己（token 相同）时才 DEL。
//
// 不能直接 DEL：如果 A 的锁已过期、B 拿到了同名锁，A 迟到的 unlock 会把 B 的锁删掉，
// 于是 B 以为自己独占、实际上第三个进程也能进来。
var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// Lock 抢一把分布式锁：抢到返回 ok=true 和释放函数，被别人占着返回 ok=false。
//
// key 自动加 ch:lock: 前缀；ttl <= 0 时用 DefaultLockTTL。
// 释放函数是幂等的（重复调用只有第一次生效），且只释放自己的锁（比对随机 token 后才 DEL）。
// 锁有 TTL 兜底，持锁进程崩溃不会永久死锁；代价是业务执行超过 ttl 时锁会提前失效——
// 临界区耗时必须小于 ttl，需要更长就显式传大一点。
func (c *Client) Lock(ctx context.Context, key string, ttl time.Duration) (unlock func(), ok bool, err error) {
	if c.Disabled() {
		return nil, false, ErrDisabled
	}
	if ttl <= 0 {
		ttl = DefaultLockTTL
	}
	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}

	fullKey := LockPrefix + key
	// SET key <token> NX PX <ttl>：只有 key 不存在时才成功，因此同一时刻只有一个进程能拿到。
	ok, err = c.rdb.SetNX(ctx, fullKey, token, ttl).Result()
	if err != nil || !ok {
		return nil, false, err
	}

	var released atomic.Bool
	unlock = func() {
		if !released.CompareAndSwap(false, true) {
			return // 已释放过，重复调用是 no-op
		}
		actx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
		defer cancel()
		// 释放失败（Redis 抖动）不向上报：签名里没有 error，且锁有 TTL 兜底会自动消失。
		_ = unlockScript.Run(actx, c.rdb, []string{fullKey}, token).Err()
	}
	return unlock, true, nil
}

// randomToken 生成 128 位随机 token，用于区分「谁的锁」。
func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("cache: 生成锁 token 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
