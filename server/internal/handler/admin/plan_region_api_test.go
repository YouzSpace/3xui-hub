package admin

// 管理端套餐接口的「可用地区」（#地区功能-15）集成测试：真连 MySQL，独立库 ch_region15_*。
//
// 要证明的三件事（打桩证明不了）：
//  1. POST/PUT 真的把 region_ids 写进 plans 表（含 JSON 编码）；
//  2. 「不勾地区」落库成 NULL，而不是 []（NULL=不限地区，与老套餐完全一致）；
//  3. PUT 没带 region_ids 时保持原值（改价格不该把地区清掉）；
//  4. 勾了不存在的地区 → 校验拦下，不落库。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// planRegion15DDL 只建套餐接口读写的两张表（列与 schema.go 对齐）。
var planRegion15DDL = []string{
	`CREATE TABLE plans (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'period',
		months int DEFAULT NULL,
		monthly_traffic bigint DEFAULT NULL,
		period_traffic bigint DEFAULT NULL,
		total_traffic bigint DEFAULT NULL,
		price decimal(10,2) NOT NULL DEFAULT 0.00,
		reset_price decimal(10,2) NOT NULL DEFAULT 0.00,
		is_active tinyint(1) NOT NULL DEFAULT 1,
		include_local tinyint(1) NOT NULL DEFAULT 1,
		third_party_sub_ids text COLLATE utf8mb4_unicode_ci,
		region_ids json DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE regions (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		sort int NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY regions_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newPlanRegion15DB 建一个干净的 ch_region15_* 测试库 + 两张表，跑完自动 DROP。
func newPlanRegion15DB(t *testing.T, caseName string) *sql.DB {
	t.Helper()

	dsn := os.Getenv("REGION15_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?timeout=3s"
	}
	schema := fmt.Sprintf("ch_region15_%s_%d", caseName, os.Getpid())

	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过套餐地区接口集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过套餐地区接口集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	for _, q := range []string{
		"DROP DATABASE IF EXISTS `" + schema + "`",
		"CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci",
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("准备测试库失败（%q）: %v", q, err)
		}
	}
	t.Cleanup(func() {
		a, err := sql.Open("mysql", dsn)
		if err != nil {
			return
		}
		defer a.Close()
		a.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	slash := strings.LastIndex(dsn, "/")
	dbDSN := dsn[:slash+1] + schema + "?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=UTC&timeout=3s"
	db, err := sql.Open("mysql", dbDSN)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range planRegion15DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// newPlanRegion15Router 只挂用户/套餐域（鉴权由 routes.go 的分组负责）。
func newPlanRegion15Router(t *testing.T, db *sql.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewUserDomain(UserDeps{
		Store: NewAdminUserSQLStore(db),
		Plans: NewPlanSQLStore(db),
	}).Register(engine.Group("/admin-api"))
	return engine
}

// plan15Response 是统一的 {code,msg,data} 响应。
type plan15Response struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// plan15Plan 是 presentPlan 的输出里本用例关心的字段。
type plan15Plan struct {
	ID        uint64  `json:"id"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	RegionIDs []int64 `json:"region_ids"`
}

// plan15Call 发一个 JSON 请求（管理端 HTTP 状态恒为 200）。
func plan15Call(t *testing.T, engine *gin.Engine, method, path, body string) plan15Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s HTTP 状态 = %d（管理端口径恒为 200，body=%s）", method, path, rec.Code, rec.Body.String())
	}
	var resp plan15Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s %s 响应不是合法 JSON: %v（body=%s）", method, path, err, rec.Body.String())
	}
	return resp
}

// plan15RawRegionIDs 直接读库里的原始值（NULL → Valid=false）。
func plan15RawRegionIDs(t *testing.T, db *sql.DB, planID uint64) sql.NullString {
	t.Helper()
	var raw sql.NullString
	if err := db.QueryRow("SELECT region_ids FROM plans WHERE id = ?", planID).Scan(&raw); err != nil {
		t.Fatalf("读取 plans.region_ids 失败: %v", err)
	}
	return raw
}

