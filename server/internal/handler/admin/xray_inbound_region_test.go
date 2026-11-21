package admin

// 入站归属地区（#入站级分组）集成测试：真连 MySQL，独立库 ch_region31_*，跑完自动 DROP。
//
// 要钉住的是四件事（打桩证明不了 SQL 语义与可空列）：
//  1. 新建入站带 region_id → 落库；列表/详情都回显 region_id + region_name；
//  2. 不传 region_id = 跟随节点（NULL），老客户端（不带这个字段）行为不变；
//  3. 修改：传 region_id=null 清空；不传则保持原值；
//  4. region_id 指向不存在的地区 → 业务错误（500），且不落库。
//
// 变异测试锚点：把 UpdateXrayInbound 里的 `if in.RegionIDSet` 分支删掉
// → TestRegion31InboundUpdateRegionID 的「清空」断言变红（region_id 仍是 1）。

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

// region31DSN 连本机 MySQL（root/空密码），可用 REGION31_MYSQL_DSN 覆盖。
func region31DSN() string {
	if dsn := os.Getenv("REGION31_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

// region31WithSchema 把 DSN 的默认库换成 schema。
func region31WithSchema(dsn, schema string) string {
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

// region31DDL 只建入站接口真正读写的表（列与 cmd/server/schema.go 对齐）。
var region31DDL = []string{
	`CREATE TABLE nodes (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(255) NOT NULL,
		host varchar(255) NOT NULL,
		port int NOT NULL DEFAULT 443,
		scheme varchar(8) NOT NULL DEFAULT 'https',
		web_base_path varchar(255) NOT NULL DEFAULT '',
		username varchar(255) NOT NULL DEFAULT '',
		password text,
		api_key text,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		verify_ssl tinyint(1) NOT NULL DEFAULT 0,
		status varchar(16) NOT NULL DEFAULT 'online',
		latency int NOT NULL DEFAULT 0,
		region_id bigint unsigned DEFAULT NULL,
		capacity int DEFAULT NULL,
		cpu decimal(5,2) DEFAULT NULL,
		mem decimal(5,2) DEFAULT NULL,
		traffic_multiplier decimal(6,2) DEFAULT NULL,
		driver_type varchar(32) NOT NULL DEFAULT 'xray',
		driver_version varchar(16) DEFAULT NULL,
		driver_config json DEFAULT NULL,
		last_check_at timestamp NULL DEFAULT NULL,
		is_custom_config tinyint NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE regions (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) NOT NULL,
		sort int NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY regions_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		region_id bigint unsigned DEFAULT NULL,
		tag varchar(64) NOT NULL,
		remark varchar(128) DEFAULT NULL,
		protocol varchar(32) NOT NULL,
		port int unsigned NOT NULL,
		listen varchar(64) NOT NULL DEFAULT '0.0.0.0',
		client_host varchar(255) DEFAULT NULL,
		settings json DEFAULT NULL,
		stream_settings json DEFAULT NULL,
		sniffing json DEFAULT NULL,
		reality_private_key text,
		salamander_password text,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		sort int unsigned NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY xray_inbounds_node_id_tag_unique (node_id, tag),
		KEY idx_xray_inbounds_region (region_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE node_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		protocol varchar(16) NOT NULL,
		driver_type varchar(32) NOT NULL DEFAULT '3x-ui',
		inbound_id bigint NOT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY node_inbounds_uniq (node_id, protocol, inbound_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE user_nodes (
		user_id bigint unsigned NOT NULL,
		node_id bigint unsigned NOT NULL,
		PRIMARY KEY (user_id, node_id),
		KEY idx_node (node_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE users (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		protocol varchar(16) NOT NULL DEFAULT 'vless',
		plan_id bigint unsigned DEFAULT NULL,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		traffic_disabled_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_certificates (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(190) NOT NULL,
		domain varchar(190) DEFAULT NULL,
		cert_pem mediumtext,
		key_enc mediumtext,
		not_after datetime DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY xray_certificates_name_unique (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newRegion31DB 建一个干净的 ch_region31_* 测试库 + 表结构，跑完自动 DROP。
func newRegion31DB(t *testing.T, caseName string) *sql.DB {
	t.Helper()

	dsn := region31DSN()
	schema := fmt.Sprintf("ch_region31_%s_%d", caseName, os.Getpid())

	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过入站地区集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过入站地区集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	for _, q := range []string{
		"DROP DATABASE IF EXISTS `" + schema + "`",
		"CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci",
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("准备测试库失败: %v\nSQL: %s", err, q)
		}
	}
	t.Cleanup(func() {
		again, err := sql.Open("mysql", dsn)
		if err != nil {
			return
		}
		defer again.Close()
		again.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region31WithSchema(dsn, schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range region31DDL {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建测试表失败: %v\nSQL: %s", err, stmt)
		}
	}
	return db
}

// region31Engine 组装只挂节点域（入站 CRUD）的 gin 引擎，并注入地区 store（region_name 回显）。
func region31Engine(t *testing.T, db *sql.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewNodeDomain(NodeDeps{
		Store:   NewNodeSQLStore(db, nil),
		Regions: NewRegionSQLStore(db),
	}).Register(engine.Group("/admin-api"))
	return engine
}

// region31Call 发一个 JSON 请求并解出 {code,msg,data}。
func region31Call(t *testing.T, engine *gin.Engine, method, path, body string) (int, map[string]any) {
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
	out := map[string]any{}
	if raw := rec.Body.Bytes(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s 响应不是 JSON（%v）: %s", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// region31SeedNode 造一台 xray 节点与两个地区，返回 (nodeID, regionID1, regionID2)。
func region31SeedNode(t *testing.T, db *sql.DB) (uint64, uint64, uint64) {
	t.Helper()
	res, err := db.Exec("INSERT INTO nodes (name, host, driver_type) VALUES ('hk-1','hk1.example.com','xray')")
	if err != nil {
		t.Fatalf("造节点失败: %v", err)
	}
	nodeID, _ := res.LastInsertId()
	r1, err := db.Exec("INSERT INTO regions (name) VALUES ('香港')")
	if err != nil {
		t.Fatalf("造地区失败: %v", err)
	}
	region1, _ := r1.LastInsertId()
	r2, err := db.Exec("INSERT INTO regions (name) VALUES ('日本')")
	if err != nil {
		t.Fatalf("造地区失败: %v", err)
	}
	region2, _ := r2.LastInsertId()
	return uint64(nodeID), uint64(region1), uint64(region2)
}

// region31CreateBody 是最小可建的入站请求体（vless/tcp/none，端口可变）。
func region31CreateBody(port int, extra string) string {
	body := fmt.Sprintf(`{"protocol":"vless","port":%d,"network":"tcp","security":"none","tag":"vless-%d"`, port, port)
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

// TestRegion31InboundCreateWithRegion：带 region_id 建入站 → 落库，列表回显 id + 地区名。
func TestRegion31InboundCreateWithRegion(t *testing.T) {
	db := newRegion31DB(t, "create")
	engine := region31Engine(t, db)
	nodeID, region1, _ := region31SeedNode(t, db)

	code, resp := region31Call(t, engine, http.MethodPost,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds", nodeID),
		region31CreateBody(443, fmt.Sprintf(`"region_id":%d`, region1)))
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("建入站失败: code=%d resp=%#v", code, resp)
	}
	data, _ := resp["data"].(map[string]any)
	inbound, _ := data["inbound"].(map[string]any)
	if inbound["region_id"] != float64(region1) {
		t.Fatalf("创建响应 region_id = %v，期望 %d", inbound["region_id"], region1)
	}
	if inbound["region_name"] != "香港" {
		t.Fatalf("创建响应 region_name = %v，期望 香港", inbound["region_name"])
	}

	// 落库确实是该地区
	var stored sql.NullInt64
	if err := db.QueryRow("SELECT region_id FROM xray_inbounds WHERE node_id = ?", nodeID).Scan(&stored); err != nil {
		t.Fatalf("回读 region_id 失败: %v", err)
	}
	if !stored.Valid || uint64(stored.Int64) != region1 {
		t.Fatalf("落库 region_id = %v，期望 %d", stored, region1)
	}

	// 列表回显
	_, listResp := region31Call(t, engine, http.MethodGet,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds", nodeID), "")
	listData, _ := listResp["data"].(map[string]any)
	list, _ := listData["inbounds"].([]any)
	if len(list) != 1 {
		t.Fatalf("列表应有 1 条入站，实际 %d", len(list))
	}
	row, _ := list[0].(map[string]any)
	if row["region_id"] != float64(region1) || row["region_name"] != "香港" {
		t.Fatalf("列表回显不对: region_id=%v region_name=%v", row["region_id"], row["region_name"])
	}
}

// TestRegion31InboundCreateWithoutRegion：不带 region_id 建入站 → NULL（跟随节点，老行为）。
func TestRegion31InboundCreateWithoutRegion(t *testing.T) {
	db := newRegion31DB(t, "default")
	engine := region31Engine(t, db)
	nodeID, _, _ := region31SeedNode(t, db)

	_, resp := region31Call(t, engine, http.MethodPost,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds", nodeID), region31CreateBody(443, ""))
	if resp["code"] != float64(0) {
		t.Fatalf("建入站失败: %#v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	inbound, _ := data["inbound"].(map[string]any)
	if inbound["region_id"] != nil {
		t.Fatalf("不带 region_id 时应为 null（跟随节点），实际 %v", inbound["region_id"])
	}
	var stored sql.NullInt64
	if err := db.QueryRow("SELECT region_id FROM xray_inbounds WHERE node_id = ?", nodeID).Scan(&stored); err != nil {
		t.Fatalf("回读 region_id 失败: %v", err)
	}
	if stored.Valid {
		t.Fatalf("不带 region_id 时库里应是 NULL，实际 %d", stored.Int64)
	}
}

// TestRegion31InboundUpdateRegionID：修改时传 null 清空、不传保持原值。
func TestRegion31InboundUpdateRegionID(t *testing.T) {
	db := newRegion31DB(t, "update")
	engine := region31Engine(t, db)
	nodeID, region1, _ := region31SeedNode(t, db)

	_, resp := region31Call(t, engine, http.MethodPost,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds", nodeID),
		region31CreateBody(443, fmt.Sprintf(`"region_id":%d`, region1)))
	data, _ := resp["data"].(map[string]any)
	inbound, _ := data["inbound"].(map[string]any)
	inboundID := int64(inbound["id"].(float64))
	path := fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds/%d", nodeID, inboundID)

	// 1) 修改但不带 region_id → 保持 1 号地区。
	_, upd := region31Call(t, engine, http.MethodPut, path, region31CreateBody(443, ""))
	if upd["code"] != float64(0) {
		t.Fatalf("改入站失败: %#v", upd)
	}
	var stored sql.NullInt64
	if err := db.QueryRow("SELECT region_id FROM xray_inbounds WHERE id = ?", inboundID).Scan(&stored); err != nil {
		t.Fatalf("回读 region_id 失败: %v", err)
	}
	if !stored.Valid || uint64(stored.Int64) != region1 {
		t.Fatalf("没带 region_id 时应保持原值 %d，实际 %v", region1, stored)
	}

	// 2) 显式 region_id=null → 清空（跟随节点）。
	_, upd = region31Call(t, engine, http.MethodPut, path, region31CreateBody(443, `"region_id":null`))
	if upd["code"] != float64(0) {
		t.Fatalf("清空 region_id 失败: %#v", upd)
	}
	if err := db.QueryRow("SELECT region_id FROM xray_inbounds WHERE id = ?", inboundID).Scan(&stored); err != nil {
		t.Fatalf("回读 region_id 失败: %v", err)
	}
	if stored.Valid {
		t.Fatalf("显式传 null 后 region_id 应为 NULL，实际 %d", stored.Int64)
	}
}

// TestRegion31InboundRegionMustExist：region_id 指向不存在的地区 → 业务错误且不落库。
func TestRegion31InboundRegionMustExist(t *testing.T) {
	db := newRegion31DB(t, "missing")
	engine := region31Engine(t, db)
	nodeID, _, _ := region31SeedNode(t, db)

	_, resp := region31Call(t, engine, http.MethodPost,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds", nodeID),
		region31CreateBody(443, `"region_id":999`))
	if resp["code"] == float64(0) {
		t.Fatalf("不存在的地区不应建成功: %#v", resp)
	}
	msg, _ := resp["msg"].(string)
	if !strings.Contains(msg, "地区") {
		t.Fatalf("报错文案应提到地区，实际 %q", msg)
	}
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM xray_inbounds WHERE node_id = ?", nodeID).Scan(&n); err != nil {
		t.Fatalf("统计入站失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("校验失败不应落库，实际 %d 行", n)
	}
}

// TestRegion31RegionNodesShowInboundRegions：GET /regions/:id/nodes 带出入站归属（前端只读展示）。
func TestRegion31RegionNodesShowInboundRegions(t *testing.T) {
	db := newRegion31DB(t, "regionnodes")
	nodeEngine := region31Engine(t, db)
	nodeID, region1, region2 := region31SeedNode(t, db)
	if _, err := db.Exec("UPDATE nodes SET region_id = ? WHERE id = ?", region1, nodeID); err != nil {
		t.Fatalf("设置节点地区失败: %v", err)
	}

	// 两个入站：一个跟随节点（NULL），一个归 2 号地区。
	if _, resp := region31Call(t, nodeEngine, http.MethodPost,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds", nodeID), region31CreateBody(443, "")); resp["code"] != float64(0) {
		t.Fatalf("建跟随入站失败: %#v", resp)
	}
	if _, resp := region31Call(t, nodeEngine, http.MethodPost,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds", nodeID),
		region31CreateBody(8443, fmt.Sprintf(`"region_id":%d`, region2))); resp["code"] != float64(0) {
		t.Fatalf("建归属入站失败: %#v", resp)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewRegionDomain(RegionDeps{Store: NewRegionSQLStore(db)}).Register(engine.Group("/admin-api"))

	_, resp := region31Call(t, engine, http.MethodGet, fmt.Sprintf("/admin-api/regions/%d/nodes", region1), "")
	if resp["code"] != float64(0) {
		t.Fatalf("查地区节点失败: %#v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	nodes, _ := data["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("地区应有 1 台节点，实际 %d", len(nodes))
	}
	node, _ := nodes[0].(map[string]any)
	inbounds, _ := node["inbounds"].([]any)
	if len(inbounds) != 2 {
		t.Fatalf("节点应带 2 条入站归属，实际 %d", len(inbounds))
	}
	byTag := map[string]map[string]any{}
	for _, raw := range inbounds {
		ib, _ := raw.(map[string]any)
		byTag[fmt.Sprintf("%v", ib["tag"])] = ib
	}
	// 跟随入站：有效归属回落到节点地区（香港）
	follow := byTag["vless-443"]
	if follow == nil || follow["region_name"] != "香港" {
		t.Fatalf("跟随入站的有效归属应是节点地区「香港」，实际 %#v", follow)
	}
	// 归 2 号地区的入站：显示自己的地区名
	own := byTag["vless-8443"]
	if own == nil || own["region_name"] != "日本" {
		t.Fatalf("归属入站应显示「日本」，实际 %#v", own)
	}
}
