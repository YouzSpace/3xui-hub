<?php

use Illuminate\Support\Facades\Schedule;

// 流量自动同步：每5分钟
Schedule::command('traffic:sync')->everyFiveMinutes()->name('traffic-sync')->withoutOverlapping();

// 每月1号重置周期套餐月流量
Schedule::command('traffic:monthly-reset')->monthlyOn(1, '00:00')->name('monthly-reset-traffic')->withoutOverlapping();
