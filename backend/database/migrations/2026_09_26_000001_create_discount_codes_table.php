<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 幂等：备份导入自带表结构但不带 migrations 记录，重复执行会撞 1050
        if (Schema::hasTable('discount_codes')) {
            return;
        }

        Schema::create('discount_codes', function (Blueprint $table) {
            $table->id();
            $table->string('code', 32)->unique()->comment('码本身，大写');
            $table->string('source', 16)->comment('invite | redeem | admin');
            $table->unsignedBigInteger('user_id')->nullable()->comment('持有者（admin 生成的为 null）');
            $table->decimal('discount', 3, 2)->comment('0.90 表示 9 折');
            $table->unsignedInteger('max_uses')->default(1)->comment('该码总可用次数');
            $table->unsignedInteger('used_count')->default(0)->comment('已使用次数');
            $table->unsignedInteger('max_uses_per_user')->default(1)->comment('每用户可用次数');
            $table->timestamp('expires_at')->nullable()->comment('有效期，null = 永久');
            $table->string('note', 255)->nullable()->comment('自定义说明文字（展示给用户）');
            $table->string('status', 16)->default('active')->comment('active | used_up | expired | refreshed');
            $table->unsignedBigInteger('plan_id')->nullable()->comment('绑定套餐，null = 不限');
            $table->unsignedBigInteger('source_order_id')->nullable()->comment('兑换码来源订单 id（发奖幂等）');
            $table->timestamp('refreshed_at')->nullable()->comment('邀请码最近刷新时间');
            $table->timestamp('next_refresh_at')->nullable()->comment('邀请码下次刷新时间');
            $table->timestamps();

            $table->index(['user_id', 'source']);
            $table->index(['source', 'status']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('discount_codes');
    }
};
