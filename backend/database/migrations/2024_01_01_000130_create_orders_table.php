<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('orders', function (Blueprint $table) {
            $table->id();
            $table->string('order_no', 32)->unique()->comment('订单号');
            $table->foreignId('user_id')->constrained()->cascadeOnDelete();
            $table->foreignId('plan_id')->constrained()->cascadeOnDelete();
            $table->decimal('amount', 10, 2)->comment('订单金额(元)');
            $table->string('status', 16)->default('pending')->comment('pending/paid/failed/expired');
            $table->foreignId('payment_config_id')->nullable()->constrained('payment_configs')->nullOnDelete();
            $table->string('trade_no', 64)->nullable()->comment('第三方交易号');
            $table->timestamp('paid_at')->nullable();
            $table->string('pay_ip', 45)->nullable();
            $table->timestamps();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('orders');
    }
};
