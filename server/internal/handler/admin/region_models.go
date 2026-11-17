package admin

// 管理端地区域（/admin-api/regions…）：地区字典 CRUD + 节点归属/容量 + 总览。
//
// 数据表（cmd/server/schema.go 已建，老库升级时增量补齐）：
//
//	regions(id,name,sort)        地区字典（id 小、数量少，不做分页）
//	nodes.region_id              NULL = 该节点不参与地区过滤（全量下发，升级默认态）
//	nodes.capacity               NULL = 不限容量
//	user_nodes(user_id,node_id)  用户 ↔ 节点绑定
//
// 本域**只做 CRUD 与查询**：迁移（/nodes/:id/migrate）、均衡（/regions/:id/balance）
// 属于调度域（internal/scheduler + 迁移接口任务），这里不写 user_nodes 一行。
//
// 统计口径（三处必须一致，否则看板数字对不上）：
//   - node_count     该地区节点数；
//   - user_count     绑定到该地区节点的**去重用户数**（一人绑同区两台只算 1 人）；
//   - capacity_total SUM(nodes.capacity)，capacity 为 NULL（不限）的节点不计入；
//   - capacity_used  该地区节点上的绑定条数（user_nodes 行数，一节点一人一条）。

import (
	"context"
	"errors"
)

// ErrRegionNameTaken 表示地区重名（regions.name 有唯一键 regions_name_unique）。
// SQL 层把 MySQL 1062 翻译成它，handler 统一回 422 而不是 500。
var ErrRegionNameTaken = errors.New("admin: 地区名称已存在")

// RegionRecord 是 regions 一行。
type RegionRecord struct {
	ID   uint64
	Name string
	Sort int64
}

// RegionSummary 是地区列表/总览的一行：地区 + 统计。
type RegionSummary struct {
	RegionRecord
	NodeCount     int64
	UserCount     int64
	CapacityTotal int64
	CapacityUsed  int64
}

// RegionNode 是「地区内的节点」一行（/regions/:id/nodes 与总览的未归属节点块共用）。
type RegionNode struct {
	ID         uint64
	Name       string
	Host       string
	DriverType string
	Status     string
	Capacity   *int64 // NULL = 不限
	BoundUsers int64
}

// RegionOverview 是调度看板首屏数据。
type RegionOverview struct {
	NodeTotal     int64
	CapacityTotal int64
	CapacityUsed  int64
	Regions       []RegionSummary
	// UnassignedNodes 是 region_id IS NULL 的节点（不参与地区过滤、全量下发的老节点）。
	UnassignedNodes []RegionNode
}

// RegionStore 是地区域对数据库的全部依赖（SQL 见 region_sqlstore.go）。
//
// 独立成接口：路由层测试可用内存实现，SQL 语义另有真库用例钉住。
type RegionStore interface {
	// RegionList 返回全部地区（sort 升序，sort 相同按 id 升序）带统计。
	RegionList(ctx context.Context) ([]RegionSummary, error)
	// RegionByID 查单个地区；不存在返回 sql.ErrNoRows。
	RegionByID(ctx context.Context, id uint64) (*RegionRecord, error)
	RegionCreate(ctx context.Context, name string, sort int64) (*RegionRecord, error)
	// RegionUpdate 只改传入的字段（nil = 不动）；地区不存在返回 sql.ErrNoRows。
	RegionUpdate(ctx context.Context, id uint64, name *string, sort *int64) (*RegionRecord, error)
	// RegionDelete 删除地区；地区不存在返回 sql.ErrNoRows。
	RegionDelete(ctx context.Context, id uint64) error
	RegionExists(ctx context.Context, id uint64) (bool, error)
	// RegionNameTaken 判断名称是否已被别的地区占用（excludeID=0 表示新建）。
	RegionNameTaken(ctx context.Context, name string, excludeID uint64) (bool, error)
	// RegionNodeCount 是该地区下的节点数（删除前的守卫）。
	RegionNodeCount(ctx context.Context, id uint64) (int64, error)
	RegionNodes(ctx context.Context, id uint64) ([]RegionNode, error)
	UnassignedNodes(ctx context.Context) ([]RegionNode, error)
	// NodeTotals 是全局合计：节点数、容量合计、绑定合计。
	NodeTotals(ctx context.Context) (nodes, capacityTotal, capacityUsed int64, err error)
	// NodeBinding 读节点当前归属；节点不存在返回 sql.ErrNoRows。
	NodeBinding(ctx context.Context, nodeID uint64) (regionID *uint64, capacity *int64, err error)
	NodeSetRegion(ctx context.Context, nodeID uint64, regionID *uint64) error
	NodeSetCapacity(ctx context.Context, nodeID uint64, capacity *int64) error
}
