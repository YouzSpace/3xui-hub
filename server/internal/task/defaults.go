package task

import (
	"context"
	"database/sql"
	"fmt"

	"controlhub-server/internal/collector"
)

// ── 调度清单：routes/console.php 全量复刻 ──
//
// 18 条 Schedule，名称/时间/条件与 PHP 链式调用逐条对应。
// 调度时刻按 Laravel 的 config/app.php timezone=UTC（全项目同一口径）。
//
// 已迁移到 Go 的任务体走 collector（P6）/ AsyncTaskService；
// 尚未迁移的 artisan 命令走 CommandRunner（P10 接上真实实现前返回 ErrUnimplemented，
// 调度项照常触发并记日志 —— 时间表是契约的一部分，不能缺条目）。

// Deps 是默认调度清单的依赖面（都是小接口，测试可 fake）。
type Deps struct {
	// Collector 是 P6 采集层（*collector.Collector 满足 CollectorAPI）。
	Collector CollectorAPI
	// Tasks 是异步任务体系（async-task-timeout 用）；nil 则跳过该条。
	Tasks TimeoutStaler
	// Commands 承接尚未迁移到 Go 的 artisan 命令（备份/邮件/邀请码/…）；
	// nil → DefaultCommandRunner（返回 ErrUnimplemented 并记日志）。
	Commands CommandRunner
	// Lookups 是 when() 条件查询；nil → SQLLookups 需要 DB，两者都缺则带条件的条目按「跳过」处理。
	Lookups ConditionLookups
	// SyncLocalCerts 是本机证书目录同步（xray-certificates-sync 用）：扫面板机上的
	// acme.sh 证书目录，把续签后的 PEM 刷新进 TLS 证书库。nil → 该条跳过（单测/无库场景）。
	SyncLocalCerts func(ctx context.Context) (LocalCertSyncSummary, error)
	// Logf 是日志出口。
	Logf func(format string, args ...any)
}

// LocalCertSyncSummary 是「本机证书目录同步」的结果口径（由 admin 侧适配过来）。
type LocalCertSyncSummary struct {
	Dir     string
	Scanned int
	Created int
	Updated int
	Skipped int
	Reasons []string
}

// CollectorAPI 是调度层对 P6 采集的最小依赖面（*collector.Collector 直接满足）。
type CollectorAPI interface {
	RunTrafficSync(ctx context.Context) (collector.TrafficSyncResult, error)
	ScanBans(ctx context.Context) (collector.BanScanResult, error)
	CheckAllEnabled(ctx context.Context) (collector.CheckManyResult, error)
	MonthlyReset(ctx context.Context) (collector.MonthlyResetResult, error)
}

// TimeoutStaler 是异步任务超时清扫（AsyncTaskService.TimeoutStale）。
type TimeoutStaler interface {
	TimeoutStale(ctx context.Context, minutes int) (int, error)
}

// ErrUnimplemented 是「命令尚未迁移到 Go」的哨兵错误（P10 收口）。
var ErrUnimplemented = fmt.Errorf("command not yet migrated to Go (P10)")

// CommandRunner 承接 artisan 命令（PHP Schedule::command(...)）。
// args 是命令行参数（PHP 的 ['--hours' => 1] 形态）。
type CommandRunner interface {
	Run(ctx context.Context, command string, args map[string]string) error
}

// DefaultCommandRunner 是 CommandRunner 的占位实现：
// 未迁移命令返回 ErrUnimplemented，让调度项可见地失败而不是假装成功。
type DefaultCommandRunner struct {
	Logf func(format string, args ...any)
}

// Run 返回 ErrUnimplemented。
func (d DefaultCommandRunner) Run(_ context.Context, command string, args map[string]string) error {
	if d.Logf != nil {
		d.Logf("[schedule] 命令 %s 尚未迁移到 Go（P10）args=%v", command, args)
	}
	return ErrUnimplemented
}

// ConditionLookups 是 console.php 里 ->when(...) 的条件查询面。
type ConditionLookups interface {
	// MailNotifyAnyEnabled 对齐 MailNotifyService::anyEnabled()：四场景开关任一开启。
	MailNotifyAnyEnabled(ctx context.Context) (bool, error)
	// MailScheduleEnabled 对齐 MailScheduleService::isEnabled()。
	MailScheduleEnabled(ctx context.Context) (bool, error)
	// ThirdPartyFetchEnabled 对齐 third_party_enabled=='1' && exists enabled ThirdPartySub。
	ThirdPartyFetchEnabled(ctx context.Context) (bool, error)
	// WarpRotateEnabled 对齐 exists NodeWarpAccount（auto_rotate_hours>0 && enabled）。
	WarpRotateEnabled(ctx context.Context) (bool, error)
	// OutboundSubsEnabled 对齐 exists XrayOutboundSubscription（enabled）。
	OutboundSubsEnabled(ctx context.Context) (bool, error)
}

