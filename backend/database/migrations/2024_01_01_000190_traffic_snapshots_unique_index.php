<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Schema;

/**
 * 快照表改为 upsert 模式：每 user+node 只保留1条记录。
 * 添加 UNIQUE(user_id, node_id) 索引，支持 ON DUPLICATE KEY UPDATE。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 1. 删除重复记录（保留每组最新的一条）
        DB::statement('SET SESSION sql_mode = REPLACE(@@sql_mode, "ONLY_FULL_GROUP_BY", "")');
        DB::statement("
            DELETE FROM traffic_snapshots
            WHERE id NOT IN (
                SELECT max_id FROM (
                    SELECT MAX(id) as max_id
                    FROM traffic_snapshots
                    GROUP BY user_id, node_id
                ) as tmp
            )
        ");

        // 2. 先加唯一索引（保证外键列有索引覆盖）
        Schema::table('traffic_snapshots', function (Blueprint $table) {
            $table->unique(['user_id', 'node_id']);
        });

        // 3. 再删旧的复合索引（此时唯一索引已覆盖外键列）
        Schema::table('traffic_snapshots', function (Blueprint $table) {
            $table->dropIndex(['user_id', 'node_id', 'created_at']);
        });
    }

    public function down(): void
    {
        Schema::table('traffic_snapshots', function (Blueprint $table) {
            $table->dropUnique(['user_id', 'node_id']);
            $table->index(['user_id', 'node_id', 'created_at']);
        });
    }
};
