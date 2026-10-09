<?php

namespace App\Services;

use App\Jobs\SendMailJob;
use App\Models\MailLog;
use App\Models\User;
use Illuminate\Database\Eloquent\Builder;

/**
 * 批量发信 / 按月定时发信 共用的收件人构造 + 队列发信。
 *
 * 从 EmailController::batchSend 抽出：两条链路（手动批量、每月定时）收件人规则
 * 必须完全一致——手动点一下发 100 人，到点月发也是同一套 100 人，不能各写各的。
 */
class MailBatchService
{
    /** 收件人没解析出来时抛（single 找不到人 / filter 没命中），调用方决定怎么报 */
    public const ERR_NO_RECIPIENT = 'no_recipient';

    /**
     * 按发信配置解析收件人。
     *
     * @param array $data mode(all|filter|single) / plan_id / user_status / single_query
     * @return \Illuminate\Support\Collection 用户集合（values() 过，下标连续）
     * @throws \RuntimeException ERR_NO_RECIPIENT
     */
    public function resolveRecipients(array $data): \Illuminate\Support\Collection
    {
        if (($data['mode'] ?? '') === 'single') {
            $q = trim((string) ($data['single_query'] ?? ''));
            if ($q === '') {
                throw new \RuntimeException(self::ERR_NO_RECIPIENT);
            }
            // 单发：邮箱精确或用户 ID 精确，最多命中 1 人
            $user = User::where('email', $q)->orWhere('id', ctype_digit($q) ? (int) $q : 0)->first();
            if ($user === null) {
                throw new \RuntimeException(self::ERR_NO_RECIPIENT);
            }

            return collect([$user]);
        }

        $recipients = $this->recipientQuery($data)->get();
        if ($recipients->isEmpty()) {
            throw new \RuntimeException(self::ERR_NO_RECIPIENT);
        }

        // values() 拿 0..n-1 连续下标：Eloquent 集合 keys() 是主键，
        // 直接拿它算「第几分钟发」会得到错误的错开量。
        return $recipients->values();
    }

    /** 按筛选条件构造收件人查询（all / filter，与 single 无关） */
    private function recipientQuery(array $data): Builder
    {
        $q = User::with('plan');

        if (!empty($data['plan_id'])) {
            $q->where('plan_id', (int) $data['plan_id']);
        }

        switch ($data['user_status'] ?? '') {
            case 'over':
                // 超量：周期或总量任一超限
                $q->where(function ($w) {
                    $w->where(function ($x) {
                        $x->where('traffic_limit', '>', 0)->whereColumn('traffic_used', '>=', 'traffic_limit');
                    })->orWhere(function ($x) {
                        $x->where('monthly_traffic_limit', '>', 0)
                          ->whereColumn('monthly_traffic_used', '>=', 'monthly_traffic_limit');
                    });
                });
                break;
            case 'expired':
                $q->whereNotNull('expired_at')->where('expired_at', '<', now());
                break;
            case 'normal':
            default:
                // 正常：有套餐、未禁用、未过期、未超量。
                // 必须有套餐：按产品规则套餐到期即变「无套餐」，这类账号 expired_at
                // 为空、traffic_limit 为 0，不显式排除就会落进「正常」里，月月收到群发。
                $q->whereNotNull('plan_id')
                  ->where('enabled', true)
                  ->where(function ($x) {
                      $x->whereNull('expired_at')->orWhere('expired_at', '>', now());
                  })
                  ->where(function ($x) {
                      $x->where('traffic_limit', '<=', 0)
                        ->orWhereColumn('traffic_used', '<', 'traffic_limit');
                  });
                break;
        }

        return $q;
    }

    /** 全局限速配置：管理员在「限制」页配置（mail_rate_limit_*），批量发信与按月定时发信共用 */
    public static function rateLimitConfig(): array
    {
        return [
            'enabled'    => \App\Models\SiteConfig::getValue('mail_rate_limit_enabled') === '1',
            'per_minute' => max(1, (int) (\App\Models\SiteConfig::getValue('mail_rate_per_minute') ?: 30)),
        ];
    }

    /**
     * 给一批用户入队发信（限速错开 + 逐用户渲染变量 + 落 batch 号）。
     * 限速读全局配置 rateLimitConfig()（「限制」页），调用方不需要传。
     *
     * @return int 入队封数
     */
    public function dispatchToUsers(\Illuminate\Support\Collection $users, string $subject, string $body, int $batchId, string $type = MailLog::TYPE_BATCH): int
    {
        $total = $users->count();
        $rate = self::rateLimitConfig();
        $perMinute = $rate['per_minute'];

        foreach ($users as $i => $user) {
            $delaySeconds = $rate['enabled'] ? intdiv($i, $perMinute) * 60 : 0;

            $job = SendMailJob::dispatch(
                toEmail: (string) $user->email,
                subject: $subject,
                htmlBody: $this->renderForUser($body, $user),
                type: $type,
                userId: $user->id,
                batchId: $batchId,
            );

            if ($delaySeconds > 0) {
                $job->delay(now()->addSeconds($delaySeconds));
            }
        }

        return $total;
    }

    /** 为单个用户渲染正文变量 */
    private function renderForUser(string $body, User $user): string
    {
        $limit = (int) $user->traffic_limit;
        $used  = (int) $user->traffic_used;
        $vars = [
            '{{email}}'         => (string) $user->email,
            '{{user_id}}'       => (string) $user->id,
            '{{site_title}}'    => \App\Models\SiteConfig::getValue('site_title', 'ControlHub'),
            '{{used}}'          => $this->formatBytes($used),
            '{{limit}}'         => $this->formatBytes($limit),
            '{{percent}}'       => (string) ($limit > 0 ? round($used / $limit * 100, 1) : 0),
            '{{plan_name}}'     => $user->plan?->name ?? '无',
            '{{expire_date}}'   => $user->expired_at?->format('Y-m-d') ?? '—',
            '{{days_left}}'     => (string) ($user->expired_at ? (int) ceil(now()->diffInDays($user->expired_at, false)) : 0),
            '{{subscribe_url}}' => $user->token ? url('/api/sub/' . $user->token) : '',
        ];
        return strtr($body, $vars);
    }

    private function formatBytes(int $bytes): string
    {
        if ($bytes <= 0) return '0 B';
        $units = ['B', 'KB', 'MB', 'GB', 'TB'];
        $i = max(0, min((int) floor(log($bytes, 1024)), count($units) - 1));
        return round($bytes / pow(1024, $i), 2) . ' ' . $units[$i];
    }
}
