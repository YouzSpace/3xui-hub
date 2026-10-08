<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * xray_outbound_traffic 表：按出站 tag 的流量累计（覆盖写，无历史）。
 *
 * 数据源：agent 每 5s report 里的 outbounds 段（statsquery pattern=outbound 聚合），
 * WS 服务收到后按 (node_id, tag) upsert —— 与 traffic_snapshots 的「覆盖写」口径一致。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('xray_outbound_traffic', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->string('tag', 128);
            $table->unsignedBigInteger('upload')->default(0);
            $table->unsignedBigInteger('download')->default(0);
            $table->timestamp('updated_at')->nullable();

            $table->unique(['node_id', 'tag']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('xray_outbound_traffic');
    }
};
