<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * node_inbounds 唯一约束改为 (node_id, protocol, inbound_id)，支持多入站。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::table('node_inbounds', function (Blueprint $table) {
            // MySQL 不允许删被外键引用的索引，先删外键
            $table->dropForeign(['node_id']);
            $table->dropUnique(['node_id', 'protocol']);
            $table->unique(['node_id', 'protocol', 'inbound_id']);
            $table->foreign('node_id')->references('id')->on('nodes')->cascadeOnDelete();
        });
    }

    public function down(): void
    {
        Schema::table('node_inbounds', function (Blueprint $table) {
            $table->dropForeign(['node_id']);
            $table->dropUnique(['node_id', 'protocol', 'inbound_id']);
            $table->unique(['node_id', 'protocol']);
            $table->foreign('node_id')->references('id')->on('nodes')->cascadeOnDelete();
        });
    }
};
