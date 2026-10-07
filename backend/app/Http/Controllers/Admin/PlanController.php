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

    /**
     * 列表分页：每页 50 条，orderByDesc('id') 保证翻页稳定。
     * 显式 ?all=1 才返回全量（用户/优惠码表单里的套餐选择器要列全部套餐）。
     */
    public function index(Request $request): \Illuminate\Http\JsonResponse
    {
        $query = Plan::orderByDesc('id');

        if ($request->boolean('all')) {
            return $this->success($query->get()->map(fn (Plan $p) => $this->present($p))->values());
        }

        return $this->successPage($query, fn (Plan $p) => $this->present($p));
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

    public function deactivate(Plan $plan): \Illuminate\Http\JsonResponse
    {
        $plan->forceFill(['is_active' => false])->save();

        return $this->success($this->present($plan), '已下架（已购用户不受影响）');
    }

    public function activate(Plan $plan): \Illuminate\Http\JsonResponse
    {
        $plan->forceFill(['is_active' => true])->save();

        return $this->success($this->present($plan), '已上架');
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
            // 第三方：本地节点开关 + 勾选的第三方订阅 ID 列表（JSON 数组）
            'include_local' => ['sometimes', 'boolean'],
            'third_party_sub_ids' => ['sometimes', 'array'],
            'third_party_sub_ids.*' => ['integer', 'exists:third_party_subs,id'],
        ]);

        // 纯第三方套餐（不勾本地、勾了第三方）：第三方不管流量只管时间 → 强制周期套餐。
        // 必须在「按类型清理无关字段」之前改 type：否则 type=total 的清理分支会把 months 置 null，
        // 传入的天数就丢了（测试 test_pure_third_party_plan_forced_period_zero_traffic 验证过）。
        $includeLocal = array_key_exists('include_local', $data) ? (bool) $data['include_local'] : true;
        if (!$includeLocal && !empty($data['third_party_sub_ids'] ?? [])) {
            $data['type'] = 'period';
        }

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

        // 第三方字段：未传时给默认值（include_local 默认 true，老套餐行为不变；ids 默认空数组）
        $data['include_local'] = array_key_exists('include_local', $data)
            ? (bool) $data['include_local']
            : true;
        $data['third_party_sub_ids'] = $data['third_party_sub_ids'] ?? [];

        // 纯第三方套餐（不勾本地、勾了第三方）：流量字段全部归零（上面已把 type 锁成周期）。
        // 用户端据此不显示流量、只显示到期日期。
        if (!$data['include_local'] && !empty($data['third_party_sub_ids'])) {
            $data['monthly_traffic'] = 0;
            $data['period_traffic'] = 0;
            $data['total_traffic'] = null;
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
            'is_active' => (bool) $p->is_active,
            // 第三方：本地节点开关 + 勾选的第三方订阅 ID 列表
            'include_local' => (bool) ($p->include_local ?? true),
            'third_party_sub_ids' => array_values(array_map('intval', (array) ($p->third_party_sub_ids ?? []))),
            'created_at' => $p->created_at?->toIso8601String(),
        ];
    }
}
