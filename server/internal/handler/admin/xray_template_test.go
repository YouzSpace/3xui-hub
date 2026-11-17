package admin

// 配置模板（地区功能-19）集成测试：真连 MySQL，独立库 ch_region19_*，跑完自动 DROP。
//
// 打桩证明不了的三件事，只能连真库：
//  1. 套用是**一个事务**内的删/改/插（入站删旧、出站全量替换、路由全量替换）；
//  2. reality 私钥**每条入站现场生成**，两条入站的私钥必须不同，且库里存的是密文、
//     解出来能推出 stream_settings.realitySettings.publicKey 里那把公钥；
//  3. is_custom_config 这条链路：现有入站写接口把它置 1，套用模板把它清回 0。
//
// 变异测试锚点：把 applyNodeConfig 里的「每条入站调用 xray.GenerateKeypair()」
// 换成「直接复制模板里的值」，TestXrayTemplate19ApplyToXrayNode 的
// 「两条私钥不相同」断言必须变红。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"controlhub-server/internal/crypto"
	"controlhub-server/internal/driver/xray"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
)

// xrayTemplate19AppKey 是 32 字节 AES key（reality 私钥加密用，与生产同一个算法）。
const xrayTemplate19AppKey = "0123456789abcdef0123456789abcdef"

// xrayTemplate19DSN 连本机 MySQL（root/空密码），可用 REGION19_MYSQL_DSN 覆盖。
func xrayTemplate19DSN() string {
	if dsn := os.Getenv("REGION19_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s&parseTime=true&loc=UTC"
}

// xrayTemplate19WithSchema 把 DSN 的默认库换成 schema。
func xrayTemplate19WithSchema(dsn, schema string) string {
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

// xrayTemplate19DDL 是本次流程真正读写的表（列与 cmd/server/schema.go / schema.sql 对齐）。
//
// 三张 xray_template_* 表与 nodes.is_custom_config 由 ensureSchema 在生产里建；
// 本用例只测行为，所以在测试库里按同一份 DDL 建（建表本身的幂等性由
// cmd/server/schema_template_test.go 覆盖）。
var xrayTemplate19DDL = []string{
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
		driver_type varchar(32) NOT NULL DEFAULT '3x-ui',
		driver_version varchar(16) DEFAULT NULL,
		driver_config json DEFAULT NULL,
		verify_ssl tinyint(1) NOT NULL DEFAULT 0,
		status varchar(16) NOT NULL DEFAULT 'offline',
		traffic_multiplier decimal(6,2) DEFAULT NULL,
		latency int NOT NULL DEFAULT 0,
		region_id bigint unsigned DEFAULT NULL,
		capacity int DEFAULT NULL,
		cpu decimal(5,2) DEFAULT NULL,
		mem decimal(5,2) DEFAULT NULL,
		last_check_at timestamp NULL DEFAULT NULL,
		is_custom_config tinyint NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
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
		UNIQUE KEY xray_inbounds_node_id_tag_unique (node_id, tag)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_outbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		tag varchar(64) NOT NULL,
		protocol varchar(32) NOT NULL,
		settings json DEFAULT NULL,
		stream_settings json DEFAULT NULL,
		secret_key text,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		sort int unsigned NOT NULL DEFAULT 0,
		remark varchar(128) DEFAULT NULL,
		relay_target_node_id bigint unsigned DEFAULT NULL,
		relay_target_inbound_id bigint unsigned DEFAULT NULL,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY xray_outbounds_node_id_tag_unique (node_id, tag)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE node_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		protocol varchar(16) NOT NULL,
		driver_type varchar(32) NOT NULL DEFAULT '3x-ui',
		inbound_id bigint NOT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_routing_rules (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		node_id bigint unsigned NOT NULL,
		domains json DEFAULT NULL,
		ips json DEFAULT NULL,
		port varchar(64) DEFAULT NULL,
		network varchar(16) DEFAULT NULL,
		protocol varchar(16) DEFAULT NULL,
		inbound_tag varchar(64) DEFAULT NULL,
		outbound_tag varchar(64) NOT NULL,
		remark varchar(128) DEFAULT NULL,
		enabled tinyint(1) NOT NULL DEFAULT 1,
		sort int unsigned NOT NULL DEFAULT 0,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_template_inbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) NOT NULL,
		sort int NOT NULL DEFAULT 0,
		tag varchar(64) NOT NULL,
		remark varchar(128) DEFAULT NULL,
		protocol varchar(32) NOT NULL,
		port int unsigned NOT NULL,
		listen varchar(64) NOT NULL DEFAULT '0.0.0.0',
		settings json DEFAULT NULL,
		stream_settings json DEFAULT NULL,
		sniffing json DEFAULT NULL,
		enabled tinyint NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_template_outbounds (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) NOT NULL,
		sort int NOT NULL DEFAULT 0,
		tag varchar(64) NOT NULL,
		protocol varchar(32) NOT NULL,
		settings json DEFAULT NULL,
		stream_settings json DEFAULT NULL,
		enabled tinyint NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE xray_template_routing (
		id bigint unsigned NOT NULL AUTO_INCREMENT,
		name varchar(64) NOT NULL,
		sort int NOT NULL DEFAULT 0,
		domains json DEFAULT NULL,
		ips json DEFAULT NULL,
		port varchar(64) DEFAULT NULL,
		network varchar(16) DEFAULT NULL,
		protocol varchar(16) DEFAULT NULL,
		inbound_tag varchar(64) DEFAULT NULL,
		outbound_tag varchar(64) NOT NULL,
		remark varchar(128) DEFAULT NULL,
		enabled tinyint NOT NULL DEFAULT 1,
		created_at timestamp NULL DEFAULT NULL,
		updated_at timestamp NULL DEFAULT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// newXrayTemplate19DB 建一个干净的 ch_region19_* 测试库 + 表结构，跑完自动 DROP。
func newXrayTemplate19DB(t *testing.T, caseName string) *sql.DB {
	t.Helper()

	dsn := xrayTemplate19DSN()
	schema := fmt.Sprintf("ch_region19_%s_%d", caseName, os.Getpid())

	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过配置模板集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过配置模板集成测试", err)
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
		admin2, err := sql.Open("mysql", dsn)
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", xrayTemplate19WithSchema(dsn, schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range xrayTemplate19DDL {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建测试表失败: %v\nSQL: %s", err, stmt)
		}
	}
	return db
}

// xrayTemplate19Engine 组装一个只挂管理端路由的 gin 引擎（无鉴权）。
func xrayTemplate19Engine(t *testing.T, db *sql.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group("/admin-api")

	tpl := NewXrayTemplateDomain(XrayTemplateDeps{
		DB:     db,
		AppKey: []byte(xrayTemplate19AppKey),
		Logger: log.New(testWriter{t}, "", 0),
	})
	tpl.Register(group)

	// 入站 CRUD 那一路（验证「改入站 → is_custom_config 自动置 1」）。
	NewNodeDomain(NodeDeps{Store: NewNodeSQLStore(db, []byte(xrayTemplate19AppKey))}).Register(group)
	return engine
}

// testWriter 把域内日志写进测试输出（失败时能看到原因）。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// doJSON 发一个 JSON 请求，返回 (HTTP 状态码, 解析后的响应体)。
func doJSON(t *testing.T, engine *gin.Engine, method, path, body string) (int, map[string]any) {
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

// dataOf 取响应里的 data 对象（不是对象就报错）。
func dataOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应 data 不是对象: %#v", resp)
	}
	return data
}

