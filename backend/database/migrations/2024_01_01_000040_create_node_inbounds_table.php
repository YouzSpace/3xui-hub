<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * ControlHub node_inbounds 表（system-design §5.1）。
 * 每节点每协议对应一个 3x-ui inbound_id（协议切换 attach/detach 用）。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('node_inbounds', function (Blueprint $table) {
            $table->id();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->string('protocol', 16);
            $table->bigInteger('inbound_id');
            $table->timestamps();

            $table->unique(['node_id', 'protocol']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('node_inbounds');
    }
};
