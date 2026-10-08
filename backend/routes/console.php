<?php

use App\Jobs\BanCheckJob;
use App\Jobs\HealthCheckJob;
use App\Jobs\MailNotifyScanJob;
use App\Jobs\MailScheduleJob;
use Illuminate\Support\Facades\Schedule;

// 流量自动同步：每5分钟
Schedule::command('traffic:sync')->everyFiveMinutes()->name('traffic-sync')->withoutOverlapping();

// 全量封禁扫描：每5分钟遍历启用用户（到期/超量 → 关闭 3x-ui 流量，不封禁账号）。
// 不依赖流量增量，兜底关闭「耗尽后停止传输」的长期套餐用户（增量门控检查漏掉的场景）。
Schedule::job(BanCheckJob::class)->everyFiveMinutes()->name('ban-check')->withoutOverlapping();

// 节点健康检查：每5分钟遍历 enabled 节点刷新 status/latency。
// 没有它节点 status 一旦变 offline 就只能靠管理员手点「测试连接」恢复（订阅要求 status=online）。
Schedule::job(HealthCheckJob::class)->everyFiveMinutes()->name('health-check')->withoutOverlapping();

// 每天检查并重置周期套餐月流量（基于用户的 next_traffic_reset_at）
Schedule::command('traffic:monthly-reset')->dailyAt('00:00')->name('monthly-reset-traffic')->withoutOverlapping();

// 每天刷新到期的邀请码（旧码立即失效，只认最新码）
Schedule::command('invite:refresh')->dailyAt('00:10')->name('invite-code-refresh')->withoutOverlapping();

// 作废超时（1 小时）未支付的订单，防止 pending 单无限堆积：
// 带码死单会被反复复用导致用户永远付不了款；无码死单会被反复重发网关刷 ERROR 日志。
// TTL 显式传参，不依赖命令默认值 —— 默认值一旦被改动，这里的口径会跟着变，容易踩坑。
Schedule::command('order:expire-pending', ['--hours' => 1])->hourly()->name('expire-pending-orders')->withoutOverlapping();

// 任务超时兜底：超过阈值（config('tasks.stale_after_minutes')）未终态的异步任务标记失败，防止 running 永久停留
Schedule::call(fn () => app(\App\Services\AsyncTaskService::class)->timeoutStale())->everyFiveMinutes()->name('async-task-timeout')->withoutOverlapping();

// 邮件自动通知扫描：每 5 分钟检查「流量即将用尽 / 即将到期 / 已到期 / 无套餐」。
// 四个开关默认关闭（SiteConfig 未配置即关闭），未开启时本任务不查库也不发信。
// 同一天同一场景对同一用户只发一次，防重复逻辑在 MailNotifyScanJob 内。
Schedule::job(MailNotifyScanJob::class)->everyFiveMinutes()->name('mail-notify-scan')->withoutOverlapping();

// 按月定时发信：每 5 分钟检查「管理员配置的每月第 N 天 HH:MM」是否到点，
// 到点且当月没发过就把配好的那封信入队（收件人/标题/正文/限速复用批量发信配置）。
// 默认关闭（SiteConfig 未配置即关）；同一自然月只发一轮（MailScheduleJob 内去重）。
Schedule::job(MailScheduleJob::class)->everyFiveMinutes()->name('mail-schedule-scan')->withoutOverlapping();

// 第三方订阅拉取：每 2 小时抓取启用的第三方订阅并缓存节点链接。
// 没有启用订阅时命令直接跳过；拉取失败保留上次成功缓存（ThirdPartyService 内处理）。
Schedule::command('third-party:fetch')->everyTwoHours()->name('third-party-fetch')->withoutOverlapping();

// WARP 定时换 IP：每分钟检查各节点 WARP 账户的 auto_rotate_hours 是否到点（0=关闭）。
// 到点换 IP = 重新注册设备 → 重组 wireguard 出站 → bump config → 节点秒级拉取生效。
Schedule::command('xray:warp-rotate')->everyMinute()->name('xray-warp-rotate')->withoutOverlapping();
