package nodeapi

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ───────────────────── 地区分片：节点下发按绑定过滤（实连 MySQL） ─────────────────────
//
// 只验证 eligibleUsers 的过滤语义，不碰 render()/订阅/对账：
//   - 本节点在 user_nodes 里没有任何记录 → 全量（老部署升级后行为不变）
//   - 本节点有绑定记录 → 只返回绑到本节点、且仍满足渲染判据的用户
//   - plan_id 为空 / enabled=0 / traffic_disabled_at 非空的用户，绑定了也不返回
//
// 刻意不用 controlhub_test 做主战场（那里有真实业务数据会被断言干扰）；
// 这里自建 ch_region02_eligible 库，跑完 DROP，不污染本机任何已有库。

// region02TestDSN 连本机 MySQL（root/空密码），可用 REGION02_MYSQL_DSN 覆盖。
func region02TestDSN() string {
	if dsn := os.Getenv("REGION02_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3306)/?timeout=3s"
}

// region02WithSchema 把 DSN 的默认库换成 schema，并补齐生产同款参数
// （parseTime=true&loc=UTC 与 config.DBConfig.DSN() 一致，否则 datetime 扫不进 time.Time）。
func region02WithSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	return dsn[:slash+1] + schema + "?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=UTC&timeout=3s"
}

// region02DDL 只建本次断言用到的两张表（列类型照 server/schema/schema.sql 的 users）。
const region02DDL = `
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
`

// newRegion02Repo 建干净测试库 + 两张表，返回 Repo；t.Cleanup 负责 DROP。
func newRegion02Repo(t *testing.T) *Repo {
	t.Helper()

	// 库名带 pid：多个 lane/多次运行可能同时跑这个用例，
	// 固定库名会互相 DROP（实测会把对方的库在自己跑到一半时删掉，报 1049）。
	schema := fmt.Sprintf("ch_region02_eligible_%d", os.Getpid())

	admin, err := sql.Open("mysql", region02TestDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过节点下发过滤集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过节点下发过滤集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("mysql", region02TestDSN())
		if err != nil {
			return
		}
		defer admin2.Close()
		admin2.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", region02WithSchema(region02TestDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range strings.Split(region02DDL, ";\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("建表失败: %v\nSQL: %s", err, stmt)
		}
	}
	return NewRepo(db)
}

// seedRegion02Users 准备 6 个用户：u1/u2/u3 合格，u4 无套餐、u5 禁用、u6 流量禁用。
// u2 带 expired_at，用来顺带确认扫进 RenderUser 的时间列没被这次改动动到。
func seedRegion02Users(t *testing.T, r *Repo) {
	t.Helper()

	const ins = "INSERT INTO users (id, uuid, protocol, plan_id, traffic_limit, expired_at, enabled, traffic_disabled_at) VALUES "
	rows := []struct {
		id      int64
		planID  any
		expired any
		enabled int
		// trafficDisabled 为 true 时写 now()
		trafficDisabled bool
	}{
		{id: 1, planID: 1, expired: nil, enabled: 1},
		{id: 2, planID: 1, expired: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC), enabled: 1},
		{id: 3, planID: 2, expired: nil, enabled: 1},
		{id: 4, planID: nil, expired: nil, enabled: 1},                      // 无套餐
		{id: 5, planID: 1, expired: nil, enabled: 0},                        // 禁用
		{id: 6, planID: 1, expired: nil, enabled: 1, trafficDisabled: true}, // 流量禁用
	}
	var values []string
	for _, u := range rows {
		dis := "NULL"
		if u.trafficDisabled {
			dis = "NOW()"
		}
		values = append(values, fmt.Sprintf(
			"(%d, '%s', 'vless', %s, 107374182400, %s, %d, %s)",
			u.id, fmt.Sprintf("00000000-0000-0000-0000-00000000000%d", u.id),
			sqlLiteral(u.planID), sqlLiteral(u.expired), u.enabled, dis))
	}
	if _, err := r.DB.Exec(ins + strings.Join(values, ", ")); err != nil {
		t.Fatalf("插入测试用户失败: %v", err)
	}
}

// sqlLiteral 把 nil / int64 / time.Time 转成 SQL 字面量（测试数据量小，不走占位符更直观）。
func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case time.Time:
		return "'" + x.Format("2006-01-02 15:04:05") + "'"
	default:
		return "NULL"
	}
}

func bindRegion02(t *testing.T, r *Repo, nodeID int64, userIDs ...int64) {
	t.Helper()
	for _, uid := range userIDs {
		if _, err := r.DB.Exec("INSERT INTO user_nodes (user_id, node_id) VALUES (?, ?)", uid, nodeID); err != nil {
			t.Fatalf("插入绑定 (%d,%d) 失败: %v", uid, nodeID, err)
		}
	}
}

// eligibleIDs 跑一次 eligibleUsers 并把结果压成排序后的 id 列表。
func eligibleIDs(t *testing.T, r *Repo, node Node) []int64 {
	t.Helper()
	users, err := r.eligibleUsers(context.Background(), node)
	if err != nil {
		t.Fatalf("eligibleUsers(node=%d) 出错: %v", node.ID, err)
	}
	ids := make([]int64, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// nodeIn 构造一个节点：region=nil 表示未归属地区（全量下发），非空表示归属某地区（按绑定切片）。
func nodeIn(id int64, region *int64) Node {
	return Node{ID: id, RegionID: region}
}

func int64p(v int64) *int64 { return &v }

func assertIDs(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：得到 %v，期望 %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s：得到 %v，期望 %v", label, got, want)
		}
	}
}

