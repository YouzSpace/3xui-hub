package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 单地区 3 台节点（已绑 10 / 5 / 0 人，capacity 都 100）→ 分到「绑 0 人」那台。
func TestAssignRegionPicksLeastBoundNode(t *testing.T) {
	locker := &fakeLocker{}
	s, db := newTestScheduler(t, locker)

	addRegion(t, db, 7, "香港", 1)
	addNode(t, db, 101, 7, 100, true, "online")
	addNode(t, db, 102, 7, 100, true, "online")
	addNode(t, db, 103, 7, 100, true, "online")
	bindUsers(t, db, 101, 1, 10)
	bindUsers(t, db, 102, 11, 5)

	nodeID, err := s.AssignRegion(context.Background(), 900, 7)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if nodeID != 103 {
		t.Fatalf("应分到已绑 0 人的节点 103，实际分到 %d", nodeID)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 900 AND node_id = 103"); n != 1 {
		t.Fatalf("绑定没有写入，user_nodes 命中 %d 行", n)
	}

	calls, unlocks := locker.stats()
	if calls != 1 || unlocks != 1 {
		t.Fatalf("锁调用次数不对：calls=%d unlocks=%d，期望各 1 次", calls, unlocks)
	}
	key, ttl := locker.lastCall()
	if key != "ch:lock:assign:region:7" {
		t.Fatalf("锁 key 不对：%s", key)
	}
	if ttl != 3*time.Second {
		t.Fatalf("锁 TTL 不对：%v，期望 3s", ttl)
	}
}

// 负载率打平 → 取 id 最小的节点（保证确定性）。
func TestAssignRegionTieBreaksBySmallestID(t *testing.T) {
	s, db := newTestScheduler(t, &fakeLocker{})

	addRegion(t, db, 71, "日本", 1)
	addNode(t, db, 702, 71, 100, true, "online")
	addNode(t, db, 701, 71, 100, true, "online")

	nodeID, err := s.AssignRegion(context.Background(), 901, 71)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if nodeID != 701 {
		t.Fatalf("平局应取 id 最小的 701，实际 %d", nodeID)
	}
}

// capacity 为 NULL（不限）的节点排最后：能选有容量的就绝不选它。
func TestAssignRegionUnlimitedCapacityRanksLast(t *testing.T) {
	s, db := newTestScheduler(t, &fakeLocker{})

	addRegion(t, db, 72, "美国", 1)
	addNode(t, db, 711, 72, -1, true, "online") // capacity NULL
	addNode(t, db, 712, 72, 100, true, "online")
	bindUsers(t, db, 711, 1, 50)
	bindUsers(t, db, 712, 1000, 50) // 负载率 50/100=0.5，711 是「不限」应排后面

	nodeID, err := s.AssignRegion(context.Background(), 902, 72)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if nodeID != 712 {
		t.Fatalf("应分到有容量上限的 712，实际 %d", nodeID)
	}
}

// 候选为空 / 全满 / 离线 / 禁用 → 都是 ErrNoCapacity。
func TestAssignRegionNoCapacity(t *testing.T) {
	s, db := newTestScheduler(t, &fakeLocker{})

	// 地区 8：一台节点都没有
	addRegion(t, db, 8, "空地区", 1)

	// 地区 9：唯一节点已满容量
	addRegion(t, db, 9, "满容量", 2)
	addNode(t, db, 111, 9, 2, true, "online")
	bindUsers(t, db, 111, 1, 2)

	// 地区 10：唯一节点离线
	addRegion(t, db, 10, "离线", 3)
	addNode(t, db, 121, 10, 100, true, "offline")

	// 地区 11：唯一节点被禁用
	addRegion(t, db, 11, "禁用", 4)
	addNode(t, db, 131, 11, 100, false, "online")

	cases := []struct {
		name     string
		regionID int64
	}{
		{"没有候选节点", 8},
		{"候选全部满容量", 9},
		{"候选只有离线节点", 10},
		{"候选只有被禁用节点", 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodeID, err := s.AssignRegion(context.Background(), 903, tc.regionID)
			if !errors.Is(err, ErrNoCapacity) {
				t.Fatalf("期望 ErrNoCapacity，实际 nodeID=%d err=%v", nodeID, err)
			}
			if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 903"); n != 0 {
				t.Fatalf("失败时不该写入绑定，user_nodes 命中 %d 行", n)
			}
		})
	}
}

