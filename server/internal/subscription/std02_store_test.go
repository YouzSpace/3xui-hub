package subscription

// std02_store_test.go 是订阅链路的数据层集成测试（真库，生产 schema）。
//
// 重点覆盖规范化-02 场景 2（用户绑定）：**有绑定的用户绝不能拿到别人的节点链接**。
// 之前 region03 用例只验到「查询返回了哪些节点 id」，这里再往前一步 ——
// 走完整条 Generate 链路，断言「订阅正文里出现的是哪个域名」，
// 因为用户抱怨的形态永远是「订阅里少了/多了节点」，不是「SQL 返回几行」。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// std02SubAdminDSN 返回管理连接 DSN（TEST_DSN 可覆盖，缺省本机 root 空密码）。
func std02SubAdminDSN() string {
	if dsn := os.Getenv("TEST_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// std02SubSchemaStatements 读生产 schema 并按语句切开。
func std02SubSchemaStatements(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("拿不到测试文件路径")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "schema", "schema.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读生产 schema 失败（%s）: %v", path, err)
	}
	var out []string
	var cur []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur = append(cur, line)
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSuffix(strings.TrimSpace(strings.Join(cur, "\n")), ";")
			if stmt != "" {
				out = append(out, stmt)
			}
			cur = nil
		}
	}
	if len(out) < 40 {
		t.Fatalf("schema.sql 只解析出 %d 条语句", len(out))
	}
	return out
}

