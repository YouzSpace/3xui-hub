<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/**
 * 给 nodes 加 verify_ssl：自托管 3x-ui 面板证书常自签/链不完整，默认关闭校验。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::table('nodes', function (Blueprint $table) {
            $table->boolean('verify_ssl')->default(false)->after('enabled');
        });
    }

    public function down(): void
    {
        Schema::table('nodes', function (Blueprint $table) {
            $table->dropColumn('verify_ssl');
        });
    }
};
