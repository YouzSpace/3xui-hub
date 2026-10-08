<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_inbounds 表：xray 节点自建入站（协议不写死，面板侧管理）。
 *
 * 每个节点独立一份入站列表；渲染 config 时把「用户 clients（按协议匹配）」+
 * 「Reality 私钥（解密注入）」动态组装进对应入站片段。
 *
 * 字段口径：
 * - protocol：vless / vmess / trojan / shadowsocks（面板管理的主流四类）
 * - settings：协议级参数（不含 clients 用户列表，渲染时注入）
 * - stream_settings：传输 + 安全参数；security=reality 时 realitySettings
 *   只存 dest/serverNames/shortIds 等公开参数 —— 私钥单独进 reality_private_key（加密）
 * - reality_private_key：X25519 私钥（Crypt 加密；与 xray x25519 同 base64url 格式，已对拍验证）
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('xray_inbounds', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->string('tag', 64); // 同节点内唯一（xray 链路定位 + 指令通道按 tag 操作）
            $table->string('protocol', 32); // vless | vmess | trojan | shadowsocks
            $table->unsignedInteger('port');
            $table->string('listen', 64)->default('0.0.0.0');
            $table->json('settings')->nullable();
            $table->json('stream_settings')->nullable();
            $table->json('sniffing')->nullable();
            $table->text('reality_private_key')->nullable();
            $table->boolean('enabled')->default(true);
            $table->unsignedInteger('sort')->default(0);
            $table->timestamps();

            $table->unique(['node_id', 'tag']);
            $table->index(['node_id', 'enabled']); // 渲染/指令通道：按节点取启用入站
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('xray_inbounds');
    }
};
