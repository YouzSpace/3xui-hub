<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Plan;
use App\Traits\ApiResponse;

/**
 * 用户端套餐列表。
 * GET /api/plans
 */
class PlanController extends Controller
{
    use ApiResponse;

    public function index(): \Illuminate\Http\JsonResponse
    {
        $plans = Plan::where('is_active', true)->orderBy('price')->get();

        return $this->success($plans->map(fn (Plan $p) => [
            'id' => $p->id,
            'name' => $p->name,
            'price' => (float) $p->price,
            'type' => $p->type,
            'months' => $p->months,
            'monthly_traffic' => $p->monthly_traffic,
            'period_traffic' => $p->period_traffic,
            'total_traffic' => $p->total_traffic,
            // 纯第三方套餐（不勾本地）的购买页展示用：只显示天数，不显示流量
            'include_local' => (bool) ($p->include_local ?? true),
            'third_party_sub_ids' => array_values(array_map('intval', (array) ($p->third_party_sub_ids ?? []))),
        ])->values());
    }
}