// newStd02SubDB 建一个干净的 ch_std02_sub_* 测试库（生产 schema + ensureSchema 补列）。
func newStd02SubDB(t *testing.T, caseName string) (*sql.DB, string) {
	t.Helper()

	adminDSN := std02SubAdminDSN()
	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过订阅集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过订阅集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	schema := fmt.Sprintf("ch_std02_sub_%s_%d", caseName, os.Getpid())
	if len(schema) > 64 {
		schema = schema[:64]
	}
	for _, q := range []string{
		"DROP DATABASE IF EXISTS `" + schema + "`",
		"CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci",
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("准备测试库失败（%q）: %v", q, err)
		}
	}
	t.Cleanup(func() {
		a, err := sql.Open("mysql", adminDSN)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	slash := strings.LastIndex(adminDSN, "/")
	q := strings.Index(adminDSN, "?")
	dbDSN := adminDSN
	if slash >= 0 {
		tail := "?charset=utf8mb4&parseTime=true&loc=UTC&timeout=5s"
		if q > slash {
			tail = adminDSN[q:]
			if !strings.Contains(tail, "parseTime") {
				tail += "&parseTime=true&loc=UTC"
			}
		}
		dbDSN = adminDSN[:slash+1] + schema + tail
	}
	db, err := sql.Open("mysql", dbDSN)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(6)
	t.Cleanup(func() { db.Close() })

	for _, stmt := range std02SubSchemaStatements(t) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建表失败: %v\n语句: %s", err, stmt)
		}
	}
	// ensureSchema 在启动时补的列/表（schema.sql 是全新安装形态，没有 user_nodes 与这两列）。
	for _, ddl := range []string{
		"ALTER TABLE plans ADD COLUMN region_ids json DEFAULT NULL",
		"ALTER TABLE nodes ADD COLUMN region_id bigint unsigned DEFAULT NULL",
		`CREATE TABLE user_nodes (
			user_id bigint unsigned NOT NULL,
			node_id bigint unsigned NOT NULL,
			PRIMARY KEY (user_id, node_id),
			KEY idx_node (node_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("补列失败（%s）: %v", ddl, err)
		}
	}
	return db, schema
}

// subMustExec 插数据失败即 Fatal。
func subMustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("执行失败: %v\nSQL: %s", err, query)
	}
}

// addXrayNode 插一台「可订阅」的 xray 节点（本地拼链，不需要面板 HTTP）。
func addXrayNode(t *testing.T, db *sql.DB, id int64, name, host string, sort int64) {
	t.Helper()
	subMustExec(t, db, `INSERT INTO nodes (id, name, host, port, scheme, web_base_path, username, enabled, driver_type, verify_ssl, status)
		VALUES (?, ?, ?, 443, 'https', '', 'admin', 1, 'xray', 0, 'online')`, id, name, host)
	subMustExec(t, db, `INSERT INTO xray_inbounds (node_id, tag, remark, protocol, port, listen, settings, stream_settings, enabled, sort)
		VALUES (?, ?, ?, 'vless', 443, '0.0.0.0', ?, ?, 1, ?)`,
		id, "inbound-"+name, name,
		`{"clients":[],"decryption":"none"}`,
		`{"network":"tcp","security":"tls","tlsSettings":{"serverName":"`+host+`"}}`,
		sort)
}

// ─────────── 场景 2：绑定过滤（走完整 Generate 链路） ───────────

// TestStd02SubBoundUserOnlySeesOwnNode：有绑定的用户订阅里只有自己的节点域名。
//
// 变异靶子：把 SubscribableNodes 的 user_nodes 过滤条件去掉 → 两个用户的订阅会互相串节点，本用例红。
func TestStd02SubBoundUserOnlySeesOwnNode(t *testing.T) {
	db, _ := newStd02SubDB(t, "bound")
	ctx := context.Background()

	subMustExec(t, db, `INSERT INTO plans (id, name, price, reset_price, type, months, monthly_traffic, period_traffic, total_traffic, is_active, include_local, created_at, updated_at)
		VALUES (1,'月付',10.00,0.00,'period',1,10737418240,0,0,1,1,NOW(),NOW())`)
	for _, u := range []struct {
		id    int64
		token string
		uuid  string
	}{{1, "tok-user1", "11111111-1111-1111-1111-111111111111"}, {2, "tok-user2", "22222222-2222-2222-2222-222222222222"}} {
		subMustExec(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
			monthly_traffic_used, monthly_traffic_limit, expired_at, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'vless', 1, 10737418240, 0, 0, 10737418240, NULL, 1, NOW(), NOW())`,
			u.id, fmt.Sprintf("u%d@example.com", u.id), u.token, u.uuid)
	}
	addXrayNode(t, db, 10, "日本节点", "jp.example.com", 1)
	addXrayNode(t, db, 20, "美国节点", "us.example.com", 2)

	// 用户 1 只绑日本节点；用户 2 没有任何绑定（老用户行为不变）。
	subMustExec(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (1, 10)")

	repo := NewSQLRepository(db, nil)
	svc := NewService(repo)
	svc.Now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }
	svc.Logf = func(string, ...any) {}

	for _, c := range []struct {
		token      string
		wantHost   string
		bannedHost string
	}{
		{"tok-user1", "jp.example.com", "us.example.com"},
		{"tok-user2", "us.example.com", ""},
	} {
		user, err := repo.UserByToken(ctx, c.token)
		if err != nil || user == nil {
			t.Fatalf("查用户 %s 失败: %v", c.token, err)
		}
		body, err := svc.Generate(ctx, user, FormatClash)
		if err != nil {
			t.Fatalf("%s 生成订阅失败: %v", c.token, err)
		}
		if !strings.Contains(body, c.wantHost) {
			t.Errorf("%s 的订阅里没有自己的节点 %s:\n%s", c.token, c.wantHost, body)
		}
		if c.bannedHost != "" && strings.Contains(body, c.bannedHost) {
			t.Errorf("%s 的订阅里出现了别人的节点 %s（绑定过滤失效）:\n%s", c.token, c.bannedHost, body)
		}
	}

	// 用户 2 无绑定 → 老行为：两台都可见。
	user2, _ := repo.UserByToken(ctx, "tok-user2")
	body2, err := svc.Generate(ctx, user2, FormatClash)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body2, "jp.example.com") {
		t.Error("无绑定用户应能看到全部节点（升级后老行为不变）")
	}
}

