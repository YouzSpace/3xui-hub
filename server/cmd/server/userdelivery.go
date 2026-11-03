package main

// 用户节点下发的异步任务（UserDeliveryJob）。
//
// 为什么要有：后台改套餐、支付完成都要「把新套餐推到所有节点」。线上 7 台节点、
// 每台 2–3 次串行往返，一轮 3–5 秒；前端有几十秒超时、支付平台回调也有自己的超时，
// 同步做等于把这段时间押在「节点是否健康」上。
//
// 所以拆成两步：
//  1. 接口只改库，立刻返回（admin 改套餐 / 支付回调都适用）；
//  2. 下发丢给队列，worker 里跑 ProvisionUser + SetClientEnabled。
//
// 失败可见：任务与子项状态落 async_tasks / async_task_items，「异步任务」页能直接看到
// 「用户节点下发失败」和具体原因（task.FailItem 写 error 列）；同时写服务器日志。
//
// 与既有 ProvisionClientJob 的分工：那个是建号专用、单节点粒度、无任务记录；
// 这个是「整用户一次下发」，带任务记录、失败可追溯。

import (
	"context"
	"fmt"
	"log"

	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

// userDeliveryItemKey 是整条任务唯一的子项键（一个用户一次下发 = 一个 item）。
const userDeliveryItemKey = "deliver"

// userDelivery 把「用户下发」派进 task.Queue，并实现 admin.UserDelivery。
type userDelivery struct {
	queue  *task.Queue
	svc    *task.Service
	users  admin.AdminUserStore
	ops    admin.UserPanelOps
	logger *log.Logger
}

// NewUserDelivery 构造用户下发派发器。
func NewUserDelivery(queue *task.Queue, svc *task.Service, users admin.AdminUserStore, ops admin.UserPanelOps, logger *log.Logger) *userDelivery {
	if logger == nil {
		logger = log.Default()
	}
	return &userDelivery{queue: queue, svc: svc, users: users, ops: ops, logger: logger}
}

// Dispatch 实现 admin.UserDelivery：建一条 user_delivery 任务并入队，立即返回。
func (u *userDelivery) Dispatch(ctx context.Context, userID uint64) error {
	if u.queue == nil || u.svc == nil {
		return fmt.Errorf("异步队列未就绪")
	}
	subject := "user"
	id := int64(userID)
	t, err := u.svc.Create(ctx, task.CreateInput{
		Type:        task.TypeUserDelivery,
		SubjectType: &subject,
		SubjectID:   &id,
		Total:       1,
		MaxAttempts: 1,
		ItemKeys:    []string{userDeliveryItemKey},
	})
	if err != nil {
		return err
	}
	u.queue.Enqueue(task.JobPayload{
		Kind:    task.JobUserDelivery,
		TaskID:  t.ID,
		ItemKey: userDeliveryItemKey,
		UserID:  &id,
	})
	return nil
}

// registerUserDelivery 注册 UserDeliveryJob 的执行体。
func registerUserDelivery(q *task.Queue, u *userDelivery) {
	q.Register(task.JobUserDelivery, func(ctx context.Context, job task.JobPayload) error {
		return u.handle(ctx, job)
	})
}

// handle 执行一次用户下发：provision（全节点，内部并发 + 跳过离线 3x-ui）→ 开 client。
//
// 失败一律 FailItem 落终态（原因写进任务，页面上看得到），返回 nil 不让队列重放：
// 重放下一次也是同样的节点状态，没有意义；漏掉的下发由 5 分钟一轮的封禁扫描 /
// 对账任务补。
func (u *userDelivery) handle(ctx context.Context, job task.JobPayload) error {
	if job.UserID == nil {
		return nil
	}
	userID := *job.UserID

	claimed, err := u.svc.ClaimItem(ctx, job.TaskID, job.ItemKey)
	if err != nil || !claimed {
		return err
	}

	fail := func(reason string) error {
		u.logger.Printf("[delivery] 用户下发失败 user=%d: %s", userID, reason)
		if _, err := u.svc.FailItem(ctx, job.TaskID, job.ItemKey, reason); err != nil {
			u.logger.Printf("[delivery] 写失败终态出错 user=%d: %v", userID, err)
		}
		return nil
	}

	user, err := u.users.UserByID(ctx, uint64(userID))
	if err != nil {
		return fail("读取用户失败：" + err.Error())
	}
	if user == nil {
		// 用户已被删：任务直接算完成，不必报错。
		_, _ = u.svc.CompleteItem(ctx, job.TaskID, job.ItemKey)
		return nil
	}

	// 1) 建/更新 client（内部已按节点并发、跳过离线 3x-ui）
	if err := u.ops.ProvisionUser(ctx, user); err != nil {
		return fail("下发节点失败：" + err.Error())
	}
	// 2) 打开 client（含「全部节点确认后才清 traffic_disabled_at」的记账）
	if user.Enabled && user.PlanID != nil {
		if err := u.ops.SetClientEnabled(ctx, user, true); err != nil {
			return fail("开启 client 失败：" + err.Error())
		}
	}

	if _, err := u.svc.CompleteItem(ctx, job.TaskID, job.ItemKey); err != nil {
		u.logger.Printf("[delivery] 写完成终态出错 user=%d: %v", userID, err)
	}
	return nil
}

var _ admin.UserDelivery = (*userDelivery)(nil)
