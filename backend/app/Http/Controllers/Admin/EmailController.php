<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\MailLog;
use App\Models\SiteConfig;
use App\Services\MailNotifyService;
use App\Services\MailBatchService;
use App\Services\MailScheduleService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Crypt;
use Illuminate\Support\Facades\Log;
use Illuminate\Support\Facades\Mail;

class EmailController extends Controller
{
    use ApiResponse;

    private const KEYS = [
        'smtp_host',
        'smtp_port',
        'smtp_username',
        'smtp_password',   // 加密存储
        'smtp_encryption',
        'smtp_from_name',
        'email_template',
        'register_email_verify', // 注册时启用邮箱验证
        // 发信速度限制（「限制」页配置，批量发信与按月定时发信共用）
        'mail_rate_limit_enabled',
        'mail_rate_per_minute',

        // 用户端接口限流（见 App\Services\RateGuardService）：
        // 空值 = 用 config/panel.php 的默认值，0 = 该项不限
        'rate_code_interval',
        'rate_code_per_day',
        'rate_code_ip_hourly',
        'rate_verify_max_attempts',
        'rate_verify_lock_seconds',
        'rate_login_max_attempts',
        'rate_login_lock_seconds',
        'rate_register_ip_hourly',
        // 折扣码校验限流（rate_discount_*）不在这里：那 5 个参数归「优惠码管理 → 邀请机制设置」页，
        // 见 Admin\DiscountCodeController::inviteSettings()。
    ];

    /** 获取 SMTP 配置和邮件模板 */
    public function show(): \Illuminate\Http\JsonResponse
    {
        $config = SiteConfig::getMany(self::KEYS);

        // 解密密码
        if (!empty($config['smtp_password'])) {
            try {
                $config['smtp_password'] = Crypt::decryptString($config['smtp_password']);
            } catch (\Throwable) {
                $config['smtp_password'] = '';
            }
        }

        return $this->success($config);
    }

