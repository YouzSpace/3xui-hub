<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\SiteConfig;
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
        'smtp_from_address',
        'smtp_from_name',
        'email_template',
        'register_email_verify', // 注册时启用邮箱验证
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
            'smtp_from_address' => ['nullable', 'string', 'max:255'],
            'smtp_from_name'    => ['nullable', 'string', 'max:100'],
            'email_template'    => ['nullable', 'string', 'max:65535'],
            'register_email_verify' => ['nullable', 'boolean'],
        ]);

        $updates = [];
        foreach ($data as $key => $value) {
            if ($key === 'smtp_password' && !empty($value)) {
                $value = Crypt::encryptString($value);
            }
            $updates[$key] = $value ?? '';
        }

        // 如果 smtp_from_address 为空，自动用 smtp_username
        if (empty($updates['smtp_from_address']) && !empty($updates['smtp_username'])) {
            $updates['smtp_from_address'] = $updates['smtp_username'];
        }

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

        $fromAddress = $config['smtp_from_address'] ?: ($config['smtp_username'] ?? '');
        $fromName    = $config['smtp_from_name']    ?? '';

        // 用模板发送，{{code}} 替换为 "TEST1234"
        $template = !empty($config['email_template']) ? $config['email_template'] : '<div style="padding:20px;font-family:sans-serif"><h2>注册验证码</h2><p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p><p style="color:#666">5分钟内有效，请勿泄露。</p></div>';
        $body = str_replace('{{code}}', 'TEST1234', $template);

        try {
            Mail::html($body, function ($message) use ($data, $fromAddress, $fromName) {
                $message->to($data['to'])
                    ->subject(!empty($fromName) ? $fromName : 'ControlHub');
                if ($fromAddress) {
                    $message->from($fromAddress, $fromName ?: null);
                }
            });
        } catch (\Throwable $e) {
            Log::error('Email test failed', ['error' => $e->getMessage()]);
            return $this->error('发送失败：' . $e->getMessage(), 500);
        }

        return $this->success(null, '测试邮件已发送');
    }
}
