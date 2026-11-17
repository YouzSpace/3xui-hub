package admin

// 后台任务执行体：故障迁移 / 批量均衡（地区功能 #10）。
//
// 两个任务都是「后台 goroutine + 内存进度」：POST 立刻返回 job_id，前端轮询
// GET /migrations/jobs/:job_id。选型理由见 migration_jobs.go 头注释。

import (
	"context"
	"time"
)

// runMigrateJob 搬一个节点的用户到目标节点。
//
// 分片驱动引擎（migrationChunk 个一批）：引擎一次只回传「这批搬了几个」，
// 不分片的话 done 只能等任务结束才从 0 跳到 N，前端看到的就是「一直 0 然后突然完成」。
func (d *MigrationDomain) runMigrateJob(job *migrationJob, from, to int64, count int) {
	ctx, cancel := context.WithTimeout(context.Background(), migrationJobTimeout)
	defer cancel()

	moved := 0
	for {
		if err := ctx.Err(); err != nil {
			job.abort("任务超时或已取消: " + err.Error())
			break
		}

		want := migrationChunk
		if count > 0 {
			remaining := count - moved
			if remaining <= 0 {
				break
			}
			if remaining < want {
				want = remaining
			}
		}

		n, err := d.engine.Migrate(ctx, from, to, want)
		if err != nil {
			job.addFailed(err.Error())
			job.abort(err.Error())
			break
		}
		moved += n
		job.setProgress(moved)

		// 一个都没搬动 = 源节点上已经没有可搬的用户了（不是错误）。
		// 不用「没搬满这一批」判断，是因为并发搬移会让某批少搬几个但节点上还有人。
		if n == 0 {
			break
		}
		if count > 0 && moved >= count {
			break
		}
	}

	d.stampJobBatch(job, "failover", &from, &to)
	job.finish()
}

// runBalanceJob 做一次地区内均衡（dry=false 的后台形态）。
//
// Balance 本身是一次调用、内部自己决定搬谁，进度只有「搬完时的 moved」，
// 所以 done 在结束时一次性写入 —— 这是引擎的接口决定的，不在这里编造中间值。
func (d *MigrationDomain) runBalanceJob(job *migrationJob, regionID int64, limit int) {
	ctx, cancel := context.WithTimeout(context.Background(), migrationJobTimeout)
	defer cancel()

	moved, _, err := d.engine.Balance(ctx, regionID, false, limit)
	if err != nil {
		job.addFailed(err.Error())
		job.abort(err.Error())
	} else {
		job.setProgress(moved)
	}

	// 均衡的迁移记录不做节点对收窄（一次均衡会涉及多组 from→to），只按 reason + id 水位线圈。
	d.stampJobBatch(job, "balance", nil, nil)
	job.finish()
}

// stampJobBatch 给本任务期间新写入的迁移记录打批次号。
//
// 失败也要打：搬到一半的任务恰恰最需要能撤销。打标失败只记日志，不改任务结论
// （搬移本身已经落库，历史接口仍能按 node_id 查到，只是不能按批次一键撤销）。
func (d *MigrationDomain) stampJobBatch(job *migrationJob, reason string, from, to *int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	n, err := d.stampBatch(ctx, job.batchID, reason, job.afterID, from, to)
	if err != nil {
		d.logger.Printf("迁移任务 %s 打批次号失败: %v", job.id, err)
		return
	}
	if n > 0 {
		d.logger.Printf("迁移任务 %s 批次 %s：%d 条迁移记录已打标", job.id, job.batchID, n)
	}
}