// ── console.php 里的调度名（与 ->name(...) 逐字一致） ──

const (
	NameTrafficSync          = "traffic-sync"
	NameBanCheck             = "ban-check"
	NameHealthCheck          = "health-check"
	NameMonthlyResetTraffic  = "monthly-reset-traffic"
	NameInviteCodeRefresh    = "invite-code-refresh"
	NameDBBackup             = "db-backup"
	NameMonitorRollup        = "monitor-rollup"
	NameExpirePendingOrders  = "expire-pending-orders"
	NameProxyMaintain        = "proxy-maintain"
	NameAsyncTaskTimeout     = "async-task-timeout"
	NameMailNotifyScan       = "mail-notify-scan"
	NameMailScheduleScan     = "mail-schedule-scan"
	NameThirdPartyFetch      = "third-party-fetch"
	NameXrayWarpRotate       = "xray-warp-rotate"
	NameXrayRefreshOutbound  = "xray-refresh-outbound-subs"
	NameGeodataUpdate        = "geodata-update"
	NameUsersReconcileEnable = "users-reconcile-enable"
	NameRealityAudit         = "reality-audit"
	// NameXrayCertificateSync 是「本机证书目录 → TLS 证书库」的同步（Go 侧新增，PHP 没有）。
	NameXrayCertificateSync = "xray-certificates-sync"
)

// ── cron 表达式（Laravel 调度宏 → 5 字段 cron，UTC） ──
//
//	everyFiveMinutes()  → */5 * * * *
//	hourly()            → 0 * * * *
//	everyTwoHours()     → 0 */2 * * *
//	dailyAt('HH:MM')    → MM HH * * *
//	weeklyOn(1,'04:45') → 45 4 * * 1   （PHP 的 1 = 周一）
const (
	SpecEveryFiveMinutes = "*/5 * * * *"
	SpecHourly           = "0 * * * *"
	SpecEveryTwoHours    = "0 */2 * * *"
	SpecEverySixHours    = "0 */6 * * *"
	SpecDaily0000        = "0 0 * * *"
	SpecDaily0010        = "10 0 * * *"
	SpecDaily0330        = "30 3 * * *"
	SpecDaily0430        = "30 4 * * *"
	SpecDaily0410        = "10 4 * * *"
	SpecWeeklyMon0445    = "45 4 * * 1"
)

