<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_outbounds 表：xray 节点自定义出站（中转/出口切换）。
 *
 * 每个节点独立一份出站列表；渲染 config 时在系统内置
 * direct/blocked 之外，按 sort 追加本表记录。
 *
 * 字段口径：
 * - protocol：freedom / blackhole / vmess / vless / trojan / shadowsocks / socks / http / wireguard
 * - settings：协议参数（wireguard 的 secretKey 除外 —— 单独加密列）
 * - secret_key：wireguard 出站私钥（Crypt 加密）
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('xray_outbounds', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->string('tag', 64); // 同节点内唯一（路由规则按 tag 引用）
            $table->string('protocol', 32);
            $table->json('settings')->nullable();
            $table->json('stream_settings')->nullable();
            $table->text('secret_key')->nullable(); // wireguard 私钥（加密）
            $table->boolean('enabled')->default(true);
            $table->unsignedInteger('sort')->default(0);
            $table->string('remark', 128)->nullable();
            $table->timestamps();

            $table->unique(['node_id', 'tag']);
            $table->index(['node_id', 'enabled']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('xray_outbounds');
    }
};
