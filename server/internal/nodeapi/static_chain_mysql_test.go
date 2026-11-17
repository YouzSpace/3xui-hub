package nodeapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"controlhub-server/internal/crypto"
)

// ── 静态 IP 专属出站：实连 MySQL 跑 LoadRenderData → RenderJSON ──────────
//
// 独立库 ch_region32_chain_*（自建自删），不碰本机任何已有库。
// 覆盖：正常静态用户 / 账号过期 / 绑在别的节点 / 超过 STATIC_CHAIN_MAX_USERS。

// region32TestDSN 连本机 MySQL（root/空密码），可用 REGION32_MYSQL_DSN 覆盖。
func region32TestDSN() string {
	if dsn := os.Getenv("REGION32_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// region32WithSchema 把 DSN 的默认库换成 schema，参数与生产 config.DBConfig.DSN() 一致
// （parseTime=true&loc=UTC，否则 datetime 扫不进 time.Time）。
func region32WithSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	return dsn[:slash+1] + schema + "?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=UTC&timeout=3s"
}

// region32DDL 只建本次链路真正读到的表（列类型照 server/schema/schema.sql）。
const region32DDL = `
CREATE TABLE users (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  uuid char(36) COLLATE utf8mb4_unicode_ci NOT NULL,
  protocol varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'vless',
  plan_id bigint unsigned DEFAULT NULL,
  traffic_limit bigint NOT NULL DEFAULT '0',
  expired_at datetime DEFAULT NULL,
  enabled tinyint(1) NOT NULL DEFAULT '1',
  traffic_disabled_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE user_nodes (
  user_id bigint unsigned NOT NULL,
  node_id bigint unsigned NOT NULL,
  PRIMARY KEY (user_id, node_id),
  KEY idx_node (node_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE plans (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  exit_type varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'direct',
  exit_profile_id bigint unsigned DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE exit_profiles (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  provider_id bigint unsigned NOT NULL DEFAULT '0',
  socks_host varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  socks_port int NOT NULL DEFAULT '1080',
  enabled tinyint NOT NULL DEFAULT '1',
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE proxy_accounts (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  provider_id bigint unsigned NOT NULL DEFAULT '0',
  upstream_id varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  user_id bigint unsigned DEFAULT NULL,
  label varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  username varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  password text COLLATE utf8mb4_unicode_ci,
  expires_at timestamp NULL DEFAULT NULL,
  status varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active',
  PRIMARY KEY (id),
  KEY proxy_accounts_user_id_status_index (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_inbounds (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  remark varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
  port int unsigned NOT NULL,
  listen varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '0.0.0.0',
  settings json DEFAULT NULL,
  stream_settings json DEFAULT NULL,
  sniffing json DEFAULT NULL,
  reality_private_key text COLLATE utf8mb4_unicode_ci,
  salamander_password text COLLATE utf8mb4_unicode_ci,
  enabled tinyint(1) NOT NULL DEFAULT '1',
  sort int unsigned NOT NULL DEFAULT '0',
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_outbounds (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
  settings json DEFAULT NULL,
  stream_settings json DEFAULT NULL,
  secret_key text COLLATE utf8mb4_unicode_ci,
  enabled tinyint(1) NOT NULL DEFAULT '1',
  sort int unsigned NOT NULL DEFAULT '0',
  remark varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_balancers (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  selectors json NOT NULL,
  strategy varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'roundRobin',
  fallback_tag varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  enabled tinyint(1) NOT NULL DEFAULT '1',
  sort int unsigned NOT NULL DEFAULT '0',
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_routing_rules (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  domains json DEFAULT NULL,
  ips json DEFAULT NULL,
  port varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  network varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  protocol varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  inbound_tag varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  outbound_tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  enabled tinyint(1) NOT NULL DEFAULT '1',
  sort int unsigned NOT NULL DEFAULT '0',
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_outbound_subscriptions (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  prepend tinyint(1) NOT NULL DEFAULT '0',
  fetched json DEFAULT NULL,
  enabled tinyint(1) NOT NULL DEFAULT '1',
  priority int unsigned NOT NULL DEFAULT '0',
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_relay_clients (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  node_id bigint unsigned NOT NULL,
  inbound_id bigint unsigned NOT NULL,
  email varchar(128) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  client_id varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  source_node_id bigint unsigned DEFAULT NULL,
  source_inbound_id bigint unsigned DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE xray_certificates (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  cert_pem longtext COLLATE utf8mb4_unicode_ci,
  key_enc text COLLATE utf8mb4_unicode_ci,
  updated_at timestamp NULL DEFAULT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

// region32Seq 让同一进程里多次调用拿到不同库名（同一次 go test 会跑多个用例）。
var region32Seq int64

// newRegion32Repo 建干净测试库 + 全套表，返回 Repo；t.Cleanup 负责 DROP。
func newRegion32Repo(t *testing.T) *Repo {
	t.Helper()

	// 库名带 pid + 序号：多个 lane / 多次运行可能同时跑，固定库名会互相 DROP。
	schema := fmt.Sprintf("ch_region32_chain_%d_%d", os.Getpid(), atomic.AddInt64(&region32Seq, 1))

	admin, err := sql.Open("mysql", region32TestDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过静态 IP 出站集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过静态 IP 出站集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", region32TestDSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region32WithSchema(region32TestDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range strings.Split(region32DDL, ";\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, stmt)
		}
	}
	return NewRepo(db)
}

// region32Node 造一个只有 ID 的节点（LoadRenderData 只需要 ID/名称/地址）。
func region32Node(id int64) Node {
	return Node{ID: id, Name: fmt.Sprintf("node%d", id), Host: fmt.Sprintf("node%d.example.com", id), DriverType: "xray"}
}

// seedRegion32 准备一份同时覆盖「正常静态用户 / 账号过期 / 绑在别的节点」的数据。
//
//	plans:           1=direct, 2=static_chain(→ profile 10)
//	exit_profiles:   10 = p.webshare.io:1080，enabled
//	users:           1/2 普通（plan 1）、3 静态、4 静态（账号已过期）、5 静态（绑 node2）
//	user_nodes:      1,2,3,4 → node1；5 → node2
//	proxy_accounts:  3/4/5 各一条，只有 3 是「未过期 + active」
//
// 返回静态用户 3 的明文密码（库里存的是密文）。
func seedRegion32(t *testing.T, r *Repo) string {
	t.Helper()

	mustExec := func(sqlText string, args ...any) {
		t.Helper()
		if _, err := r.DB.Exec(sqlText, args...); err != nil {
			t.Fatalf("种子 SQL 失败: %v\nSQL: %s", err, sqlText)
		}
	}

	mustExec("INSERT INTO plans (id, name, exit_type, exit_profile_id) VALUES (1,'直连','direct',NULL),(2,'静态IP','static_chain',10)")
	mustExec("INSERT INTO exit_profiles (id, name, provider_id, socks_host, socks_port, enabled) VALUES (10,'美国静态','7','p.webshare.io',1080,1)")

	mustExec("INSERT INTO users (id, uuid, protocol, plan_id, traffic_limit, enabled) VALUES " +
		"(1,'00000000-0000-0000-0000-000000000001','vless',1,107374182400,1)," +
		"(2,'00000000-0000-0000-0000-000000000002','vless',1,107374182400,1)," +
		"(3,'00000000-0000-0000-0000-000000000003','vless',2,107374182400,1)," +
		"(4,'00000000-0000-0000-0000-000000000004','vless',2,107374182400,1)," +
		"(5,'00000000-0000-0000-0000-000000000005','vless',2,107374182400,1)")

	mustExec("INSERT INTO user_nodes (user_id, node_id) VALUES (1,1),(2,1),(3,1),(4,1),(5,2)")

	// 静态用户的密码按生产口径加密存（crypto.EncryptString = Laravel encrypted cast）。
	plain := "p3-s3cret"
	enc, err := crypto.EncryptString(plain, renderFixtureKey)
	if err != nil {
		t.Fatalf("加密测试密码失败: %v", err)
	}
	mustExec("INSERT INTO proxy_accounts (id, upstream_id, user_id, username, password, expires_at, status) VALUES "+
		"(31,'up-3',3,'u3-chain',?, NOW() + INTERVAL 30 DAY,'active'),"+ // 正常
		"(41,'up-4',4,'u4-chain',?, NOW() - INTERVAL 1 DAY,'active'),"+ // 账号过期
		"(51,'up-5',5,'u5-chain',?, NOW() + INTERVAL 30 DAY,'active')", // 绑在 node2
		enc, enc, enc)

	// 一个入站 + 一个管理员配的 socks 出站 + 一条自定义路由（验证「原有内容不变、动态内容追加」）。
	mustExec("INSERT INTO xray_inbounds (id, node_id, tag, protocol, port, listen, settings, stream_settings) VALUES " +
		"(11,1,'in-443','vless',443,'0.0.0.0','{\"decryption\":\"none\",\"clients\":[]}','{\"network\":\"tcp\",\"security\":\"none\"}')")
	mustExec("INSERT INTO xray_outbounds (id, node_id, tag, protocol, settings, enabled, sort) VALUES " +
		"(21,1,'proxy-a','socks','{\"servers\":[{\"address\":\"10.0.0.2\",\"port\":1080}]}',1,0)")
	mustExec("INSERT INTO xray_routing_rules (id, node_id, domains, outbound_tag, enabled, sort) VALUES " +
		"(31,1,'[\"example.com\"]','proxy-a',1,0)")

	return plain
}

// renderNode 跑一遍 LoadRenderData + RenderJSON，并把结果解析成通用 JSON（数组保序）。
func renderNode(t *testing.T, r *Repo, nodeID int64) map[string]any {
	t.Helper()
	data, err := r.LoadRenderData(context.Background(), region32Node(nodeID))
	if err != nil {
		t.Fatalf("LoadRenderData(node=%d) 出错: %v", nodeID, err)
	}
	body, err := RenderJSON(data, renderFixtureKey)
	if err != nil {
		t.Fatalf("RenderJSON(node=%d) 出错: %v", nodeID, err)
	}
	t.Logf("节点 #%d 渲染结果（%d 字节）", nodeID, len(body))
	if len(body) < 4096 {
		// 小配置直接把原始字节打出来（顺序/字段名都在里面，报告里要贴真货）。
		t.Logf("原始渲染: %s", body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("解析渲染结果失败: %v", err)
	}
	return doc
}

// findOutbound 按 tag 找一条出站。
func findOutbound(doc map[string]any, tag string) map[string]any {
	outbounds, _ := doc["outbounds"].([]any)
	for _, ob := range outbounds {
		obj, ok := ob.(map[string]any)
		if !ok {
			continue
		}
		if obj["tag"] == tag {
			return obj
		}
	}
	return nil
}

func outboundTags(doc map[string]any) []string {
	outbounds, _ := doc["outbounds"].([]any)
	tags := make([]string, 0, len(outbounds))
	for _, ob := range outbounds {
		if obj, ok := ob.(map[string]any); ok {
			tags = append(tags, fmt.Sprintf("%v", obj["tag"]))
		}
	}
	return tags
}

func routingRules(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	routing, ok := doc["routing"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果没有 routing: %v", doc)
	}
	raw, _ := routing["rules"].([]any)
	rules := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if obj, ok := r.(map[string]any); ok {
			rules = append(rules, obj)
		}
	}
	return rules
}

// TestStaticChainRender 覆盖「正常 / 账号过期 / 绑在别的节点」三种静态用户。
func TestStaticChainRender(t *testing.T) {
	r := newRegion32Repo(t)
	chainPass := seedRegion32(t, r)

	doc := renderNode(t, r, 1)

	t.Run("静态用户生成专属 socks 出站_追加在管理员出站之后", func(t *testing.T) {
		tags := outboundTags(doc)
		t.Logf("outbounds tags = %v", tags)

		ob := findOutbound(doc, "ch-chain-3")
		if ob == nil {
			t.Fatalf("outbounds 里没有 ch-chain-3，实际 tags=%v", tags)
		}
		frag, _ := json.Marshal(ob)
		t.Logf("ch-chain-3 = %s", frag)

		if ob["protocol"] != "socks" {
			t.Fatalf("ch-chain-3 protocol 期望 socks，实得 %v", ob["protocol"])
		}
		settings, _ := ob["settings"].(map[string]any)
		servers, _ := settings["servers"].([]any)
		if len(servers) != 1 {
			t.Fatalf("ch-chain-3 settings.servers 期望 1 条，实得 %v", servers)
		}
		srv, _ := servers[0].(map[string]any)
		if srv["address"] != "p.webshare.io" || srv["port"] != float64(1080) {
			t.Fatalf("ch-chain-3 出口地址错: %v", srv)
		}
		users, _ := srv["users"].([]any)
		if len(users) != 1 {
			t.Fatalf("ch-chain-3 users 期望 1 条，实得 %v", users)
		}
		u, _ := users[0].(map[string]any)
		if u["user"] != "u3-chain" || u["pass"] != chainPass {
			t.Fatalf("ch-chain-3 凭证错（密码应为解密后的明文）: %v", u)
		}

		// 管理员配的出站照旧，动态出站在它后面。
		if tags[0] != "direct" || tags[1] != "blocked" {
			t.Fatalf("系统出站顺序被改动: %v", tags)
		}
		idxA, idxChain := -1, -1
		for i, tag := range tags {
			if tag == "proxy-a" {
				idxA = i
			}
			if tag == "ch-chain-3" {
				idxChain = i
			}
		}
		if idxA < 0 || idxChain < 0 || idxChain < idxA {
			t.Fatalf("动态出站应追加在管理员出站之后: proxy-a@%d, ch-chain-3@%d，tags=%v", idxA, idxChain, tags)
		}
		if len(tags) != 4 {
			t.Fatalf("期望 4 条出站（direct/blocked/proxy-a/ch-chain-3），实得 %v", tags)
		}
	})

	t.Run("路由第一条是该用户的 user 规则", func(t *testing.T) {
		rules := routingRules(t, doc)
		first, _ := json.Marshal(rules[0])
		t.Logf("routing.rules[0] = %s", first)
		t.Logf("routing.rules 共 %d 条", len(rules))

		if rules[0]["type"] != "field" {
			t.Fatalf("第一条规则 type 期望 field: %s", first)
		}
		users, ok := rules[0]["user"].([]any)
		if !ok || len(users) != 1 || users[0] != "ch_user_3" {
			t.Fatalf("第一条规则 user 期望 [ch_user_3]: %s", first)
		}
		if rules[0]["outboundTag"] != "ch-chain-3" {
			t.Fatalf("第一条规则 outboundTag 期望 ch-chain-3: %s", first)
		}
		// 内置规则与管理员规则都必须排在它后面（否则静态用户的流量会先被吃掉）。
		if rules[1]["ip"] == nil || rules[1]["outboundTag"] != "blocked" {
			t.Fatalf("第 2 条应是内置 geoip:private → blocked: %v", rules[1])
		}
		for i, rule := range rules[1:] {
			if rule["outboundTag"] == "ch-chain-3" {
				t.Fatalf("ch-chain-3 的规则出现了不止一次（第 %d 条之后又出现）", i+1)
			}
		}
	})

	t.Run("账号已过期的静态用户不生成出站和路由", func(t *testing.T) {
		if ob := findOutbound(doc, "ch-chain-4"); ob != nil {
			t.Fatalf("账号过期的用户 4 不应生成出站: %v", ob)
		}
		for _, rule := range routingRules(t, doc) {
			users, _ := rule["user"].([]any)
			for _, u := range users {
				if u == "ch_user_4" {
					t.Fatalf("账号过期的用户 4 不应生成路由: %v", rule)
				}
			}
		}
	})

	t.Run("绑在别的节点的静态用户不出现在本节点", func(t *testing.T) {
		if ob := findOutbound(doc, "ch-chain-5"); ob != nil {
			t.Fatalf("用户 5 绑在 node2，node1 不应出现他的出站: %v", ob)
		}

		// 正向对照：node2 上他必须出现（证明不是「一起被漏掉」）。
		doc2 := renderNode(t, r, 2)
		ob := findOutbound(doc2, "ch-chain-5")
		if ob == nil {
			t.Fatalf("用户 5 绑在 node2，node2 的 outbounds 里应有 ch-chain-5: %v", outboundTags(doc2))
		}
		frag, _ := json.Marshal(ob)
		t.Logf("node2 的 ch-chain-5 = %s", frag)
	})
}

// TestStaticChainMissingSchemaTolerated 验证「静态 IP 的表还没建好」时的降级：
// 静态 IP 依赖 plans/proxy_accounts/exit_profiles，这些表由其它模块的迁移建。
// 缺表时不能把整份节点配置下发搞挂 —— 必须当作「没有静态用户」继续下发普通配置。
func TestStaticChainMissingSchemaTolerated(t *testing.T) {
	r := newRegion32Repo(t)

	// 把静态 IP 依赖的三张表删掉（模拟这些迁移还没落地的老库）。
	for _, tbl := range []string{"exit_profiles", "plans", "proxy_accounts"} {
		if _, err := r.DB.Exec("DROP TABLE `" + tbl + "`"); err != nil {
			t.Fatalf("删表 %s 失败: %v", tbl, err)
		}
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	chains, err := r.staticChainUsers(context.Background(), 1)
	if err != nil {
		t.Fatalf("缺表时不应返回错误（否则整个节点的配置下发都会失败）: %v", err)
	}
	if len(chains) != 0 {
		t.Fatalf("缺表时应返回空列表，实得 %d 条", len(chains))
	}
	logs := buf.String()
	t.Logf("日志: %s", strings.TrimSpace(logs))
	if !strings.Contains(logs, "表/列还不存在") {
		t.Fatalf("缺表应打一次告警，实际日志: %q", logs)
	}

	// 渲染也要能照常产出（普通用户的 outbounds 不受影响）。
	data, err := r.LoadRenderData(context.Background(), region32Node(1))
	if err != nil {
		t.Fatalf("缺表时 LoadRenderData 不应失败: %v", err)
	}
	if _, err := RenderJSON(data, renderFixtureKey); err != nil {
		t.Fatalf("缺表时 RenderJSON 不应失败: %v", err)
	}
}

// TestStaticChainMaxUsers 验证 STATIC_CHAIN_MAX_USERS 的上限保护。
func TestStaticChainMaxUsers(t *testing.T) {
	r := newRegion32Repo(t)
	t.Setenv("STATIC_CHAIN_MAX_USERS", "500")

	// 600 个静态用户全绑在一个节点上（id 从 1000 起，避开其它用例的固定 id）。
	const users = 600
	const nodeID int64 = 900

	if _, err := r.DB.Exec("INSERT INTO plans (id, name, exit_type, exit_profile_id) VALUES (2,'静态IP','static_chain',10)"); err != nil {
		t.Fatalf("播种套餐失败: %v", err)
	}
	if _, err := r.DB.Exec("INSERT INTO exit_profiles (id, name, provider_id, socks_host, socks_port, enabled) VALUES (10,'美国静态','7','p.webshare.io',1080,1)"); err != nil {
		t.Fatalf("播种分类失败: %v", err)
	}

	var userVals, bindVals, accVals []string
	for i := 0; i < users; i++ {
		uid := 1000 + i
		userVals = append(userVals, fmt.Sprintf("(%d,'00000000-0000-0000-0000-%012d','vless',2,0,1)", uid, uid))
		bindVals = append(bindVals, fmt.Sprintf("(%d,%d)", uid, nodeID))
		accVals = append(accVals, fmt.Sprintf("(%d,'up-%d',%d,'u%d-chain','pw-%d',NOW() + INTERVAL 30 DAY,'active')",
			uid, uid, uid, uid, uid))
	}
	if _, err := r.DB.Exec("INSERT INTO users (id, uuid, protocol, plan_id, traffic_limit, enabled) VALUES " + strings.Join(userVals, ",")); err != nil {
		t.Fatalf("播种 600 个用户失败: %v", err)
	}
	if _, err := r.DB.Exec("INSERT INTO user_nodes (user_id, node_id) VALUES " + strings.Join(bindVals, ",")); err != nil {
		t.Fatalf("播种 600 条绑定失败: %v", err)
	}
	if _, err := r.DB.Exec("INSERT INTO proxy_accounts (id, upstream_id, user_id, username, password, expires_at, status) VALUES " +
		strings.Join(accVals, ",")); err != nil {
		t.Fatalf("播种 600 条代理账号失败: %v", err)
	}

	// 抓日志：超限必须有告警。
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	chains, err := r.staticChainUsers(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("staticChainUsers 出错: %v", err)
	}
	if len(chains) != 500 {
		t.Fatalf("超过上限应只取 500 条，实得 %d", len(chains))
	}
	// 「取前 500」必须是稳定的：按 user_id 升序 → 1000..1499。
	if chains[0].UserID != 1000 || chains[499].UserID != 1499 {
		t.Fatalf("取的不是稳定前 500 条：首=%d 末=%d", chains[0].UserID, chains[499].UserID)
	}

	logs := buf.String()
	t.Logf("日志: %s", strings.TrimSpace(logs))
	if !strings.Contains(logs, "静态用户超过上限 500") {
		t.Fatalf("超过上限应打告警日志，实际日志: %q", logs)
	}

	// 渲染侧同样只出现 500 条专属出站 / 500 条 user 路由。
	doc := renderNode(t, r, nodeID)
	n := 0
	for _, tag := range outboundTags(doc) {
		if strings.HasPrefix(tag, "ch-chain-") {
			n++
		}
	}
	if n != 500 {
		t.Fatalf("渲染结果里专属出站应为 500 条，实得 %d", n)
	}
	userRules := 0
	for _, rule := range routingRules(t, doc) {
		if rule["user"] != nil {
			userRules++
		}
	}
	if userRules != 500 {
		t.Fatalf("渲染结果里 user 路由应为 500 条，实得 %d", userRules)
	}
	t.Logf("600 个静态用户 → 专属出站 %d 条、user 路由 %d 条", n, userRules)
}
