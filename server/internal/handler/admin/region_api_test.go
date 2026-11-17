package admin

// 地区管理接口集成测试（真连 MySQL，独立库 ch_region09_*，自建自删）。
//
// 为什么必须连真库：这几条接口的核心是 SQL 语义 ——
//   - 列表的四项统计是 LEFT JOIN 聚合（没有节点的地区不能丢行、不能算成 NULL）；
//   - 删除守卫要按 nodes.region_id 真数一遍；
//   - 节点归属/容量写的是 nodes 表的可空列，读写一致性只有真库能证明。
// 用 fake store 只能证明 handler 传参，证明不了统计口径。
//
// 变异测试锚点：删掉 regionDestroy 里的 `if nodes > 0` 守卫，
// TestRegion09DeleteRegionWithNodesRejected 必须变红（删除会成功，code=0）。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// region09DSN 连本机 MySQL（root/空密码），可用 REGION09_MYSQL_DSN 覆盖。
func region09DSN() string {
	if dsn := os.Getenv("REGION09_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// region09WithSchema 把 DSN 的默认库换成 schema。
func region09WithSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	return head + schema + tail
}

// region09DDL 只建本次接口真正读写的 3 张表（列与 schema.go 一致，省掉无关列）。
var region09DDL = []string{
	`CREATE TABLE regions (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
		sort int NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY regions_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		host varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
		driver_type varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '3x-ui',
		status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'offline',
		capacity int DEFAULT NULL,
		region_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		KEY idx_nodes_region (region_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newRegion09DB 建一个干净的测试库 + 三张表，跑完自动 DROP。
func newRegion09DB(t *testing.T, schema string) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", region09DSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过地区接口集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过地区接口集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", region09DSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region09WithSchema(region09DSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	for _, ddl := range region09DDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, ddl)
		}
	}
	return db
}

// newRegion09Router 只挂地区域（不挂 AdminAuth：鉴权由 routes.go 的分组负责）。
func newRegion09Router(t *testing.T, db *sql.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewRegionDomain(RegionDeps{Store: NewRegionSQLStore(db)}).Register(engine.Group("/admin-api"))
	return engine
}

// region09Response 是统一的 {code,msg,data} 响应。
type region09Response struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// call 发一个 JSON 请求并解析响应；body 为空串表示不带请求体。
func region09Call(t *testing.T, engine *gin.Engine, method, path, body string) region09Response {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	var resp region09Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s %s 响应不是合法 JSON: %v（body=%s）", method, path, err, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s HTTP 状态 = %d，管理端口径恒为 200（body=%s）", method, path, rec.Code, rec.Body.String())
	}
	return resp
}

// region09Regions 解出 GET /regions 的 data.regions。
type region09Summary struct {
	ID            uint64 `json:"id"`
	Name          string `json:"name"`
	Sort          int64  `json:"sort"`
	NodeCount     int64  `json:"node_count"`
	UserCount     int64  `json:"user_count"`
	CapacityTotal int64  `json:"capacity_total"`
	CapacityUsed  int64  `json:"capacity_used"`
}

func region09DecodeRegions(t *testing.T, raw json.RawMessage) []region09Summary {
	t.Helper()
	var data struct {
		Regions []region09Summary `json:"regions"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析 regions 失败: %v（raw=%s）", err, raw)
	}
	return data.Regions
}

// region09Node 是地区节点一行的 JSON 形状。
type region09Node struct {
	ID         uint64   `json:"id"`
	Name       string   `json:"name"`
	Host       string   `json:"host"`
	DriverType string   `json:"driver_type"`
	Status     string   `json:"status"`
	Capacity   *int64   `json:"capacity"`
	BoundUsers int64    `json:"bound_users"`
	AgreeRatio *float64 `json:"agree_ratio"`
}

