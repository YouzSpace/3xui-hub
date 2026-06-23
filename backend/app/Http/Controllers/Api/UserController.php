<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端（M3.1）：GET /api/me 返回当前用户脱敏信息。
 */
class UserController extends Controller
{
    use ApiResponse;

    public function me(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        if (!$user) {
            return $this->error('未认证', 401);
        }

        $user->load('plan');

        return $this->success([
            'id' => $user->id,
            'email' => $user->email,
            'token' => $user->token,
            'protocol' => $user->protocol,
            'plan_id' => $user->plan_id,
            'plan_type' => $user->plan?->type,
            'plan_name' => $user->plan?->name,
            'traffic_limit' => (int) $user->traffic_limit,
            'traffic_used' => (int) $user->traffic_used,
            'monthly_traffic_used' => (int) $user->monthly_traffic_used,
            'monthly_traffic_limit' => $user->plan?->monthly_traffic ?? 0,
            'expired_at' => $user->expired_at?->toIso8601String(),
            'enabled' => (bool) $user->enabled,
        ]);
    }
}
