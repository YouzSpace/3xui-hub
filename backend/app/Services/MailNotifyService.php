<?php

namespace App\Services;

use App\Models\SiteConfig;
use App\Models\User;
use Illuminate\Support\Facades\Cache;

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

    /**
     * 该用户当前的通知「时代号」。
     *
     * 防重 key 里带上它：时代号一变，该用户所有场景的旧防重标记全部自动作废
     * （旧 key 永远不会再被读到），不需要逐场景去清标记。
     * 时代号只会被 restoreReminders() 递增；Cache 丢失时回落 0，代价仅是
     * 「极端情况下多收一封」，不会漏发。
     */
    private static function claimGenerationKey(User $user): string
    {
        return "mail_notify_gen:{$user->id}";
    }

    private static function generationOf(User $user): int
    {
        return (int) Cache::get(self::claimGenerationKey($user), 0);
    }

    /**
     * 恢复该用户的所有自动通知（重新购买 / 续费 / 重置流量后调用）。
     *
     * 原理：递增时代号 → 该用户所有场景的「已发过」标记立即全部失效，
     * 之后任何场景再次触发条件都会重新提醒一次。
     * 调用方：applyPlan（购买/续费/管理员改套餐）、renew（总量续费）、
     * resetTraffic（管理员重置）、completeResetOrder（付费重置订单）。
     */
    public static function restoreReminders(User $user): void
    {
        $key = self::claimGenerationKey($user);

        Cache::put($key, self::generationOf($user) + 1, now()->addDays(400));
    }

    /**
     * 该用户该场景是否还能发通知 —— **同一时代只发一次**，与自然月无关。
     *
     * 返回 true = 该时代第一封（继续发），false = 已发过（跳过）。
     * 所有自动通知的防重口径只有这一处（扫描类 3 个场景 + 流量用尽）。
     *
     * 与原月度去重的差异：不再按 Ym 月历格过期（跨月重发刷屏），
     * 而是跟着「用户状态时代」走 —— 只有重新购买/续费/重置（restoreReminders
     * 递增时代号）才会解锁下一封。用户一直不续费就永远只收一封。
     *
     * 去重维度跟着「收件人」走，不跟着被通知的用户：
     * - 发给用户本人 → 按邮箱（同一邮箱的多个账号只收一封，不把人刷屏；
     *   邮箱统一小写去空白，避免换大小写绕开计数）
     * - 发给管理员 → 按触发用户（管理员的通知不能被先到的用户占掉坑，否则
     *   N 个用户到期只会收到 1 封，看起来像只有 1 个人有问题）
     *
     * **调用时机必须在「渲染 + 解析出收件地址」之后**：渲染早就落标记的话，
     * 管理员还没配收件邮箱时这一代都发不出去（补上邮箱也要等恢复提醒才重发）。
     * 落标记而不是查 mail_logs，是因为日志要等队列真正发出才写，
     * 而扫描每 5 分钟一轮，队列没跑完时靠日志判断会把同一封发两遍。
     *
     * TTL 取 365 天（覆盖一次套餐周期绰绰有余）：标记过期=回到「未发过」，
     * 极端闲置一年多的用户最多多收一封，可接受。
     */
    public static function claimOnce(string $scene, User $user, string $toType, string $to): bool
    {
        return Cache::add(self::claimKey($scene, $user, $toType, $to), 1, now()->addDays(365));
    }

    /**
     * 该场景 / 该收件人在当前时代的防重 key（口径只有这一处）。
     *
     * 单独暴露出来是给「发送失败要补发」用：入队时把这个 key 一并交给 SendMailJob，
     * 真发失败就 forget 掉它，下一轮扫描（5 分钟一轮）会自动补发；
     * 否则 SMTP 抖一下这一封就永久丢了 —— 标记已落、触发条件仍在，却永远不会再发。
     */
    public static function claimKey(string $scene, User $user, string $toType, string $to): string
    {
        $who = $toType === 'admin'
            ? 'admin:' . $user->id
            : 'email:' . strtolower(trim($to));

        return "mail_notify:{$scene}:{$who}:g" . self::generationOf($user);
    }

    /** 释放防重标记（发送失败时调用，让下一轮扫描补发）。 */
    public static function releaseClaim(?string $key): void
    {
        if ($key !== null && $key !== '') {
            Cache::forget($key);
        }
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