// msgOf 取响应里的 msg。
func msgOf(resp map[string]any) string {
	s, _ := resp["msg"].(string)
	return s
}

// insertNode 造一台节点，updated_at 刻意放在很久以前（断言「套用后 updated_at 有变化」）。
func insertNode(t *testing.T, db *sql.DB, name, driverType string, customConfig int) uint64 {
	t.Helper()
	res, err := db.Exec(
		"INSERT INTO nodes (name, host, port, driver_type, is_custom_config, created_at, updated_at) "+
			"VALUES (?, ?, 443, ?, ?, '2020-01-01 00:00:00', '2020-01-01 00:00:00')",
		name, name+".example.com", driverType, customConfig)
	if err != nil {
		t.Fatalf("造节点失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取节点 id 失败: %v", err)
	}
	return uint64(id)
}

// testInboundTemplateBody 是「一条 vless + reality 入站模板」的请求体。
func testInboundTemplateBody(name, tag string, port int) string {
	return fmt.Sprintf(`{
		"name": %q,
		"tag": %q,
		"protocol": "vless",
		"port": %d,
		"listen": "0.0.0.0",
		"remark": "模板入站",
		"settings": {"clients": [], "decryption": "none"},
		"stream_settings": {
			"network": "tcp",
			"security": "reality",
			"realitySettings": {"dest": "www.microsoft.com:443", "serverNames": ["www.microsoft.com"], "shortIds": ["abcd1234"], "publicKey": "TEMPLATE-PLACEHOLDER"}
		},
		"sniffing": {"enabled": true, "destOverride": ["http", "tls"]}
	}`, name, tag, port)
}

