package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// 定时换 IP 必须真的把节点交给 RotateWarp，而不能只打日志就把 last_rotate_at 推走。
// 旧实现（空转）会在这里失败：日志会写「已换 IP」，但 rotate 计数为 0、调用次数为 0。
func TestRunXrayWarpRotateCallsRotate(t *testing.T) {
	db := openWarpTestDB(t)
	seedRotateAccount(t, db, 41, "KR01", 24, time.Now().UTC().Add(-25*time.Hour))

	var called []uint64
	runner := NewCommandRunner(CommandDeps{
		DB:  db,
		Now: func() time.Time { return time.Now().UTC() },
		RotateWarp: func(_ context.Context, nodeID uint64) error {
			called = append(called, nodeID)
			return nil
		},
	})

	if err := runner.Run(context.Background(), "xray:warp-rotate", nil); err != nil {
		t.Fatalf("Run(xray:warp-rotate) error = %v", err)
	}
	if len(called) != 1 || called[0] != 41 {
		t.Fatalf("RotateWarp 调用 = %v, want [41]（空转实现会一次都不调）", called)
	}

	// 到点换过之后 last_rotate_at 必须推进，否则下一轮会再换一次。
	var last sql.NullTime
	if err := db.QueryRow("SELECT last_rotate_at FROM node_warp_accounts WHERE node_id = 41").Scan(&last); err != nil {
		t.Fatalf("回读 last_rotate_at: %v", err)
	}
	if !last.Valid || last.Time.Before(time.Now().UTC().Add(-5*time.Minute)) {
		t.Fatalf("last_rotate_at = %v, 应被推进到当前时间", last)
	}
}

// 换 IP 失败也要推进 last_rotate_at：否则每 5 分钟重试一次，把 CF 接口打到限流。
func TestRunXrayWarpRotateAdvancesOnFailure(t *testing.T) {
	db := openWarpTestDB(t)
	seedRotateAccount(t, db, 42, "US2", 24, time.Now().UTC().Add(-30*time.Hour))

	calls := 0
	runner := NewCommandRunner(CommandDeps{
		DB:  db,
		Now: func() time.Time { return time.Now().UTC() },
		RotateWarp: func(context.Context, uint64) error {
			calls++
			return errors.New("CF API 返回 429")
		},
	})
	if err := runner.Run(context.Background(), "xray:warp-rotate", nil); err != nil {
		t.Fatalf("Run error = %v（失败应记日志而不是让调度项失败）", err)
	}
	if calls != 1 {
		t.Fatalf("RotateWarp 调用次数 = %d, want 1", calls)
	}

	var last sql.NullTime
	if err := db.QueryRow("SELECT last_rotate_at FROM node_warp_accounts WHERE node_id = 42").Scan(&last); err != nil {
		t.Fatalf("回读 last_rotate_at: %v", err)
	}
	if !last.Valid || last.Time.Before(time.Now().UTC().Add(-5*time.Minute)) {
		t.Fatalf("失败时 last_rotate_at 也应推进，实际 = %v", last)
	}
}

// 未到点的账户一个都不能碰。
func TestRunXrayWarpRotateSkipsNotDue(t *testing.T) {
	db := openWarpTestDB(t)
	seedRotateAccount(t, db, 43, "US1", 24, time.Now().UTC().Add(-time.Hour))

	calls := 0
	runner := NewCommandRunner(CommandDeps{
		DB:  db,
		Now: func() time.Time { return time.Now().UTC() },
		RotateWarp: func(context.Context, uint64) error {
			calls++
			return nil
		},
	})
	if err := runner.Run(context.Background(), "xray:warp-rotate", nil); err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("未到点却调了 RotateWarp %d 次", calls)
	}
}
