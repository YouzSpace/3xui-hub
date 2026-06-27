<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Plan;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * Admin 套餐管理。
 * GET/POST/PUT/DELETE /admin-api/plans
 */
class PlanController extends Controller
{
    use ApiResponse;

    public function index(): \Illuminate\Http\JsonResponse
    {
        $plans = Plan::orderByDesc('id')->get();

        return $this->success($plans->map(fn (Plan $p) => $this->present($p))->values());
    }

    public function show(Plan $plan): \Illuminate\Http\JsonResponse
    {
        return $this->success($this->present($plan));
    }

    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $this->validatePlan($request);

        $plan = Plan::create($data);

        return $this->success($this->present($plan), '创建成功');
    }

    public function update(Request $request, Plan $plan): \Illuminate\Http\JsonResponse
    {
        $data = $this->validatePlan($request, $plan);

        $plan->forceFill($data)->save();

        return $this->success($this->present($plan), '更新成功');
    }

    public function destroy(Plan $plan): \Illuminate\Http\JsonResponse
    {
        // 有关联用户时不允许删除（订单会级联删除）
        if ($plan->users()->exists()) {
            return $this->error('该套餐下还有用户，请先迁移用户到其他套餐');
        }

        $plan->delete();

        return $this->success(null, '已删除');
    }

    private function validatePlan(Request $request, ?Plan $plan = null): array
    {
        $data = $request->validate([
            'name' => ['required', 'string', 'max:64'],
            'price' => ['sometimes', 'numeric', 'min:0'],
            'reset_price' => ['sometimes', 'numeric', 'min:0'],
            'type' => ['required', 'in:period,total'],
            'months' => ['nullable', 'integer', 'min:1'],
            'monthly_traffic' => ['nullable', 'integer', 'min:0'],
            'period_traffic' => ['nullable', 'integer', 'min:0'],
            'total_traffic' => ['nullable', 'integer', 'min:0'],
        ]);

        // 根据类型清理无关字段
        if ($data['type'] === 'period') {
            $data['total_traffic'] = null;
            if (empty($data['months'])) {
                $data['months'] = 1;
            }
            // 周期总流量自动计算 = 每月流量 × 月数
            $data['period_traffic'] = ($data['monthly_traffic'] ?? 0) * $data['months'];
        } else {
            $data['months'] = null;
            $data['monthly_traffic'] = null;
            $data['period_traffic'] = null;
        }

        return $data;
    }

    private function present(Plan $p): array
    {
        return [
            'id' => $p->id,
            'name' => $p->name,
            'price' => (float) $p->price,
            'reset_price' => (float) ($p->reset_price ?? 0),
            'type' => $p->type,
            'months' => $p->months,
            'monthly_traffic' => $p->monthly_traffic,
            'period_traffic' => $p->period_traffic,
            'total_traffic' => $p->total_traffic,
            'created_at' => $p->created_at?->toIso8601String(),
        ];
    }
}
