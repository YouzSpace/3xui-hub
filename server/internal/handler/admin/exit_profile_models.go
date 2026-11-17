package admin

// 管理端静态 IP 出口分类域（/admin-api/exit-profiles…）：CRUD + 套餐绑定字段。
//
// 数据表（cmd/server/schema.go 已建，老库升级时增量补齐）：
//
//	exit_profiles(id,name,provider_id,traffic_gb,duration_days,max_thread_count,
//	              socks_host,socks_port,enabled,created_at,updated_at)
//	exit_profile_nodes(profile_id,node_id)   分类 ↔ 前置节点，多对多
//	plans.exit_type / plans.exit_profile_id  套餐挂哪个分类
//
// 口径：一个分类 = 一组前置节点 + 一个 webshare 账号（proxy_providers.id）。
// 买了绑该分类的套餐的用户，连这些前置节点时出站走 webshare 的住宅 socks。
//
// 本域**只做数据层**：
//   - 不调 webshare 开号（购买流程是后续任务）；
//   - 不渲染 xray 配置（出站生成是后续任务）；
//   - 不碰 proxy_providers / proxy_accounts / proxy_skus / regions。
//
// 删除守卫：还有套餐绑着这个分类时拒绝删除。分类被删了而套餐还指着它，
// 出站生成就无从下手（外键也没有，只会留下悬空 id）。

import (
	"context"
	"errors"
	"time"
)

// ErrExitProfileNameTaken 表示分类重名（exit_profiles.name 有唯一键 exit_profiles_name_unique）。
// SQL 层把 MySQL 1062 翻译成它，handler 统一回 422 而不是 500。
var ErrExitProfileNameTaken = errors.New("admin: 出口分类名称已存在")

// ErrExitProfileInUse 表示删除时该分类仍被套餐绑定。
//
// 带上绑定套餐数与名字：管理员看到「还有 2 个套餐在用（A、B）」才知道去改哪里，
// 只报一句「被占用」等于让人自己去翻。
type ErrExitProfileInUse struct {
	Count int64
	Names []string
}

func (e *ErrExitProfileInUse) Error() string {
	return "admin: 出口分类仍被套餐绑定"
}

// ExitProfileRecord 是 exit_profiles 一行（present 的输入）。
type ExitProfileRecord struct {
	ID             uint64
	Name           string
	ProviderID     uint64
	TrafficGB      int64
	DurationDays   int64
	MaxThreadCount int64
	SocksHost      string
	SocksPort      int64
	Enabled        bool
	// NodeIDs 是该分类绑定的前置节点 id（按关联表读出顺序 = node_id 升序）。
	NodeIDs []uint64
	// ProviderName 是 JOIN proxy_providers 得到的账号名；账号被删时为空（前端显示「—」）。
	ProviderName string
	CreatedAt    *time.Time
	UpdatedAt    *time.Time
}

// ExitProfileNode 是详情接口里「前置节点列表」的一行。
type ExitProfileNode struct {
	ID         uint64
	Name       string
	Host       string
	DriverType string
	Status     string
	Enabled    bool
}

// ExitProfileBinding 是「绑定了某分类的套餐」一行（删除守卫的文案用）。
type ExitProfileBinding struct {
	ID   uint64
	Name string
}

// ExitProfileInput 是校验通过后、落库之前的完整字段。
//
// 用值而不是指针：本接口是「整行替换」语义（PUT 与 POST 收同一套字段），
// 调用方（前端弹窗）每次都把整份表单发上来，不存在「只改一个字段」的场景，
// 与套餐的 sometimes 语义不同。
type ExitProfileInput struct {
	Name           string
	ProviderID     uint64
	TrafficGB      int64
	DurationDays   int64
	MaxThreadCount int64
	SocksHost      string
	SocksPort      int64
	Enabled        bool
	// NodeIDs 是本次要绑定的前置节点全集（去重后）。空 = 该分类暂时没有入口。
	NodeIDs []uint64
}

// ExitProfileStore 是出口分类域对数据库的全部依赖（SQL 见 exit_profile_sqlstore.go）。
//
// 独立成接口：handler 用例可以用内存实现验路由与文案，SQL 语义另有真库用例钉住。
type ExitProfileStore interface {
	// ExitProfileList 返回全部分类（id 降序，新建的在前）带 node_ids / provider_name。
	ExitProfileList(ctx context.Context) ([]ExitProfileRecord, error)
	// ExitProfileByID 查单个分类（含 node_ids / provider_name）；不存在返回 sql.ErrNoRows。
	ExitProfileByID(ctx context.Context, id uint64) (*ExitProfileRecord, error)
	// ExitProfileCreate 新建分类并写入关联表；重名返回 ErrExitProfileNameTaken。
	// 分类行与关联行在同一个事务里写：不能出现「分类建好了但没有节点」的半成品。
	ExitProfileCreate(ctx context.Context, in ExitProfileInput) (*ExitProfileRecord, error)
	// ExitProfileUpdate 整行替换并同步关联表（旧的全删、新的全插）；不存在返回 sql.ErrNoRows。
	ExitProfileUpdate(ctx context.Context, id uint64, in ExitProfileInput) (*ExitProfileRecord, error)
	// ExitProfileDelete 删除分类并清理关联表；不存在返回 sql.ErrNoRows。
	ExitProfileDelete(ctx context.Context, id uint64) error
	// ExitProfileNameTaken 判断名称是否已被别的分类占用（excludeID=0 表示新建）。
	ExitProfileNameTaken(ctx context.Context, name string, excludeID uint64) (bool, error)
	// ExitProfileBindings 返回绑定了该分类的套餐（删除前的守卫）。
	ExitProfileBindings(ctx context.Context, id uint64) ([]ExitProfileBinding, error)
	// ProviderExists 判断 provider_id 是不是一个真实的 proxy_providers 行。
	ProviderExists(ctx context.Context, id uint64) (bool, error)
	// NodesByIDs 返回这些节点里真实存在的那些（校验 node_ids 用）。
	NodesByIDs(ctx context.Context, ids []uint64) ([]ExitProfileNode, error)
	// ExitProfileNodes 返回某分类绑定的前置节点（详情接口）。
	ExitProfileNodes(ctx context.Context, id uint64) ([]ExitProfileNode, error)
}
