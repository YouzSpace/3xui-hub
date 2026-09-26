<?php

namespace App\Services;

use App\Models\DiscountCode;
use App\Models\Domain;
use App\Models\Order;
use App\Models\PaymentConfig;
use App\Models\Plan;
use App\Models\User;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Http;
use Illuminate\Support\Facades\Log;

/**
 * 支付服务。
 */
class PaymentService
{
    public function __construct(
        private UserAdminService $userAdminService,
        private BanService $banService,
        private DiscountCodeService $discountCodeService,
        private RateGuardService $rateGuard,
    ) {}

    /**
     * 创建订单并返回支付链接。
     *
     * 折扣在下单时算定并固化到订单（amount = 折后实付，original_amount = 原价），
     * 支付回调不再重新计算。
     *
     * 传了折扣码时走与 /api/discount/check 同一套限流（RateGuardService），
     * 否则下单接口就是一条可被脚本枚举的旁路。
     */
    public function createOrder(User $user, int $planId, ?int $paymentConfigId = null, ?string $discountCode = null): array
    {
        $plan = Plan::find($planId);
        if (!$plan) {
            throw new \InvalidArgumentException('套餐不存在');
        }

        if (!$plan->is_active) {
            throw new \InvalidArgumentException('该套餐已下架');
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

        $originalAmount = (float) $plan->price;
        // 原价为 0 的免费套餐不适用折扣码（折后价不可能 ≥ 原价），直接跳过校验
        $useDiscount = $discountCode !== null && trim($discountCode) !== '' && $originalAmount > 0;
        $reused = false;

        if ($useDiscount) {
            $userId = (int) $user->id;
            $ip     = (string) request()->ip();

            // 限流与 /api/discount/check 共用同一套计数（RateGuardService）：下单是第二个
            // 能拿码试错的入口，不接这里脚本就能绕过 check 的限流直接枚举。
            // 检查排在查库之前：被限流/锁定时直接抛，一个字节的库都不查，也就没有枚举反馈。
            $blocked = $this->rateGuard->discountCheckBlocked($userId, $ip);
            if ($blocked !== null) {
                throw new \InvalidArgumentException($blocked);
            }
            $this->rateGuard->recordDiscountRequest($userId);

            try {
                // 校验 + 建单放在同一事务里，失败整体回滚。
                // **不占用次数**：这里只把 discount_code_id 登记到订单上，真正占用
                // （used_count+1、邀请码锁死用户）后移到支付成功的 completeOrder()。
                // 未支付/放弃/超时的订单因此不需要任何回滚 —— 压根没占用过。
                $order = DB::transaction(function () use ($user, $plan, $planId, $payment, $originalAmount, $discountCode, &$reused) {
                    $result = $this->discountCodeService->validateAndApply($user, $discountCode, $plan, $originalAmount);

                    // 同一用户已有一笔用同一张码、同一套餐的 pending 单 → 复用那笔，不新建。
                    // 占用后移后，重复建单没有意义，只会让同一张码在多笔 pending 单上排队。
                    if ($result['reuse_order']) {
                        $reused = true;

                        return $result['reuse_order'];
                    }

                    return Order::create([
                        'order_no' => Order::generateOrderNo(),
                        'user_id' => $user->id,
                        'plan_id' => $planId,
                        'amount' => $result['final_amount'],
                        'original_amount' => $originalAmount,
                        'discount_amount' => round($originalAmount - $result['final_amount'], 2),
                        'discount_code_id' => $result['code']->id,
                        'status' => 'pending',
                        'payment_config_id' => $payment->id,
                        'pay_ip' => request()->ip(),
                    ]);
                });
            } catch (\InvalidArgumentException $e) {
                // 码无效 / 已失效 / 已被占用：记一次失败。记账放在事务之外，订单回滚掉，
                // 失败计数必须留下 —— 计数跟着回滚等于没限流（计数走 RateLimiter，
                // 与订单事务互不牵连）。
                $this->rateGuard->recordDiscountFailure($userId, $ip);

                throw $e;
            }

            // 校验通过：清掉自己攒的失败计数（同 check 接口口径，不清 IP 维度）
            $this->rateGuard->clearDiscountFailures($userId);
        } else {
            $order = Order::create([
                'order_no' => Order::generateOrderNo(),
                'user_id' => $user->id,
                'plan_id' => $planId,
                'amount' => $originalAmount,
                'original_amount' => $originalAmount,
                'status' => 'pending',
                'payment_config_id' => $payment->id,
                'pay_ip' => request()->ip(),
            ]);
        }

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

        // 调用支付网关获取支付链接。
        // 复用旧单时用订单自己的支付配置（与 PaymentController 重发 pending 单的口径一致），
        // 新建单才用本次请求选中的支付方式。
        $payUrl = $this->buildPayUrl($reused ? ($order->paymentConfig ?: $payment) : $payment, $order);

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
        // notify_url 必须是完整的回调 URL，如果不是则用主域名默认值（网关固定打主域）
        $notifyUrl = ($payment->notify_url && str_starts_with($payment->notify_url, 'http'))
            ? $payment->notify_url
            : $this->defaultNotifyUrl();
        // 回跳地址 = 下单请求所在域名（多域名：b.zes.one 下单回 b.zes.one，不回主域）
        $callbackUrl = request()->getSchemeAndHttpHost() . '/';

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
     * 缺省异步通知地址：domains 主域名 + /api/payment/notify；无主域行（老站）回退 url()。
     */
    private function defaultNotifyUrl(): string
    {
        $primary = Domain::where('is_primary', true)->where('enabled', true)->first();
        if (!$primary) {
            return url('/api/payment/notify');
        }

        // scheme https 优先（域名接入层默认走 TLS）
        return 'https://' . $primary->domain . '/api/payment/notify';
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
     * 处理支付回调（区分普通订单和重置订单）。
     */
    public function completeOrder(Order $order, ?string $tradeNo = null): void
    {
        // 重置流量订单走专用逻辑
        if (str_starts_with($order->order_no, 'RST')) {
            $this->completeResetOrder($order, $tradeNo);
            return;
        }

        if ($order->status === 'paid') {
            return;
        }

        $order->forceFill([
            'status' => 'paid',
            'trade_no' => $tradeNo,
            'paid_at' => now(),
        ])->save();

        $user = $order->user;

        // 支付成功才真正占用折扣码：used_count+1、满额置 used_up、邀请码锁死用户。
        // 必须排在 rewardInviter 之前（发奖建立在「本次邀请已生效」之上）。
        // 首次完成才走到这里（上面已有 paid 早退），重复回调不会二次占用。
        if ($order->discount_code_id && $user) {
            $usedCode = DiscountCode::find($order->discount_code_id);
            if ($usedCode) {
                try {
                    $this->discountCodeService->consume($usedCode, $user);
                } catch (\Throwable $e) {
                    // 并发兜底：码在支付前已被别处用满。用户钱已经付了，订单必须照常完成，
                    // 这里只记日志，绝不向上抛 —— 抛出去回调会返回 FAIL，网关会反复重推。
                    Log::warning('支付成功但折扣码占用失败（码可能已用满）', [
                        'order_no' => $order->order_no,
                        'discount_code_id' => $order->discount_code_id,
                        'error' => $e->getMessage(),
                    ]);
                }
            }
        }

        // 首次完成才走到这里（上面已有 paid 早退），再叠加 rewardInviter 内部幂等判断形成双保险
        $this->discountCodeService->rewardInviter($order);

        $plan = $order->plan;

        if ($user && $plan) {
            $user->forceFill(['plan_id' => $plan->id])->save();
            $user->load('plan'); // 刷新关系，确保 applyPlan 读到新套餐

            $this->userAdminService->applyPlan($user);
            $this->userAdminService->provisionClient($user);

            // 续费后总是启用 3x-ui client
            if (!$user->enabled) {
                $user->forceFill(['enabled' => true])->save();
            }
            $this->banService->unban($user);
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

    /**
     * 创建重置流量订单。
     * 价格 = 套餐价格，重置量 = 套餐月流量。
     */
    public function createResetOrder(User $user, ?int $paymentConfigId = null): array
    {
        $plan = $user->plan;
        if (!$plan || $plan->type !== 'period') {
            throw new \InvalidArgumentException('仅周期套餐可购买重置流量');
        }

        if ($paymentConfigId) {
            $payment = PaymentConfig::where('id', $paymentConfigId)->where('enabled', true)->first();
        } else {
            $payment = PaymentConfig::where('enabled', true)->first();
        }

        if (!$payment) {
            throw new \InvalidArgumentException('暂无可用支付方式');
        }

        $resetPrice = (float) ($plan->reset_price ?? 0);

        $order = Order::create([
            'order_no' => 'RST' . Order::generateOrderNo(),
            'user_id' => $user->id,
            'plan_id' => $plan->id,
            'amount' => $resetPrice,
            'status' => 'pending',
            'payment_config_id' => $payment->id,
            'pay_ip' => request()->ip(),
        ]);

        // 免费直接完成
        if ($resetPrice <= 0) {
            $this->completeResetOrder($order);
            return [
                'order_no' => $order->order_no,
                'status' => 'paid',
            ];
        }

        $payUrl = $this->buildPayUrl($payment, $order);

        return [
            'order_no' => $order->order_no,
            'pay_url' => $payUrl,
            'status' => 'pending',
        ];
    }

    /**
     * 完成重置流量订单：加总流量 + 清零月流量 + 打开 3x-ui。
     */
    public function completeResetOrder(Order $order, ?string $tradeNo = null): void
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

        if (!$user || !$plan) return;

        // 加总流量（3x-ui 总流量 + 月流量额度）
        $addTraffic = $plan->monthly_traffic ?? 0;
        $user->forceFill([
            'traffic_limit' => ($user->traffic_limit ?? 0) + $addTraffic,
            'monthly_traffic_used' => 0,
        ])->save();

        // 同步新的流量限制到 3x-ui + 重置流量计数 + 打开连接
        $user->refresh();
        $this->userAdminService->provisionClient($user);
        $this->userAdminService->resetClientOnNodes($user);
        $this->banService->toggleClient($user, true);
    }
}
