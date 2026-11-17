// Package admin 实现管理端（/admin-api）的商店/上游/优惠码/教程/站点配置域，
// 逐字段对齐 backend/app/Http/Controllers/Admin 下的对应控制器。
//
// 契约：
//   - 响应格式、分页、admin session 鉴权 → .hermes/plans/go-migration/contracts/api-contract.md
//   - 时间/布尔/金额/JSON 字段 → contracts/data-contract.md
//
// 设计约束（与 P0-P4 一致）：database/sql 手写 SQL，不用 ORM；handler 只依赖本文件的 Store
// 接口，测试用内存实现（不连库），真实 SQL 另由 live_test.go 覆盖。
package admin

import (
	"context"
	"database/sql"
	"time"
)

// ---- 模型 ----

// Product 是 proxy_products 一行。
type Product struct {
	ID          uint64
	Name        string
	Subtitle    *string
	Description *string
	ImageURL    *string
	IsActive    bool
	SortOrder   int64
	CreatedAt   *time.Time
	SKUs        []SKU
}

// SKU 是 proxy_skus 一行（金额用 decimal 字符串保存，避免 float 精度丢失）。
type SKU struct {
	ID             uint64
	ProductID      uint64
	Name           string
	SourceType     *string
	Allocation     *string
	SourceID       *uint64
	Region         *string
	TrafficGB      int64
	DurationDays   int64
	MaxThreadCount int64
	Price          string // decimal(10,2)
	CostPrice      string // decimal(10,2)
	StockTotal     int64
	StockLocked    int64
	StockSold      int64
	IsActive       bool
	SortOrder      int64
}

// AvailableStock 对齐 ProxySku::availableStock()：负数归零。
func (s *SKU) AvailableStock() int64 {
	v := s.StockTotal - s.StockLocked - s.StockSold
	if v < 0 {
		return 0
	}
	return v
}

// Sock 是 proxy_socks 一行；Password 是**明文**（由 SQL 层解密后放入，
// 与 Eloquent 的 encrypted cast 在模型上可见明文的行为一致）。
type Sock struct {
	ID        uint64
	Name      string
	Host      string
	Port      int64
	Username  *string
	Password  *string
	Country   *string
	City      *string
	Note      *string
	IsActive  bool
	CreatedAt *time.Time
	// 分配方式（#地区功能-17）：shared=多人共用 / exclusive=一条只卖一人。
	Allocation string
	// SoldTo / SoldAt 只在独占模式有意义：这条卖给了谁、什么时候。
	// SoldTo 为 nil = 还在池子里。
	SoldTo *uint64
	SoldAt *time.Time
}

// HasPassword 对齐 `(string) $sock->password !== ”`。
func (s *Sock) HasPassword() bool { return s.Password != nil && *s.Password != "" }

