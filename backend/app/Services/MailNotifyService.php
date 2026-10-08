<?php

namespace App\Services;

use App\Models\SiteConfig;
use App\Models\User;

/**
 * 邮件自动通知配置与渲染。
 *
 * 设计要点：
 * - 所有通知项**默认关闭**（notify_*_enabled 未配置时视为false），管理员需显式开启。
 * - 配置存SiteConfig（key-value），无需改表结构。
 * - 渲染变量：{{email}} {{user_id}} {{site_title}} {{used}} {{limit}} {{percent}}
 *            {{plan_name}} {{expire_date}} {{days_left}} {{subscribe_url}}
 */
class MailNotifyService
{
    /** 通知项定义：key => [场景名, 默认标题, 默认正文, 默认收件人类型] */
    public const SCENES = [
        'traffic_exhausted' => [
            'label'  => '流量用尽',
            'title'  => '您的流量已用尽',
            'body'   => '<p>您好，</p><p>您的流量已用尽，流量已被暂停。请续费或重置流量后继续使用。</p>',
            'to'     => 'user',
        ],
        'traffic_almost' => [
            'label'  => '流量即将用尽',
            'title'  => '您的流量即将用尽',
            'body'   => '<p>您好，</p><p>您的流量已使用 {{percent}}%，请及时续费，避免服务中断。</p>',
            'to'     => 'user',
        ],
        'expiring' => [
            'label'  => '套餐即将到期',
            'title'  => '您的套餐即将到期',
            'body'   => '<p>您好，</p><p>您的套餐将于 {{expire_date}} 到期（剩余 {{days_left}} 天），请及时续费。</p>',
            'to'     => 'user',
        ],
        'expired' => [
            'label'  => '套餐已到期',
            'title'  => '您的套餐已到期',
            'body'   => '<p>您好，</p><p>您的套餐已于 {{expire_date}} 到期，流量已关闭。续费后可继续使用。</p>',
            'to'     => 'user',
        ],
        'no_plan' => [
            'label'  => '无套餐',
            'title'  => '您当前没有有效套餐',
            'body'   => '<p>您好，</p><p>您当前没有有效套餐，请选择套餐后使用。</p>',
            'to'     => 'user',
        ],
    ];

    /** 配置键：通知开关 + 可调参数 */
    public const CONFIG_KEYS = [
        'notify_traffic_exhausted_enabled',
        'notify_traffic_exhausted_title',
        'notify_traffic_exhausted_body',
        'notify_traffic_exhausted_to',

        'notify_traffic_almost_enabled',
        'notify_traffic_almost_threshold',
        'notify_traffic_almost_title',
        'notify_traffic_almost_body',
        'notify_traffic_almost_to',

        'notify_expiring_enabled',
        'notify_expiring_days',
        'notify_expiring_title',
        'notify_expiring_body',
        'notify_expiring_to',

        'notify_expired_enabled',
        'notify_expired_title',
        'notify_expired_body',
        'notify_expired_to',

        'notify_no_plan_enabled',
        'notify_no_plan_title',
        'notify_no_plan_body',
        'notify_no_plan_to',

        'notify_admin_email',
    ];

    /** 某场景是否开启（未配置 → false，即默认关闭） */
    public static function isEnabled(string $scene): bool
    {
        return SiteConfig::getValue("notify_{$scene}_enabled") === '1';
    }

    /** 读取某场景的通知配置（合并默认值） */
    public static function configFor(string $scene): array
    {
        $def = self::SCENES[$scene] ?? null;
        if ($def === null) {
            return [];
        }
        $cfg = SiteConfig::getMany([
            "notify_{$scene}_title",
            "notify_{$scene}_body",
            "notify_{$scene}_to",
        ]);

        return [
            'enabled' => self::isEnabled($scene),
            'label'   => $def['label'],
            'title'   => $cfg["notify_{$scene}_title"] !== '' ? $cfg["notify_{$scene}_title"] : $def['title'],
            'body'    => $cfg["notify_{$scene}_body"] !== '' ? $cfg["notify_{$scene}_body"] : $def['body'],
            'to'      => $cfg["notify_{$scene}_to"] !== '' ? $cfg["notify_{$scene}_to"] : $def['to'],
        ];
    }

    /** 全部通知项配置（供接口返回，扁平键与前端 loadNotify 的读取一致） */
    public static function allConfig(): array
    {
        $extra = SiteConfig::getMany([
            'notify_traffic_almost_threshold',
            'notify_expiring_days',
            'notify_admin_email',
        ]);

        $out = [];
        foreach (array_keys(self::SCENES) as $scene) {
            $cfg = self::configFor($scene);
            // enabled 存成 '1'/''（字符串）：前端用 === '1' 严格判断，布尔 true 会变成 false
            $out["notify_{$scene}_enabled"] = $cfg['enabled'] ? '1' : '';
            $out["notify_{$scene}_title"]   = $cfg['title'];
            $out["notify_{$scene}_body"]    = $cfg['body'];
            $out["notify_{$scene}_to"]      = $cfg['to'];
        }

        // 顶层扁平键：与前端 notify.notify_* 字段一一对应（原 _settings 折叠结构前端读不到）
        $out['notify_traffic_almost_threshold'] = (int) ($extra['notify_traffic_almost_threshold'] ?: 90);
        $out['notify_expiring_days']            = (int) ($extra['notify_expiring_days'] ?: 3);
        $out['notify_admin_email']              = $extra['notify_admin_email'];

        return $out;
    }

    /**
     * 渲染模板变量。
     *
     * @param string $scene 场景 key
     * @param User   $user  收件用户
     */
    public static function render(string $scene, User $user): array
    {
        $cfg = self::configFor($scene);
        if ($cfg === []) {
            return [];
        }

        $limit = (int) $user->traffic_limit;
        $used  = (int) $user->traffic_used;
        $percent = $limit > 0 ? round($used / $limit * 100, 1) : 0;

        $expireDate = $user->expired_at?->format('Y-m-d') ?? '—';
        $daysLeft   = $user->expired_at
            ? (int) ceil(now()->diffInDays($user->expired_at, false))
            : 0;

        $siteTitle = SiteConfig::getValue('site_title', 'ControlHub');
        $subUrl    = $user->token ? url('/api/sub/' . $user->token) : '';

        $vars = [
            '{{email}}'         => (string) $user->email,
            '{{user_id}}'       => (string) $user->id,
            '{{site_title}}'    => $siteTitle,
            '{{used}}'          => self::formatBytes($used),
            '{{limit}}'         => self::formatBytes($limit),
            '{{percent}}'       => (string) $percent,
            '{{plan_name}}'     => $user->plan?->name ?? '无',
            '{{expire_date}}'   => $expireDate,
            '{{days_left}}'     => (string) $daysLeft,
            '{{subscribe_url}}' => $subUrl,
        ];

        return [
            'subject' => strtr($cfg['title'], $vars),
            'body'    => strtr($cfg['body'], $vars),
            'to_type' => $cfg['to'],
        ];
    }

    /** 字节转可读文本（用于 {{used}} {{limit}} 展示） */
    private static function formatBytes(int $bytes): string
    {
        if ($bytes <= 0) return '0 B';
        $units = ['B', 'KB', 'MB', 'GB', 'TB'];
        $i = (int) floor(log($bytes, 1024));
        $i = max(0, min($i, count($units) - 1));
        return round($bytes / pow(1024, $i), 2) . ' ' . $units[$i];
    }
}