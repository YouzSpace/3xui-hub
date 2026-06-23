<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Order;
use App\Models\PaymentConfig;
use App\Services\PaymentService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端支付接口。
 */
class PaymentController extends Controller
{
    use ApiResponse;

    public function __construct(private PaymentService $paymentService)
    {
    }

    /**
     * 创建订单。
     */
    public function create(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        $data = $request->validate([
            'plan_id' => ['required', 'integer'],
            'payment_config_id' => ['sometimes', 'nullable', 'integer'],
        ]);

        // 检查是否有1分钟内的待支付订单（同一个套餐）
        $recentOrder = Order::where('user_id', $user->id)
            ->where('plan_id', $data['plan_id'])
            ->where('status', 'pending')
            ->where('created_at', '>=', now()->subMinute())
            ->first();

        if ($recentOrder) {
            return $this->error('请勿频繁下单，请1分钟后再试', 429);
        }

        // 查找该套餐的待支付订单（超过1分钟的，可以重新发起支付）
        $pendingOrder = Order::where('user_id', $user->id)
            ->where('plan_id', $data['plan_id'])
            ->where('status', 'pending')
            ->where('created_at', '<', now()->subMinute())
            ->orderByDesc('id')
            ->first();

        if ($pendingOrder) {
            // 重新获取支付链接
            $payment = $pendingOrder->paymentConfig;
            if ($payment) {
                $payUrl = $this->paymentService->buildPayUrl($payment, $pendingOrder);
                if ($payUrl) {
                    return $this->success([
                        'order_no' => $pendingOrder->order_no,
                        'amount' => (float) $pendingOrder->amount,
                        'status' => 'pending',
                        'pay_url' => $payUrl,
                    ]);
                }
            }
        }

        try {
            $result = $this->paymentService->createOrder($user, $data['plan_id'], $data['payment_config_id'] ?? null);
        } catch (\InvalidArgumentException $e) {
            return $this->error($e->getMessage(), 400);
        } catch (\Throwable $e) {
            report($e);
            return $this->error('创建订单失败', 500);
        }

        if (empty($result['pay_url']) && $result['status'] === 'pending') {
            return $this->error('获取支付链接失败，请稍后再试', 500);
        }

        return $this->success($result);
    }

    /**
     * 支付回调（无需鉴权）。
     */
    public function notify(Request $request): string
    {
        $data = $request->all();
        $success = $this->paymentService->handleNotify($data);
        return $success ? 'OK' : 'FAIL';
    }

    /**
     * 查询订单状态。
     */
    public function status(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        $data = $request->validate([
            'order_no' => ['required', 'string'],
        ]);

        $order = Order::where('order_no', $data['order_no'])
            ->where('user_id', $user->id)
            ->first();

        if (!$order) {
            return $this->error('订单不存在', 404);
        }

        if ($order->status === 'pending') {
            $this->paymentService->queryOrder($order);
            $order->refresh();
        }

        return $this->success([
            'order_no' => $order->order_no,
            'status' => $order->status,
            'amount' => (float) $order->amount,
            'paid_at' => $order->paid_at?->toIso8601String(),
        ]);
    }

    /**
     * 获取可用支付方式列表。
     */
    public function methods(): \Illuminate\Http\JsonResponse
    {
        $methods = PaymentConfig::where('enabled', true)
            ->select('id', 'name', 'bank_code')
            ->get();

        return $this->success($methods);
    }

    /**
     * 用户订单列表。
     */
    public function orders(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        $orders = Order::where('user_id', $user->id)
            ->with('plan')
            ->orderByDesc('id')
            ->limit(20)
            ->get()
            ->map(fn (Order $o) => [
                'order_no' => $o->order_no,
                'plan_id' => $o->plan_id,
                'plan_name' => $o->plan->name ?? '-',
                'amount' => (float) $o->amount,
                'status' => $o->status,
                'paid_at' => $o->paid_at?->toIso8601String(),
                'created_at' => $o->created_at->toIso8601String(),
            ]);

        return $this->success($orders);
    }

    /**
     * 继续支付（获取待支付订单的支付链接）。
     */
    public function retry(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        $data = $request->validate([
            'order_no' => ['required', 'string'],
        ]);

        $order = Order::where('order_no', $data['order_no'])
            ->where('user_id', $user->id)
            ->where('status', 'pending')
            ->first();

        if (!$order) {
            return $this->error('订单不存在或已支付', 404);
        }

        $payment = $order->paymentConfig;
        if (!$payment) {
            return $this->error('支付配置异常', 500);
        }

        $payUrl = $this->paymentService->buildPayUrl($payment, $order);

        if (!$payUrl) {
            return $this->error('获取支付链接失败', 500);
        }

        return $this->success([
            'order_no' => $order->order_no,
            'pay_url' => $payUrl,
        ]);
    }
}
