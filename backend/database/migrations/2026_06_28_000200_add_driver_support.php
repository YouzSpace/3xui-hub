<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        // 1. nodes 表：加 driver 标识
        Schema::table('nodes', function (Blueprint $table) {
            $table->string('driver_type', 32)->default('3x-ui')->after('enabled');
            $table->string('driver_version', 16)->nullable()->after('driver_type');
            $table->json('driver_config')->nullable()->after('driver_version');
            $table->index(['driver_type', 'enabled']);
        });

        // 2. users 表： driver 专属标识
        Schema::table('users', function (Blueprint $table) {
            $table->string('driver_identifier', 128)->nullable()->after('uuid');
            $table->json('client_config')->nullable()->after('driver_identifier');
        });

        // 3. node_inbounds 表
        Schema::table('node_inbounds', function (Blueprint $table) {
            $table->string('driver_type', 32)->default('3x-ui')->after('protocol');
        });

        // 4. payment_configs 表：从硬编码字段 → JSON 配置
        Schema::table('payment_configs', function (Blueprint $table) {
            $table->string('driver_type', 32)->default('wwspay')->after('name');
            $table->json('driver_config')->nullable()->after('driver_type');
        });

        // 5. orders 表
        Schema::table('orders', function (Blueprint $table) {
            $table->string('currency', 8)->default('CNY')->after('amount');
            $table->string('payment_driver_type', 32)->nullable()->after('payment_config_id');
            $table->json('payment_metadata')->nullable()->after('payment_driver_type');
        });
    }

    public function down(): void
    {
        Schema::table('nodes', function (Blueprint $table) {
            $table->dropIndex(['driver_type', 'enabled']);
            $table->dropColumn(['driver_type', 'driver_version', 'driver_config']);
        });

        Schema::table('users', function (Blueprint $table) {
            $table->dropColumn(['driver_identifier', 'client_config']);
        });

        Schema::table('node_inbounds', function (Blueprint $table) {
            $table->dropColumn('driver_type');
        });

        Schema::table('payment_configs', function (Blueprint $table) {
            $table->dropColumn(['driver_type', 'driver_config']);
        });

        Schema::table('orders', function (Blueprint $table) {
            $table->dropColumn(['currency', 'payment_driver_type', 'payment_metadata']);
        });
    }
};
