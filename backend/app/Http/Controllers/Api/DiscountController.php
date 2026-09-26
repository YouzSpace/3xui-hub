<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\DiscountCode;
use App\Models\Order;
use App\Models\Plan;
use App\Services\DiscountCodeService;
use App\Services\RateGuardService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端折扣码接口（邀请码 / 兑换码）。
 *
 * check 是「仅校验不占用次数」的接口，码空间只有 6~8 位，天然是穷举靶子：
 * 限流（RateGuardService）挡频率、锁定挡连续试错、统一文案抹掉枚举反馈，
 * 三者缺一不可。限流只影响接口调用，不动任何业务数据（不增 used_count、不锁用户）。
 */
class DiscountController extends Controller
{
    use ApiResponse;

    public function __construct(
        private DiscountCodeService $discountCodeService,
        private RateGuardService $rateGuard,
    ) {
    }

    /**
     * 我的邀请码信息 + 我的兑换码列表。
     */
    public function myInvite(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();
        $invite = $this->discountCodeService->ensureInviteCode($user);

        // 已邀请成功人数：用我名下（含已刷新的历史码）任意邀请码下单且已支付的订单数。
        // 只算当前码会在刷新后把历史邀请数清零。
        $inviteIds = DiscountCode::where('source', DiscountCode::SOURCE_INVITE)
            ->where('user_id', $user->id)
            ->pluck('id');

        $invitedCount = Order::whereIn('discount_code_id', $inviteIds)
            ->where('status', 'paid')
            ->count();

        $redeemCodes = DiscountCode::where('source', DiscountCode::SOURCE_REDEEM)
            ->where('user_id', $user->id)
            ->orderByDesc('id')
            ->get()
            ->map(function (DiscountCode $c) {
                // 过期未落库的按过期展示（只读推导，不写库）
                $status = ($c->status === DiscountCode::STATUS_ACTIVE && $c->expires_at && $c->expires_at->isPast())
                    ? DiscountCode::STATUS_EXPIRED
                    : $c->status;

                return [
                    'code' => $c->code,
                    'discount' => (float) $c->discount,
                    'note' => $c->note,
                    'status' => $status,
                    'expires_at' => $c->expires_at?->toIso8601String(),
                ];
            });

        return $this->success([
            'invite' => [
                'code' => $invite->code,
                'discount' => (float) $invite->discount,
                'next_refresh_at' => $invite->next_refresh_at?->toIso8601String(),
                'invited_count' => $invitedCount,
                'enabled' => DiscountCodeService::inviteEnabled(),
                'used_invite' => (bool) $user->used_invite_code_id,
                'used_invite_at' => $user->invite_code_used_at?->toIso8601String(),
            ],
            'redeem_codes' => $redeemCodes,
        ]);
    }

    /**
     * 校验一个码（仅校验，不占用次数），给前端"输码即时反馈"用。
     *
     * 限流是入口第一步，排在查库前面：被限流/锁定时直接返回、**一个字节的库都不查**，
     * 拿不到「码存不存在」的任何信号（包括查询耗时）。放行即记一次请求，失败再各记一笔。
     */
    public function check(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();
        $userId = (int) $user->id;
        $ip = (string) $request->ip();

        $data = $request->validate([
            'code' => ['required', 'string', 'max:32'],
            'plan_id' => ['required', 'integer'],
        ]);

        $blocked = $this->rateGuard->discountCheckBlocked($userId, $ip);
        if ($blocked !== null) {
            return $this->error($blocked, 429);
        }

        $this->rateGuard->recordDiscountRequest($userId);

        $plan = Plan::find($data['plan_id']);
        if (! $plan) {
            return $this->error('套餐不存在', 400);
        }

        try {
            $result = $this->discountCodeService->validateAndApply($user, $data['code'], $plan, (float) $plan->price);
        } catch (\InvalidArgumentException $e) {
            // 失败记账：用户维度（连续失败 + 每日）+ IP 维度
            $this->rateGuard->recordDiscountFailure($userId, $ip);

            return $this->success([
                'usable' => false,
                'discount' => null,
                'final_amount' => null,
                'note' => null,
                'message' => $e->getMessage(),
            ]);
        }

        // 校验通过：清掉自己攒的失败计数，别让几次手误拖累后面的正常输码
        $this->rateGuard->clearDiscountFailures($userId);

        return $this->success([
            'usable' => true,
            'discount' => $result['discount'],
            'final_amount' => $result['final_amount'],
            'note' => $result['code']->note,
            'message' => '优惠码可用',
        ]);
    }
}
