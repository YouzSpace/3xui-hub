<?php

namespace App\Providers;

use Illuminate\Support\Facades\DB;
use Illuminate\Support\ServiceProvider;

class AppServiceProvider extends ServiceProvider
{
    /**
     * Register any application services.
     */
    public function register(): void
    {
        //
    }

    /**
     * Bootstrap any application services.
     */
    public function boot(): void
    {
        // SQLite 优化：WAL 提升并发写，开启外键约束
        if (config('database.default') === 'sqlite') {
            try {
                DB::statement('PRAGMA journal_mode=WAL;');
                DB::statement('PRAGMA foreign_keys=ON;');
            } catch (\Throwable $e) {
                // 数据库未就绪时忽略（如 migrate 阶段）
            }
        }
    }
}
