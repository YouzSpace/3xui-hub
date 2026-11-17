package scheduler

import (
	"context"
	"testing"
)

// Balance dry=true：只产出计划，库里数据一条都不动。
func TestBalanceDryRunKeepsDataIntact(t *testing.T) {
	s, db := newTestScheduler(t, nil)

	addRegion(t, db, 41, "均衡地区", 1)
	addNode(t, db, 401, 41, 2, true, "online")  // 超容量：绑 4 > 容量 2
	addNode(t, db, 402, 41, 10, true, "online") // 余量 10
	addNode(t, db, 403, 41, 10, true, "online") // 余量 10
	addNode(t, db, 404, 41, -1, true, "online") // capacity NULL：不参与均衡
	bindUsers(t, db, 401, 1, 4)
	addUsers(t, db, 1, 4, "2020-01-01 00:00:00")

	moved, plan, err := s.Balance(context.Background(), 41, true, 0)
	if err != nil {
		t.Fatalf("均衡失败: %v", err)
	}
	if moved != 2 || len(plan) != 2 {
		t.Fatalf("应有 2 条搬运计划，实际 moved=%d len(plan)=%d %+v", moved, len(plan), plan)
	}
	for i, mv := range plan {
		if mv.FromNodeID != 401 {
			t.Fatalf("第 %d 条计划的源节点应是 401，实际 %d", i, mv.FromNodeID)
		}
		if mv.ToNodeID == 404 {
			t.Fatalf("capacity 为 NULL 的 404 不该被当成均衡目标")
		}
		if mv.ToNodeID != 402 {
			t.Fatalf("第 %d 条计划应搬往剩余容量最大的 402，实际 %d", i, mv.ToNodeID)
		}
	}

	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 401"); n != 4 {
		t.Fatalf("dry=true 不该改库：401 仍应有 4 条绑定，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 402"); n != 0 {
		t.Fatalf("dry=true 不该改库：402 应仍是 0 条绑定，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_node_migrations"); n != 0 {
		t.Fatalf("dry=true 不该写迁移历史，实际 %d 行", n)
	}
}

// Balance dry=false：真的搬，limit 生效，写迁移历史，并同步 users.updated_at。
func TestBalanceMovesOverCapacityUsers(t *testing.T) {
	s, db := newTestScheduler(t, nil)

	addRegion(t, db, 51, "均衡地区", 1)
	addNode(t, db, 501, 51, 2, true, "online") // 绑 4，超 2
	addNode(t, db, 502, 51, 10, true, "online")
	addUsers(t, db, 1, 4, "2020-01-01 00:00:00")
	bindUsers(t, db, 501, 1, 4)

	// limit=1：只搬 1 个
	moved, plan, err := s.Balance(context.Background(), 51, false, 1)
	if err != nil {
		t.Fatalf("均衡失败: %v", err)
	}
	if moved != 1 || len(plan) != 1 {
		t.Fatalf("limit=1 应只搬 1 个，实际 moved=%d plan=%+v", moved, plan)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 501"); n != 3 {
		t.Fatalf("501 应剩 3 条绑定，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 502"); n != 1 {
		t.Fatalf("502 应收到 1 条绑定，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_node_migrations WHERE reason = 'balance'"); n != 1 {
		t.Fatalf("应写 1 条 balance 迁移历史，实际 %d", n)
	}

	// limit=0：把剩下的超容量用户也搬完
	moved, _, err = s.Balance(context.Background(), 51, false, 0)
	if err != nil {
		t.Fatalf("第二次均衡失败: %v", err)
	}
	if moved != 1 {
		t.Fatalf("第二次应再搬 1 个，实际 %d", moved)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE node_id = 501"); n != 2 {
		t.Fatalf("501 应降到容量 2 条绑定，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_node_migrations WHERE reason = 'balance'"); n != 2 {
		t.Fatalf("应有 2 条 balance 迁移历史，实际 %d", n)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM users WHERE updated_at > '2021-01-01 00:00:00'"); n != 2 {
		t.Fatalf("搬过的 2 个用户 updated_at 都必须变，实际变了 %d 个", n)
	}

	// 已经不再超容量，再跑一次应无事可做
	moved, plan, err = s.Balance(context.Background(), 51, false, 0)
	if err != nil || moved != 0 || len(plan) != 0 {
		t.Fatalf("已均衡后再跑应为空操作，实际 moved=%d plan=%+v err=%v", moved, plan, err)
	}
}

// capacity 为 NULL 的节点既不搬出也不接收；离线 / 禁用节点不当目标。
func TestBalanceSkipsUnlimitedAndUnavailableNodes(t *testing.T) {
	s, db := newTestScheduler(t, nil)

	addRegion(t, db, 61, "边界地区", 1)
	addNode(t, db, 601, 61, -1, true, "online") // capacity NULL，绑 10 也不搬
	addNode(t, db, 602, 61, 2, true, "online")  // 绑 4，超 2
	addNode(t, db, 603, 61, 10, false, "online")
	addNode(t, db, 604, 61, 10, true, "offline")
	addNode(t, db, 605, 61, 10, true, "online") // 唯一可用目标
	bindUsers(t, db, 601, 100, 10)
	bindUsers(t, db, 602, 200, 4)

	moved, plan, err := s.Balance(context.Background(), 61, true, 0)
	if err != nil {
		t.Fatalf("均衡失败: %v", err)
	}
	if moved != 2 || len(plan) != 2 {
		t.Fatalf("应只搬 602 超出的 2 个，实际 moved=%d plan=%+v", moved, plan)
	}
	for _, mv := range plan {
		if mv.FromNodeID != 602 || mv.ToNodeID != 605 {
			t.Fatalf("计划应该是 602 → 605，实际 %+v", mv)
		}
	}
}
