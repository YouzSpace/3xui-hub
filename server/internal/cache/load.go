package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NodeLoad 是节点最近一次上报的实时负载快照。
//
// 内存单位是 MB；Ts 是上报时间（unix 秒），读侧可据此判断数据比 TTL 更精确的新鲜度。
type NodeLoad struct {
	Users    int64   `json:"users"`
	MemUsed  int64   `json:"mem_used"`  // MB
	MemTotal int64   `json:"mem_total"` // MB
	CPU      float64 `json:"cpu"`       // 0..100
	Conns    int64   `json:"conns"`
	Ts       int64   `json:"ts"` // unix 秒
}

// LoadTTL 是节点负载的存活时间：超过 5 分钟没有更新即视为过期，
// 读侧拿到空结果后应回源数据库，避免拿着僵尸数据做分配决策。
const LoadTTL = 300 * time.Second

// NodeLoadKey 返回节点负载的 key（ch:node:{id}:load）。
func NodeLoadKey(nodeID int64) string {
	return fmt.Sprintf("ch:node:%d:load", nodeID)
}

// SetNodeLoad 写入节点负载快照，并把 TTL 重置为 LoadTTL。
func (c *Client) SetNodeLoad(ctx context.Context, nodeID int64, load NodeLoad) error {
	if c.Disabled() {
		return ErrDisabled
	}
	payload, err := json.Marshal(load)
	if err != nil {
		return fmt.Errorf("cache: 序列化节点 %d 负载失败: %w", nodeID, err)
	}
	return c.rdb.Set(ctx, NodeLoadKey(nodeID), payload, LoadTTL).Err()
}

// GetNodeLoad 读取节点负载。key 不存在（从未上报或已过期）时返回 (nil, nil)，
// 这是缓存未命中而不是错误，调用方回源即可。
func (c *Client) GetNodeLoad(ctx context.Context, nodeID int64) (*NodeLoad, error) {
	if c.Disabled() {
		return nil, ErrDisabled
	}
	raw, err := c.rdb.Get(ctx, NodeLoadKey(nodeID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeNodeLoad(nodeID, raw)
}

// GetNodeLoads 批量读取节点负载，用 pipeline 保证只走一次网络往返。
//
// 未上报/已过期的节点不出现在返回的 map 里（调用方按缺失回源）；
// 单条内容损坏也只跳过该节点，不让一条脏缓存拖垮整批读取。
func (c *Client) GetNodeLoads(ctx context.Context, nodeIDs []int64) (map[int64]NodeLoad, error) {
	out := make(map[int64]NodeLoad, len(nodeIDs))
	if c.Disabled() {
		return out, ErrDisabled
	}
	if len(nodeIDs) == 0 {
		return out, nil
	}

	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(nodeIDs))
	for i, id := range nodeIDs {
		cmds[i] = pipe.Get(ctx, NodeLoadKey(id))
	}
	// pipeline 里任何一条 key 不存在都会让 Exec 返回 redis.Nil，这不是传输错误。
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}

	for i, cmd := range cmds {
		raw, err := cmd.Bytes()
		if errors.Is(err, redis.Nil) {
			continue // 未上报/已过期
		}
		if err != nil {
			continue // 单条读失败不影响整批
		}
		load, err := decodeNodeLoad(nodeIDs[i], raw)
		if err != nil {
			continue // 脏数据当作未命中
		}
		out[nodeIDs[i]] = *load
	}
	return out, nil
}

func decodeNodeLoad(nodeID int64, raw []byte) (*NodeLoad, error) {
	var load NodeLoad
	if err := json.Unmarshal(raw, &load); err != nil {
		return nil, fmt.Errorf("cache: 解析节点 %d 负载失败: %w", nodeID, err)
	}
	return &load, nil
}
