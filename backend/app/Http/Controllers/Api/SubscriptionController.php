<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\User;
use App\Services\SubscriptionException;
use App\Services\SubscriptionService;
use Illuminate\Http\Request;
use Illuminate\Http\Response;

/**
 * 订阅接口（M6）：GET /api/sub/{token}
 * 返回 text/plain 的 Base64 文本。
 */
class SubscriptionController extends Controller
{
    public function __construct(private SubscriptionService $service)
    {
    }

    public function show(Request $request, string $token): Response
    {
        /** @var User|null $user */
        $user = User::where('token', $token)->first();

        if (!$user) {
            return $this->text('[404] 用户不存在', 404, '用户不存在');
        }

        try {
            $body = $this->service->generate($user);
        } catch (SubscriptionException $e) {
            return $this->text("[{$e->codeValue}] {$e->getMessage()}", $e->codeValue, $e->getMessage());
        }

        // Subscription-Userinfo 头：代理客户端（V2rayN/Clash/v2rayNG）据此显示
        // 剩余流量与到期时间，更新订阅即刷新。格式：upload;download;total;expire
        $upload = 0;
        $download = (int) $user->traffic_used; // ControlHub 只记合计，放 download
        $total = (int) $user->traffic_limit;   // 0 = 不限
        $expire = $user->expired_at ? $user->expired_at->timestamp : 0; // 0 = 永久

        return $this->text($body)
            ->header('Subscription-Userinfo', "upload={$upload}; download={$download}; total={$total}; expire={$expire}");
    }

    private function text(string $body, int $code = 0, string $msg = 'ok'): Response
    {
        // 订阅接口固定 text/plain；非 0 code 时用自定义 header 表达错误（前端可读）
        return response($body, 200)
            ->header('Content-Type', 'text/plain; charset=UTF-8')
            ->header('X-CH-Code', (string) $code)
            ->header('X-CH-Msg', $msg);
    }
}
