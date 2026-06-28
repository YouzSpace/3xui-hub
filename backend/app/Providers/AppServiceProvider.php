<?php

namespace App\Providers;

use App\Drivers\DriverRegistry;
use App\Drivers\NodeDriverFactory;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\ServiceProvider;

class AppServiceProvider extends ServiceProvider
{
    /**
     * Register any application services.
     */
    public function register(): void
    {
        // 驱动注册中心（单例）
        $this->app->singleton(DriverRegistry::class);

        // 节点驱动工厂（单例）
        $this->app->singleton(NodeDriverFactory::class, function ($app) {
            return new NodeDriverFactory($app->make(DriverRegistry::class));
        });
    }

    /**
     * Bootstrap any application services.
     */
    public function boot(): void
    {
        // SQLite 优化（仅当使用 SQLite 时生效）
        if (config('database.default') === 'sqlite') {
            try {
                DB::statement('PRAGMA journal_mode=WAL;');
                DB::statement('PRAGMA foreign_keys=ON;');
            } catch (\Throwable $e) {
                // 数据库未就绪时忽略
            }
        }
    }
}
