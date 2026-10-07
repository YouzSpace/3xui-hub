<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 第三方订阅链接表。
 * 管理员接入外部机场订阅，后台定时拉取并缓存节点链接，注入到套餐用户的订阅里。
 * 节点在用户端统一命名「地区+序号」，不暴露机场原名（第三方保密）。
 */
return new class extends Migration
{
    public function up(): void
    {
        // 表已存在（备份导入 / 重复执行）→ 跳过，避免 1050 撞车
        if (Schema::hasTable('third_party_subs')) {
            return;
        }

        Schema::create('third_party_subs', function (Blueprint $table) {
            $table->id();
            $table->string('name', 64)->comment('管理员备注名（仅后台可见）');
            $table->text('url')->comment('第三方订阅链接（含机场 token，仅管理端展示）');
            $table->boolean('enabled')->default(true)->comment('这条订阅是否参与注入');
            // 拉取缓存：多行节点链接（ss/vless/vmess/trojan/hysteria2…），一行一条
            // mediumText（16MB）：真实机场几百上千条链接（带超长参数）远超 text 的 64KB 上限，
            // 用 mediumText 兜底；已建过表的库由 000004 迁移负责列升级。
            $table->mediumText('cached_links')->nullable()->comment('最近一次拉取到的节点链接缓存');
            $table->unsignedInteger('node_count')->default(0)->comment('最近一次拉取到的节点数');
            $table->timestamp('last_fetched_at')->nullable()->comment('最近一次拉取时间');
            $table->string('last_status', 16)->default('pending')->comment('pending/success/failed');
            $table->string('last_error', 255)->nullable()->comment('最近一次拉取失败原因');
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('third_party_subs');
    }
};