// TestStd02SubOfflineNodeExcluded：离线节点不进订阅（用户拿到只会连不上）。
func TestStd02SubOfflineNodeExcluded(t *testing.T) {
	db, _ := newStd02SubDB(t, "offline")
	ctx := context.Background()

	subMustExec(t, db, `INSERT INTO plans (id, name, price, reset_price, type, months, monthly_traffic, period_traffic, total_traffic, is_active, include_local, created_at, updated_at)
		VALUES (1,'月付',10.00,0.00,'period',1,10737418240,0,0,1,1,NOW(),NOW())`)
	subMustExec(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		monthly_traffic_used, monthly_traffic_limit, expired_at, enabled, created_at, updated_at)
		VALUES (1, 'u1@example.com', 'tok1', '11111111-1111-1111-1111-111111111111', 'vless', 1, 10737418240, 0, 0, 10737418240, NULL, 1, NOW(), NOW())`)
	addXrayNode(t, db, 10, "在线节点", "online.example.com", 1)
	addXrayNode(t, db, 20, "离线节点", "offline.example.com", 2)
	subMustExec(t, db, "UPDATE nodes SET status = 'offline' WHERE id = 20")

	repo := NewSQLRepository(db, nil)
	nodes, err := repo.SubscribableNodes(ctx, "vless", 1)
	if err != nil {
		t.Fatalf("查询可订阅节点失败: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Host != "online.example.com" {
		t.Fatalf("可订阅节点 = %v，期望只有在线节点", nodeHosts(nodes))
	}
}

// nodeHosts 便于失败时看清实际值。
func nodeHosts(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Host)
	}
	return out
}

// ─────────── 数据层其余读路径 ───────────

// TestStd02SubStoreReadPaths：site_configs / plans / third_party_subs / 诊断查询 / 入站读取。
func TestStd02SubStoreReadPaths(t *testing.T) {
	db, _ := newStd02SubDB(t, "store")
	ctx := context.Background()
	repo := NewSQLRepository(db, nil)

	// SiteConfigs：一次性读全表。
	subMustExec(t, db, "INSERT INTO site_configs (`key`, `value`) VALUES ('sub_clash_enabled', '1'), ('sub_title', 'MySub')")
	cfgs, err := repo.SiteConfigs(ctx)
	if err != nil {
		t.Fatalf("SiteConfigs 失败: %v", err)
	}
	if cfgs["sub_clash_enabled"] != "1" || cfgs["sub_title"] != "MySub" {
		t.Errorf("site_configs 读取不完整: %v", cfgs)
	}

	// PlanByID：include_local 缺省 true；third_party_sub_ids 解析成 id 列表（丢 0）。
	subMustExec(t, db, `INSERT INTO plans (id, name, price, reset_price, type, months, monthly_traffic, period_traffic, total_traffic, is_active, include_local, third_party_sub_ids, created_at, updated_at)
		VALUES (1,'含本地',10.00,0.00,'period',3,10737418240,0,0,1,1,'[5,0,7]',NOW(),NOW())`)
	subMustExec(t, db, `INSERT INTO plans (id, name, price, reset_price, type, months, monthly_traffic, period_traffic, total_traffic, is_active, created_at, updated_at)
		VALUES (2,'NULL本地',10.00,0.00,'total',0,0,10737418240,10737418240,1,NOW(),NOW())`)
	plan, err := repo.PlanByID(ctx, 1)
	if err != nil || plan == nil {
		t.Fatalf("PlanByID 失败: %v", err)
	}
	if !plan.IncludeLocal || plan.Months != 3 || plan.Type != "period" {
		t.Errorf("套餐字段读错: %+v", plan)
	}
	if len(plan.ThirdPartySubIDs) != 2 || plan.ThirdPartySubIDs[0] != 5 || plan.ThirdPartySubIDs[1] != 7 {
		t.Errorf("third_party_sub_ids 解析错: %v", plan.ThirdPartySubIDs)
	}
	plan2, err := repo.PlanByID(ctx, 2)
	if err != nil || plan2 == nil {
		t.Fatalf("PlanByID(2) 失败: %v", err)
	}
	if !plan2.IncludeLocal {
		t.Error("include_local 为 NULL 时应默认 true（老套餐行为）")
	}
	if p, err := repo.PlanByID(ctx, 999); err != nil || p != nil {
		t.Errorf("不存在的套餐应返回 (nil,nil)，实际 %v/%v", p, err)
	}

	// ThirdPartySubs：只返回启用的，按 id 升序。
	subMustExec(t, db, "INSERT INTO third_party_subs (id, name, url, enabled, cached_links) VALUES (7,'a','https://a.example.com',1,'vless://a@a.example.com:443#a')")
	subMustExec(t, db, "INSERT INTO third_party_subs (id, name, url, enabled, cached_links) VALUES (5,'b','https://b.example.com',1,'vless://b@b.example.com:443#b')")
	subMustExec(t, db, "INSERT INTO third_party_subs (id, name, url, enabled) VALUES (9,'c','https://c.example.com',0)")
	subs, err := repo.ThirdPartySubs(ctx, []int64{9, 7, 5})
	if err != nil {
		t.Fatalf("ThirdPartySubs 失败: %v", err)
	}
	if len(subs) != 2 || subs[0].ID != 5 || subs[1].ID != 7 {
		t.Fatalf("第三方订阅 = %+v，期望启用且按 id 升序的 5/7", subs)
	}
	if got, err := repo.ThirdPartySubs(ctx, nil); err != nil || got != nil {
		t.Errorf("空 id 列表应返回 (nil,nil)，实际 %v/%v", got, err)
	}

	// 诊断查询：xray 节点在线但 host 为空。
	if blank, err := repo.HasEnabledOnlineXrayNodeWithoutHost(ctx); err != nil || blank {
		t.Errorf("没有空白节点时诊断应为 false，实际 %v/%v", blank, err)
	}
	subMustExec(t, db, `INSERT INTO nodes (id, name, host, port, enabled, driver_type, status) VALUES (30,'没地址','',443,1,'xray','online')`)
	if blank, err := repo.HasEnabledOnlineXrayNodeWithoutHost(ctx); err != nil || !blank {
		t.Errorf("存在空白 xray 节点时诊断应为 true，实际 %v/%v", blank, err)
	}

	// XrayInbounds：按 sort,id 升序，跳过 disabled。
	subMustExec(t, db, `INSERT INTO xray_inbounds (node_id, tag, protocol, port, listen, enabled, sort) VALUES
		(30,'b','vless',8443,'0.0.0.0',1,2),
		(30,'a','vless',443,'0.0.0.0',1,1),
		(30,'off','vless',9443,'0.0.0.0',0,0)`)
	inbounds, err := repo.XrayInbounds(ctx, 30)
	if err != nil {
		t.Fatalf("XrayInbounds 失败: %v", err)
	}
	if len(inbounds) != 2 || inbounds[0].Tag != "a" || inbounds[1].Tag != "b" {
		t.Fatalf("入站顺序/过滤不对: %+v", inbounds)
	}
	if inbounds[0].Listen != "0.0.0.0" {
		t.Errorf("listen 读取错: %q", inbounds[0].Listen)
	}

	// NodeInboundIDs：按 id 升序（3x-ui 节点）。
	subMustExec(t, db, "INSERT INTO node_inbounds (node_id, protocol, inbound_id) VALUES (30,'vless',3),(30,'vless',1)")
	ids, err := repo.NodeInboundIDs(ctx, 30, "vless")
	if err != nil {
		t.Fatalf("NodeInboundIDs 失败: %v", err)
	}
	if len(ids) != 2 || ids[0] != 3 || ids[1] != 1 {
		t.Fatalf("入站 id = %v，期望按插入顺序 3/1", ids)
	}

	// 用户不存在 → (nil,nil)，不是错误。
	if u, err := repo.UserByToken(ctx, "不存在的token"); err != nil || u != nil {
		t.Errorf("不存在的 token 应返回 (nil,nil)，实际 %v/%v", u, err)
	}
}
