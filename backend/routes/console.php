<?php

use App\Jobs\BanCheckJob;
use App\Jobs\HealthCheckJob;
use App\Jobs\SyncTrafficJob;
use Illuminate\Foundation\Inspiring;
use Illuminate\Support\Facades\Artisan;
use Illuminate\Support\Facades\Schedule;

Artisan::command('inspire', function () {
    $this->comment(Inspiring::quote());
})->purpose('Display an inspiring quote');

// M7.7 流量同步：每 5 分钟
Schedule::job(new SyncTrafficJob())->everyFiveMinutes()->name('sync-traffic')->withoutOverlapping();

// M9.3 节点健康检查：每分钟
Schedule::job(new HealthCheckJob())->everyMinute()->name('health-check')->withoutOverlapping();

// M8.3 封禁检查：每 5 分钟（与流量同步错开由队列消化）
Schedule::job(new BanCheckJob())->everyFiveMinutes()->name('ban-check')->withoutOverlapping();
