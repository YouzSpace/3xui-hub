<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：列已存在（备份导入 / 重复执行）→ 跳过，避免 1060 duplicate column
        if (! Schema::hasColumn('users', 'used_invite_code_id')) {
            Schema::table('users', function (Blueprint $table) {
                $table->unsignedBigInteger('used_invite_code_id')->nullable()->after('traffic_disabled_at')
                    ->comment('该用户用过的邀请码 id，非空即永久锁死');
            });
        }

        if (! Schema::hasColumn('users', 'invite_code_used_at')) {
            Schema::table('users', function (Blueprint $table) {
                $table->timestamp('invite_code_used_at')->nullable()->after('used_invite_code_id')
                    ->comment('邀请码使用时间');
            });
        }
    }

    public function down(): void
    {
        if (Schema::hasColumn('users', 'invite_code_used_at')) {
            Schema::table('users', function (Blueprint $table) {
                $table->dropColumn('invite_code_used_at');
            });
        }

        if (Schema::hasColumn('users', 'used_invite_code_id')) {
            Schema::table('users', function (Blueprint $table) {
                $table->dropColumn('used_invite_code_id');
            });
        }
    }
};