// DefaultJobs 复刻 console.php 的全部 Schedule 条目。
//
// 条目顺序与 PHP 文件顺序一致（阅读对账方便；cron 触发互不依赖顺序）。
// 每条都带 ->withoutOverlapping()（PHP 里除了个别隐式，全表都带），when 条件同源。
func DefaultJobs(d Deps) []Job {
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	commands := d.Commands
	if commands == nil {
		commands = DefaultCommandRunner{Logf: logf}
	}

	jobs := []Job{
		// 流量自动同步：每5分钟
		{
			Name: NameTrafficSync, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
			Run: func(ctx context.Context) error {
				if d.Collector == nil {
					return fmt.Errorf("collector 未配置")
				}
				_, err := d.Collector.RunTrafficSync(ctx)
				return err
			},
		},
		// 全量封禁扫描：每5分钟遍历启用用户（到期/超量 → 关闭 3x-ui 流量，不封禁账号）。
		{
			Name: NameBanCheck, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
			Run: func(ctx context.Context) error {
				if d.Collector == nil {
					return fmt.Errorf("collector 未配置")
				}
				_, err := d.Collector.ScanBans(ctx)
				return err
			},
		},
		// 节点健康检查：每5分钟遍历 enabled 节点刷新 status/latency。
		{
			Name: NameHealthCheck, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
			Run: func(ctx context.Context) error {
				if d.Collector == nil {
					return fmt.Errorf("collector 未配置")
				}
				_, err := d.Collector.CheckAllEnabled(ctx)
				return err
			},
		},
		// 每天检查并重置周期套餐月流量（基于用户的 next_traffic_reset_at）
		{
			Name: NameMonthlyResetTraffic, Spec: SpecDaily0000, WithoutOverlap: true,
			Run: func(ctx context.Context) error {
				if d.Collector == nil {
					return fmt.Errorf("collector 未配置")
				}
				_, err := d.Collector.MonthlyReset(ctx)
				return err
			},
		},
		// 每天刷新到期的邀请码（旧码立即失效，只认最新码）
		{
			Name: NameInviteCodeRefresh, Spec: SpecDaily0010, WithoutOverlap: true,
			Run: func(ctx context.Context) error { return commands.Run(ctx, "invite:refresh", nil) },
		},
		// 数据库每日备份：03:30 导出到 config('backup.dir')，保留最新 config('backup.keep') 份。
		{
			Name: NameDBBackup, Spec: SpecDaily0330, WithoutOverlap: true,
			Run: func(ctx context.Context) error { return commands.Run(ctx, "backup:run", nil) },
		},
		// 机器流量降采样：超过保留期的 L0 采样折算进 monitor_daily。
		{
			Name: NameMonitorRollup, Spec: SpecDaily0430, WithoutOverlap: true,
			Run: func(ctx context.Context) error { return commands.Run(ctx, "monitor:rollup", nil) },
		},
		// 作废超时（1 小时）未支付的订单。TTL 显式传参，不依赖命令默认值。
		{
			Name: NameExpirePendingOrders, Spec: SpecHourly, WithoutOverlap: true,
			Run: func(ctx context.Context) error {
				return commands.Run(ctx, "order:expire-pending", map[string]string{"--hours": "1"})
			},
		},
		// 代理商城的维护：回采 webshare 用量 + 到期权益标记 + 到期停用上游子账号。
		{
			Name: NameProxyMaintain, Spec: SpecHourly, WithoutOverlap: true,
			Run: func(ctx context.Context) error { return commands.Run(ctx, "proxy:maintain", nil) },
		},
	}

	// 任务超时兜底：超过阈值未终态的异步任务标记失败。
	if d.Tasks != nil {
		jobs = append(jobs, Job{
			Name: NameAsyncTaskTimeout, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
			Run: func(ctx context.Context) error {
				_, err := d.Tasks.TimeoutStale(ctx, defaultStaleAfterMinutes)
				return err
			},
		})
	}

	// 邮件自动通知扫描：四开关全关时任务整体跳过、不空转。
	jobs = append(jobs, Job{
		Name: NameMailNotifyScan, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
		When: func(ctx context.Context) (bool, error) {
			if d.Lookups == nil {
				return false, nil
			}
			return d.Lookups.MailNotifyAnyEnabled(ctx)
		},
		Run: func(ctx context.Context) error {
			return commands.Run(ctx, "mail:notify-scan", nil) // MailNotifyScanJob
		},
	})

	// 按月定时发信：到点且当月没发过就把配好的那封信入队；默认关闭。
	jobs = append(jobs, Job{
		Name: NameMailScheduleScan, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
		When: func(ctx context.Context) (bool, error) {
			if d.Lookups == nil {
				return false, nil
			}
			return d.Lookups.MailScheduleEnabled(ctx)
		},
		Run: func(ctx context.Context) error {
			return commands.Run(ctx, "mail:schedule-scan", nil) // MailScheduleJob
		},
	})

	// 第三方订阅拉取：每 2 小时；总开关开启且有启用订阅才跑。
	jobs = append(jobs, Job{
		Name: NameThirdPartyFetch, Spec: SpecEveryTwoHours, WithoutOverlap: true,
		When: func(ctx context.Context) (bool, error) {
			if d.Lookups == nil {
				return false, nil
			}
			return d.Lookups.ThirdPartyFetchEnabled(ctx)
		},
		Run: func(ctx context.Context) error { return commands.Run(ctx, "third-party:fetch", nil) },
	})

	// WARP 定时换 IP：每 5 分钟；没有任何「已启用 + 开启定时换 IP」的账户时跳过。
	jobs = append(jobs, Job{
		Name: NameXrayWarpRotate, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
		When: func(ctx context.Context) (bool, error) {
			if d.Lookups == nil {
				return false, nil
			}
			return d.Lookups.WarpRotateEnabled(ctx)
		},
		Run: func(ctx context.Context) error { return commands.Run(ctx, "xray:warp-rotate", nil) },
	})

	// 出站订阅刷新：每 5 分钟；存在启用订阅才跑。
	jobs = append(jobs, Job{
		Name: NameXrayRefreshOutbound, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
		When: func(ctx context.Context) (bool, error) {
			if d.Lookups == nil {
				return false, nil
			}
			return d.Lookups.OutboundSubsEnabled(ctx)
		},
		Run: func(ctx context.Context) error {
			return commands.Run(ctx, "xray:refresh-outbound-subs", nil)
		},
	})

	// Geo 数据更新：每周一 04:45（UTC），有更新则分发到所有 enabled xray 节点。
	jobs = append(jobs, Job{
		Name: NameGeodataUpdate, Spec: SpecWeeklyMon0445, WithoutOverlap: true,
		Run: func(ctx context.Context) error {
			return commands.Run(ctx, "geodata:update", map[string]string{"--dispatch": "1"})
		},
	})

	// 节点侧「已关闭但面板仍放行」的对账恢复。
	jobs = append(jobs, Job{
		Name: NameUsersReconcileEnable, Spec: SpecEveryFiveMinutes, WithoutOverlap: true,
		Run: func(ctx context.Context) error { return commands.Run(ctx, "users:reconcile-enable", nil) },
	})

	// REALITY 入站巡检：每日 04:10（UTC）。
	jobs = append(jobs, Job{
		Name: NameRealityAudit, Spec: SpecDaily0410, WithoutOverlap: true,
		Run: func(ctx context.Context) error { return commands.Run(ctx, "xray:reality-audit", nil) },
	})

	// 本机证书目录同步：每 6 小时，与面板机 crontab 里的 acme.sh --cron 同频。
	// 入站只存 tlsCertId，库里 PEM 一刷新，下次下发配置就生效，入站不用改 ——
	// 所以这条任务的价值就是「续签后自动把新证书搬进库」，不做任何节点侧动作。
	jobs = append(jobs, Job{
		Name: NameXrayCertificateSync, Spec: SpecEverySixHours, WithoutOverlap: true,
		Run: func(ctx context.Context) error {
			if d.SyncLocalCerts == nil {
				return nil // 没接实现（单测 / 无库场景）不报错，避免刷失败记录
			}
			sum, err := d.SyncLocalCerts(ctx)
			if err != nil {
				return err
			}
			logf("[schedule] %s 目录=%s 扫描=%d 新增=%d 更新=%d 跳过=%d reasons=%v",
				NameXrayCertificateSync, sum.Dir, sum.Scanned, sum.Created, sum.Updated, sum.Skipped, sum.Reasons)
			return nil
		},
	})

	return jobs
}

