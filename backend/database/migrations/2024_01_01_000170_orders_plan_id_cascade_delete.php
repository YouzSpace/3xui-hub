<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Support\Facades\DB;

return new class extends Migration
{
    public function up(): void
    {
        // 先尝试删外键约束（服务器有），再删索引（本地只有索引）
        try { DB::statement('ALTER TABLE orders DROP FOREIGN KEY orders_plan_id_foreign'); } catch (\Exception $e) {}
        try { DB::statement('ALTER TABLE orders DROP INDEX orders_plan_id_foreign'); } catch (\Exception $e) {}
        DB::statement('ALTER TABLE orders ADD CONSTRAINT orders_plan_id_foreign FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE');
    }

    public function down(): void
    {
        DB::statement('ALTER TABLE orders DROP FOREIGN KEY orders_plan_id_foreign');
        DB::statement('ALTER TABLE orders ADD INDEX orders_plan_id_foreign (plan_id)');
    }
};