func TestEligibleUsersRegionBinding(t *testing.T) {
	r := newRegion02Repo(t)
	seedRegion02Users(t, r)

	const (
		nodeUnbound    = 101 // 从未绑定、未归属地区
		nodeBound2     = 102 // 归属地区，绑 u1、u2
		nodeBoundMixed = 103 // 归属地区，绑 u1 + 三个不合格用户
		nodeA          = 104 // 归属地区，绑 u3
		nodeB          = 105 // 归属地区，但无绑定
		nodeMovedOut   = 106 // 曾归属地区并绑过 u1，现已移出地区（region=nil）但旧绑定仍在
	)
	bindRegion02(t, r, nodeBound2, 1, 2)
	bindRegion02(t, r, nodeBoundMixed, 1, 4, 5, 6)
	bindRegion02(t, r, nodeA, 3)
	bindRegion02(t, r, nodeMovedOut, 1) // 移出地区后旧绑定保留在库里

	// 兜底：种子没进去的话后面的断言会"整齐地"全红/全绿，先把行数钉死。
	var userCount, bindCount int
	if err := r.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&userCount); err != nil {
		t.Fatalf("统计 users 失败: %v", err)
	}
	if err := r.DB.QueryRow("SELECT COUNT(*) FROM user_nodes").Scan(&bindCount); err != nil {
		t.Fatalf("统计 user_nodes 失败: %v", err)
	}
	t.Logf("种子: users=%d user_nodes=%d", userCount, bindCount)
	if userCount != 6 || bindCount != 8 {
		t.Fatalf("种子数据不对：users=%d（期望 6）user_nodes=%d（期望 8）", userCount, bindCount)
	}

	t.Run("未归属地区=全量下发（不看它有没有绑定）", func(t *testing.T) {
		assertIDs(t, "从未绑定、未归属地区的节点", eligibleIDs(t, r, nodeIn(nodeUnbound, nil)), []int64{1, 2, 3})
	})

	t.Run("归属地区+有绑定=只返回这些绑定用户", func(t *testing.T) {
		assertIDs(t, "归属地区、绑 u1/u2 的节点", eligibleIDs(t, r, nodeIn(nodeBound2, int64p(1))), []int64{1, 2})

		// 顺带确认 RenderUser 的其余字段没被这次改动影响（u2 带 expired_at）。
		users, err := r.eligibleUsers(context.Background(), nodeIn(nodeBound2, int64p(1)))
		if err != nil {
			t.Fatalf("eligibleUsers 出错: %v", err)
		}
		for _, u := range users {
			if u.ID != 2 {
				continue
			}
			if u.UUID != "00000000-0000-0000-0000-000000000002" ||
				u.Protocol != "vless" || u.TrafficLimit != 107374182400 {
				t.Fatalf("u2 字段被改动: %+v", u)
			}
			if u.ExpiredAt == nil || !u.ExpiredAt.Equal(time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)) {
				t.Fatalf("u2 expired_at 期望 2027-01-02T03:04:05Z，实得 %v", u.ExpiredAt)
			}
		}
	})

	t.Run("无套餐/禁用/流量禁用即使绑定也不返回", func(t *testing.T) {
		assertIDs(t, "归属地区、绑 u1+u4+u5+u6 的节点", eligibleIDs(t, r, nodeIn(nodeBoundMixed, int64p(1))), []int64{1})
	})

	t.Run("A归属地区只返回自己的_B归属地区无绑定返回空", func(t *testing.T) {
		assertIDs(t, "节点A（归属地区，绑 u3）", eligibleIDs(t, r, nodeIn(nodeA, int64p(2))), []int64{3})
		// 已归属地区但没绑任何人 → 该地区没人可分到这台，返回空（分片本意）。
		assertIDs(t, "节点B（归属地区，无绑定）", eligibleIDs(t, r, nodeIn(nodeB, int64p(2))), []int64{})
	})

	t.Run("移出地区后旧绑定仍在，但仍是全量下发（回归：曾经回不到全量）", func(t *testing.T) {
		// 关键回归：这台节点库里有旧绑定(u1)，但 region=nil → 必须全量，且不因旧绑定被切片。
		assertIDs(t, "移出地区、仍留旧绑定的节点", eligibleIDs(t, r, nodeIn(nodeMovedOut, nil)), []int64{1, 2, 3})

		// 同时确认：移出地区不会删除旧绑定（保留原则）。
		var still int
		if err := r.DB.QueryRow("SELECT COUNT(*) FROM user_nodes WHERE node_id = ?", nodeMovedOut).Scan(&still); err != nil {
			t.Fatalf("统计移出节点绑定失败: %v", err)
		}
		if still != 1 {
			t.Fatalf("移出地区不应删除旧绑定，期望仍 1 条，实际 %d", still)
		}
	})
}
