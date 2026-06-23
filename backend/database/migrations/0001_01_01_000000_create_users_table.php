<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * ControlHub users 表（system-design §5.1）。
 * 替换 Laravel 默认 users schema：移除 name/password/email_verified_at/remember_token，
 * 改为业务字段 token、uuid、protocol、traffic_limit、traffic_used、expired_at、enabled。
 * 保留同文件 password_reset_tokens、sessions（admin session 用 SESSION_DRIVER=database）。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('users', function (Blueprint $table) {
            $table->id();
            $table->string('email')->nullable();
            $table->string('token', 64)->unique();
            $table->uuid('uuid')->unique();
            $table->string('protocol', 16)->default('vless');
            $table->bigInteger('traffic_limit')->default(0);
            $table->bigInteger('traffic_used')->default(0);
            $table->timestamp('expired_at')->nullable();
            $table->boolean('enabled')->default(true);
            $table->timestamps();
        });

        Schema::create('password_reset_tokens', function (Blueprint $table) {
            $table->string('email')->primary();
            $table->string('token');
            $table->timestamp('created_at')->nullable();
        });

        Schema::create('sessions', function (Blueprint $table) {
            $table->string('id')->primary();
            $table->foreignId('user_id')->nullable()->index();
            $table->string('ip_address', 45)->nullable();
            $table->text('user_agent')->nullable();
            $table->longText('payload');
            $table->integer('last_activity')->index();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('users');
        Schema::dropIfExists('password_reset_tokens');
        Schema::dropIfExists('sessions');
    }
};
