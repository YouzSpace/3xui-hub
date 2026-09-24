<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\SiteConfig;
use App\Services\RateGuardService;
use App\Services\SimpleMailerService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Log;
use Illuminate\Support\Facades\Cache;

/**
 * 注册邮箱验证码。
 * POST /api/email-verify/send  → 发送注册验证码
 * POST /api/email-verify/check → 校验注册验证码
 *
 * 本控制器不查「邮箱是否已注册」，没有枚举面；限流键与找回密码那条链路共用
 * （同 IP 每小时发码额度是两个接口合计），见 RateGuardService。
 */
class EmailVerifyController extends Controller
{
    use ApiResponse;

    public function __construct(
        private SimpleMailerService $mailer,
        private RateGuardService $rateGuard,
    ) {
    }

    /**
     * 发送注册验证码
     */
    public function sendCode(Request $request)
    {
        $data = $request->validate([
            'email' => ['required', 'email'],
        ]);

        $ip = (string) $request->ip();

        // 入口先过限流，再管「有没有开邮箱验证 / SMTP 配没配」这些分支
        $blocked = $this->rateGuard->codeSendBlocked($data['email'], $ip);
        if ($blocked !== null) {
            return $this->error($blocked, 429);
        }

        // 放行即记数（与 /api/password-reset/send 共享同一套计数）
        $this->rateGuard->recordCodeSend($data['email'], $ip);

        // 检查是否启用邮箱验证
        if (!SiteConfig::getValue('register_email_verify')) {
            return response()->json([
                'code' => 400,
                'msg' => '未启用邮箱验证',
                'data' => null,
            ], 400);
        }

        // 检查SMTP配置
        $config = SiteConfig::getMany(['smtp_host', 'smtp_from_name', 'email_template']);

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

        // 发送邮件
        $fromName = $config['smtp_from_name'] ?? '';
        $template = !empty($config['email_template']) ? $config['email_template'] : '<div style="padding:20px;font-family:sans-serif"><h2>注册验证码</h2><p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p><p style="color:#666">5分钟内有效，请勿泄露。</p></div>';
        $body     = str_replace('{{code}}', $code, $template);

        try {
            $this->mailer->send($data['email'], !empty($fromName) ? $fromName : 'ControlHub', $body);
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

        $ip = (string) $request->ip();

        // 试错打满后锁定期内一律拒，正确验证码也不例外
        $blocked = $this->rateGuard->verifyBlocked($data['email'], $ip);
        if ($blocked !== null) {
            return $this->error($blocked, 429);
        }

        $cached = Cache::get('email_verify_' . $data['email']);

        if (!$cached || $cached !== $data['code']) {
            $this->rateGuard->recordVerifyFailure($data['email'], $ip);

            return response()->json([
                'code' => 400,
                'msg' => '验证码错误',
                'data' => null,
            ], 400);
        }

        // 验证成功后删除验证码
        Cache::forget('email_verify_' . $data['email']);

        $this->rateGuard->clearVerifyFailures($data['email'], $ip);

        return response()->json([
            'code' => 0,
            'msg' => '验证成功',
            'data' => null,
        ]);
    }
}
