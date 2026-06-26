<?php

namespace App\Services;

use App\Models\Order;
use App\Models\PaymentConfig;
use App\Models\Plan;
use App\Models\User;
use Illuminate\Support\Facades\Http;
use Illuminate\Support\Facades\Log;

/**
 * 支付服务。
 */
class PaymentService
{
    /**
     * 创建订单并返回支付链接。
     */
    public function createOrder(User $user, int $planId, int $paymentConfigId = null): array
    {
        $plan = Plan::find($planId);
        if (!$plan) {
            throw new \InvalidArgumentException('套餐不存在');
        }

        if ($user->plan_id == $planId && $user->expired_at && $user->expired_at->isFuture()) {
            throw new \InvalidArgumentException('您已购买该套餐');
        }

        if ($paymentConfigId) {
            $payment = PaymentConfig::where('id', $paymentConfigId)->where('enabled', true)->first();
        } else {
            $payment = PaymentConfig::where('enabled', true)->first();
        }

        if (!$payment) {
            throw new \InvalidArgumentException('暂无可用支付方式');
        }

        $order = Order::create([
            'order_no' => Order::generateOrderNo(),
            'user_id' => $user->id,
            'plan_id' => $planId,
            'amount' => $plan->price,
            'status' => 'pending',
            'payment_config_id' => $payment->id,
            'pay_ip' => request()->ip(),
        ]);

        // 免费套餐直接完成
        if ($plan->price <= 0) {
            $this->completeOrder($order);
            return [
                'order_no' => $order->order_no,
                'amount' => 0,
                'status' => 'paid',
                'pay_url' => null,
            ];
        }

        // 调用支付网关获取支付链接
        $payUrl = $this->buildPayUrl($payment, $order);

        return [
            'order_no' => $order->order_no,
            'amount' => (float) $order->amount,
            'status' => 'pending',
            'pay_url' => $payUrl,
        ];
    }

    /**
     * 构建支付链接（POST请求获取h5_url）。
     */
    public function buildPayUrl(PaymentConfig $payment, Order $order): string
    {
        // notify_url 必须是完整的回调 URL，如果不是则用默认值
        $notifyUrl = ($payment->notify_url && str_starts_with($payment->notify_url, 'http'))
            ? $payment->notify_url
            : url('/api/payment/notify');
        $callbackUrl = url('/');

        $params = [
            'pay_memberid' => $payment->member_id,
            'pay_orderid' => $order->order_no,
            'pay_applydate' => $order->created_at->format('Y-m-d H:i:s'),
            'pay_bankcode' => $payment->bank_code,
            'pay_notifyurl' => $notifyUrl,
            'pay_callbackurl' => $callbackUrl,
            'pay_amount' => number_format($order->amount, 2, '.', ''),
            'pay_productname' => '套餐购买-' . ($order->plan->name ?? ''),
            'pay_ip' => $order->pay_ip ?: '127.0.0.1',
            'pay_type' => 'JSON',
        ];

        $params['pay_md5sign'] = $this->generateSign($params, $payment->api_key);

        // POST请求支付网关
        try {
            $response = Http::asForm()
                ->timeout(10)
                ->post($payment->gateway, $params);

            $data = $response->json();

            Log::info('支付网关响应', ['order_no' => $order->order_no, 'response' => $data]);

            if (($data['status'] ?? 0) == 1 && !empty($data['h5_url'])) {
                return $data['h5_url'];
            }

            Log::error('支付网关下单失败', ['order_no' => $order->order_no, 'msg' => $data['msg'] ?? 'unknown']);
            return '';
        } catch (\Throwable $e) {
            Log::error('支付网关请求异常', ['order_no' => $order->order_no, 'error' => $e->getMessage()]);
            return '';
        }
    }

