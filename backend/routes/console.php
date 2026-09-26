<?php

use App\Jobs\BanCheckJob;
use App\Jobs\HealthCheckJob;
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

// 任务超时兜底：超过阈值（config('tasks.stale_after_minutes')）未终态的异步任务标记失败，防止 running 永久停留
Schedule::call(fn () => app(\App\Services\AsyncTaskService::class)->timeoutStale())->everyFiveMinutes()->name('async-task-timeout')->withoutOverlapping();
