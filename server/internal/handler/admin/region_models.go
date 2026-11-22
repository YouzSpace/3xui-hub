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

// RegionNode 是「地区内/未归属的节点」一行（/regions/:id/nodes 与总览的未归属节点块共用）。
type RegionNode struct {
	ID         uint64
	Name       string
	Host       string
	DriverType string
	Status     string
	Capacity   *int64 // NULL = 不限
	// RegionID 是节点当前归属；NULL = 未归属（全量下发）。前端「设置归属」弹窗回填当前地区用。
	RegionID   *uint64
	BoundUsers int64
	// Inbounds 是挂在节点上的入站及其归属（#入站级分组），只读展示。
	// 节点列表页的表格不展开它，前端在「地区节点」接口的返回里按需展示。
	Inbounds []RegionInbound
}

// RegionInbound 是「地区内节点的一个入站」只读展示行（Regions.vue 节点展开时用）。
//
// 只要求展示归属，不做内联编辑：编辑仍在「xray 入站」页。
// RegionID/RegionName 为 nil 表示该入站跟随节点地区（region_id IS NULL）。
type RegionInbound struct {
	ID         uint64
	Tag        string
	Protocol   string
	Port       int64
	Enabled    bool
	RegionID   *uint64
	RegionName *string
}

// RegionOverview 是调度看板首屏数据。
//
// 口径（与卡片一一对应，不能再混用绑定行数）：
//   - NodeTotal     节点总数；
//   - CapacityTotal 有上限节点的容量合计（capacity 为 NULL 的不计）；
//   - CapacityUsed  这些有上限节点上的绑定席位（不含不限容量节点），与 CapacityTotal 同范围；
//   - BoundUsers    去重绑定用户数（一人绑多地区只算 1）；
//   - UnlimitedNodes 无上限（不限容量）的节点数，前端据此说明「N 台未计入使用率」。
type RegionOverview struct {
	NodeTotal      int64
	CapacityTotal  int64
	CapacityUsed   int64
	BoundUsers     int64
	UnlimitedNodes int64
	Regions        []RegionSummary
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
	// RegionNames 返回「地区 id → 名称」，供入站列表回显归属地区名（#入站级分组）。
	RegionNames(ctx context.Context) (map[uint64]string, error)
	// RegionNameTaken 判断名称是否已被别的地区占用（excludeID=0 表示新建）。
	RegionNameTaken(ctx context.Context, name string, excludeID uint64) (bool, error)
	// RegionNodeCount 是该地区下的节点数（删除前的守卫）。
	RegionNodeCount(ctx context.Context, id uint64) (int64, error)
	RegionNodes(ctx context.Context, id uint64) ([]RegionNode, error)
	UnassignedNodes(ctx context.Context) ([]RegionNode, error)
	// NodeTotals 是全局合计：节点数、有上限节点容量合计、这些节点的绑定席位、
	// 去重绑定用户数、无上限节点数（见 region_sqlstore.go 的口径说明）。
	NodeTotals(ctx context.Context) (nodes, capacityTotal, capacityUsed, userCount, unlimitedNodes int64, err error)
	// NodeBinding 读节点当前归属；节点不存在返回 sql.ErrNoRows。
	NodeBinding(ctx context.Context, nodeID uint64) (regionID *uint64, capacity *int64, err error)
	NodeSetRegion(ctx context.Context, nodeID uint64, regionID *uint64) error
	NodeSetCapacity(ctx context.Context, nodeID uint64, capacity *int64) error
}
