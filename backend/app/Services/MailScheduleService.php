<?php

namespace App\Services;

use App\Models\SiteConfig;

/**
 * 按月定时发信的配置存取。
 *
 * 管理员在「邮箱 → 定时」页配置一次，之后每月固定某天某点，后台扫描任务自动
 * 把配好的那封信发出去（收件人/标题/正文/限速全部复用批量发信那套）。
 *
 * 配置存 SiteConfig（key-value，跟自动通知一致，不改表）：
 * - schedule_mail_enabled   '1' 开启 / '' 关闭（默认关）
 * - schedule_mail_day       每月第 1-31 天（超出当月实际天数则当月跳过）
 * - schedule_mail_time      'HH:MM' 24 小时制
 * - schedule_mail_config    JSON：mode/plan_id/user_status/single_query/subject/body
 *                           （限速不在这里：统一读「限制」页的 mail_rate_limit_*）
 * - schedule_mail_armed_at  最近一次「关→开」的开启时刻（HH:MM 判定用：
 *                           开启晚于当月到点则当月不补发）
 *
 * 去重：一个自然月只发一轮。判断依据是「当月有没有成功发过」（看发信日志），
 * 所以同一轮扫描跑 N 次、或隔了几天才上线，都不会重发。
 */
class MailScheduleService
{
    public const KEY_ENABLED = 'schedule_mail_enabled';
    public const KEY_DAY     = 'schedule_mail_day';
    public const KEY_TIME    = 'schedule_mail_time';
    public const KEY_CONFIG  = 'schedule_mail_config';
    public const KEY_ARMED   = 'schedule_mail_armed_at';

    /** 是否开启（未配置/空串 = 关闭，默认关，与自动通知同口径） */
    public static function isEnabled(): bool
    {
        return SiteConfig::getValue(self::KEY_ENABLED) === '1';
    }

    /** 读全份配置，合并默认值；未配置 enabled 时 enabled=false */
    public static function config(): array
    {
        $raw = SiteConfig::getMany([self::KEY_ENABLED, self::KEY_DAY, self::KEY_TIME, self::KEY_CONFIG]);

        $cfg = json_decode((string) ($raw[self::KEY_CONFIG] ?? ''), true);
        if (!is_array($cfg)) {
            $cfg = [];
        }

        return [
            'enabled' => ($raw[self::KEY_ENABLED] ?? '') === '1',
            // 默认 1 号 09:00（常见「月初发账单/提醒」的直觉值）
            'day'     => (int) ($raw[self::KEY_DAY] ?: 1),
            'time'    => ($raw[self::KEY_TIME] ?? '') ?: '09:00',
            'config'  => $cfg,
        ];
    }

    /**
     * 每月第 day 天的实际日期（当月不足 day 天则跳过，返回 null）。
     * 例：day=31，2 月没有 31 号 → 当月不发。
     *
     * @param \Illuminate\Support\Carbon|null $at 基准时间，默认 now()
     */
    public static function nextSendAt(?int $day = null, ?\Illuminate\Support\Carbon $at = null): ?\Illuminate\Support\Carbon
    {
        $day = $day ?? (self::config()['day'] ?? 1);
        $at  = $at ?? now();

        $lastDay = (int) $at->copy()->endOfMonth()->day;
        if ($day < 1 || $day > $lastDay) {
            return null;
        }

        return $at->copy()->startOfMonth()->day($day)->setTimeFromTimeString(self::config()['time']);
    }

    /** 当月是否已成功发过一轮（去重依据）。发信日志 type=schedule 且 created_at 落在当月 */
    public static function alreadySentThisMonth(): bool
    {
        $thisMonth = now()->startOfMonth();
        return \App\Models\MailLog::where('type', \App\Models\MailLog::TYPE_SCHEDULE)
            ->where('created_at', '>=', $thisMonth)
            ->where('created_at', '<', $thisMonth->copy()->addMonthNoOverflow())
            ->exists();
    }

    /**
     * 最近一次「关→开」开启时刻。从未记录（老数据/未开启过）返回 null，
     * Job 按「长期开启」处理，不拦截补发。
     */
    public static function armedAt(): ?\Illuminate\Support\Carbon
    {
        $raw = SiteConfig::getValue(self::KEY_ARMED);
        if ($raw === '') {
            return null;
        }
        try {
            return \Illuminate\Support\Carbon::parse($raw);
        } catch (\Throwable) {
            return null;
        }
    }

    /**
     * 保存配置。$in 接受前端提交的扁平字段：
     * enabled / day / time / config（数组或 JSON 字符串）。
     */
    public static function save(array $in): array
    {
        $wasEnabled = self::isEnabled();

        $updates = [];

        $updates[self::KEY_ENABLED] = !empty($in['enabled']) ? '1' : '';

        $day = (int) ($in['day'] ?? 1);
        $updates[self::KEY_DAY] = $day < 1 ? '1' : ($day > 31 ? '31' : (string) $day);

        $time = (string) ($in['time'] ?? '09:00');
        if (!preg_match('/^([01]\d|2[0-3]):[0-5]\d$/', $time)) {
            $time = '09:00';
        }
        $updates[self::KEY_TIME] = $time;

        $cfg = $in['config'] ?? [];
        if (is_string($cfg)) {
            $cfg = json_decode($cfg, true) ?: [];
        }
        $updates[self::KEY_CONFIG] = json_encode($cfg, JSON_UNESCAPED_UNICODE);

        // 关→开转变：记下开启时刻。Job 用它区分「刚开启」与「长期开启」：
        // 开启时间晚于当月到点时刻 → 当月不补发（下月起按点发），避免管理员
        // 到点后才打开开关、保存完就立刻群发。开着状态下再次保存（改内容等）
        // 不刷新该时刻，保留「服务器错过当天、同月补发」的能力。
        if (!$wasEnabled && !empty($in['enabled'])) {
            $updates[self::KEY_ARMED] = now()->toDateTimeString();
        }

        SiteConfig::setMany($updates);

        return self::config();
    }
}
