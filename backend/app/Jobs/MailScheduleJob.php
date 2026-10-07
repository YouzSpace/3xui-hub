<?php

namespace App\Jobs;

use App\Models\MailLog;
use App\Services\MailBatchService;
use App\Services\MailScheduleService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Support\Facades\Log;

/**
 * 按月定时发信：后台每 5 分钟扫一次，到管理员配置的「每月第 N 天 HH:MM」就发信。
 *
 * 收件人/标题/正文全部复用批量发信那套（MailBatchService），手动批量和月发
 * 走同一条发信链路；发信限速统一读「限制」页全局配置（mail_rate_limit_*）。
 *
 * 去重：当月只要成功发过一轮（mail_logs type=schedule 有记录），之后任何一轮
 * 扫描都不再重发；下个月自动重新发。所以扫描任务跑多少遍、服务器中途重启，
 * 都不会把同一轮的邮件发第二遍。
 *
 * 补发：到点后没赶上（当时服务器没起来 / 队列挂着），同月内下一次扫描自动补上。
 */
class MailScheduleJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable;

    public function __construct(private MailBatchService $batch)
    {
    }

    public function handle(): void
    {
        if (!MailScheduleService::isEnabled()) {
            return;
        }

        $cfg = MailScheduleService::config();
        $sendAt = MailScheduleService::nextSendAt((int) $cfg['day'], now());
        if ($sendAt === null) {
            // 配置的第 N 天当月不存在（如 31 号配在 2 月）→ 当月跳过，下个月再看
            return;
        }

        // 还没到点 → 这轮不发，等下一次扫描
        if (now()->lt($sendAt)) {
            return;
        }

        // 开启时间晚于本月到点时刻 → 当月不补发（刚打开开关的常见情形：
        // 保存完不该立刻群发一封），下个月到点起正常发。
        // 从未记录开启时刻（老数据）不拦截，保持「错过当天、同月补发」行为。
        $armedAt = MailScheduleService::armedAt();
        if ($armedAt !== null && $armedAt->gt($sendAt)) {
            return;
        }

        // 到点了，但本月已经发过一轮 → 跳过（去重）
        if (MailScheduleService::alreadySentThisMonth()) {
            return;
        }

        $mailCfg = $cfg['config'] ?? [];
        $subject = trim((string) ($mailCfg['subject'] ?? ''));
        $body    = (string) ($mailCfg['body'] ?? '');
        if ($subject === '' || $body === '') {
            Log::warning('MailScheduleJob: 定时发信未配置标题/正文，跳过本轮');
            return;
        }

        try {
            $users = $this->batch->resolveRecipients($mailCfg + ['mode' => 'all']);
        } catch (\RuntimeException) {
            Log::warning('MailScheduleJob: 没有符合条件的收件人，跳过本轮');
            return;
        }

        // 去重标记必须在入队那一刻就落（跟 MailNotifyScanJob 同口径）：
        // 发信日志要等队列真正跑完才写，扫描每 5 分钟一轮，只靠日志判断
        // 会在两轮之间把同一批发两遍。Cache 标记按月生效，跨进程共享。
        $markKey = 'mail_schedule:' . now()->format('Ym');
        if (!\Illuminate\Support\Facades\Cache::add($markKey, 1, now()->addMonthNoOverflow()->endOfMinute())) {
            return;
        }

        $batchId = (int) now()->format('YmdHis');
        $sent = $this->batch->dispatchToUsers(
            users: $users,
            subject: $subject,
            body: $body,
            batchId: $batchId,
            type: MailLog::TYPE_SCHEDULE,
        );

        Log::info('MailScheduleJob: 定时发信已入队', ['batch' => $batchId, 'total' => $sent]);
    }
}
