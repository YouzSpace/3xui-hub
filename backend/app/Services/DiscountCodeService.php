<?php

namespace App\Services;

use App\Models\DiscountCode;
use App\Models\Order;
use App\Models\Plan;
use App\Models\SiteConfig;
use App\Models\User;
use Illuminate\Support\Facades\DB;

/**
 * 折扣码服务。
 * 集中所有校验与发奖逻辑，Controller 里不写业务判断。
 */
class DiscountCodeService
{
    /** 邀请码"总次数"：邀请码按「每人一次」限用（users.used_invite_code_id 锁死），码本身不设总量上限 */
    private const INVITE_MAX_USES = 999999;

    /**
     * 「码本身无效」类的统一文案（防枚举）。
     *
     * 不存在 / 已失效 / 已过期 / 已用完 必须共用这一句：一旦区分开，攻击者就能从文案里
     * 读出「这个码存在吗、走到哪一步了」，穷举脚本据此能大幅收敛搜索空间。
     * 反过来，「码有效但你不能用」类（自己的邀请码、已用过、非本人、限套餐、折后不足 1 元）
     * 不泄露码的存在性，且用户需要知道确切原因，保留各自明确文案，不改。
     */
    private const CODE_INVALID_MSG = '优惠码无效或不可用';

    /** 邀请码默认折扣 */
    public static function inviteDiscount(): float
    {
        return (float) SiteConfig::getValue('invite_discount', '0.90');
    }

    /** 系统发放兑换码的折扣 */
    public static function redeemDiscount(): float
    {
        return (float) SiteConfig::getValue('redeem_discount', '0.90');
    }

    /** 邀请码刷新周期（天） */
    public static function inviteRefreshDays(): int
    {
        return max(1, (int) SiteConfig::getValue('invite_refresh_days', '30'));
    }

    /** 邀请机制总开关 */
    public static function inviteEnabled(): bool
    {
        return SiteConfig::getValue('invite_enabled', '1') === '1';
    }

    /**
     * 校验并使用一个码，返回折扣结果。
     * 失败抛 InvalidArgumentException，message 直接面向用户显示。
     *
     * 注意：实际写入（used_count 自增、用户锁、发兑换码）不在这里，
     * 由 consume() / rewardInviter() 在事务 + 锁中完成。
     *
     * @return array{discount: float, final_amount: float, code: DiscountCode}
     */
    public function validateAndApply(User $user, string $code, Plan $plan, float $originalAmount): array
    {
        // 1. 查码（统一转大写去空格）
        $discountCode = DiscountCode::where('code', strtoupper(trim($code)))->first();
        if (! $discountCode) {
            throw new \InvalidArgumentException(self::CODE_INVALID_MSG);
        }

        // 2. 邀请码来源校验：总开关 → 不能自邀 → 一个用户一生只能用一次邀请码
        if ($discountCode->source === DiscountCode::SOURCE_INVITE) {
            if (! self::inviteEnabled()) {
                throw new \InvalidArgumentException('邀请机制已关闭，邀请码暂不可用');
            }
            if ((int) $discountCode->user_id === (int) $user->id) {
                throw new \InvalidArgumentException('不能使用自己的邀请码');
            }
            if ($user->used_invite_code_id) {
                throw new \InvalidArgumentException('您已使用过邀请码，无法再次使用');
            }
        }

        // 3. 兑换码来源校验：仅限持有者本人
        if ($discountCode->source === DiscountCode::SOURCE_REDEEM
            && (int) $discountCode->user_id !== (int) $user->id) {
            throw new \InvalidArgumentException('该兑换码仅限本人使用');
        }

        // 4. 状态校验（四种情况共用同一句文案，见 CODE_INVALID_MSG）
        if ($discountCode->status !== DiscountCode::STATUS_ACTIVE) {
            throw new \InvalidArgumentException(self::CODE_INVALID_MSG);
        }
        if ($discountCode->expires_at && $discountCode->expires_at->isPast()) {
            // 顺手把过期状态落库（幂等）
            $discountCode->forceFill(['status' => DiscountCode::STATUS_EXPIRED])->save();
            throw new \InvalidArgumentException(self::CODE_INVALID_MSG);
        }
        if ($discountCode->used_count >= $discountCode->max_uses) {
            throw new \InvalidArgumentException(self::CODE_INVALID_MSG);
        }

        // 5. 套餐绑定校验
        if ($discountCode->plan_id && (int) $discountCode->plan_id !== (int) $plan->id) {
            throw new \InvalidArgumentException('该优惠码仅限指定套餐使用');
        }

        // 6. 计算折后价
        $finalAmount = round($originalAmount * (float) $discountCode->discount, 2);

        // 7. 折后价不足 1 元不允许使用
        if ($originalAmount > 0 && $finalAmount < 1) {
            throw new \InvalidArgumentException('折后金额不足 1 元，无法使用优惠码');
        }

        return [
            'discount' => (float) $discountCode->discount,
            'final_amount' => $finalAmount,
            'code' => $discountCode,
        ];
    }

