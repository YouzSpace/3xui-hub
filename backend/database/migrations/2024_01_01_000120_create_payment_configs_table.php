<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('payment_configs', function (Blueprint $table) {
            $table->id();
            $table->string('name', 64)->comment('配置名称');
            $table->string('gateway', 255)->comment('支付网关地址');
            $table->string('query_gateway', 255)->nullable()->comment('查询网关地址');
            $table->string('member_id', 64)->comment('商户号');
            $table->string('api_key', 255)->comment('API密钥');
            $table->string('notify_url', 255)->nullable()->comment('回调地址');
            $table->string('bank_code', 64)->default('alipay')->comment('支付方式编码');
            $table->boolean('enabled')->default(true);
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('payment_configs');
    }
};
