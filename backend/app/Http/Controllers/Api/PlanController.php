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
        $plans = Plan::orderBy('price')->get();

        return $this->success($plans->map(fn (Plan $p) => [
            'id' => $p->id,
            'name' => $p->name,
            'price' => (float) $p->price,
            'type' => $p->type,
            'months' => $p->months,
            'monthly_traffic' => $p->monthly_traffic,
            'period_traffic' => $p->period_traffic,
            'total_traffic' => $p->total_traffic,
        ])->values());
    }
}