    /**
     * 处理支付回调。
     */
    public function handleNotify(array $data): bool
    {
        $memberId = $data['memberid'] ?? '';
        $orderId = $data['orderid'] ?? '';
        $amount = $data['amount'] ?? '';
        $returnCode = $data['returncode'] ?? '';
        $sign = $data['sign'] ?? '';
        $tradeNo = $data['transaction_id'] ?? '';

        $order = Order::where('order_no', $orderId)->first();
        if (!$order) {
            Log::error('支付回调: 订单不存在', ['order_no' => $orderId]);
            return false;
        }

        $payment = $order->paymentConfig;
        if (!$payment || $payment->member_id != $memberId) {
            Log::error('支付回调: 商户号不匹配', ['order_no' => $orderId]);
            return false;
        }

        // 验证签名
        $verifyData = [
            'memberid' => $memberId,
            'orderid' => $orderId,
            'amount' => $amount,
            'transaction_id' => $tradeNo,
            'datetime' => $data['datetime'] ?? '',
            'returncode' => $returnCode,
        ];
        $expectedSign = $this->generateSign($verifyData, $payment->api_key);
        if (strcasecmp($sign, $expectedSign) !== 0) {
            Log::error('支付回调: 签名验证失败', ['order_no' => $orderId, 'expected' => $expectedSign, 'got' => $sign]);
            return false;
        }

        if ($returnCode !== '00') {
            Log::error('支付回调: 状态异常', ['returncode' => $returnCode]);
            return false;
        }

        if (abs((float)$amount - (float)$order->amount) > 0.01) {
            Log::error('支付回调: 金额不匹配', ['expected' => $order->amount, 'actual' => $amount]);
            return false;
        }

        $this->completeOrder($order, $tradeNo);

        return true;
    }

    /**
     * 完成订单。
     */
    public function completeOrder(Order $order, ?string $tradeNo = null): void
    {
        if ($order->status === 'paid') {
            return;
        }

        $order->forceFill([
            'status' => 'paid',
            'trade_no' => $tradeNo,
            'paid_at' => now(),
        ])->save();

        $user = $order->user;
        $plan = $order->plan;

        if ($user && $plan) {
            $user->forceFill(['plan_id' => $plan->id])->save();

            $userAdminService = app(\App\Services\UserAdminService::class);
            $userAdminService->provisionClient($user);
            $userAdminService->applyPlan($user);

            // 续费后总是启用 3x-ui client
            if (!$user->enabled) {
                $user->forceFill(['enabled' => true])->save();
            }
            app(\App\Services\BanService::class)->unban($user);
        }
    }

    /**
     * 查询订单状态。
     */
    public function queryOrder(Order $order): ?array
    {
        $payment = $order->paymentConfig;
        if (!$payment || !$payment->query_gateway) {
            return null;
        }

        $params = [
            'pay_memberid' => $payment->member_id,
            'pay_orderid' => $order->order_no,
        ];
        $params['pay_md5sign'] = $this->generateSign($params, $payment->api_key);

        try {
            $response = Http::asForm()
                ->timeout(10)
                ->post($payment->query_gateway, $params)
                ->json();

            if (($response['returncode'] ?? '') === '00') {
                $tradeState = $response['trade_state'] ?? '';

                if ($tradeState === 'SUCCESS' && $order->status !== 'paid') {
                    $this->completeOrder($order, $response['transaction_id'] ?? null);
                    $order->refresh();
                }

                return [
                    'status' => $tradeState === 'SUCCESS' ? 'paid' : 'pending',
                    'trade_state' => $tradeState,
                ];
            }
        } catch (\Throwable $e) {
            Log::error('订单查询失败', ['order_no' => $order->order_no, 'error' => $e->getMessage()]);
        }

        return null;
    }

    /**
     * 生成 MD5 签名。
     */
    public function generateSign(array $params, string $apiKey): string
    {
        $filtered = array_filter($params, fn ($v) => $v !== '' && $v !== null);
        ksort($filtered);
        $stringSignTemp = http_build_query($filtered) . '&key=' . $apiKey;
        return strtoupper(md5($stringSignTemp));
    }
}
