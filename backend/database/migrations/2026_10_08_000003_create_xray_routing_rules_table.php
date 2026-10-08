<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_routing_rules 表：xray 节点路由规则（按序渲染进 routing.rules）。
 *
 * 基础三件套（阻私有网段 / 阻 BT）由渲染器作为内置规则先行输出，
 * 本表承载管理员自定义规则（排在基础规则之后，可用 sort 微调顺序）。
 *
 * 字段口径（与 xray routing rule 对齐）：
 * - domains：["geosite:cn","domain:example.com",...]
 * - ips：["geoip:cn","1.1.1.1/32",...]
 * - port：单端口 "443" / 范围 "1000-2000" / 离开（null=任意），字符串存储
 * - network：tcp / udp（null=不限）
 * - protocol：http / tls / bittorrent（sniffing 识别出的协议）
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('xray_routing_rules', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->json('domains')->nullable();
            $table->json('ips')->nullable();
            $table->string('port', 64)->nullable();
            $table->string('network', 16)->nullable();
            $table->string('protocol', 16)->nullable();
            $table->string('inbound_tag', 64)->nullable();
            $table->string('outbound_tag', 64); // 目标出站（rule 必须有出口）
            $table->string('remark', 128)->nullable();
            $table->boolean('enabled')->default(true);
            $table->unsignedInteger('sort')->default(0);
            $table->timestamps();

            $table->index(['node_id', 'enabled', 'sort']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('xray_routing_rules');
    }
};
