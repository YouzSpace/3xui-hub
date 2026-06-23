<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\PaymentConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * Admin 支付配置管理。
 * GET/POST/PUT/DELETE /admin-api/payments
 */
class PaymentController extends Controller
{
    use ApiResponse;

    public function index(): \Illuminate\Http\JsonResponse
    {
        $configs = PaymentConfig::orderByDesc('id')->get();

        return $this->success($configs->map(fn (PaymentConfig $c) => $this->present($c))->values());
    }

    public function show(PaymentConfig $payment): \Illuminate\Http\JsonResponse
    {
        return $this->success($this->present($payment, true));
    }

    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateConfig($request);

        $config = PaymentConfig::create($data);

        return $this->success($this->present($config), '创建成功');
    }

    public function update(Request $request, PaymentConfig $payment): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateConfig($request, true);

        $payment->forceFill($data)->save();

        return $this->success($this->present($payment), '更新成功');
    }

    public function destroy(PaymentConfig $payment): \Illuminate\Http\JsonResponse
    {
        $payment->delete();

        return $this->success(null, '已删除');
    }

    private function validateConfig(Request $request, bool $isUpdate = false): array
    {
        $rules = [
            'name' => ['required', 'string', 'max:64'],
            'gateway' => ['required', 'string', 'max:255'],
            'query_gateway' => ['sometimes', 'nullable', 'string', 'max:255'],
            'member_id' => ['required', 'string', 'max:64'],
            'api_key' => [$isUpdate ? 'sometimes' : 'required', 'string', 'max:255'],
            'notify_url' => ['sometimes', 'nullable', 'string', 'max:255'],
            'bank_code' => ['sometimes', 'string', 'max:64'],
            'enabled' => ['sometimes', 'boolean'],
        ];

        return $request->validate($rules);
    }

    private function present(PaymentConfig $c, bool $full = false): array
    {
        $data = [
            'id' => $c->id,
            'name' => $c->name,
            'gateway' => $c->gateway,
            'query_gateway' => $c->query_gateway,
            'member_id' => $c->member_id,
            'notify_url' => $c->notify_url,
            'bank_code' => $c->bank_code,
            'enabled' => (bool) $c->enabled,
            'created_at' => $c->created_at?->toIso8601String(),
        ];

        if ($full) {
            $data['api_key'] = $c->api_key;
        } else {
            $data['has_api_key'] = !empty($c->api_key);
        }

        return $data;
    }
}
