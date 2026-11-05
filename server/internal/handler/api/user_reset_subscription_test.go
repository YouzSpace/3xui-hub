package api

// user_reset_subscription_test.go 锁住「重置订阅后必须重建节点凭据」这条语义。
//
// 线上事故（2026-11-04）：用户自助重置订阅后，3x-ui 节点被漏掉、裸内核收到的
// rmu/adu 载荷格式不对 ⇒ 库里 uuid 换了，节点上一台都没换，用户更新订阅后全断。
// 这两个用例把「覆盖所有节点」「交给 UserPanelOps 统一实现」钉死。

import (
	"context"
	"errors"
	"testing"

	"controlhub-server/internal/handler/admin"
)

// fakePanelOps 记录 RebuildUserCredentials 的调用，并可注入错误。
type fakePanelOps struct {
	admin.NoopUserPanelOps
	calls  int
	last   *admin.UserRow
	failWith error
}

func (f *fakePanelOps) RebuildUserCredentials(_ context.Context, user *admin.UserRow) error {
	f.calls++
	f.last = user
	return f.failWith
}

// fakeUserStore 只实现 UserByID，其余走内嵌最小实现所需的错误。
type fakeUserStore struct {
	admin.AdminUserStore
	user *admin.UserRow
	err  error
}

func (f *fakeUserStore) UserByID(context.Context, uint64) (*admin.UserRow, error) {
	return f.user, f.err
}

func newResetExtras(ops admin.UserPanelOps, users admin.AdminUserStore) *UserExtras {
	return &UserExtras{handler: &Handler{}, ops: ops, users: users}
}

// TestRebuildNodeCredentials_CallsPanelOps 确认重建走的是 UserPanelOps（而不是手拼
// node_api_commands），并且用的是「重读出来的完整用户行」。
func TestRebuildNodeCredentials_CallsPanelOps(t *testing.T) {
	ops := &fakePanelOps{}
	row := &admin.UserRow{ID: 169, UUID: "c7f8b45c-21ca-4dbb-8e13-b7394a42de18", Protocol: "vless"}
	e := newResetExtras(ops, &fakeUserStore{user: row})

	if err := e.rebuildNodeCredentials(context.Background(), 169); err != nil {
		t.Fatalf("rebuildNodeCredentials 返回错误: %v", err)
	}
	if ops.calls != 1 {
		t.Fatalf("期望 RebuildUserCredentials 被调用 1 次，实际 %d", ops.calls)
	}
	if ops.last == nil || ops.last.ID != 169 {
		t.Fatalf("下发的用户行不对: %+v", ops.last)
	}
	if ops.last.UUID != row.UUID {
		t.Fatalf("下发的 uuid 不是新值: %s", ops.last.UUID)
	}
}

// TestRebuildNodeCredentials_SkipsWhenNotWired 未接线时不该 panic，也不该报错
// （测试环境/精简部署的既有语义：交给对账兜底）。
func TestRebuildNodeCredentials_SkipsWhenNotWired(t *testing.T) {
	e := newResetExtras(nil, nil)
	if err := e.rebuildNodeCredentials(context.Background(), 169); err != nil {
		t.Fatalf("未接线时应返回 nil，实际: %v", err)
	}
}

// TestRebuildNodeCredentials_SurfacesReloadError 重读用户失败要能报上来，
// 不能静默当成功（否则又是「看着成功、节点没动」）。
func TestRebuildNodeCredentials_SurfacesReloadError(t *testing.T) {
	ops := &fakePanelOps{}
	e := newResetExtras(ops, &fakeUserStore{err: errors.New("db down")})
	if err := e.rebuildNodeCredentials(context.Background(), 169); err == nil {
		t.Fatal("重读用户失败时应返回错误")
	}
	if ops.calls != 0 {
		t.Fatalf("重读失败时不该继续下发，实际调用 %d 次", ops.calls)
	}
}
