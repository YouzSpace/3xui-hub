// Package cache 封装面板的可选 Redis 依赖：连接管理、节点实时负载读写、分布式锁。
//
// Redis 是「可选增强」而不是硬依赖：没装 Redis 或暂时连不上时面板必须照常工作
// （只是失去跨实例共享的负载视图与跨实例分布式锁），所以 NewFromEnv 在探测失败时
// 返回 nil Client 而不是 error，上层用 (*Client).Disabled 判断后自行降级。
//
// nil Client 视为「未启用」：本包所有方法对 nil 接收者安全（返回 ErrDisabled），
// 上层拿到降级后的 client 不需要到处判空。
package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrDisabled 是 Redis 未启用（NewFromEnv 探测失败）时各方法返回的错误。
var ErrDisabled = errors.New("cache: redis 未启用")

const (
	// DefaultAddr 是 REDIS_ADDR 未设置时的默认地址。
	DefaultAddr = "127.0.0.1:6379"
	// pingTimeout 是 NewFromEnv 的探测超时。本机/内网 Redis 正常都在毫秒级，
	// 1 秒还没响应就当作不可用，避免拖慢面板启动。
	pingTimeout = time.Second
)

// Client 是 Redis 客户端的薄封装。零值/nil 都表示未启用。
type Client struct {
	rdb *redis.Client
}

// New 按地址、密码、库号创建客户端。只构造不连接（连接在首次命令时建立），
// 因此返回 error 仅代表参数非法，不代表 Redis 不可用——可用性用 Ping 判断。
func New(addr, password string, db int) (*Client, error) {
	if addr == "" {
		return nil, errors.New("cache: REDIS_ADDR 为空")
	}
	if db < 0 {
		return nil, fmt.Errorf("cache: REDIS_DB=%d 非法（必须 >= 0）", db)
	}
	return &Client{rdb: redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})}, nil
}

// NewFromEnv 从 REDIS_ADDR / REDIS_PASSWORD / REDIS_DB 读取配置并探测连通性。
//
// 探测失败（没装 Redis、端口不通）不返回 error：返回 (nil, nil)，此时
// c.Disabled() == true，上层按「无缓存」降级即可。只有配置本身非法
// （比如 REDIS_DB=-1）才返回 error，因为那属于部署错误，静默降级会掩盖它。
func NewFromEnv() (*Client, error) {
	c, err := New(
		envOr("REDIS_ADDR", DefaultAddr),
		os.Getenv("REDIS_PASSWORD"),
		envInt("REDIS_DB", 0),
	)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		_ = c.Close()   // 释放探测用的连接
		return nil, nil // 未装 Redis 是合法部署形态，不当作错误
	}
	return c, nil
}

// Ping 探测连通性。
func (c *Client) Ping(ctx context.Context) error {
	if c.Disabled() {
		return ErrDisabled
	}
	return c.rdb.Ping(ctx).Err()
}

// Close 释放连接池。幂等，未启用时是 no-op。
func (c *Client) Close() error {
	if c.Disabled() {
		return nil
	}
	return c.rdb.Close()
}

// Disabled 报告缓存是否未启用（nil 接收者也返回 true），供上层决定是否降级。
func (c *Client) Disabled() bool {
	return c == nil || c.rdb == nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
