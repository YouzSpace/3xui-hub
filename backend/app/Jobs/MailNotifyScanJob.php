<?php

namespace App\Jobs;

use App\Models\MailLog;
use App\Models\SiteConfig;
use App\Models\User;
use App\Services\MailNotifyService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 扫描类通知：流量即将用尽 / 套餐即将到期 / 已到期。
 *
 * 这几类不能挂在某个同步动作上（流量用尽能挂BanService，但「即将」是阈值判断），
 * 所以由定时任务扫描。
 *
 * 「无套餐」场景已下线：按产品规则套餐到期即变无套餐，这类账号会长期存在，
 * 反复提醒没有意义（该提醒的是到期那一刻，由 expired 负责）。
 *
 * 防重复发信：靠 Cache 标记（入队那一刻就落），不用 mail_logs ——后者要等
 * 真正发出才写入，而本任务每 5 分钟一轮，队列未跑完时会重复发信。
 * 同一自然月同一场景同一收件人只发一封，口径统一在 MailNotifyService::claimMonthly()。
 */
class MailNotifyScanJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function handle(): void
    {
        $this->scanTrafficAlmost();
        $this->scanExpiring();
        $this->scanExpired();
    }

    /** 流量即将用尽（默认 90%，管理员可调） */
    private function scanTrafficAlmost(): void
    {
        if (!MailNotifyService::isEnabled('traffic_almost')) return;

        $threshold = (int) (SiteConfig::getValue('notify_traffic_almost_threshold') ?: 90);
        if ($threshold <= 0 || $threshold > 100) $threshold = 90;

        // 用量达到 limit 的 threshold%，但还没用尽（用尽那一路挂在 BanService 上）
        // whereColumn 不接受闭包，阈值直接算成 traffic_used >= traffic_limit * (pct/100)
        $ratio = $threshold / 100;

        User::where('enabled', true)
            ->where('traffic_limit', '>', 0)
            ->whereRaw('traffic_used >= FLOOR(traffic_limit * ?)', [$ratio])
            ->whereColumn('traffic_used', '<', 'traffic_limit')
            ->with('plan')
            ->chunkById(200, function ($users) {
                foreach ($users as $user) {
                    $this->dispatchOnce('traffic_almost', $user);
                }
            });
    }

    /** 套餐即将到期（默认提前 3 天，管理员可调） */
    private function scanExpiring(): void
    {
        if (!MailNotifyService::isEnabled('expiring')) return;

        $days = (int) (SiteConfig::getValue('notify_expiring_days') ?: 3);

        User::where('enabled', true)->whereNotNull('expired_at')
            ->where('expired_at', '>', now())
            ->where('expired_at', '<=', now()->addDays($days))
            ->with('plan')
            ->chunkById(200, function ($users) {
                foreach ($users as $user) {
                    $this->dispatchOnce('expiring', $user);
                }
            });
    }

    /**
     * 已到期。
     *
     * 扫描窗口是到期后 7 天，7 天之后不再扫到这个用户。配合月度去重，窗口内
     * 最多发一封（窗口跨月时最多两封）；改前没有月度去重，窗口内是每天一封、
     * 最多 7 封。要「一直提醒到续费为止」得同时放开这个窗口，当前按既定行为保留。
     */
    private function scanExpired(): void
    {
        if (!MailNotifyService::isEnabled('expired')) return;

        User::where('enabled', true)->whereNotNull('expired_at')
            ->where('expired_at', '<=', now())
            ->where('expired_at', '>=', now()->subDays(7))
            ->with('plan')
            ->chunkById(200, function ($users) {
                foreach ($users as $user) {
                    $this->dispatchOnce('expired', $user);
                }
            });
    }

    /**
     * 同一自然月、同一场景、同一收件人只发一次（口径见 MailNotifyService::claimMonthly）。
     *
     * 防重靠 Cache 而不是 mail_logs：mail_logs 是真正发出后才写的，
     * 而本任务是每 5 分钟一轮 —— 两轮之间队列还没跑完的话，
     * 用 mail_logs 判断会把同一封发两遍。Cache 标记在入队那一刻就落。
     */
    private function dispatchOnce(string $scene, User $user): void
    {
        if (empty($user->email)) return;

        // 先渲染 + 解析收件人，确认这封真的能发，再落防重标记。
        // 顺序反过来的话：管理员没配收件邮箱时标记已经写进 Cache，
        // 之后补上邮箱这一整月都不会再发了。
        $rendered = MailNotifyService::render($scene, $user->loadMissing('plan'));
        if ($rendered === []) return;

        $to = $rendered['to_type'] === 'admin'
            ? SiteConfig::getValue('notify_admin_email')
            : $user->email;

        if (empty($to)) return;

        // 本月已给这个收件人发过该场景 → 跳过
        if (!MailNotifyService::claimMonthly($scene, $user, $rendered['to_type'], $to)) {
            return;
        }

        SendMailJob::dispatch(
            toEmail: $to,
            subject: $rendered['subject'],
            htmlBody: $rendered['body'],
            type: MailLog::TYPE_NOTIFY,
            userId: $user->id,
            scene: $scene,
        );
    }
}