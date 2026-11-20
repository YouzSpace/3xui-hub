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
	"database/sql"
	"time"
)

// ---- 模型 ----

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

// 状态/类型常量（与 PHP 模型常量同值）。
const (
	KindStatusActive   = "active"

	OrderTypePlan  = "plan"

	OrderStatusPending   = "pending"
	OrderStatusPaid      = "paid"
	OrderStatusExpired   = "expired"
	OrderStatusCancelled = "cancelled"

	DiscountSourceAdmin = "admin"
	DiscountStatusActiv = "active"

)

// ---- 输入 DTO ----

// ProductInput 是 validateProduct 的产物（只含请求里出现的键）。
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
type KV struct {
	Key   string
	Value *string
}

// ---- Store 接口 ----

// Store 是本包对数据库的全部依赖。实现见 sqlstore.go；测试用内存实现。
// NoRows 判定「查无此行」（供 handler 区分 404 与真实错误）。
func NoRows(err error) bool { return err == sql.ErrNoRows }