// region09DecodeNodes 解出 data.nodes。
func region09DecodeNodes(t *testing.T, raw json.RawMessage) []region09Node {
	t.Helper()
	var data struct {
		RegionID   uint64         `json:"region_id"`
		RegionName string         `json:"region_name"`
		Nodes      []region09Node `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("解析 nodes 失败: %v（raw=%s）", err, raw)
	}
	return data.Nodes
}

// TestRegion09RoutesRegistered 钉住 8 条路由的路径与方法：
// 路径写错前端就是 404，编译期不会提醒。
func TestRegion09RoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	adminGroup := engine.Group("/admin-api")
	// 模拟真实挂载顺序：NodeDomain 已占用 /nodes/:node（参数名 :node），
	// 地区域再挂 /nodes/:node/region 与 /nodes/:node/capacity。
	// 参数名若改成 :id，gin 路由树会 panic（"conflicts with existing wildcard"），这条用例就是钉子。
	adminGroup.PUT("/nodes/:node", func(*gin.Context) {})
	NewRegionDomain(RegionDeps{Store: NewRegionSQLStore(nil)}).Register(adminGroup)

	want := []string{
		"GET /admin-api/regions",
		"GET /admin-api/regions/overview",
		"POST /admin-api/regions",
		"PUT /admin-api/regions/:region",
		"DELETE /admin-api/regions/:region",
		"GET /admin-api/regions/:region/nodes",
		"PUT /admin-api/nodes/:node/region",
		"PUT /admin-api/nodes/:node/capacity",
	}
	have := map[string]bool{}
	for _, r := range engine.Routes() {
		have[r.Method+" "+r.Path] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Fatalf("路由缺失: %s（已注册 %d 条）", w, len(have))
		}
	}
}

// TestRegion09CreateListDuplicateDelete 覆盖：建地区 → 列表能查到；重名被拒；
// 改名字/排序生效；删空地区成功。
func TestRegion09CreateListDuplicateDelete(t *testing.T) {
	db := newRegion09DB(t, "ch_region09_crud")
	engine := newRegion09Router(t, db)

	// 建地区
	resp := region09Call(t, engine, http.MethodPost, "/admin-api/regions", `{"name":"香港","sort":10}`)
	if resp.Code != 0 {
		t.Fatalf("建地区失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var created region09Summary
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatalf("解析建地区响应失败: %v（data=%s）", err, resp.Data)
	}
	if created.ID == 0 || created.Name != "香港" || created.Sort != 10 {
		t.Fatalf("建地区返回不对: %+v", created)
	}

	// 列表能查到
	list := region09DecodeRegions(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions", "").Data)
	if len(list) != 1 || list[0].ID != created.ID || list[0].Name != "香港" || list[0].NodeCount != 0 {
		t.Fatalf("列表内容不对: %+v", list)
	}

	// 重名：必须是业务错误（422），不能 500 崩溃，也不能插入第二行
	dup := region09Call(t, engine, http.MethodPost, "/admin-api/regions", `{"name":"香港","sort":0}`)
	if dup.Code == 0 {
		t.Fatalf("重名应被拒绝，实际 code=0（data=%s）", dup.Data)
	}
	if dup.Code != 422 {
		t.Fatalf("重名应回 422，实际 %d（msg=%s）", dup.Code, dup.Msg)
	}
	var regionRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM regions").Scan(&regionRows); err != nil {
		t.Fatalf("统计 regions 失败: %v", err)
	}
	if regionRows != 1 {
		t.Fatalf("重名请求不应插入新行，regions 实际 %d 行", regionRows)
	}

	// 改名 + 改排序
	upd := region09Call(t, engine, http.MethodPut, "/admin-api/regions/1", `{"name":"香港-HK","sort":3}`)
	if upd.Code != 0 {
		t.Fatalf("改地区失败: code=%d msg=%s", upd.Code, upd.Msg)
	}
	var updated region09Summary
	if err := json.Unmarshal(upd.Data, &updated); err != nil {
		t.Fatalf("解析改地区响应失败: %v（data=%s）", err, upd.Data)
	}
	if updated.ID != 1 || updated.Name != "香港-HK" || updated.Sort != 3 {
		t.Fatalf("改地区响应不对: %+v", updated)
	}
	list = region09DecodeRegions(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions", "").Data)
	if list[0].Name != "香港-HK" || list[0].Sort != 3 {
		t.Fatalf("改名/排序未生效: %+v", list[0])
	}

	// 改成已存在的另一个名字 → 拒绝（排除自身的重名检查要准，否则自己改自己也会被拒）
	resp = region09Call(t, engine, http.MethodPost, "/admin-api/regions", `{"name":"日本","sort":20}`)
	if resp.Code != 0 {
		t.Fatalf("建第二个地区失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	selfRename := region09Call(t, engine, http.MethodPut, "/admin-api/regions/2", `{"name":"日本","sort":21}`)
	if selfRename.Code != 0 {
		t.Fatalf("只改排序（名字不变）应成功，实际 code=%d msg=%s", selfRename.Code, selfRename.Msg)
	}
	clash := region09Call(t, engine, http.MethodPut, "/admin-api/regions/2", `{"name":"香港-HK"}`)
	if clash.Code != 422 {
		t.Fatalf("改成别人的名字应回 422，实际 code=%d msg=%s", clash.Code, clash.Msg)
	}

	// 删空地区成功
	del := region09Call(t, engine, http.MethodDelete, "/admin-api/regions/2", "")
	if del.Code != 0 {
		t.Fatalf("删除空地区失败: code=%d msg=%s", del.Code, del.Msg)
	}
	if list = region09DecodeRegions(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions", "").Data); len(list) != 1 {
		t.Fatalf("删除后列表应剩 1 个地区，实际 %d", len(list))
	}
}

// TestRegion09DeleteRegionWithNodesRejected 覆盖：删除有节点的地区必须拒绝，
// 文案里带节点数（运维要知道还要移几台）。
func TestRegion09DeleteRegionWithNodesRejected(t *testing.T) {
	db := newRegion09DB(t, "ch_region09_del")
	engine := newRegion09Router(t, db)

	if resp := region09Call(t, engine, http.MethodPost, "/admin-api/regions", `{"name":"日本","sort":1}`); resp.Code != 0 {
		t.Fatalf("建地区失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if _, err := db.Exec("INSERT INTO nodes (name, host, region_id) VALUES ('jp-1','1.1.1.1',1),('jp-2','2.2.2.2',1),('jp-3','3.3.3.3',NULL)"); err != nil {
		t.Fatalf("造节点失败: %v", err)
	}

	del := region09Call(t, engine, http.MethodDelete, "/admin-api/regions/1", "")
	if del.Code == 0 {
		t.Fatalf("有 2 台节点的地区不应删成功（data=%s）", del.Data)
	}
	if del.Code != 422 {
		t.Fatalf("拒绝删除应回 422，实际 %d（msg=%s）", del.Code, del.Msg)
	}
	if !strings.Contains(del.Msg, "2 台节点") {
		t.Fatalf("拒绝文案应含节点数「2 台节点」，实际: %s", del.Msg)
	}
	var stillThere int
	if err := db.QueryRow("SELECT COUNT(*) FROM regions WHERE id = 1").Scan(&stillThere); err != nil {
		t.Fatalf("统计 regions 失败: %v", err)
	}
	if stillThere != 1 {
		t.Fatalf("被拒绝的删除不能真删掉地区（regions 剩 %d 行）", stillThere)
	}

	// 把节点移出地区后应能删（证明守卫只看当前归属，不是永久锁死）
	for _, nodeID := range []string{"1", "2"} {
		if resp := region09Call(t, engine, http.MethodPut, "/admin-api/nodes/"+nodeID+"/region", `{"region_id":null}`); resp.Code != 0 {
			t.Fatalf("移出节点 %s 失败: code=%d msg=%s", nodeID, resp.Code, resp.Msg)
		}
	}
	if resp := region09Call(t, engine, http.MethodDelete, "/admin-api/regions/1", ""); resp.Code != 0 {
		t.Fatalf("节点全部移出后应能删地区，实际 code=%d msg=%s", resp.Code, resp.Msg)
	}
}

// TestRegion09NodeRegionAndCapacity 覆盖：设置节点 region_id / capacity → 查询能读到；
// null 复位（移出地区 / 不限容量）也生效。
func TestRegion09NodeRegionAndCapacity(t *testing.T) {
	db := newRegion09DB(t, "ch_region09_node")
	engine := newRegion09Router(t, db)

	if resp := region09Call(t, engine, http.MethodPost, "/admin-api/regions", `{"name":"香港","sort":1}`); resp.Code != 0 {
		t.Fatalf("建地区失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if _, err := db.Exec("INSERT INTO nodes (name, host, driver_type, status) VALUES ('hk-1','1.1.1.1','xray','online')"); err != nil {
		t.Fatalf("造节点失败: %v", err)
	}
	if _, err := db.Exec("INSERT INTO user_nodes (user_id, node_id) VALUES (1,1),(2,1),(3,1)"); err != nil {
		t.Fatalf("造绑定失败: %v", err)
	}

	// 设置归属
	if resp := region09Call(t, engine, http.MethodPut, "/admin-api/nodes/1/region", `{"region_id":1}`); resp.Code != 0 {
		t.Fatalf("设置节点归属失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	// 设置容量
	if resp := region09Call(t, engine, http.MethodPut, "/admin-api/nodes/1/capacity", `{"capacity":6}`); resp.Code != 0 {
		t.Fatalf("设置容量失败: code=%d msg=%s", resp.Code, resp.Msg)
	}

	nodes := region09DecodeNodes(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions/1/nodes", "").Data)
	if len(nodes) != 1 {
		t.Fatalf("地区节点应有 1 台，实际 %d", len(nodes))
	}
	n := nodes[0]
	if n.ID != 1 || n.Host != "1.1.1.1" || n.DriverType != "xray" || n.Status != "online" {
		t.Fatalf("节点基础字段不对: %+v", n)
	}
	if n.Capacity == nil || *n.Capacity != 6 {
		t.Fatalf("capacity 应为 6，实际 %v", n.Capacity)
	}
	if n.BoundUsers != 3 {
		t.Fatalf("bound_users 应为 3，实际 %d", n.BoundUsers)
	}
	if n.AgreeRatio == nil || *n.AgreeRatio != 0.5 {
		t.Fatalf("agree_ratio 应为 3/6=0.5，实际 %v", n.AgreeRatio)
	}

	// 列表统计同步：1 节点、3 个去重用户、容量 6
	list := region09DecodeRegions(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions", "").Data)
	if list[0].NodeCount != 1 || list[0].UserCount != 3 || list[0].CapacityTotal != 6 || list[0].CapacityUsed != 3 {
		t.Fatalf("地区统计不对: %+v", list[0])
	}

	// capacity=null → 不限（agree_ratio 变 null）
	if resp := region09Call(t, engine, http.MethodPut, "/admin-api/nodes/1/capacity", `{"capacity":null}`); resp.Code != 0 {
		t.Fatalf("清空容量失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	nodes = region09DecodeNodes(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions/1/nodes", "").Data)
	if nodes[0].Capacity != nil || nodes[0].AgreeRatio != nil {
		t.Fatalf("容量置 null 后应输出 null，实际 capacity=%v ratio=%v", nodes[0].Capacity, nodes[0].AgreeRatio)
	}

	// region_id=null → 移出地区，列表里该地区节点清零
	if resp := region09Call(t, engine, http.MethodPut, "/admin-api/nodes/1/region", `{"region_id":null}`); resp.Code != 0 {
		t.Fatalf("移出地区失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if nodes = region09DecodeNodes(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions/1/nodes", "").Data); len(nodes) != 0 {
		t.Fatalf("移出后地区节点应为空，实际 %d", len(nodes))
	}
	list = region09DecodeRegions(t, region09Call(t, engine, http.MethodGet, "/admin-api/regions", "").Data)
	if list[0].NodeCount != 0 || list[0].CapacityUsed != 0 {
		t.Fatalf("移出后地区统计应清零: %+v", list[0])
	}

	// 不存在的地区 / 节点 → 业务错误，不是 500
	if resp := region09Call(t, engine, http.MethodPut, "/admin-api/nodes/1/region", `{"region_id":999}`); resp.Code != 422 {
		t.Fatalf("把节点挂到不存在的地区应回 422，实际 %d（msg=%s）", resp.Code, resp.Msg)
	}
	if resp := region09Call(t, engine, http.MethodPut, "/admin-api/nodes/999/capacity", `{"capacity":5}`); resp.Code != 404 {
		t.Fatalf("给不存在的节点设容量应回 404，实际 %d（msg=%s）", resp.Code, resp.Msg)
	}
}

// TestRegion09Overview 覆盖：总览的容量合计、已绑合计与真实数据一致。
//
// 数据布局：
//
//	地区1（香港）: node1(cap 10) + node2(cap 20)，绑定 (u1,n1) (u2,n1) (u2,n2)
//	地区2（日本）: node3(cap NULL，不限)，绑定 (u3,n3)
//	未归属      : node4(cap NULL)，无绑定
//
// 期望：node_total=4、capacity_total=30、capacity_used=4；
// 地区1: node_count=2、user_count=2（u1,u2 去重）、capacity_used=3；
// 地区2: node_count=1、capacity_total=0（NULL 不计入）、user_count=1；
// unassigned_nodes 只有 node4。
func TestRegion09Overview(t *testing.T) {
	db := newRegion09DB(t, "ch_region09_overview")
	engine := newRegion09Router(t, db)

	for _, stmt := range []string{
		"INSERT INTO regions (name, sort) VALUES ('香港', 1), ('日本', 2)",
		"INSERT INTO nodes (name, host, status, capacity, region_id) VALUES ('hk-1','1.1.1.1','online',10,1), ('hk-2','2.2.2.2','online',20,1), ('jp-1','3.3.3.3','online',NULL,2), ('old-1','4.4.4.4','offline',NULL,NULL)",
		"INSERT INTO user_nodes (user_id, node_id) VALUES (1,1),(2,1),(2,2),(3,3)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("造数据失败: %v\nSQL: %s", err, stmt)
		}
	}

	resp := region09Call(t, engine, http.MethodGet, "/admin-api/regions/overview", "")
	if resp.Code != 0 {
		t.Fatalf("总览失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
	var data struct {
		NodeTotal     int64             `json:"node_total"`
		CapacityTotal int64             `json:"capacity_total"`
		CapacityUsed  int64             `json:"capacity_used"`
		Regions       []region09Summary `json:"regions"`
		Unassigned    []region09Node    `json:"unassigned_nodes"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatalf("解析总览失败: %v（data=%s）", err, resp.Data)
	}

	if data.NodeTotal != 4 || data.CapacityTotal != 30 || data.CapacityUsed != 4 {
		t.Fatalf("全局合计不对: node_total=%d capacity_total=%d capacity_used=%d（期望 4/30/4）",
			data.NodeTotal, data.CapacityTotal, data.CapacityUsed)
	}
	if len(data.Regions) != 2 {
		t.Fatalf("应有 2 个地区，实际 %d", len(data.Regions))
	}
	hk, jp := data.Regions[0], data.Regions[1]
	if hk.Name != "香港" || hk.NodeCount != 2 || hk.UserCount != 2 || hk.CapacityTotal != 30 || hk.CapacityUsed != 3 {
		t.Fatalf("香港统计不对: %+v", hk)
	}
	if jp.Name != "日本" || jp.NodeCount != 1 || jp.UserCount != 1 || jp.CapacityTotal != 0 || jp.CapacityUsed != 1 {
		t.Fatalf("日本统计不对: %+v", jp)
	}
	if len(data.Unassigned) != 1 || data.Unassigned[0].Name != "old-1" || data.Unassigned[0].BoundUsers != 0 {
		t.Fatalf("未归属节点不对: %+v", data.Unassigned)
	}
}