    /**
     * 下单时真正占用一次使用次数（需在事务 + lockForUpdate 中调用）。
     * 重新加锁读一次该码，防并发"双花"。
     */
    public function consume(DiscountCode $code, User $user): void
    {
        $locked = DiscountCode::whereKey($code->id)->lockForUpdate()->first();
        if (! $locked) {
            throw new \InvalidArgumentException(self::CODE_INVALID_MSG);
        }
        if ($locked->status !== DiscountCode::STATUS_ACTIVE) {
            throw new \InvalidArgumentException(self::CODE_INVALID_MSG);
        }
        if ($locked->used_count >= $locked->max_uses) {
            throw new \InvalidArgumentException(self::CODE_INVALID_MSG);
        }

        $locked->used_count = $locked->used_count + 1;
        if ($locked->used_count >= $locked->max_uses) {
            $locked->status = DiscountCode::STATUS_USED_UP;
        }
        $locked->save();

        // 邀请码：永久锁死该用户，之后填任何邀请码都无效
        if ($locked->source === DiscountCode::SOURCE_INVITE) {
            $user->forceFill([
                'used_invite_code_id' => $locked->id,
                'invite_code_used_at' => now(),
            ])->save();
        }
    }

    /**
     * 支付成功后发放兑换码给邀请人（幂等）。
     * 只有订单用了 invite 来源的码才发奖。
     */
    public function rewardInviter(Order $order): void
    {
        if (! $order->discount_code_id) {
            return;
        }

        $usedCode = DiscountCode::find($order->discount_code_id);
        if (! $usedCode || $usedCode->source !== DiscountCode::SOURCE_INVITE) {
            return;
        }

        $inviter = $usedCode->user;
        if (! $inviter) {
            return;
        }

        // 锁订单行，保证并发回调串行判断；已有该订单的奖励码则直接返回（幂等）
        DB::transaction(function () use ($order, $inviter) {
            Order::whereKey($order->id)->lockForUpdate()->first();

            $exists = DiscountCode::where('source', DiscountCode::SOURCE_REDEEM)
                ->where('source_order_id', $order->id)
                ->exists();
            if ($exists) {
                return;
            }

            DiscountCode::create([
                'code' => DiscountCode::generateCode('GIF'),
                'source' => DiscountCode::SOURCE_REDEEM,
                'user_id' => $inviter->id,
                'discount' => self::redeemDiscount(),
                'max_uses' => 1,
                'used_count' => 0,
                'max_uses_per_user' => 1,
                'expires_at' => null,
                'note' => null,
                'status' => DiscountCode::STATUS_ACTIVE,
                'source_order_id' => $order->id,
            ]);
        });
    }

    /**
     * 取用户当前有效邀请码；没有就自动生成一张（系统给每个用户自动生成）。
     */
    public function ensureInviteCode(User $user): DiscountCode
    {
        $existing = DiscountCode::where('source', DiscountCode::SOURCE_INVITE)
            ->where('user_id', $user->id)
            ->where('status', DiscountCode::STATUS_ACTIVE)
            ->orderByDesc('id')
            ->first();

        if ($existing) {
            return $existing;
        }

        return $this->issueInviteCode($user);
    }

    /**
     * 给用户新发一张邀请码（刷新时也走这里）。
     *
     * 发新码前先把该用户已有的 active 邀请码置为 refreshed，在事务内完成 ——
     * 「同一用户最多一张 active 邀请码」这条不变量（只认最新码）必须由这里保证，
     * 否则将来任何新入口直接调本方法，都会留下两张都能用的邀请码。
     * 刷新命令 RefreshInviteCodes 本来就已经先作废再发新码，这层对它只是幂等冗余（0 行）。
     */
    public function issueInviteCode(User $user): DiscountCode
    {
        return DB::transaction(function () use ($user) {
            DiscountCode::where('source', DiscountCode::SOURCE_INVITE)
                ->where('user_id', $user->id)
                ->where('status', DiscountCode::STATUS_ACTIVE)
                ->update([
                    'status' => DiscountCode::STATUS_REFRESHED,
                    'refreshed_at' => now(),
                ]);

            return DiscountCode::create([
                'code' => DiscountCode::generateCode('INV'),
                'source' => DiscountCode::SOURCE_INVITE,
                'user_id' => $user->id,
                'discount' => self::inviteDiscount(),
                'max_uses' => self::INVITE_MAX_USES,
                'used_count' => 0,
                'max_uses_per_user' => 1,
                'expires_at' => null,
                'note' => null,
                'status' => DiscountCode::STATUS_ACTIVE,
                'refreshed_at' => null,
                'next_refresh_at' => now()->addDays(self::inviteRefreshDays()),
            ]);
        });
    }
}
