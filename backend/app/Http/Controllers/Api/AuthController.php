<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\AccessToken;
use App\Models\User;
use App\Services\RateGuardService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Hash;
use Illuminate\Support\Str;
use App\Services\UserAdminService;

/**
 * 用户端认证。
 * POST /api/login       → Token 登录
 * POST /api/login-email → 邮箱密码登录
 * POST /api/register    → 邮箱注册
 * GET  /api/captcha     → 图形验证码
 *
 * 限流规则（管理员可在后台「邮箱配置 → 安全限制」调）见 RateGuardService。
 */
class AuthController extends Controller
{
    use ApiResponse;

    public function __construct(private RateGuardService $rateGuard)
    {
    }

    /** Token 登录 */
    public function login(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->json()->all();
        $token = $data['token'] ?? null;

        if (!$token) {
            return $this->error('token 必填', 400);
        }

        $user = User::where('token', $token)->first();

        if (!$user || !$user->enabled) {
            return $this->error('token 无效', 401);
        }

        return $this->success(['access_token' => $this->createAccessToken($user)]);
    }

    /** 邮箱密码登录 */
    public function loginEmail(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->json()->all();
        $email = $data['email'] ?? '';
        $password = $data['password'] ?? '';

        if (!$email || !$password) {
            return $this->error('邮箱和密码必填', 400);
        }

        $ip = (string) $request->ip();

        // 登录锁定（与管理员登录同款语义）：锁定期间连正确密码也拒
        $blocked = $this->rateGuard->loginBlocked($email, $ip);
        if ($blocked !== null) {
            return $this->error($blocked, 429);
        }

        $user = User::where('email', $email)->first();

        if (!$user || !$user->password || !Hash::check($password, $user->password)) {
            // 邮箱不存在与密码错误共用同一条文案，计数也共用同一个键
            $this->rateGuard->recordLoginFailure($email, $ip);
            return $this->error('邮箱或密码错误', 401);
        }

        if (!$user->enabled) {
            return $this->error('账号已禁用', 403);
        }

        $this->rateGuard->clearLoginFailures($email, $ip);

        return $this->success(['access_token' => $this->createAccessToken($user)]);
    }

    /** 邮箱注册 */
    public function register(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->json()->all();
        $email = $data['email'] ?? '';
        $password = $data['password'] ?? '';
        $captcha = $data['captcha'] ?? '';
        $emailCode = $data['email_code'] ?? '';

        $ip = (string) $request->ip();

        // 注册按 IP 限流，入口先卡一道；放行即记数（后面的校验失败分支同样占额度）
        $blocked = $this->rateGuard->registerBlocked($ip);
        if ($blocked !== null) {
            return $this->error($blocked, 429);
        }
        $this->rateGuard->recordRegister($ip);

        if (!$email || !$password) {
            return $this->error('邮箱和密码必填', 400);
        }

        if (!filter_var($email, FILTER_VALIDATE_EMAIL)) {
            return $this->error('邮箱格式不正确', 400);
        }

        if (strlen($password) < 6) {
            return $this->error('密码至少 6 位', 400);
        }

        // 检查是否启用邮箱验证注册
        $useEmailVerify = \App\Models\SiteConfig::getValue('register_email_verify');

        if ($useEmailVerify) {
            // 邮箱验证码校验
            if (!$emailCode) {
                return $this->error('邮箱验证码必填', 400);
            }

            $cachedCode = \Illuminate\Support\Facades\Cache::get('email_verify_' . $email);
            if (!$cachedCode || $cachedCode !== $emailCode) {
                return $this->error('邮箱验证码错误', 400);
            }

            // 验证成功后删除验证码
            \Illuminate\Support\Facades\Cache::forget('email_verify_' . $email);
        } else {
            // 图形验证码校验
            $sessionCaptcha = session('captcha');
            if (!$sessionCaptcha || strtolower($captcha) !== strtolower($sessionCaptcha)) {
                return $this->error('验证码错误', 400);
            }
            session()->forget('captcha');
        }

        if (User::where('email', $email)->exists()) {
            return $this->error('邮箱已注册', 409);
        }

        $user = User::create([
            'email' => $email,
            'password' => Hash::make($password),
            'token' => 'sub_' . Str::random(32),
            'uuid' => Str::uuid()->toString(),
            'protocol' => 'vless',
            'enabled' => true,
        ]);

        // 建号改为异步：按「用户 × enabled 节点」派发 Job，注册请求不再等面板 HTTPS。
        // 派发失败与原来的 provisionClient 失败同语义：只记日志，注册照样成功。
        try {
            app(UserAdminService::class)->dispatchProvisionClient($user);
        } catch (\Throwable $e) {
            \Illuminate\Support\Facades\Log::error('dispatchProvisionClient failed on register', ['error' => $e->getMessage()]);
        }

        return $this->success([
            'access_token' => $this->createAccessToken($user),
            'token' => $user->token,
            'provisioning' => true,
        ], '注册成功');
    }

    /** 图形验证码 */
    public function captcha(): \Illuminate\Http\Response
    {
        $width = 120;
        $height = 40;
        $image = imagecreatetruecolor($width, $height);

        // 背景
        $bgColor = imagecolorallocate($image, 245, 245, 245);
        imagefill($image, 0, 0, $bgColor);

        // 随机字符
        $chars = '23456789ABCDEFGHJKLMNPQRSTUVWXYZ';
        $code = '';
        for ($i = 0; $i < 4; $i++) {
            $code .= $chars[random_int(0, strlen($chars) - 1)];
        }

        // 画干扰线
        for ($i = 0; $i < 5; $i++) {
            $lineColor = imagecolorallocate($image, random_int(100, 200), random_int(100, 200), random_int(100, 200));
            imageline($image, random_int(0, $width), random_int(0, $height), random_int(0, $width), random_int(0, $height), $lineColor);
        }

        // 画干扰点
        for ($i = 0; $i < 50; $i++) {
            $dotColor = imagecolorallocate($image, random_int(100, 200), random_int(100, 200), random_int(100, 200));
            imagesetpixel($image, random_int(0, $width), random_int(0, $height), $dotColor);
        }

        // 画字符
        for ($i = 0; $i < 4; $i++) {
            $textColor = imagecolorallocate($image, random_int(0, 100), random_int(0, 100), random_int(0, 100));
            $fontSize = random_int(14, 18);
            $angle = random_int(-15, 15);
            $x = 10 + $i * 28;
            $y = random_int(25, 35);
            imagestring($image, 5, $x, $y - 15, $code[$i], $textColor);
        }

        session(['captcha' => $code]);

        ob_start();
        imagepng($image);
        $content = ob_get_clean();
        imagedestroy($image);

        return response($content)
            ->header('Content-Type', 'image/png')
            ->header('Cache-Control', 'no-cache, no-store');
    }

    private function createAccessToken(User $user): string
    {
        $accessToken = AccessToken::create([
            'user_id' => $user->id,
            'token' => Str::random(64),
            'expires_at' => now()->addDays(30),
        ]);

        return $accessToken->token;
    }
}
