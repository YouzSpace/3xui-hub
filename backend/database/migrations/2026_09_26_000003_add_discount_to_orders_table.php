<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (! Schema::hasColumn('orders', 'original_amount')) {
            Schema::table('orders', function (Blueprint $table) {
                $table->decimal('original_amount', 10, 2)->nullable()->after('amount')->comment('原价');
            });
        }

        if (! Schema::hasColumn('orders', 'discount_code_id')) {
            Schema::table('orders', function (Blueprint $table) {
                $table->unsignedBigInteger('discount_code_id')->nullable()->after('original_amount')
                    ->comment('使用的折扣码 id');
            });
        }

        if (! Schema::hasColumn('orders', 'discount_amount')) {
            Schema::table('orders', function (Blueprint $table) {
                $table->decimal('discount_amount', 10, 2)->nullable()->after('discount_code_id')
                    ->comment('实际优惠掉的金额');
            });
        }
    }

    public function down(): void
    {
        foreach (['discount_amount', 'discount_code_id', 'original_amount'] as $column) {
            if (Schema::hasColumn('orders', $column)) {
                Schema::table('orders', function (Blueprint $table) use ($column) {
                    $table->dropColumn($column);
                });
            }
        }
    }
};
