<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * node_warp_accounts 表：Cloudflare WARP 账户（每节点一份）。
 *
 * 异机架构：WARP 注册在面板做（纯 HTTP 调 CF API），生成的 wireguard
 * 私钥随「面板渲染 config → 节点拉取」链路下发 —— 正好复用现有通道。
 *
 * 一个节点一份 device（注册得到 device_id/access_token/license 与 peer 参数）；
 * 「应用到节点」时组装 wireguard 出站写入 xray_outbounds（tag=warp），
 * 「换 IP」= 重新注册 device → 更新本表 → 重组出站 → bump config 版本。
 *
 * 敏感字段（access_token / private_key）Crypt 加密存储。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('node_warp_accounts', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->unique()->constrained()->cascadeOnDelete();
            $table->string('device_id', 64)->nullable();
            $table->text('access_token')->nullable(); // 加密
            $table->text('private_key')->nullable(); // wireguard 私钥（加密）
            $table->string('public_key', 64)->nullable(); // 本端公钥（注册用）
            $table->string('peer_public_key', 128)->nullable(); // CF 侧 peer 公钥
            $table->string('peer_endpoint', 190)->nullable();
            $table->json('addresses')->nullable(); // ["172.16.0.2/32","fd01::2/128"]
            $table->json('reserved')->nullable(); // [0,0,0]
            $table->string('license_key', 128)->nullable(); // WARP+ license（可选）
            $table->string('client_id', 64)->nullable(); // CF config.client_id（reserved 来源）
            $table->unsignedSmallInteger('auto_rotate_hours')->default(0); // 定时换 IP 间隔（0=关闭）
            $table->timestamp('last_rotate_at')->nullable();
            $table->boolean('enabled')->default(false); // 是否已应用到节点（产生 warp 出站）
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('node_warp_accounts');
    }
};
