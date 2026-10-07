<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * plans 加「本地节点 / 第三方订阅」两列。
 * include_local：套餐订阅是否包含自有节点（默认 true，老套餐行为不变）。
 * third_party_sub_ids：JSON 数组（third_party_subs.id），该套餐注入哪些第三方订阅；空 = 不含。
 */
return new class extends Migration
{
    public function up(): void
    {
        if (Schema::hasColumn('plans', 'include_local')) {
            return;
        }

        Schema::table('plans', function (Blueprint $table) {
            $table->boolean('include_local')->default(true)->after('is_active')
                ->comment('订阅是否包含本地节点');
            $table->text('third_party_sub_ids')->nullable()->after('include_local')
                ->comment('注入的第三方订阅 ID（JSON 数组，空 = 不含第三方）');
        });
    }

    public function down(): void
    {
        Schema::table('plans', function (Blueprint $table) {
            $table->dropColumn(['include_local', 'third_party_sub_ids']);
        });
    }
};
