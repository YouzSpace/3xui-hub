package scheduler

import (
	"context"
	"strings"
	"testing"
)

// Migrate 2500 人 → 分 3 批（1000 / 1000 / 500）迁完：
// 绑定全搬到目标节点、迁移历史按用户留痕、被迁用户的 updated_at 都变了。
func TestMigrateAllUsersInBatches(t *testing.T) {
	s, db := newTestScheduler(t, nil)

	addRegion(t, db, 71, "迁移地区", 1)
	addNode(t, db, 701, 71, -1, true, "online")
	addNode(t, db, 702, 71, -1, true, "online")

	const users = 2500
	addUsers(t, db, 1, users, "2020-01-01 00:00:00")
	bindUsers(t, db, 701, 1, users)

	var batches []int
	s.onMigrateBatch = func(moved int) { batches = append(batches, moved) }

	moved, err := s.Migrate(context.Background(), 701, 702, 0)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if moved != users {
		t.Fatalf("应迁 %d 人，实际 %d", users, moved)
	}

	// 分批证据：3 批，1000 / 1000 / 500
	want := []int{1000, 1000, 500}
	if len(batches) != len(want) {
		t.Fatalf("应分 %d 批，实际 %d 批：%v", len(want), len(batches), batches)
	}
	for i := range want {
		if batches[i] != want[i] {
			t.Fatalf("第 %d 批应迁 %d 人，实际 %d（全部批次：%v）", i+1, want[i], batches[i], batches)
		}
	}

	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 701"); n != 0 {
		t.Fatalf("源节点应被搬空，实际还剩 %d 条", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 702"); n != users {
		t.Fatalf("目标节点应有 %d 条绑定，实际 %d", users, n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_node_migrations WHERE reason = 'failover' AND from_node_id = 701 AND to_node_id = 702"); n != users {
		t.Fatalf("迁移历史应有 %d 行，实际 %d", users, n)
	}
	// 关键断言：updated_at 必须跟着变，否则节点不会重拉配置
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM users WHERE updated_at > '2021-01-01 00:00:00'"); n != users {
		t.Fatalf("被迁用户的 updated_at 必须全部更新，实际只更新了 %d / %d 个", n, users)
	}
}

// count>0 时只迁这么多个。
func TestMigrateRespectsCount(t *testing.T) {
	s, db := newTestScheduler(t, nil)

	addRegion(t, db, 81, "迁移地区", 1)
	addNode(t, db, 801, 81, -1, true, "online")
	addNode(t, db, 802, 81, -1, true, "online")
	addUsers(t, db, 1, 5, "2020-01-01 00:00:00")
	bindUsers(t, db, 801, 1, 5)

	moved, err := s.Migrate(context.Background(), 801, 802, 2)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if moved != 2 {
		t.Fatalf("应只迁 2 人，实际 %d", moved)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 801"); n != 3 {
		t.Fatalf("源节点应剩 3 条，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 802"); n != 2 {
		t.Fatalf("目标节点应有 2 条，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_node_migrations WHERE reason = 'failover'"); n != 2 {
		t.Fatalf("迁移历史应有 2 行，实际 %d", n)
	}
}

// 参数校验：源=目标、节点不存在，都要明确报错且不写库。
func TestMigrateValidatesNodes(t *testing.T) {
	s, db := newTestScheduler(t, nil)

	addRegion(t, db, 91, "迁移地区", 1)
	addNode(t, db, 901, 91, -1, true, "online")
	addUsers(t, db, 1, 3, "2020-01-01 00:00:00")
	bindUsers(t, db, 901, 1, 3)

	if _, err := s.Migrate(context.Background(), 901, 901, 0); err == nil || !strings.Contains(err.Error(), "相同") {
		t.Fatalf("源=目标应报错，实际 %v", err)
	}
	if _, err := s.Migrate(context.Background(), 901, 9999, 0); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("目标节点不存在应报错，实际 %v", err)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 901"); n != 3 {
		t.Fatalf("校验失败时不该写库，901 应有 3 条，实际 %d", n)
	}
}
