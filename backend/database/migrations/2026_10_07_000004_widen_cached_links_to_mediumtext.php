<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Support\Facades\DB;

/**
 * third_party_subs.cached_links 由 TEXT(64KB) 升为 MEDIUMTEXT(16MB)。
 *
 * 现象：接入真实机场（如 ss-4e4，211 条 vless、单行 200+ 字符带超长 ech 参数）时，
 * 整段缓存超出 TEXT 的 64KB 上限，写入报「Data too long for column cached_links」。
 * 模拟测试源只有几条短链接，故未暴露；真实机场几百上千条会撞上。
 *
 * MEDIUMTEXT 上限 16MB，足够覆盖数千条节点。用原生 ALTER ... MODIFY 而非 change()
 * （跨 MySQL 版本 / 不依赖 doctrine/dbal），MODIFY 幂等，重复执行不报错。
 * down() 改回 TEXT。
 */
return new class extends Migration
{
    public function up(): void
    {
        if (!Schema::hasTable('third_party_subs')) {
            return; // 表尚未建（全新库由 000001 直接建成 mediumtext，无需本步）
        }

        DB::statement('ALTER TABLE `third_party_subs` MODIFY COLUMN `cached_links` MEDIUMTEXT NULL COMMENT \'最近一次拉取到的节点链接缓存\'');
    }

    public function down(): void
    {
        DB::statement('ALTER TABLE `third_party_subs` MODIFY COLUMN `cached_links` TEXT NULL COMMENT \'最近一次拉取到的节点链接缓存\'');
    }
};
