package main

import (
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

// taskMailSender 把 admin.MailQueue（单 goroutine 串行队列）适配成 task.MailSender：
// mail:notify-scan / mail:schedule-scan 的定时通知邮件复用同一个串行队列，与
// 管理端群发/测试邮件挤同一条通道，保证「单并发发信 + DelaySeconds 限速」，
// 不会各自起 worker 并发触发 SMTP 风控。
type taskMailSender struct {
	q admin.MailQueue
}

// EnqueueMail 实现 task.MailSender（字段与 admin.MailJob 1:1 拷贝）。
func (t *taskMailSender) EnqueueMail(job task.MailJob) error {
	return t.q.Enqueue(admin.MailJob{
		ToEmail:  job.ToEmail,
		Subject:  job.Subject,
		HTMLBody: job.HTMLBody,
		Type:     job.Type,
		UserID:   job.UserID,
		BatchID:  job.BatchID,
		Scene:    job.Scene,
	})
}

// taskQueueStater 把 task.Queue 的内存水位适配成 admin.TaskQueueCounter。
//
// 系统监控页的「排队中 / 运行中 / 失败累计」要读**节点任务队列**的真实水位：
// 这条队列跑在进程内 goroutine + channel 上，不落任何表，Laravel 的
// jobs/failed_jobs 表在纯 Go 部署下恒为 0 行。以前只接了邮件队列，
// 用户跑「节点接入初始化」时监控页始终显示 0/0/0，看起来像任务没跑。
type taskQueueStater struct {
	q *task.Queue
}

// Stats 实现 admin.TaskQueueCounter。q 为 nil 时 ok=false（回退 jobs 表口径）。
func (t taskQueueStater) Stats() (pending, running, failed int64, ok bool) {
	if t.q == nil {
		return 0, 0, 0, false
	}
	st := t.q.Stats()
	return st.Pending, st.Running, st.Failed, true
}
