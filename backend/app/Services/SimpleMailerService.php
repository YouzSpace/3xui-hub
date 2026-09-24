<?php

namespace App\Services;

use App\Models\SiteConfig;
use Illuminate\Support\Facades\Crypt;
use Illuminate\Support\Facades\Log;
use Illuminate\Support\Facades\Mail;

/**
 * 轻量发信：读 SiteConfig 里的 SMTP 配置 → 动态切 mailer → 发一封 HTML 邮件。
 *
 * 坑：动态改 mail 配置后必须 forgetMailers()，否则 Mail 会静默复用已解析的旧 mailer。
 * 注册验证码（EmailVerifyController）与找回密码验证码（PasswordResetController）共用本类，
 * 避免「解密密码 + config + forgetMailers + Mail::html」这套逻辑写两份。
 *
 * 发件人地址固定 = smtp_username（SMTP 授权账号），不读 smtp_from_address：
 * QQ 等邮箱会拒绝 MAIL FROM 与授权账号不一致的信（SMTP 501 "Mail from address must be
 * same as authorization user"）。历史上的 smtp_from_address 残留值一律不读，改账号即自动生效。
 * 发件人显示名 smtp_from_name 仍可自定义。
 */
class SimpleMailerService
{
    /** 发信涉及的 SiteConfig 键。 */
    public const CONFIG_KEYS = [
        'smtp_host', 'smtp_port', 'smtp_username', 'smtp_password',
        'smtp_encryption', 'smtp_from_name', 'email_template',
    ];

    /**
     * 发一封 HTML 邮件。
     * 异常不吞：由调用方决定记什么日志、回给用户什么文案。
     */
    public function send(string $to, string $subject, string $htmlBody): void
    {
        $config = SiteConfig::getMany(self::CONFIG_KEYS);

        // 解密密码：解密失败只记日志、按空密码继续（与原 EmailVerifyController 行为一致）
        $password = '';
        if (!empty($config['smtp_password'])) {
            try {
                $password = Crypt::decryptString($config['smtp_password']);
            } catch (\Throwable $e) {
                Log::error('SMTP password decrypt failed', ['error' => $e->getMessage()]);
            }
        }

        // 发件地址 = 授权账号，二者必须一致（见类注释），不读 smtp_from_address
        $fromAddress = $config['smtp_username'] ?? '';
        $fromName    = $config['smtp_from_name'] ?? '';

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
            'mail.mailers.smtp.stream_options' => [
                'ssl' => [
                    'verify_peer'       => false,
                    'verify_peer_name'  => false,
                    'allow_self_signed' => true,
                ],
            ],
        ]);
        app('mail.manager')->forgetMailers();

        Mail::html($htmlBody, function ($message) use ($to, $subject, $fromAddress, $fromName) {
            $message->to($to)->subject($subject);
            if ($fromAddress) {
                $message->from($fromAddress, $fromName ?: null);
            }
        });
    }
}
