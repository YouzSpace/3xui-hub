<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Support\Facades\DB;

return new class extends Migration
{
    public function up(): void
    {
        // 当前只有索引没有外键，先删索引再加 CASCADE 外键（MySQL 自动建索引）
        DB::statement('ALTER TABLE orders DROP INDEX orders_plan_id_foreign, ADD CONSTRAINT orders_plan_id_foreign FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE');
    }

    public function down(): void
    {
        DB::statement('ALTER TABLE orders DROP FOREIGN KEY orders_plan_id_foreign');
        DB::statement('ALTER TABLE orders ADD INDEX orders_plan_id_foreign (plan_id)');
    }
};
