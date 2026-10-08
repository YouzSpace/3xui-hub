<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * ControlHub node_api_commands 表（xray 节点双通道 API 指令队列）。
 *
 * xray 节点的用户级操作（建/改/删/开关用户）不走全量 config reload，
 * 而是由 Driver 下发一条 xray API 指令（adu/rmu），agent 经 execute
 * 长轮询取走、本地执行 `xray api ...` 后回执。
 *
 * - action = adu：payload 存「完整入站片段 JSON」（{"inbounds":[{tag,listen,port,protocol,settings:{clients:[...]}}]}），
 *   agent 落地成文件后执行 `xray api adu --server=... <file>`（真机验证 v26.3.27 格式）。
 * - action = rmu：payload 存 {"tag":"<inbound>","emails":["u1","u2"]}，
 *   agent 执行 `xray api rmu --server=... -tag=<tag> "u1" "u2"`。
 *
 * 全量 config 通道（结构变更/兜底）不经此表，走 GET /node-api/config + ETag。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('node_api_commands', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->foreignId('user_id')->nullable()->constrained()->nullOnDelete(); // 关联用户（对账用），rmu 可空
            $table->string('action', 16); // adu | rmu
            $table->json('payload')->nullable(); // 见类注释
            $table->string('status', 16)->default('pending')->index(); // pending | acknowledged | success | failed
            $table->text('result')->nullable(); // agent 回执（xray 命令 stdout / 错误）
            $table->timestamp('acknowledged_at')->nullable(); // agent 取走时刻
            $table->timestamp('executed_at')->nullable(); // agent 执行完成时刻
            $table->timestamps();

            $table->index(['node_id', 'status', 'created_at']); // agent 长轮询：按节点取 pending
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('node_api_commands');
    }
};
