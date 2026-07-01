<?php

use Illuminate\Support\Facades\Schedule;

// 流量自动同步：每5分钟
Schedule::command('traffic:sync')->everyFiveMinutes()->name('traffic-sync')->withoutOverlapping();

// 每天检查并重置周期套餐月流量（基于用户的 next_traffic_reset_at）
Schedule::command('traffic:monthly-reset')->dailyAt('00:00')->name('monthly-reset-traffic')->withoutOverlapping();
