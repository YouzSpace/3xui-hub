<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('plans', function (Blueprint $table) {
            $table->id();
            $table->string('name');
            $table->string('type', 16); // 'period' | 'total'
            // 周期套餐字段
            $table->integer('months')->nullable();          // 周期月数
            $table->bigInteger('monthly_traffic')->nullable(); // 当月流量上限（字节）
            $table->bigInteger('period_traffic')->nullable();  // 周期总流量上限（字节）
            // 总量套餐字段
            $table->bigInteger('total_traffic')->nullable();   // 总流量上限（字节）
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('plans');
    }
};
