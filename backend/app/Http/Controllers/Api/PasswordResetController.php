<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\AccessToken;
use App\Models\SiteConfig;
use App\Models\User;
use App\Services\RateGuardService;
use App\Services\SimpleMailerService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Cache;
use Illuminate\Support\Facades\Hash;
use Illuminate\Support\Facades\Log;

/**
 * 用户自助找回密码（邮箱验证码）。
 * POST /api/password-reset/send  → 发送重置验证码
 * POST /api/password-reset/reset → 校验验证码并改密
 *
 * 验证码走独立缓存键 pwd_reset_{email}，与注册的 email_verify_{email} 互不干扰；
 * 改密成功后清空该用户全部 access_tokens（旧登录态强制下线）。
 *
 * 防邮箱枚举：本控制器不得暴露「某邮箱是否已注册」。
 *  - send：邮箱格式合法即返回同一响应（SENT_MSG），未注册 / SMTP 未配置 / 发信失败
 *    只记日志（不含邮箱明文），且一律不落验证码缓存；只有真正发信成功才写缓存。
 *  - reset：「邮箱未注册」与「验证码错误」共用同一响应（CODE_INVALID_MSG + 400）。
 *  - 限流（RateGuardService）也必须守同一条规矩：判定在「是否注册」之前，且放行后
 *    所有分支无条件记数，否则限流状态自己就会把注册状态漏出去。
 */
class PasswordResetController extends Controller
{
    use ApiResponse;

    /** 验证码有效期（秒）。 */
    private const CODE_TTL = 300;

    /** 防枚举：send 的所有分支共用同一响应文案，别在分支里另写字面量。 */
    private const SENT_MSG = '验证码已发送';

    /** 防枚举：reset 的「码不对」与「邮箱未注册」共用同一响应文案。 */
    private const CODE_INVALID_MSG = '验证码错误或已过期';

    public function __construct(
        private SimpleMailerService $mailer,
        private RateGuardService $rateGuard,
    ) {
    }

    /**
     * 发送重置验证码。
     *
     * 防枚举：邮箱格式过校验后就只有一种返回值，未注册 / SMTP 未配置 / 发信异常
     * 都是记日志 + 返回 SENT_MSG，调用方无法据此判断邮箱是否存在。
     */
    public function sendCode(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'email' => ['required', 'email'],
        ]);

        $email = $data['email'];
        $ip    = (string) $request->ip();

        // 限流是入口第一步，排在「邮箱是否注册」前面：
        // 注册与未注册邮箱撞的是同一堵墙、同一句话，不构成枚举信号。
        $blocked = $this->rateGuard->codeSendBlocked($email, $ip);
        if ($blocked !== null) {
            return $this->error($blocked, 429);
        }

        // 放行即记数：未注册 / SMTP 未配置 / 发信失败等分支在下面早退，但账已经记上了
        $this->rateGuard->recordCodeSend($email, $ip);

        if (!User::where('email', $email)->exists()) {
            Log::warning('password reset for unregistered email', $this->emailLogContext($email));
            return $this->success(null, self::SENT_MSG);
        }

        if (empty(SiteConfig::getMany(['smtp_host'])['smtp_host'])) {
            Log::warning('password reset skipped: smtp not configured', $this->emailLogContext($email));
            return $this->success(null, self::SENT_MSG);
        }

        // 生成6位验证码
        $code = str_pad((string) random_int(0, 999999), 6, '0', STR_PAD_LEFT);

        // 重置专用文案：不读 SiteConfig 的 email_template（那是注册模板，两者互不影响）
        $template = '<div style="padding:20px;font-family:sans-serif"><h2>找回密码</h2><p style="font-size:24px;color:#2563eb;font-weight:bold">{{code}}</p><p style="color:#666">5 分钟内有效，请勿泄露。</p></div>';
        $body     = str_replace('{{code}}', $code, $template);

        try {
            $this->mailer->send($email, '找回密码', $body);
        } catch (\Throwable $e) {
            Log::error('Send password reset code failed', ['error' => $e->getMessage()] + $this->emailLogContext($email));
            return $this->success(null, self::SENT_MSG);
        }

        // 只有真正发信成功才落码（5分钟有效）：发不出去的码不允许用来改密
        Cache::put('pwd_reset_' . $email, $code, self::CODE_TTL);

        return $this->success(null, self::SENT_MSG);
    }

    /**
     * 日志用的邮箱标识：掩码 + sha1，不留完整明文。
     * sha1 便于把同一邮箱的多条日志串起来排查，掩码便于人眼粗看。
     */
    private function emailLogContext(string $email): array
    {
        $at = strpos($email, '@');

        return [
            'email_masked' => $at === false ? '***' : $email[0] . '***' . substr($email, $at),
            'email_sha1'   => sha1($email),
        ];
    }

    /** 校验验证码并重置密码 */
    public function reset(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'email'    => ['required', 'email'],
            'code'     => ['required', 'string', 'size:6'],
            'password' => ['required', 'string', 'min:6'],
        ]);

        $ip = (string) $request->ip();

        // 试错次数打满后，锁定期内连正确验证码也一律拒（防在线穷举 6 位码）
        $blocked = $this->rateGuard->verifyBlocked($data['email'], $ip);
        if ($blocked !== null) {
            return $this->error($blocked, 429);
        }

        $cached = Cache::get('pwd_reset_' . $data['email']);

        if (!$cached || $cached !== $data['code']) {
            $this->rateGuard->recordVerifyFailure($data['email'], $ip);
            return $this->error(self::CODE_INVALID_MSG, 400);
        }

        $user = User::where('email', $data['email'])->first();

        // 防枚举：与上面「验证码错误或已过期」同一文案同一 code，不暴露注册状态
        if (!$user) {
            return $this->error(self::CODE_INVALID_MSG, 400);
        }

        $user->forceFill(['password' => Hash::make($data['password'])])->save();

        // 验证码一次性
        Cache::forget('pwd_reset_' . $data['email']);

        // 旧登录态全部作废
        AccessToken::where('user_id', $user->id)->delete();

        // 改密成功，之前几次试错不算数了
        $this->rateGuard->clearVerifyFailures($data['email'], $ip);

        return $this->success(null, '密码已重置');
    }
}
