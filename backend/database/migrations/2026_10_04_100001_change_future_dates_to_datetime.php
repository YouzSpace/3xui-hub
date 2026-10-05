<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

/*
 * 修复：套餐到期时间超过 2038-01-19 时写入失败（TIMESTAMP 上限）。
 *
 * 现象：有效期较长的套餐（如 600 个月）在支付下发/后台绑定时报
 * "Incorrect datetime value: '2076-10-04 00:00:00' for column users.expired_at"，
 * 整条用户记录写入失败，套餐无法下发。
 *
 * 根因：MySQL TIMESTAMP 类型最大只能存到 2038-01-19 03:14:07（UTC）。
 * 改为 DATETIME（最大 9999-12-31）后，长效套餐即可正常写入。
 *
 * 范围：只改「会存未来时间」的业务字段；created_at / paid_at 等
 * 存当前时间的字段不受影响，暂不改动，保持最小变更。
 *
 * 说明：down() 故意不做反向变更——DATETIME 改回 TIMESTAMP 会让已写入的
 * 长效日期（>2038）再次失效，属于危险操作；如确需回滚请先清理超限数据。
 */
return new class extends Migration
{
    public function up(): void
    {
        Schema::table('users', function (Blueprint $table) {
            $table->dateTime('expired_at')->nullable()->change();
            $table->dateTime('next_traffic_reset_at')
                ->nullable()
                ->comment('下次流量重置时间')
                ->change();
        });

        Schema::table('discount_codes', function (Blueprint $table) {
            $table->dateTime('expires_at')
                ->nullable()
                ->comment('有效期，null = 永久')
                ->change();
        });
    }

    public function down(): void
    {
        // 有意留空，原因见文件头注释。
    }
};
