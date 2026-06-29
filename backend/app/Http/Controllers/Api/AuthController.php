<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\AccessToken;
use App\Models\User;
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
 */
class AuthController extends Controller
{
    use ApiResponse;

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

        $user = User::where('email', $email)->first();

        if (!$user || !$user->password || !Hash::check($password, $user->password)) {
            return $this->error('邮箱或密码错误', 401);
        }

        if (!$user->enabled) {
            return $this->error('账号已禁用', 403);
        }

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

        // 同步到各 3x-ui 节点
        try {
            app(UserAdminService::class)->provisionClient($user);
        } catch (\Throwable $e) {
            \Illuminate\Support\Facades\Log::error('provisionClient failed on register', ['error' => $e->getMessage()]);
        }

        return $this->success([
            'access_token' => $this->createAccessToken($user),
            'token' => $user->token,
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
