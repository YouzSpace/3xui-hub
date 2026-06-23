<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * ControlHub traffic_snapshots 表（system-design §5.1）。
 * 每次 sync 写一条，下次以最新 snapshot 为基准算增量 → 幂等。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('traffic_snapshots', function (Blueprint $table) {
            $table->id();
            $table->foreignId('user_id')->constrained()->cascadeOnDelete();
            $table->foreignId('node_id')->constrained()->cascadeOnDelete();
            $table->bigInteger('upload')->default(0);
            $table->bigInteger('download')->default(0);
            $table->timestamps();

            $table->index(['user_id', 'node_id', 'created_at']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('traffic_snapshots');
    }
};