    /** 保存 SMTP 配置和邮件模板 */
    public function save(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'smtp_host'         => ['nullable', 'string', 'max:255'],
            'smtp_port'         => ['nullable', 'integer', 'min:1', 'max:65535'],
            'smtp_username'     => ['nullable', 'string', 'max:255'],
            'smtp_password'     => ['nullable', 'string', 'max:255'],
            'smtp_encryption'   => ['nullable', 'in:tls,ssl,none'],
            'smtp_from_name'    => ['nullable', 'string', 'max:100'],
            'email_template'    => ['nullable', 'string', 'max:65535'],
            'register_email_verify' => ['nullable', 'boolean'],
            // 发信速度限制（批量与定时共用）：开关存 '1'/''，速率 1-600
            'mail_rate_limit_enabled' => ['nullable', 'boolean'],
            'mail_rate_per_minute'    => ['nullable', 'integer', 'min:1', 'max:600'],

            // 限流：非负整数，0 = 不限（空值走 config/panel.php 默认）
            'rate_code_interval'       => ['nullable', 'integer', 'min:0'],
            'rate_code_per_day'        => ['nullable', 'integer', 'min:0'],
            'rate_code_ip_hourly'      => ['nullable', 'integer', 'min:0'],
            'rate_verify_max_attempts' => ['nullable', 'integer', 'min:0'],
            'rate_verify_lock_seconds' => ['nullable', 'integer', 'min:0'],
            'rate_login_max_attempts'  => ['nullable', 'integer', 'min:0'],
            'rate_login_lock_seconds'  => ['nullable', 'integer', 'min:0'],
            'rate_register_ip_hourly'  => ['nullable', 'integer', 'min:0'],
        ]);

        $updates = [];
        foreach ($data as $key => $value) {
            if ($key === 'smtp_password' && !empty($value)) {
                $value = Crypt::encryptString($value);
            }
            $updates[$key] = $value ?? '';
        }

        // 空值保护：SMTP 三件套（服务器/账号/授权码）一旦被空串写进去，
        // 面板就再也发不出邮件，而且没有任何提示——只能靠人工重新填授权码。
        // 触发路径：前端 load() 静默失败时表单还是初始空值，用户此时点保存，
        // 整份表单就会把库里的配置覆盖成空。这里挡下最后一道。
        // 语义：只提交非空值即视为「保持原样」；要真正清空某项请在数据库侧操作。
        $protected = ['smtp_host', 'smtp_username', 'smtp_password'];
        foreach ($protected as $key) {
            if (($updates[$key] ?? '') === '' && ($current = SiteConfig::getValue($key)) !== null && $current !== '') {
                unset($updates[$key]);
            }
        }

        // 发件地址不再单独存储：发信时固定取 smtp_username（见 SimpleMailerService）
        SiteConfig::setMany($updates);

        return $this->success(null, '保存成功');
    }

    /** 发送测试邮件 */
    public function test(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'to' => ['required', 'email'],
        ]);

        $config = SiteConfig::getMany(self::KEYS);

        if (empty($config['smtp_host'])) {
            return $this->error('请先配置 SMTP 服务器地址', 400);
        }

        // 解密密码
        $password = '';
        if (!empty($config['smtp_password'])) {
            try {
                $password = Crypt::decryptString($config['smtp_password']);
            } catch (\Throwable) {
            }
        }

        // 临时设置邮件驱动
        config([
            'mail.default'            => 'smtp',
            'mail.mailers.smtp.host'       => $config['smtp_host'],
            'mail.mailers.smtp.port'       => (int) ($config['smtp_port'] ?? 587),
            'mail.mailers.smtp.username'   => $config['smtp_username'] ?? null,
            'mail.mailers.smtp.password'   => $password,
            'mail.mailers.smtp.encryption' => ($config['smtp_encryption'] === 'none') ? null : $config['smtp_encryption'],
            'mail.mailers.smtp.local_domain' => 'localhost',
            'mail.mailers.smtp.auth_mode'  => 'login',
            'mail.mailers.smtp.timeout'    => 30,
            // 允许自签名证书（开发环境）
            'mail.mailers.smtp.stream_options' => [
                'ssl' => [
                    'verify_peer'       => false,
                    'verify_peer_name'  => false,
                    'allow_self_signed' => true,
                ],
            ],
        ]);

        // 强制刷新 mailer，否则会使用缓存的配置（log/null）
        app('mail.manager')->forgetMailers();

        // 发件地址 = 授权账号，二者必须一致（QQ 等邮箱会拒信，SMTP 501），不读 smtp_from_address
        $fromAddress = $config['smtp_username'] ?? '';
        $fromName    = $config['smtp_from_name']    ?? '';

        // 用模板发送，{{code}} 替换为 "TEST1234"
        $template = !empty($config['email_template']) ? $config['email_template'] : '<div style="padding:20px;font-family:sans-serif"><h2>注册验证码</h2><p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p><p style="color:#666">5分钟内有效，请勿泄露。</p></div>';
        $body = str_replace('{{code}}', 'TEST1234', $template);

        $subject = !empty($fromName) ? $fromName : 'ControlHub';

        try {
            Mail::html($body, function ($message) use ($data, $fromAddress, $fromName, $subject) {
                $message->to($data['to'])
                    ->subject($subject);
                if ($fromAddress) {
                    $message->from($fromAddress, $fromName ?: null);
                }
            });

            // 测试邮件也要落日志：否则发没发出去、失败原因都无处可查
            MailLog::create([
                'type'     => MailLog::TYPE_TEST,
                'status'   => MailLog::STATUS_SENT,
                'to_email' => $data['to'],
                'subject'  => $subject,
            ]);
        } catch (\Throwable $e) {
            Log::error('Email test failed', ['error' => $e->getMessage()]);

            MailLog::create([
                'type'     => MailLog::TYPE_TEST,
                'status'   => MailLog::STATUS_FAILED,
                'to_email' => $data['to'],
                'subject'  => $subject,
                'error'    => $e->getMessage(),
            ]);

            return $this->error('发送失败：' . $e->getMessage(), 500);
        }

        return $this->success(null, '测试邮件已发送');
    }

    // ============ 自动通知 ============

    /** 读取自动通知配置（全部开关默认关闭） */
    public function notifyConfig(): \Illuminate\Http\JsonResponse
    {
        return $this->success(MailNotifyService::allConfig());
    }

    /** 保存自动通知配置 */
    public function saveNotifyConfig(Request $request): \Illuminate\Http\JsonResponse
    {
        $scenes = array_keys(MailNotifyService::SCENES);
        $rules = [];
        foreach ($scenes as $scene) {
            $rules["notify_{$scene}_enabled"] = ['nullable', 'boolean'];
            $rules["notify_{$scene}_title"]   = ['nullable', 'string', 'max:255'];
            $rules["notify_{$scene}_body"]    = ['nullable', 'string', 'max:65535'];
            $rules["notify_{$scene}_to"]      = ['nullable', 'in:user,admin'];
        }
        $rules['notify_traffic_almost_threshold'] = ['nullable', 'integer', 'min:1', 'max:100'];
        $rules['notify_expiring_days']            = ['nullable', 'integer', 'min:0', 'max:365'];
        $rules['notify_admin_email']              = ['nullable', 'email'];

        $data = $request->validate($rules);

        $updates = [];
        foreach ($data as $key => $value) {
            if ($key === 'notify_admin_email') {
                $updates[$key] = $value ?? '';
                continue;
            }
            if (str_ends_with($key, '_enabled')) {
                // checkbox 传的是 0/1，存成 '1' / ''（未配置 = 关闭）
                $updates[$key] = $value ? '1' : '';
                continue;
            }
            $updates[$key] = $value ?? '';
        }

        SiteConfig::setMany($updates);

        return $this->success(MailNotifyService::allConfig(), '通知配置已保存');
    }

    // ============ 批量发信 ============

    /**
     * 群发 / 单发。
     * 全部走队列，页面不阻塞；限速在「限制」页统一配置（默认关闭），与按月定时共用。
     * 收件人解析与发信共用 MailBatchService（按月定时发信也走这里，规则单一来源）。
     */
    public function batchSend(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'subject'      => ['required', 'string', 'max:255'],
            'body'         => ['required', 'string', 'max:65535'],
            'mode'         => ['required', 'in:all,filter,single'],
            'plan_id'      => ['nullable', 'integer'],
            'user_status'  => ['nullable', 'in:normal,over,expired'],
            'single_query' => ['nullable', 'string', 'max:255'],
        ]);

        $config = SiteConfig::getMany(['smtp_host']);
        if (empty($config['smtp_host'])) {
            return $this->error('请先配置 SMTP 服务器地址', 400);
        }

        $batch = app(MailBatchService::class);

        try {
            $recipients = $batch->resolveRecipients($data);
        } catch (\RuntimeException) {
            if ($data['mode'] === 'single') {
                $q = trim((string) ($data['single_query'] ?? ''));
                return $this->error($q === '' ? '请填写要发给谁（邮箱或用户 ID）' : '没找到该用户', $q === '' ? 400 : 404);
            }
            return $this->error('没有符合条件的用户', 400);
        }

        $batchId = (int) now()->format('YmdHis');

        // 限速统一取「限制」页的全局配置（mail_rate_limit_*）：批量与按月定时共用一份
        $rate = MailBatchService::rateLimitConfig();

        $total = $batch->dispatchToUsers(
            users: $recipients,
            subject: $data['subject'],
            body: $data['body'],
            batchId: $batchId,
        );

        return $this->success([
            'batch_id'     => $batchId,
            'total'        => $total,
            'rate_limited' => $rate['enabled'] ? $rate['per_minute'] : null,
        ], "已加入发送队列，共 {$total} 封");
    }

    // ============ 按月定时发信 ============

    /** 读取按月定时发信配置（默认关闭） */
    public function scheduleMailConfig(): \Illuminate\Http\JsonResponse
    {
        return $this->success(MailScheduleService::config());
    }

    /** 保存按月定时发信配置（收件人/标题/正文同批量发信；发信限速走「限制」页全局配置） */
    public function saveScheduleMailConfig(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'enabled' => ['nullable', 'boolean'],
            'day'     => ['nullable', 'integer', 'min:1'],
            // 正则自带冒号，Laravel 解析规则串会按第一个 : 截断参数 → 必须套 / 定界符
            'time'    => ['nullable', 'string', 'regex:/^([01]\d|2[0-3]):[0-5]\d$/'],
            'config'  => ['nullable', 'array'],
            // 内容不强制必填：本页是「修改自动保存」，先开开关后补内容是常态；
            // 标题/正文为空时 MailScheduleJob 到点会跳过并记日志（不会误发空信）。
            'config.subject'       => ['nullable', 'string', 'max:255'],
            'config.body'          => ['nullable', 'string', 'max:65535'],
            'config.mode'          => ['nullable', 'in:all,filter,single'],
            'config.plan_id'       => ['nullable', 'integer'],
            'config.user_status'   => ['nullable', 'in:normal,over,expired'],
            'config.single_query' => ['nullable', 'string', 'max:255'],
        ]);

        $out = MailScheduleService::save($data);

        return $this->success($out, '定时发信配置已保存');
    }

    /** 发信日志（分页） */
    public function mailLogs(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'type'   => ['nullable', 'in:notify,batch,test,schedule'],
            'status' => ['nullable', 'in:sent,failed'],
            'page'   => ['nullable', 'integer', 'min:1'],
        ]);

        $query = MailLog::orderByDesc('id');
        if (!empty($data['type']))   $query->where('type', $data['type']);
        if (!empty($data['status'])) $query->where('status', $data['status']);

        $page   = (int) ($data['page'] ?? 1);
        $total  = (clone $query)->count();
        $rows   = $query->forPage($page, 50)->get();

        return response()->json([
            'code' => 0,
            'msg'  => 'ok',
            'data' => $rows->map(fn ($l) => [
                'id'         => $l->id,
                'type'       => $l->type,
                'status'     => $l->status,
                'to_email'   => $l->to_email,
                'to_user_id' => $l->to_user_id,
                'subject'    => $l->subject,
                'scene'      => $l->scene,
                'batch_id'   => $l->batch_id,
                'error'      => $l->error,
                'created_at' => $l->created_at?->toIso8601String(),
            ])->values()->all(),
            'pagination' => [
                'current_page' => $page,
                'last_page'    => max(1, (int) ceil($total / 50)),
                'total'        => $total,
                'per_page'     => 50,
            ],
        ], 200);
    }
}
