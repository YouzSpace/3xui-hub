<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\SiteConfig;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Log;
use Illuminate\Support\Facades\Mail;
use Illuminate\Support\Facades\Cache;

class EmailVerifyController extends Controller
{
    /**
     * 发送注册验证码
     */
    public function sendCode(Request $request)
    {
        $data = $request->validate([
            'email' => ['required', 'email'],
        ]);

        // 检查是否启用邮箱验证
        if (!SiteConfig::getValue('register_email_verify')) {
            return response()->json([
                'code' => 400,
                'msg' => '未启用邮箱验证',
                'data' => null,
            ], 400);
        }

        // 检查SMTP配置
        $config = SiteConfig::getMany([
            'smtp_host', 'smtp_port', 'smtp_username', 'smtp_password',
            'smtp_encryption', 'smtp_from_address', 'smtp_from_name', 'email_template',
        ]);

        if (empty($config['smtp_host'])) {
            return response()->json([
                'code' => 500,
                'msg' => 'SMTP未配置',
                'data' => null,
            ], 500);
        }

        // 生成6位验证码
        $code = str_pad(random_int(0, 999999), 6, '0', STR_PAD_LEFT);

        // 存储验证码（5分钟有效）
        Cache::put('email_verify_' . $data['email'], $code, 300);

        // 解密密码
        $password = '';
        if (!empty($config['smtp_password'])) {
            try {
                $password = \Illuminate\Support\Facades\Crypt::decryptString($config['smtp_password']);
            } catch (\Throwable $e) {
                Log::error('SMTP password decrypt failed', ['error' => $e->getMessage()]);
            }
        }

        // 发送邮件
        $fromAddress = $config['smtp_from_address'] ?: ($config['smtp_username'] ?? '');
        $fromName    = $config['smtp_from_name'] ?? '';
        $template    = !empty($config['email_template']) ? $config['email_template'] : '<div style="padding:20px;font-family:sans-serif"><h2>注册验证码</h2><p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p><p style="color:#666">5分钟内有效，请勿泄露。</p></div>';
        $body        = str_replace('{{code}}', $code, $template);

        try {
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

            Mail::html($body, function ($message) use ($data, $fromAddress, $fromName) {
                $message->to($data['email'])
                    ->subject(!empty($fromName) ? $fromName : 'ControlHub');
                if ($fromAddress) {
                    $message->from($fromAddress, $fromName ?: null);
                }
            });
        } catch (\Throwable $e) {
            Log::error('Send email verify code failed', ['error' => $e->getMessage()]);
            return response()->json([
                'code' => 500,
                'msg' => '发送失败：' . $e->getMessage(),
                'data' => null,
            ], 500);
        }

        return response()->json([
            'code' => 0,
            'msg' => '验证码已发送',
            'data' => null,
        ]);
    }

    /**
     * 验证验证码
     */
    public function verifyCode(Request $request)
    {
        $data = $request->validate([
            'email' => ['required', 'email'],
            'code'  => ['required', 'string', 'size:6'],
        ]);

        $cached = Cache::get('email_verify_' . $data['email']);

        if (!$cached || $cached !== $data['code']) {
            return response()->json([
                'code' => 400,
                'msg' => '验证码错误',
                'data' => null,
            ], 400);
        }

        // 验证成功后删除验证码
        Cache::forget('email_verify_' . $data['email']);

        return response()->json([
            'code' => 0,
            'msg' => '验证成功',
            'data' => null,
        ]);
    }
}
