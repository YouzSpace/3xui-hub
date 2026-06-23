<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Order;
use App\Traits\ApiResponse;

/**
 * Admin 订单管理。
 * GET /admin-api/orders
 */
class OrderController extends Controller
{
    use ApiResponse;

    public function index(): \Illuminate\Http\JsonResponse
    {
        $orders = Order::with(['user', 'plan', 'paymentConfig'])
            ->orderByDesc('id')
            ->get();

        return $this->success($orders->map(fn (Order $o) => [
            'order_no' => $o->order_no,
            'user_id' => $o->user_id,
            'user_email' => $o->user?->email,
            'plan_name' => $o->plan?->name ?? '-',
            'amount' => (float) $o->amount,
            'status' => $o->status,
            'payment_name' => $o->paymentConfig?->name,
            'trade_no' => $o->trade_no,
            'paid_at' => $o->paid_at?->toIso8601String(),
            'created_at' => $o->created_at->toIso8601String(),
        ])->values());
    }
}