// seedTemplates 通过 HTTP 接口建 2 条入站模板 + 1 条出站模板 + 1 条路由模板。
// 两条入站模板刻意用同一个端口 443 —— 验证「冲突时按模板端口 +1 试探」。
func seedTemplates(t *testing.T, engine *gin.Engine) {
	t.Helper()

	for _, item := range []struct{ name, tag string }{
		{"标准 VLESS-1", "tmpl-vless-1"},
		{"标准 VLESS-2", "tmpl-vless-2"},
	} {
		code, resp := doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/inbound",
			testInboundTemplateBody(item.name, item.tag, 443))
		if code != http.StatusOK || resp["code"] != float64(0) {
			t.Fatalf("建入站模板 %s 失败: code=%d resp=%#v", item.tag, code, resp)
		}
	}

	code, resp := doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/outbound", `{
		"name": "标准出站",
		"tag": "tmpl-direct",
		"protocol": "freedom",
		"settings": {},
		"stream_settings": {}
	}`)
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("建出站模板失败: code=%d resp=%#v", code, resp)
	}

	code, resp = doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/routing", `{
		"name": "内网直连",
		"domains": ["geosite:private"],
		"ips": ["geoip:private"],
		"outbound_tag": "tmpl-direct",
		"remark": "内网走直连"
	}`)
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("建路由模板失败: code=%d resp=%#v", code, resp)
	}
}

// applyTo 调套用接口，返回逐节点结果（node_id → 结果对象）与三个计数。
func applyTo(t *testing.T, engine *gin.Engine, nodeIDs []uint64, overwrite bool) (map[uint64]map[string]any, map[string]any) {
	t.Helper()

	ids := make([]string, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		ids = append(ids, fmt.Sprintf("%d", id))
	}
	body := fmt.Sprintf(`{"node_ids": [%s], "overwrite_custom": %t}`, strings.Join(ids, ","), overwrite)

	code, resp := doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/apply", body)
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("套用模板失败: code=%d resp=%#v", code, resp)
	}
	data := dataOf(t, resp)
	rawResults, _ := data["results"].([]any)

	byNode := map[uint64]map[string]any{}
	for _, raw := range rawResults {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("结果项不是对象: %#v", raw)
		}
		idFloat, _ := item["node_id"].(float64)
		byNode[uint64(idFloat)] = item
	}
	return byNode, data
}

// TestXrayTemplate19ApplyToXrayNode 是主链路：建模板 → 套到 xray 节点 → 逐项核对。
func TestXrayTemplate19ApplyToXrayNode(t *testing.T) {
	db := newXrayTemplate19DB(t, "apply")
	engine := xrayTemplate19Engine(t, db)

	xrayNodeID := insertNode(t, db, "hk-xray", "xray", 0)
	threeXUINodeID := insertNode(t, db, "jp-3xui", "3x-ui", 0)

	// 节点上先塞一条「模板里没有」的入站 + 一条旧出站 + 一条旧路由：
	// 套用后这些都应该被清掉（入站按 tag 删、出站/路由全量替换）。
	if _, err := db.Exec(`INSERT INTO xray_inbounds (node_id, tag, protocol, port, settings)
		VALUES (?, 'old-manual', 'trojan', 8443, '{"clients": []}')`, xrayNodeID); err != nil {
		t.Fatalf("造旧入站失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO xray_outbounds (node_id, tag, protocol, settings)
		VALUES (?, 'old-out', 'freedom', '{}')`, xrayNodeID); err != nil {
		t.Fatalf("造旧出站失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO xray_routing_rules (node_id, outbound_tag, domains)
		VALUES (?, 'old-out', '["geosite:cn"]')`, xrayNodeID); err != nil {
		t.Fatalf("造旧路由失败: %v", err)
	}

	seedTemplates(t, engine)

	byNode, data := applyTo(t, engine, []uint64{xrayNodeID, threeXUINodeID}, false)

	// ── xray 节点：已套用 ──
	res := byNode[xrayNodeID]
	if res == nil {
		t.Fatalf("结果里没有 xray 节点 %d: %#v", xrayNodeID, byNode)
	}
	if res["status"] != xrayTemplateApplyApplied {
		t.Fatalf("xray 节点状态 = %v，期望 applied（message=%v）", res["status"], res["message"])
	}
	if data["applied"] != float64(1) || data["skipped"] != float64(1) {
		t.Fatalf("计数 = applied %v / skipped %v，期望 1 / 1", data["applied"], data["skipped"])
	}

	// ── 3x-ui 节点：跳过并说明原因 ──
	res = byNode[threeXUINodeID]
	if res["status"] != xrayTemplateApplySkipped {
		t.Fatalf("3x-ui 节点状态 = %v，期望 skipped", res["status"])
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "不支持模板") {
		t.Fatalf("3x-ui 节点 message = %q，期望含「不支持模板」", msg)
	}
	// 3x-ui 节点的数据必须一个字节都没动。
	if n := countRows(t, db, "SELECT COUNT(*) FROM xray_inbounds WHERE node_id = ?", threeXUINodeID); n != 0 {
		t.Fatalf("3x-ui 节点不该被写任何入站，实际 %d 行", n)
	}

	// ── 入站：模板 2 条，旧入站已删 ──
	rows, err := db.Query("SELECT tag, port, reality_private_key, stream_settings FROM xray_inbounds "+
		"WHERE node_id = ? ORDER BY tag", xrayNodeID)
	if err != nil {
		t.Fatalf("查入站失败: %v", err)
	}
	defer rows.Close()

	type inboundRow struct {
		tag, port, keyEnc, stream string
	}
	var got []inboundRow
	for rows.Next() {
		var (
			tag    string
			port   int64
			keyEnc sql.NullString
			stream sql.NullString
		)
		if err := rows.Scan(&tag, &port, &keyEnc, &stream); err != nil {
			t.Fatalf("扫入站失败: %v", err)
		}
		got = append(got, inboundRow{tag: tag, port: fmt.Sprintf("%d", port), keyEnc: keyEnc.String, stream: stream.String})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历入站失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("套用后入站条数 = %d，期望 2（模板里的两条，旧的 old-manual 应被删）: %+v", len(got), got)
	}
	if got[0].tag != "tmpl-vless-1" || got[1].tag != "tmpl-vless-2" {
		t.Fatalf("入站 tag = %q/%q，期望 tmpl-vless-1 / tmpl-vless-2", got[0].tag, got[1].tag)
	}
	// 端口冲突（两条模板都写 443）→ 第二条按 +1 试探成 444。
	if got[0].port != "443" || got[1].port != "444" {
		t.Fatalf("端口 = %s/%s，期望 443 / 444（同族端口冲突按 +1 试探）", got[0].port, got[1].port)
	}

	// ── 关键断言：reality 私钥每条单独生成 → 两条必须不相同 ──
	if got[0].keyEnc == "" || got[1].keyEnc == "" {
		t.Fatalf("两条入站的 reality_private_key 都必须有值，实际 %q / %q", got[0].keyEnc, got[1].keyEnc)
	}
	// 必须比**明文**：密文是随机 IV 的 AES，同一把私钥两次加密的密文也不同，
	// 拿密文比较等于什么都没验（变异测试会照常变绿）。
	plainKeys := make([]string, len(got))
	for i, row := range got {
		plain, err := crypto.DecryptString(row.keyEnc, []byte(xrayTemplate19AppKey))
		if err != nil {
			t.Fatalf("入站 %s 的 reality_private_key 解不开（应存 Laravel 兼容密文）: %v", row.tag, err)
		}
		plainKeys[i] = plain
	}
	if plainKeys[0] == plainKeys[1] {
		t.Fatalf("两条入站的 reality 私钥明文相同（%q）—— 私钥必须每个节点每条单独生成", plainKeys[0])
	}
	// 把两条私钥/公钥的实际值打出来（验收要求「贴实际值证明不同」）。
	t.Logf("入站 %s 的 reality 私钥 = %s", got[0].tag, plainKeys[0])
	t.Logf("入站 %s 的 reality 私钥 = %s", got[1].tag, plainKeys[1])
	for i, row := range got {
		t.Logf("入站 %s 的 reality 公钥 = %s", row.tag, xray.PublicKeyFromPrivateKey(plainKeys[i]))
	}
	// 库里必须是密文（AES），且能推出 stream_settings 里那把公钥。
	for i, row := range got {
		plain := plainKeys[i]
		wantPublic := xray.PublicKeyFromPrivateKey(plain)
		if wantPublic == "" {
			t.Fatalf("入站 %s 解出的私钥推导不出公钥: %q", row.tag, plain)
		}
		if !strings.Contains(row.stream, wantPublic) {
			t.Fatalf("入站 %s 的 stream_settings 里没有对应公钥 %s（订阅链接会连不上）: %s",
				row.tag, wantPublic, row.stream)
		}
		if strings.Contains(row.stream, "TEMPLATE-PLACEHOLDER") {
			t.Fatalf("入站 %s 的 stream_settings 还是模板里的占位公钥: %s", row.tag, row.stream)
		}
		// 私钥绝不能出现在 stream_settings 里。
		if strings.Contains(row.stream, plain) {
			t.Fatalf("入站 %s 的 stream_settings 泄漏了私钥明文", row.tag)
		}
	}

	// ── 出站 / 路由：全量替换 ──
	if n := countRows(t, db, "SELECT COUNT(*) FROM xray_outbounds WHERE node_id = ?", xrayNodeID); n != 1 {
		t.Fatalf("出站条数 = %d，期望 1（旧的 old-out 应被全量替换掉）", n)
	}
	var outTag string
	if err := db.QueryRow("SELECT tag FROM xray_outbounds WHERE node_id = ?", xrayNodeID).Scan(&outTag); err != nil {
		t.Fatalf("查出站失败: %v", err)
	}
	if outTag != "tmpl-direct" {
		t.Fatalf("出站 tag = %q，期望 tmpl-direct", outTag)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM xray_routing_rules WHERE node_id = ?", xrayNodeID); n != 1 {
		t.Fatalf("路由条数 = %d，期望 1（旧的 geosite:cn 规则应被全量替换掉）", n)
	}
	var ruleOut, ruleDomains string
	if err := db.QueryRow("SELECT outbound_tag, COALESCE(domains, '') FROM xray_routing_rules WHERE node_id = ?", xrayNodeID).
		Scan(&ruleOut, &ruleDomains); err != nil {
		t.Fatalf("查路由失败: %v", err)
	}
	if ruleOut != "tmpl-direct" || !strings.Contains(ruleDomains, "geosite:private") {
		t.Fatalf("路由 = %q / %q，期望 tmpl-direct / geosite:private", ruleOut, ruleDomains)
	}

	// ── 节点状态：is_custom_config 清 0、updated_at 变化、配置版本 +1 ──
	var (
		custom     int
		updatedAt  string
		driverJSON sql.NullString
	)
	if err := db.QueryRow("SELECT is_custom_config, DATE_FORMAT(updated_at, '%Y-%m-%d %H:%i:%s'), driver_config "+
		"FROM nodes WHERE id = ?", xrayNodeID).Scan(&custom, &updatedAt, &driverJSON); err != nil {
		t.Fatalf("查节点失败: %v", err)
	}
	if custom != 0 {
		t.Fatalf("套用后 is_custom_config = %d，期望 0（恢复跟随模板）", custom)
	}
	if updatedAt == "2020-01-01 00:00:00" {
		t.Fatalf("套用后 nodes.updated_at 没变化（%s）—— 指纹检测会漏判", updatedAt)
	}
	driver := map[string]any{}
	if err := json.Unmarshal([]byte(driverJSON.String), &driver); err != nil {
		t.Fatalf("driver_config 不是合法 JSON: %v（%s）", err, driverJSON.String)
	}
	if driver["config_version"] != float64(1) {
		t.Fatalf("driver_config.config_version = %v，期望 1（节点据此拉新配置），实际 %s",
			driver["config_version"], driverJSON.String)
	}
}

// TestXrayTemplate19SkipRules 覆盖两条跳过规则 + 覆盖后清零。
func TestXrayTemplate19SkipRules(t *testing.T) {
	db := newXrayTemplate19DB(t, "skip")
	engine := xrayTemplate19Engine(t, db)
	seedTemplates(t, engine)

	nodeID := insertNode(t, db, "hk-xray-custom", "xray", 0)

	// 先正常套一次，节点进入「跟随模板」状态。
	if _, data := applyTo(t, engine, []uint64{nodeID}, false); data["applied"] != float64(1) {
		t.Fatalf("首次套用应成功: %#v", data)
	}

	// 管理员手工改了这个节点的入站 → 标记该置 1（这里直接改库模拟，CRUD 路径另有用例）。
	if _, err := db.Exec("UPDATE nodes SET is_custom_config = 1 WHERE id = ?", nodeID); err != nil {
		t.Fatalf("置 is_custom_config 失败: %v", err)
	}

	// 不覆盖：跳过，且入站一字不动。
	before := countRows(t, db, "SELECT COUNT(*) FROM xray_inbounds WHERE node_id = ?", nodeID)
	byNode, data := applyTo(t, engine, []uint64{nodeID}, false)
	res := byNode[nodeID]
	if res["status"] != xrayTemplateApplySkipped {
		t.Fatalf("is_custom_config=1 + overwrite=false 时状态 = %v，期望 skipped", res["status"])
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "单独配置") {
		t.Fatalf("跳过原因 = %q，期望含「单独配置」", msg)
	}
	if data["skipped"] != float64(1) {
		t.Fatalf("skipped 计数 = %v，期望 1", data["skipped"])
	}
	if after := countRows(t, db, "SELECT COUNT(*) FROM xray_inbounds WHERE node_id = ?", nodeID); after != before {
		t.Fatalf("跳过时不该动入站：套用前 %d 行、套用后 %d 行", before, after)
	}

	// 覆盖：套用成功，is_custom_config 清回 0。
	byNode, data = applyTo(t, engine, []uint64{nodeID}, true)
	res = byNode[nodeID]
	if res["status"] != xrayTemplateApplyApplied {
		t.Fatalf("overwrite=true 时状态 = %v，期望 applied（message=%v）", res["status"], res["message"])
	}
	if data["applied"] != float64(1) {
		t.Fatalf("applied 计数 = %v，期望 1", data["applied"])
	}
	var custom int
	if err := db.QueryRow("SELECT is_custom_config FROM nodes WHERE id = ?", nodeID).Scan(&custom); err != nil {
		t.Fatalf("查 is_custom_config 失败: %v", err)
	}
	if custom != 0 {
		t.Fatalf("覆盖套用后 is_custom_config = %d，期望 0", custom)
	}
}

// TestXrayTemplate19InboundCRDMarksCustom 验证「现有入站写接口把节点标记为已单独配置」。
//
// 走真实 NodeDomain（真 store + 真库）：删一条入站后 is_custom_config 必须从 0 变 1。
func TestXrayTemplate19InboundCRDMarksCustom(t *testing.T) {
	db := newXrayTemplate19DB(t, "mark")
	engine := xrayTemplate19Engine(t, db)
	seedTemplates(t, engine)

	nodeID := insertNode(t, db, "hk-xray-crud", "xray", 0)
	applyTo(t, engine, []uint64{nodeID}, false)

	var custom int
	if err := db.QueryRow("SELECT is_custom_config FROM nodes WHERE id = ?", nodeID).Scan(&custom); err != nil {
		t.Fatalf("查 is_custom_config 失败: %v", err)
	}
	if custom != 0 {
		t.Fatalf("刚套用完 is_custom_config = %d，期望 0", custom)
	}

	var inboundID uint64
	if err := db.QueryRow("SELECT id FROM xray_inbounds WHERE node_id = ? ORDER BY id LIMIT 1", nodeID).Scan(&inboundID); err != nil {
		t.Fatalf("取入站 id 失败: %v", err)
	}

	code, resp := doJSON(t, engine, http.MethodDelete,
		fmt.Sprintf("/admin-api/nodes/%d/xray-inbounds/%d", nodeID, inboundID), "")
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("删除入站失败: code=%d resp=%#v", code, resp)
	}

	if err := db.QueryRow("SELECT is_custom_config FROM nodes WHERE id = ?", nodeID).Scan(&custom); err != nil {
		t.Fatalf("查 is_custom_config 失败: %v", err)
	}
	if custom != 1 {
		t.Fatalf("删了一条入站后 is_custom_config = %d，期望自动变 1（模板套用时会跳过它）", custom)
	}
}

// TestXrayTemplate19CRUDAndGuards 覆盖模板 CRUD、列表结构与几道护栏。
func TestXrayTemplate19CRUDAndGuards(t *testing.T) {
	db := newXrayTemplate19DB(t, "crud")
	engine := xrayTemplate19Engine(t, db)

	// 空模板套用必须被拦住（否则等于把节点配置清空）。
	nodeID := insertNode(t, db, "hk-xray-empty", "xray", 0)
	code, resp := doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/apply",
		fmt.Sprintf(`{"node_ids": [%d]}`, nodeID))
	if code != http.StatusOK || resp["code"] != float64(422) {
		t.Fatalf("空模板套用应被 422 拦住，实际 code=%d resp=%#v", code, resp)
	}

	// 没选节点 → 422。
	code, resp = doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/apply", `{"node_ids": []}`)
	if resp["code"] != float64(422) {
		t.Fatalf("没选节点应 422，实际 code=%d resp=%#v", code, resp)
	}

	// 端口越界 → 422。
	code, resp = doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/inbound",
		testInboundTemplateBody("坏端口", "bad-port", 0))
	if resp["code"] != float64(422) || !strings.Contains(msgOf(resp), "端口") {
		t.Fatalf("端口 0 应 422，实际 code=%d resp=%#v", code, resp)
	}

	// 未知 kind → 404。
	code, resp = doJSON(t, engine, http.MethodPost, "/admin-api/xray-templates/warp", `{"name": "x"}`)
	if resp["code"] != float64(404) {
		t.Fatalf("未知 kind 应 404，实际 code=%d resp=%#v", code, resp)
	}

	seedTemplates(t, engine)

	// 列表：三类都在，字段齐全。
	code, resp = doJSON(t, engine, http.MethodGet, "/admin-api/xray-templates", "")
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("列表失败: code=%d resp=%#v", code, resp)
	}
	data := dataOf(t, resp)
	for _, key := range []string{"inbounds", "outbounds", "routing"} {
		list, ok := data[key].([]any)
		if !ok || len(list) == 0 {
			t.Fatalf("列表 %s 为空或类型不对: %#v", key, data[key])
		}
	}
	inbounds := data["inbounds"].([]any)
	first := inbounds[0].(map[string]any)
	if first["tag"] != "tmpl-vless-1" || first["port"] != float64(443) || first["enabled"] != true {
		t.Fatalf("入站模板首行字段不对: %#v", first)
	}
	stream, ok := first["stream_settings"].(map[string]any)
	if !ok || stream["security"] != "reality" {
		t.Fatalf("入站模板 stream_settings 应回显为对象: %#v", first["stream_settings"])
	}

	// 修改 + 删除。
	id := first["id"].(float64)
	code, resp = doJSON(t, engine, http.MethodPut,
		fmt.Sprintf("/admin-api/xray-templates/inbound/%d", int64(id)),
		testInboundTemplateBody("改名后的模板", "tmpl-vless-1", 8443))
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("修改入站模板失败: code=%d resp=%#v", code, resp)
	}
	updated := dataOf(t, resp)
	if updated["name"] != "改名后的模板" || updated["port"] != float64(8443) {
		t.Fatalf("修改后字段不对: %#v", updated)
	}

	code, resp = doJSON(t, engine, http.MethodDelete,
		fmt.Sprintf("/admin-api/xray-templates/inbound/%d", int64(id)), "")
	if code != http.StatusOK || resp["code"] != float64(0) {
		t.Fatalf("删除入站模板失败: code=%d resp=%#v", code, resp)
	}
	code, resp = doJSON(t, engine, http.MethodDelete,
		fmt.Sprintf("/admin-api/xray-templates/inbound/%d", int64(id)), "")
	if resp["code"] != float64(404) {
		t.Fatalf("重复删除应 404，实际 code=%d resp=%#v", code, resp)
	}
	code, resp = doJSON(t, engine, http.MethodPut, "/admin-api/xray-templates/inbound/999999",
		testInboundTemplateBody("不存在", "nope", 443))
	if resp["code"] != float64(404) {
		t.Fatalf("改不存在的模板应 404，实际 code=%d resp=%#v", code, resp)
	}
}

// countRows 跑一个 COUNT 查询。
func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("计数失败（%s）: %v", query, err)
	}
	return n
}