func plan15RequireRegionIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("region_ids = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("region_ids = %v，期望 %v", got, want)
		}
	}
}

// TestPlan15CreateWithRegionIDs 建套餐时勾地区 → 落库成 JSON、回显一致、列表也能读到。
func TestPlan15CreateWithRegionIDs(t *testing.T) {
	db := newPlanRegion15DB(t, "create")
	for _, id := range []int{1, 2, 3} {
		if _, err := db.Exec("INSERT INTO regions (id, name, sort) VALUES (?, ?, ?)", id, fmt.Sprintf("地区%d", id), id); err != nil {
			t.Fatalf("建地区失败: %v", err)
		}
	}
	engine := newPlanRegion15Router(t, db)

	resp := plan15Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"限定地区套餐","type":"total","total_traffic":107374182400,"price":"10.00","region_ids":[1,3]}`)
	if resp.Code != 0 {
		t.Fatalf("建套餐失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var created plan15Plan
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatalf("解析新建套餐失败: %v（raw=%s）", err, resp.Data)
	}
	plan15RequireRegionIDs(t, created.RegionIDs, 1, 3)

	raw := plan15RawRegionIDs(t, db, created.ID)
	if !raw.Valid || raw.String != "[1, 3]" {
		t.Fatalf("库里 region_ids = %q（valid=%v），期望 [1, 3]", raw.String, raw.Valid)
	}

	// 列表接口同样要带上 region_ids（读路径扫的是同一列）。
	list := plan15Call(t, engine, http.MethodGet, "/admin-api/plans?all=1", "")
	if list.Code != 0 {
		t.Fatalf("查列表失败: code=%d msg=%s", list.Code, list.Msg)
	}
	var plans []plan15Plan
	if err := json.Unmarshal(list.Data, &plans); err != nil {
		t.Fatalf("解析套餐列表失败: %v（raw=%s）", err, list.Data)
	}
	if len(plans) != 1 {
		t.Fatalf("套餐数量 = %d，期望 1", len(plans))
	}
	plan15RequireRegionIDs(t, plans[0].RegionIDs, 1, 3)
}

// TestPlan15EmptyRegionIDsStoredAsNull「不勾地区」= 不限 → 库里必须是 NULL 而不是 []。
func TestPlan15EmptyRegionIDsStoredAsNull(t *testing.T) {
	db := newPlanRegion15DB(t, "empty")
	engine := newPlanRegion15Router(t, db)

	resp := plan15Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"不限地区套餐","type":"total","total_traffic":1024,"price":"5.00","region_ids":[]}`)
	if resp.Code != 0 {
		t.Fatalf("建套餐失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var created plan15Plan
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatalf("解析新建套餐失败: %v", err)
	}
	if len(created.RegionIDs) != 0 {
		t.Fatalf("回显 region_ids = %v，期望空数组", created.RegionIDs)
	}
	if raw := plan15RawRegionIDs(t, db, created.ID); raw.Valid {
		t.Fatalf("库里 region_ids 应为 NULL（不限地区），实际 %q", raw.String)
	}
}