// Tutorial 是 tutorials 一行（管理端列表/详情直接输出模型属性）。
type Tutorial struct {
	ID        uint64
	Title     string
	Category  string
	Content   string
	Sort      int64
	Enabled   bool
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// Provider 是 proxy_providers 一行；APIToken 为明文（解密后）。
type Provider struct {
	ID            uint64
	Name          string
	Driver        string
	APIToken      string
	Enabled       bool
	LastStatus    string
	LastError     *string
	LastCheckedAt *time.Time
}

// Account 是 proxy_accounts 一行；Password 为明文（解密后）。
type Account struct {
	ID                uint64
	ProviderID        uint64
	UpstreamID        string
	UserID            *uint64
	SKUID             *uint64
	Label             string
	Username          string
	Password          *string
	ProxyLimit        string // decimal(8,2)
	TrafficLimitBytes *int64
	ExpiresAt         *time.Time
	OrderID           *uint64
	MaxThreadCount    int64
	ProxyCountries    *string // JSON 文本
	UsedBytes         int64
	Status            string
	LastSyncedAt      *time.Time
	CreatedAt         *time.Time
	UpdatedAt         *time.Time
}

// Entitlement 是 proxy_entitlements 一行。
type Entitlement struct {
	ID        uint64
	UserID    uint64
	OrderID   *uint64
	SKUID     uint64
	Kind      string
	AccountID *uint64
	SocksID   *uint64
	ExpiresAt *time.Time
	Status    string
}

// IsActive 对齐 ProxyEntitlement::isActive()。
func (e *Entitlement) IsActive(now time.Time) bool {
	return e.Status == KindStatusActive && (e.ExpiresAt == nil || e.ExpiresAt.After(now))
}

// Order 是 orders 表里代理域需要的列。
type Order struct {
	ID           uint64
	OrderNo      string
	UserID       uint64
	OrderType    string
	SKUID        *uint64
	ProxyRenewal bool
	Amount       string
	Status       string
	PaidAt       *time.Time
}

// DiscountRow 是优惠码列表/详情用的行（含关联的用户邮箱与套餐名）。
type DiscountRow struct {
	ID             uint64
	Code           string
	Source         string
	UserID         *uint64
	UserEmail      *string
	Discount       string // decimal(3,2)
	MaxUses        int64
	MaxUsesPerUser int64
	UsedCount      int64
	ExpiresAt      *time.Time
	Note           *string
	Status         string
	PlanID         *uint64
	PlanName       *string
	FolderID       *uint64
	CreatedAt      *time.Time
}

// Folder 是 discount_code_folders 一行。
type Folder struct {
	ID   uint64
	Name string
}

// FolderCount 是「文件夹 + 当前筛选下的码数」。
type FolderCount struct {
	ID        uint64
	Name      string
	CodeCount int64
}

// OpsStats 对齐 ProxyOpsController::overview 的 stats 块。
type OpsStats struct {
	Active        int64
	Expiring      int64
	Expired       int64
	PendingOrders int64
	Accounts      int64
	SocksLines    int64
}

// SockOption 是运营页「换线路」下拉项。
type SockOption struct {
	ID      uint64
	Host    string
	Port    int64
	Country *string
}

// OpsAccount 是运营页住宅账号列表行（已带关联名称）。
type OpsAccount struct {
	Account
	EntitlementID *uint64
	UserEmail     *string
	ProviderName  *string
	SKUName       *string
}

// SocksGroupRow 是共享 socks 分组聚合所需的权益行。
type SocksGroupRow struct {
	EntitlementID uint64
	UserID        uint64
	SKUID         uint64
	SocksID       *uint64
	ExpiresAt     *time.Time
	Status        string
	// 关联
	SocksHost    *string
	SocksPort    *int64
	SocksCountry *string
	SocksActive  *bool
	UserEmail    *string
	SKUName      *string
}

// PendingOrder 是「已支付未开通」的代理订单行。
type PendingOrder struct {
	ID        uint64
	OrderNo   string
	Amount    string
	PaidAt    *time.Time
	Renewal   bool
	UserID    uint64
	UserEmail *string
	SKUName   *string
}

// 状态/类型常量（与 PHP 模型常量同值）。
const (
	KindWebshare       = "webshare"
	KindSocks          = "socks"
	KindStatusActive   = "active"
	KindStatusExpired  = "expired"
	KindStatusDisabled = "disabled"

	OrderTypePlan  = "plan"
	OrderTypeProxy = "proxy"

	OrderStatusPending   = "pending"
	OrderStatusPaid      = "paid"
	OrderStatusExpired   = "expired"
	OrderStatusCancelled = "cancelled"

	DiscountSourceAdmin = "admin"
	DiscountStatusActiv = "active"

	// DisabledProxyLimit 是停用时压到的上限（GB）：0 在 Webshare 表示不限量，不能压 0。
	DisabledProxyLimit = "0.01"
)

// ---- 输入 DTO ----

// ProductInput 是 validateProduct 的产物（只含请求里出现的键）。
type ProductInput struct {
	Name        string
	Subtitle    *string
	Description *string
	ImageURL    *string
	IsActive    *bool
	SortOrder   *int64
}

// SKUInput 是 validateSku 的产物（只含请求里出现的键；指针用于区分「未传」）。
type SKUInput struct {
	Name           string
	SourceType     *string
	Allocation     *string
	SourceID       *uint64
	// ClearSourceID = 请求里显式传了 source_id: null（独占池「不指定」时用）。
	// SourceID 为 nil 只表示「没传」，不能据此把库里的值清空。
	ClearSourceID  bool
	Region         *string
	TrafficGB      *int64
	DurationDays   int64
	MaxThreadCount *int64
	Price          string
	CostPrice      *string
	StockTotal     *int64
	IsActive       *bool
	SortOrder      *int64
}

// SockCreate 是导入时的一行解析结果。
type SockCreate struct {
	Name     string
	Host     string
	Port     int64
	Username *string
	Password *string
	Country  *string
	// Allocation 是这批导入的分配方式（#地区功能-17）：shared / exclusive，
	// nil = 用表默认（shared），老导入行为不变。
	Allocation *string
}

// SockUpdate 是 PUT /proxy-store/socks/{id} 的字段集合。
type SockUpdate struct {
	Name     *string
	Host     *string
	Port     *int64
	Username *string
	Password *string
	Country  *string
	City     *string
	Note     *string
	IsActive *bool
	// Allocation 是分配方式（shared / exclusive）；改成 shared 会把已售记录一并清掉，
	// 否则这条回到共享后再切回独占会永久占着 sold_to，谁也买不到。
	Allocation *string
}

// DiscountFilter 是列表的筛选条件（folder 单独传）。
type DiscountFilter struct {
	Source *string
	Status *string
	User   *string
}

// NewDiscountCode 是批量生成时单行的完整字段。
type NewDiscountCode struct {
	Code           string
	Discount       string
	MaxUses        int64
	MaxUsesPerUser int64
	ExpiresAt      *time.Time
	Note           *string
	PlanID         *uint64
	FolderID       *uint64
	CreatedAt      time.Time
}

// DiscountUpdate 是 PUT /discount-codes/{code} 的字段集合。
type DiscountUpdate struct {
	Discount       *string
	MaxUses        *int64
	MaxUsesPerUser *int64
	ExpiresAtSet   bool
	ExpiresAt      *time.Time
	NoteSet        bool
	Note           *string
	Status         *string
	PlanIDSet      bool
	PlanID         *uint64
}

// TutorialInput 是 POST /tutorials 的字段（validate 后的产物）。
type TutorialInput struct {
	Title    string
	Category *string
	Content  string
	Sort     *int64
	Enabled  *bool
}

// TutorialUpdate 是 PUT /tutorials/{id} 的字段（指针 = 请求里出现过）。
type TutorialUpdate struct {
	Title    *string
	Category *string
	Content  *string
	Sort     *int64
	Enabled  *bool
}

// AccountCreate 是上游建号落库的字段。
type AccountCreate struct {
	ProviderID     uint64
	UpstreamID     string
	UserID         *uint64
	Label          string
	Username       string
	Password       *string
	ProxyLimit     string
	MaxThreadCount int64
	ProxyCountries *string
	UsedBytes      int64
	Status         string
	LastSyncedAt   *time.Time
}

// AccountPatch 是账户字段的部分更新（nil = 不改）。
type AccountPatch struct {
	Password          *string
	Username          *string
	ProxyLimit        *string
	MaxThreadCount    *int64
	Status            *string
	LastSyncedAt      *time.Time
	UsedBytes         *int64
	ProxyCountries    *string
	ExpiresAt         *time.Time
	TrafficLimitBytes *int64
	SKUID             *uint64
	OrderID           *uint64
}

// ProviderPatch 是上游账号字段的部分更新。
type ProviderPatch struct {
	Name       *string
	APIToken   *string
	Enabled    *bool
	LastStatus *string
	LastError  *string
	CheckedAt  *time.Time
}

// EntitlementCreate 是开通/续费写权益的字段。
type EntitlementCreate struct {
	UserID    uint64
	OrderID   *uint64
	SKUID     uint64
	Kind      string
	AccountID *uint64
	SocksID   *uint64
	ExpiresAt *time.Time
	Status    string
}

// EntitlementPatch 是权益的部分更新（nil = 不改）。
type EntitlementPatch struct {
	OrderID   *uint64
	SKUID     *uint64
	Status    *string
	ExpiresAt *time.Time
	AccountID *uint64
	SocksID   *uint64
}

// KV 是 site_configs 的一次键值写入。
type KV struct {
	Key   string
	Value *string
}

// ---- Store 接口 ----

// Store 是本包对数据库的全部依赖。实现见 sqlstore.go；测试用内存实现。
type Store interface {
	// 商品与规格
	ProductsWithSKUs(ctx context.Context) ([]Product, error)
	ProductByID(ctx context.Context, id uint64) (*Product, error)
	CreateProduct(ctx context.Context, in ProductInput) (*Product, error)
	UpdateProduct(ctx context.Context, id uint64, in ProductInput) (*Product, error)
	ProductHasSoldSKU(ctx context.Context, id uint64) (bool, error)
	DeleteProduct(ctx context.Context, id uint64) error

	SKUByID(ctx context.Context, id uint64) (*SKU, error)
	CreateSKU(ctx context.Context, productID uint64, in SKUInput) (*SKU, error)
	UpdateSKU(ctx context.Context, id uint64, in SKUInput) (*SKU, error)
	DeleteSKU(ctx context.Context, id uint64) error

	// 手工 socks
	SocksList(ctx context.Context) ([]Sock, error)
	SockByID(ctx context.Context, id uint64) (*Sock, error)
	SockByIDExists(ctx context.Context, id uint64) (bool, error)
	SockExists(ctx context.Context, host string, port int64, username *string) (bool, error)
	CreateSock(ctx context.Context, in SockCreate) (*Sock, error)
	UpdateSock(ctx context.Context, id uint64, in SockUpdate) (*Sock, error)
	DeleteSock(ctx context.Context, id uint64) error
	SKUExistsUsingSocks(ctx context.Context, socksID uint64) (bool, error)
	ActiveSocksOptions(ctx context.Context) ([]SockOption, error)

	// 独占 socks 池（#地区功能-17）：一条只卖一人，卖完自动用下一条。
	// renewSockID = 续费时原来那条（仍归该用户就沿用）；sourceID 非空时只在指定
	// 那一条里挑。池子空 → sockpool.ErrPoolEmpty。
	AcquireExclusiveSock(ctx context.Context, renewSockID, sourceID *uint64, userID uint64, now time.Time) (uint64, error)
	// ReleaseExclusiveSock 把线路放回池子（权益到期/作废）；共享线路是空操作。
	ReleaseExclusiveSock(ctx context.Context, socksID uint64, now time.Time) error

	// 站点配置
	SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error)
	SiteConfigSetMany(ctx context.Context, kv []KV) error

	// 教程
	TutorialsAll(ctx context.Context) ([]Tutorial, error)
	TutorialByID(ctx context.Context, id uint64) (*Tutorial, error)
	CreateTutorial(ctx context.Context, in TutorialInput) (*Tutorial, error)
	UpdateTutorial(ctx context.Context, id uint64, in TutorialUpdate) (*Tutorial, error)
	DeleteTutorial(ctx context.Context, id uint64) error

	// 优惠码
	DiscountPage(ctx context.Context, f DiscountFilter, folderID *uint64, page, perPage int64) ([]DiscountRow, int64, error)
	DiscountFolderCounts(ctx context.Context, f DiscountFilter) ([]FolderCount, error)
	DiscountByIDOrCode(ctx context.Context, raw string) (*DiscountRow, error)
	DiscountsByIDs(ctx context.Context, ids []uint64) (map[uint64]*DiscountRow, error)
	ExistingCodes(ctx context.Context, codes []string) (map[string]struct{}, error)
	InsertDiscountCodes(ctx context.Context, rows []NewDiscountCode) error
	UpdateDiscount(ctx context.Context, id uint64, in DiscountUpdate) error
	DeleteDiscount(ctx context.Context, id uint64) error
	FolderCreate(ctx context.Context, name string) (*Folder, error)
	FolderByID(ctx context.Context, id uint64) (*Folder, error)
	FolderByName(ctx context.Context, name string) (*Folder, error)
	FolderUpdate(ctx context.Context, id uint64, name string) error
	FolderDelete(ctx context.Context, id uint64) error
	DetachCodesFromFolder(ctx context.Context, folderID uint64) error
	DiscountCodesInFolder(ctx context.Context, folderID uint64) ([]DiscountRow, error)

	// 代理运营
	OpsStats(ctx context.Context, now time.Time) (OpsStats, error)
	OpsAccountsPage(ctx context.Context, status, search string, page, perPage int64) ([]OpsAccount, int64, error)
	SocksGroupIDs(ctx context.Context, page, perPage int64) ([]uint64, int64, error)
	SocksGroupRows(ctx context.Context, socksIDs []uint64) ([]SocksGroupRow, error)
	PendingOrdersPage(ctx context.Context, page, perPage int64) ([]PendingOrder, int64, error)
	ProxyAccountByID(ctx context.Context, id uint64) (*Account, error)
	EntitlementByID(ctx context.Context, id uint64) (*Entitlement, error)
	OrderByID(ctx context.Context, id uint64) (*Order, error)
	LatestEntitlementForAccount(ctx context.Context, accountID uint64) (*Entitlement, error)
	UpdateEntitlement(ctx context.Context, id uint64, in EntitlementPatch) error
	PatchAccount(ctx context.Context, id uint64, in AccountPatch) error
	OrderHasEntitlement(ctx context.Context, orderID uint64) (bool, error)

	// 上游 Webshare
	Providers(ctx context.Context) ([]Provider, error)
	ProviderByID(ctx context.Context, id uint64) (*Provider, error)
	CreateProvider(ctx context.Context, name, token string, enabled bool) (*Provider, error)
	PatchProvider(ctx context.Context, id uint64, in ProviderPatch) (*Provider, error)
	DeleteProvider(ctx context.Context, id uint64) error
	ProviderAccounts(ctx context.Context, providerID uint64) ([]Account, error)
	ProviderActiveAccounts(ctx context.Context, providerID uint64) ([]Account, error)
	ProviderAccountCount(ctx context.Context, providerID uint64) (int64, error)
	AccountByID(ctx context.Context, id uint64) (*Account, error)
	CreateAccount(ctx context.Context, in AccountCreate) (*Account, error)
	UpsertAccountByUpstream(ctx context.Context, providerID uint64, upstreamID string, patch AccountPatch, label string, status string) (created bool, err error)
	DeleteAccount(ctx context.Context, id uint64) error

	// 开通（ProxyStoreService 等价）
	SKUForOrder(ctx context.Context, orderID uint64) (*SKU, error)
	EntitlementExistsForOrder(ctx context.Context, orderID uint64) (bool, error)
	FindRenewableEntitlement(ctx context.Context, userID uint64, kind string, includeExpired bool, now time.Time) (*Entitlement, error)
	CreateEntitlement(ctx context.Context, in EntitlementCreate) (*Entitlement, error)
	ReserveStock(ctx context.Context, skuID uint64, qty int64) (bool, error)
	ReleaseStock(ctx context.Context, skuID uint64, qty int64) error
	ConsumeStock(ctx context.Context, skuID uint64, qty int64) error
	LockAcquire(ctx context.Context, name string, seconds int64, owner string) (bool, error)
	LockRelease(ctx context.Context, name, owner string) error

	// 通用存在性（对应 Laravel 的 exists 规则）
	ExistsValue(ctx context.Context, table, column string, value any) (bool, error)
	// UserExists / PlanExists / FolderExists 是 exists 规则的具名包装（白名单在 SQL 层）。
	UserExists(ctx context.Context, id uint64) (bool, error)
	PlanExists(ctx context.Context, id uint64) (bool, error)
	UserEmail(ctx context.Context, id uint64) (*string, error)
}

// NoRows 判定「查无此行」（供 handler 区分 404 与真实错误）。
func NoRows(err error) bool { return err == sql.ErrNoRows }
