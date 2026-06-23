<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Admin;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Hash;
use Illuminate\Support\Facades\RateLimiter;
use PragmaRX\Google2FA\Google2FA;

/**
 * Admin 认证 + 设置 + 2FA。
 */
class AuthController extends Controller
{
    use ApiResponse;

    private function google2fa(): Google2FA
    {
        return new Google2FA();
    }

    public function login(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->json()->all();
        $username = (string) ($data['username'] ?? '');
        $password = (string) ($data['password'] ?? '');
        $otp = (string) ($data['otp'] ?? '');

        $key = 'admin.login:' . $username . '|' . $request->ip();

        if (RateLimiter::tooManyAttempts($key, 5)) {
            return $this->error('尝试过于频繁，请稍后再试', 429);
        }

        $admin = Admin::where('username', $username)->first();

        if (!$admin || !Hash::check($password, $admin->password)) {
            RateLimiter::hit($key, 60);
            return $this->error('账号或密码错误', 401);
        }

        // 2FA 验证
        if ($admin->google2fa_enabled && $admin->google2fa_secret) {
            if (!$otp) {
                return $this->error('请输入验证码', 401);
            }
            $valid = $this->google2fa()->verifyKey($admin->google2fa_secret, $otp);
            if (!$valid) {
                RateLimiter::hit($key, 60);
                return $this->error('验证码错误', 401);
            }
        }

        RateLimiter::clear($key);
        session(['admin_id' => $admin->id]);

        return $this->success([
            'admin' => ['id' => $admin->id, 'username' => $admin->username],
        ], '登录成功');
    }

    public function logout(Request $request): \Illuminate\Http\JsonResponse
    {
        $request->session()->forget('admin_id');
        return $this->success(null, '已退出');
    }

    public function settings(Request $request): \Illuminate\Http\JsonResponse
    {
        $admin = $this->currentAdmin($request);
        if (!$admin) {
            return $this->error('未登录', 401);
        }

        return $this->success([
            'id' => $admin->id,
            'username' => $admin->username,
            'google2fa_enabled' => (bool) $admin->google2fa_enabled,
        ]);
    }

    public function changePassword(Request $request): \Illuminate\Http\JsonResponse
    {
        $admin = $this->currentAdmin($request);
        if (!$admin) {
            return $this->error('未登录', 401);
        }

        $data = $request->validate([
            'old_password' => ['required', 'string'],
            'new_password' => ['required', 'string', 'min:6', 'max:64'],
        ]);

        if (!Hash::check($data['old_password'], $admin->password)) {
            return $this->error('原密码错误', 400);
        }

        $admin->forceFill(['password' => Hash::make($data['new_password'])])->save();

        return $this->success(null, '密码已修改');
    }

    public function updateUsername(Request $request): \Illuminate\Http\JsonResponse
    {
        $admin = $this->currentAdmin($request);
        if (!$admin) {
            return $this->error('未登录', 401);
        }

        $data = $request->validate([
            'username' => ['required', 'string', 'min:3', 'max:32'],
            'password' => ['required', 'string'],
        ]);

        if (!Hash::check($data['password'], $admin->password)) {
            return $this->error('密码错误', 400);
        }

        $admin->forceFill(['username' => $data['username']])->save();

        return $this->success(['username' => $admin->username], '用户名已修改');
    }

    /**
     * 生成 2FA 密钥和二维码。
     */
    public function google2faGenerate(Request $request): \Illuminate\Http\JsonResponse
    {
        $admin = $this->currentAdmin($request);
        if (!$admin) {
            return $this->error('未登录', 401);
        }

        $google2fa = $this->google2fa();
        $secret = $google2fa->generateSecretKey();

        // 临时存 session，验证后才真正启用
        session(['google2fa_secret' => $secret]);

        $qrCodeUrl = $google2fa->getQRCodeUrl(
            'ControlHub',
            $admin->username,
            $secret
        );

        return $this->success([
            'secret' => $secret,
            'qr_url' => $qrCodeUrl,
        ]);
    }

    /**
     * 验证并启用 2FA。
     */
    public function google2faEnable(Request $request): \Illuminate\Http\JsonResponse
    {
        $admin = $this->currentAdmin($request);
        if (!$admin) {
            return $this->error('未登录', 401);
        }

        $data = $request->validate([
            'otp' => ['required', 'string', 'size:6'],
        ]);

        $secret = session('google2fa_secret');
        if (!$secret) {
            return $this->error('请先生成密钥', 400);
        }

        $valid = $this->google2fa()->verifyKey($secret, $data['otp']);
        if (!$valid) {
            return $this->error('验证码错误', 400);
        }

        $admin->forceFill([
            'google2fa_secret' => $secret,
            'google2fa_enabled' => true,
        ])->save();

        session()->forget('google2fa_secret');

        return $this->success(null, '谷歌验证已启用');
    }

    /**
     * 关闭 2FA（需当前密码）。
     */
    public function google2faDisable(Request $request): \Illuminate\Http\JsonResponse
    {
        $admin = $this->currentAdmin($request);
        if (!$admin) {
            return $this->error('未登录', 401);
        }

        $data = $request->validate([
            'password' => ['required', 'string'],
        ]);

        if (!Hash::check($data['password'], $admin->password)) {
            return $this->error('密码错误', 400);
        }

        $admin->forceFill([
            'google2fa_secret' => null,
            'google2fa_enabled' => false,
        ])->save();

        return $this->success(null, '谷歌验证已关闭');
    }

    private function currentAdmin(Request $request): ?Admin
    {
        $adminId = $request->session()->get('admin_id');
        return $adminId ? Admin::find($adminId) : null;
    }
}