// TestPlan15UpdateRegionIDsOnlyWhenProvided PUT 带 region_ids 才写；不带则保持原值。
func TestPlan15UpdateRegionIDsOnlyWhenProvided(t *testing.T) {
	db := newPlanRegion15DB(t, "update")
	for _, id := range []int{1, 2} {
		if _, err := db.Exec("INSERT INTO regions (id, name, sort) VALUES (?, ?, ?)", id, fmt.Sprintf("地区%d", id), id); err != nil {
			t.Fatalf("建地区失败: %v", err)
		}
	}
	engine := newPlanRegion15Router(t, db)

	create := plan15Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"可改地区套餐","type":"total","total_traffic":1024,"price":"5.00","region_ids":[1,2]}`)
	if create.Code != 0 {
		t.Fatalf("建套餐失败: code=%d msg=%s", create.Code, create.Msg)
	}
	var created plan15Plan
	if err := json.Unmarshal(create.Data, &created); err != nil {
		t.Fatalf("解析新建套餐失败: %v", err)
	}
	path := fmt.Sprintf("/admin-api/plans/%d", created.ID)

	// 只改价格（不带 region_ids）→ 地区保持 [1,2]。
	upd := plan15Call(t, engine, http.MethodPut, path,
		`{"name":"可改地区套餐","type":"total","total_traffic":1024,"price":"6.00"}`)
	if upd.Code != 0 {
		t.Fatalf("改价格失败: code=%d msg=%s", upd.Code, upd.Msg)
	}
	if raw := plan15RawRegionIDs(t, db, created.ID); !raw.Valid || raw.String != "[1, 2]" {
		t.Fatalf("只改价格不该动地区，库里 region_ids = %q（valid=%v）", raw.String, raw.Valid)
	}

	// 传空数组 → 改成不限地区（NULL）。
	upd = plan15Call(t, engine, http.MethodPut, path,
		`{"name":"可改地区套餐","type":"total","total_traffic":1024,"price":"6.00","region_ids":[]}`)
	if upd.Code != 0 {
		t.Fatalf("清空地区失败: code=%d msg=%s", upd.Code, upd.Msg)
	}
	if raw := plan15RawRegionIDs(t, db, created.ID); raw.Valid {
		t.Fatalf("清空地区后库里应为 NULL，实际 %q", raw.String)
	}
}

// TestPlan15NullRegionIDsFromFrontend 前端「一个地区都不勾」传的是 null（不是 []）——
// 必须原样接受并落成 NULL，否则前端根本存不了「不限地区」的套餐。
func TestPlan15NullRegionIDsFromFrontend(t *testing.T) {
	db := newPlanRegion15DB(t, "null")
	engine := newPlanRegion15Router(t, db)

	resp := plan15Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"前台传null","type":"total","total_traffic":1024,"price":"5.00","region_ids":null}`)
	if resp.Code != 0 {
		t.Fatalf("region_ids:null 应被接受（前端不勾地区就是传 null），实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
	var created plan15Plan
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatalf("解析新建套餐失败: %v", err)
	}
	if raw := plan15RawRegionIDs(t, db, created.ID); raw.Valid {
		t.Fatalf("region_ids:null 应落成 NULL（不限地区），实际 %q", raw.String)
	}

	// 编辑接口同样收 null（前端每次保存都会带这个字段）。
	upd := plan15Call(t, engine, http.MethodPut, fmt.Sprintf("/admin-api/plans/%d", created.ID),
		`{"name":"前台传null","type":"total","total_traffic":1024,"price":"7.00","region_ids":null}`)
	if upd.Code != 0 {
		t.Fatalf("PUT region_ids:null 失败: code=%d msg=%s", upd.Code, upd.Msg)
	}
	if raw := plan15RawRegionIDs(t, db, created.ID); raw.Valid {
		t.Fatalf("PUT 后 region_ids 仍应为 NULL，实际 %q", raw.String)
	}
}

// TestPlan15RejectsUnknownRegion 勾了不存在的地区 → 校验拦下、不落库。
func TestPlan15RejectsUnknownRegion(t *testing.T) {
	db := newPlanRegion15DB(t, "unknown")
	engine := newPlanRegion15Router(t, db)

	resp := plan15Call(t, engine, http.MethodPost, "/admin-api/plans",
		`{"name":"脏数据套餐","type":"total","total_traffic":1024,"price":"5.00","region_ids":[999]}`)
	if resp.Code == 0 {
		t.Fatalf("不存在的地区应被校验拦下，实际 code=0 msg=%s", resp.Msg)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM plans").Scan(&n); err != nil {
		t.Fatalf("查套餐数量失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("校验失败不该落库，实际 plans 有 %d 行", n)
	}
}
