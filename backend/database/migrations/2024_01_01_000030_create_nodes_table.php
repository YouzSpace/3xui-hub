<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * ControlHub nodes 表（system-design §5.1）。
 * password / api_key 入库加密（Node 模型 accessor）。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('nodes', function (Blueprint $table) {
            $table->id();
            $table->string('name');
            $table->string('host');
            $table->integer('port')->default(443);
            $table->string('scheme', 8)->default('https');
            $table->string('web_base_path')->default('');
            $table->string('username')->default('');
            $table->text('password')->nullable();        // 加密
            $table->text('api_key')->nullable();          // 加密
            $table->boolean('enabled')->default(true);
            $table->string('status', 16)->default('offline'); // online|offline|maintenance
            $table->integer('latency')->default(0);
            $table->timestamp('last_check_at')->nullable();
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('nodes');
    }
};
