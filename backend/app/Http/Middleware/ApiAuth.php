<?php

namespace App\Http\Middleware;

use App\Models\AccessToken;
use App\Traits\ApiResponse;
use Closure;
use Illuminate\Http\Request;
use Symfony\Component\HttpFoundation\Response;

/**
 * 用户端 Bearer 鉴权（M2.6）。
 * Authorization: Bearer {access_token} → 校验 AccessToken 未过期 → 注入 $request->user()。
 */
class ApiAuth
{
    use ApiResponse;

    public function handle(Request $request, Closure $next): Response
    {
        $header = $request->header('Authorization', '');
        $token = null;

        if (str_starts_with($header, 'Bearer ')) {
            $token = substr($header, 7);
        }

        if (!$token) {
            return $this->error('未认证', 401);
        }

        /** @var AccessToken|null $accessToken */
        $accessToken = AccessToken::where('token', $token)->first();

        if (!$accessToken || $accessToken->isExpired()) {
            return $this->error('未认证或 token 已过期', 401);
        }

        $user = $accessToken->user;

        if (!$user || !$user->enabled) {
            return $this->error('账号不可用', 401);
        }

        $accessToken->forceFill(['last_used_at' => now()])->save();

        $request->setUserResolver(fn () => $user);

        return $next($request);
    }
}
