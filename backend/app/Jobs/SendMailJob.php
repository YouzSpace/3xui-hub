<?php

namespace App\Jobs;

use App\Models\MailLog;
use App\Models\User;
use App\Services\MailNotifyService;
use App\Services\SimpleMailerService;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;
use Illuminate\Support\Facades\Log;

/**
 * 发一封邮件并记录结果（自动通知 / 批量发信共用）。
 *
 * 每次投递都落一条 mail_logs：成功 sent，失败 failed + error。
 * 发信异常在此捕获，不让整个队列任务崩掉——批量发信里一封失败不该中断其余。
 */
class SendMailJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function __construct(
        public string $toEmail,
        public string $subject,
        public string $htmlBody,
        public string $type = MailLog::TYPE_BATCH,
        public ?int $userId = null,
        public ?string $scene = null,
        public ?int $batchId = null,
        public ?string $claimKey = null,
    ) {
        // 邮件走哪条队列（默认 'default' = 不拆）。邮件与控制类定时任务（封禁检查、
        // 健康检查、通知扫描）挤同一条 default 队列时，一条单线程 worker 会被一次群发
        // 独占十几分钟，期间 5 分钟定时任务只能排队。部署侧设 PANEL_MAIL_QUEUE 并另起
        // 一个 worker 监听该队列才会真正分开（config/panel.php 有同款说明）。
        $this->onQueue((string) config('panel.mail_queue', 'default'));
    }

    public function handle(SimpleMailerService $mailer): void
    {
        try {
            $mailer->send($this->toEmail, $this->subject, $this->htmlBody);

            MailLog::create([
                'type'       => $this->type,
                'status'     => MailLog::STATUS_SENT,
                'to_email'   => $this->toEmail,
                'to_user_id' => $this->userId,
                'subject'    => $this->subject,
                'scene'      => $this->scene,
                'batch_id'   => $this->batchId,
            ]);
        } catch (\Throwable $e) {
            Log::error('SendMailJob failed', [
                'to'     => $this->toEmail,
                'error'  => $e->getMessage(),
            ]);

            MailLog::create([
                'type'       => $this->type,
                'status'     => MailLog::STATUS_FAILED,
                'to_email'   => $this->toEmail,
                'to_user_id' => $this->userId,
                'subject'    => $this->subject,
                'error'      => $e->getMessage(),
                'scene'      => $this->scene,
                'batch_id'   => $this->batchId,
            ]);

            // 自动通知的防重标记在「入队那一刻」就落了，真发失败要把它释放掉，
            // 否则这一封永久丢失（标记已落、触发条件仍在，却永远不会再发）。
            // 下一轮扫描（5 分钟）会重新发一封。批量/定时发信没有这类标记，$claimKey 为 null。
            MailNotifyService::releaseClaim($this->claimKey);
        }
    }
}