// 用户已在某地区绑定 → 该地区不再重复分：直接返回原节点，不写库、不抢锁。
func TestAssignRegionAlreadyBoundReturnsExisting(t *testing.T) {
	locker := &fakeLocker{}
	s, db := newTestScheduler(t, locker)

	addRegion(t, db, 21, "新加坡", 1)
	addNode(t, db, 201, 21, 100, true, "online")
	addNode(t, db, 202, 21, 100, true, "online")
	mustExec(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (900, 201)")

	nodeID, err := s.AssignRegion(context.Background(), 900, 21)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if nodeID != 201 {
		t.Fatalf("应返回已绑定的 201，实际 %d", nodeID)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 900"); n != 1 {
		t.Fatalf("不该新增绑定，该用户现有 %d 条", n)
	}
	if calls, _ := locker.stats(); calls != 0 {
		t.Fatalf("已绑定的地区不该抢锁，实际抢了 %d 次", calls)
	}
}

// Assign：跳过已绑定地区、给未绑定地区分配，单地区失败不中断、错误汇总返回。
func TestAssignSkipsBoundRegionsAndCollectsFailures(t *testing.T) {
	s, db := newTestScheduler(t, &fakeLocker{})

	addRegion(t, db, 21, "已绑定地区", 1)
	addNode(t, db, 201, 21, 100, true, "online")
	mustExec(t, db, "INSERT INTO user_nodes (user_id, node_id) VALUES (900, 201)")

	addRegion(t, db, 22, "可分配地区", 2)
	addNode(t, db, 221, 22, 100, true, "online")

	addRegion(t, db, 23, "没有节点", 3)

	addRegion(t, db, 24, "容量已满", 4)
	addNode(t, db, 241, 24, 1, true, "online")
	bindUsers(t, db, 241, 1, 1)

	assigned, err := s.Assign(context.Background(), 900)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("期望汇总里带 ErrNoCapacity，实际 %v", err)
	}
	for _, regionID := range []string{"地区 23", "地区 24"} {
		if !strings.Contains(err.Error(), regionID) {
			t.Fatalf("错误里应包含 %s，实际: %v", regionID, err)
		}
	}
	if len(assigned) != 1 {
		t.Fatalf("应只在地区 22 分配成功，实际 %+v", assigned)
	}
	if assigned[0].RegionID != 22 || assigned[0].NodeID != 221 {
		t.Fatalf("分配结果不对：%+v", assigned[0])
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 900"); n != 2 {
		t.Fatalf("该用户应有 2 条绑定（21 原有 + 22 新增），实际 %d", n)
	}
}

// 抢锁失败重试：首次失败后最多再试 3 次（间隔 50ms），共 4 次尝试。
func TestAssignRegionLockRetry(t *testing.T) {
	t.Run("失败 3 次后第 4 次拿到锁", func(t *testing.T) {
		locker := &fakeLocker{failures: 3}
		s, db := newTestScheduler(t, locker)
		addRegion(t, db, 31, "重试地区", 1)
		addNode(t, db, 301, 31, 100, true, "online")

		nodeID, err := s.AssignRegion(context.Background(), 900, 31)
		if err != nil {
			t.Fatalf("第 4 次尝试应成功，实际报错: %v", err)
		}
		if nodeID != 301 {
			t.Fatalf("应分到 301，实际 %d", nodeID)
		}
		calls, unlocks := locker.stats()
		if calls != 4 {
			t.Fatalf("应共尝试 4 次（1 次抢占 + 3 次重试），实际 %d 次", calls)
		}
		if unlocks != 1 {
			t.Fatalf("拿到锁后应释放 1 次，实际 %d 次", unlocks)
		}
	})

	t.Run("一直拿不到锁则报错", func(t *testing.T) {
		locker := &fakeLocker{failures: 100}
		s, db := newTestScheduler(t, locker)
		addRegion(t, db, 32, "锁冲突地区", 1)
		addNode(t, db, 302, 32, 100, true, "online")

		if _, err := s.AssignRegion(context.Background(), 901, 32); err == nil {
			t.Fatal("期望报错，实际 nil")
		}
		if calls, _ := locker.stats(); calls != 4 {
			t.Fatalf("应共尝试 4 次，实际 %d 次", calls)
		}
		if n := scalarInt(t, db, "SELECT COUNT(*) FROM user_nodes WHERE user_id = 901"); n != 0 {
			t.Fatalf("拿不到锁不该写库，user_nodes 命中 %d 行", n)
		}
	})
}

// 没注入锁实现时不能「无锁分配」，要明确报错。
func TestAssignRegionRequiresLocker(t *testing.T) {
	s, db := newTestScheduler(t, nil)
	addRegion(t, db, 41, "无锁地区", 1)
	addNode(t, db, 411, 41, 100, true, "online")

	_, err := s.AssignRegion(context.Background(), 900, 41)
	if err == nil || !strings.Contains(err.Error(), "分布式锁") {
		t.Fatalf("期望「未注入分布式锁」错误，实际 %v", err)
	}
}