// SQLLookups 是 ConditionLookups 的 MySQL 实现（site_configs + 各表 EXISTS）。
type SQLLookups struct {
	DB *sql.DB
}

// MailNotifyAnyEnabled 对齐 MailNotifyService::anyEnabled()：
// notify_{traffic_exhausted,traffic_almost,expiring,expired}_enabled 任一 == '1'。
func (l *SQLLookups) MailNotifyAnyEnabled(ctx context.Context) (bool, error) {
	// PHP：SiteConfig::getValue("notify_{$scene}_enabled") === '1'，未配置 → false。
	const q = "SELECT COUNT(*) FROM site_configs WHERE " +
		"`key` IN ('notify_traffic_exhausted_enabled','notify_traffic_almost_enabled'," +
		"'notify_expiring_enabled','notify_expired_enabled') AND value = '1'"
	var n int
	if err := l.DB.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// MailScheduleEnabled 对齐 MailScheduleService::isEnabled()：schedule_mail_enabled === '1'。
func (l *SQLLookups) MailScheduleEnabled(ctx context.Context) (bool, error) {
	return l.siteConfigEquals(ctx, "schedule_mail_enabled", "1")
}

// ThirdPartyFetchEnabled 对齐 console.php：
// SiteConfig::getValue('third_party_enabled', '0') === '1' && ThirdPartySub where enabled 存在。
func (l *SQLLookups) ThirdPartyFetchEnabled(ctx context.Context) (bool, error) {
	on, err := l.siteConfigEquals(ctx, "third_party_enabled", "1")
	if err != nil || !on {
		return on, err
	}
	return l.exists(ctx, "SELECT 1 FROM third_party_subs WHERE enabled = 1 LIMIT 1")
}

// WarpRotateEnabled 对齐：exists NodeWarpAccount（auto_rotate_hours>0 && enabled）。
func (l *SQLLookups) WarpRotateEnabled(ctx context.Context) (bool, error) {
	return l.exists(ctx,
		"SELECT 1 FROM node_warp_accounts WHERE auto_rotate_hours > 0 AND enabled = 1 LIMIT 1")
}

// OutboundSubsEnabled 对齐：exists XrayOutboundSubscription（enabled）。
func (l *SQLLookups) OutboundSubsEnabled(ctx context.Context) (bool, error) {
	return l.exists(ctx, "SELECT 1 FROM xray_outbound_subscriptions WHERE enabled = 1 LIMIT 1")
}

func (l *SQLLookups) siteConfigEquals(ctx context.Context, key, want string) (bool, error) {
	var v sql.NullString
	err := l.DB.QueryRowContext(ctx, "SELECT value FROM site_configs WHERE `key` = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// PHP 用 === '1'（严格字符串比较）。
	return v.Valid && v.String == want, nil
}

func (l *SQLLookups) exists(ctx context.Context, q string) (bool, error) {
	var one int
	err := l.DB.QueryRowContext(ctx, q).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
