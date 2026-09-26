<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\DiscountCode;
use App\Models\SiteConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 管理员优惠码 + 邀请机制配置接口。
 */
class DiscountCodeController extends Controller
{
    use ApiResponse;

    /** 邀请机制可配置字段（走 SiteConfig） */
    private const INVITE_KEYS = [
        'invite_discount',
        'invite_refresh_days',
        'invite_enabled',
        'redeem_discount',

        // 优惠码校验限流（见 App\Services\RateGuardService 的 rate_discount_*）：
        // 与其它 rate_* 同一个约定 —— 空值 = 用 config/panel.php ratelimit 段默认值，0 = 该项不限。
        // 这类参数归「优惠码」业务，故放在本页而不是邮箱配置页。
        'rate_discount_per_minute',
        'rate_discount_fail_per_day',
        'rate_discount_ip_fail_hourly',
        'rate_discount_max_attempts',
        'rate_discount_lock_seconds',
    ];

    /** 上面 4 个邀请字段之外的限流项，校验规则与读值方式单独走一套。 */
    private const RATE_KEYS = [
        'rate_discount_per_minute',
        'rate_discount_fail_per_day',
        'rate_discount_ip_fail_hourly',
        'rate_discount_max_attempts',
        'rate_discount_lock_seconds',
    ];

    /**
     * 优惠码列表（可按 source/status 筛选，分页）。
     */
    public function index(Request $request): \Illuminate\Http\JsonResponse
    {
        $request->validate([
            'source' => ['sometimes', 'nullable', 'in:invite,redeem,admin'],
            'status' => ['sometimes', 'nullable', 'in:active,used_up,expired,refreshed'],
            'page' => ['sometimes', 'integer', 'min:1'],
            'per_page' => ['sometimes', 'integer', 'min:1', 'max:100'],
        ]);

        $perPage = (int) $request->input('per_page', 20);

        $query = DiscountCode::query()->with(['user:id,email', 'plan:id,name']);

        if ($request->filled('source')) {
            $query->where('source', $request->input('source'));
        }
        if ($request->filled('status')) {
            $query->where('status', $request->input('status'));
        }

        $total = (clone $query)->count();
        $items = $query->orderByDesc('id')
            ->forPage((int) $request->input('page', 1), $perPage)
            ->get()
            ->map(fn (DiscountCode $c) => [
                'id' => $c->id,
                'code' => $c->code,
                'source' => $c->source,
                'user_id' => $c->user_id,
                'user_email' => $c->user->email ?? null,
                'discount' => (float) $c->discount,
                'max_uses' => $c->max_uses,
                'used_count' => $c->used_count,
                'expires_at' => $c->expires_at?->toDateTimeString(),
                'note' => $c->note,
                'status' => $c->status,
                'plan_id' => $c->plan_id,
                'plan_name' => $c->plan->name ?? null,
                'created_at' => $c->created_at?->toDateTimeString(),
            ]);

        return $this->success([
            'list' => $items,
            'total' => $total,
            'page' => (int) $request->input('page', 1),
            'per_page' => $perPage,
        ]);
    }

    /**
     * 生成优惠码（支持批量生成 N 个）。
     */
    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'discount' => ['required', 'numeric', 'gt:0', 'lte:1'],
            'max_uses' => ['nullable', 'integer', 'min:1'],
            'max_uses_per_user' => ['nullable', 'integer', 'min:1'],
            'expires_at' => ['nullable', 'date'],
            'note' => ['nullable', 'string', 'max:255'],
            'plan_id' => ['nullable', 'integer', 'exists:plans,id'],
            'count' => ['nullable', 'integer', 'min:1', 'max:100'],
        ]);

        $count = (int) ($data['count'] ?? 1);
        $codes = [];

        for ($i = 0; $i < $count; $i++) {
            $codes[] = DiscountCode::create([
                'code' => DiscountCode::generateCode('PRO'),
                'source' => DiscountCode::SOURCE_ADMIN,
                'user_id' => null,
                'discount' => $data['discount'],
                'max_uses' => $data['max_uses'] ?? 1,
                'used_count' => 0,
                'max_uses_per_user' => $data['max_uses_per_user'] ?? 1,
                'expires_at' => $data['expires_at'] ?? null,
                'note' => $data['note'] ?? null,
                'status' => DiscountCode::STATUS_ACTIVE,
                'plan_id' => $data['plan_id'] ?? null,
            ])->code;
        }

        return $this->success(['count' => count($codes), 'codes' => $codes], '生成成功');
    }

    /**
     * 修改：折扣 / 有效期 / 总次数 / 说明文字 / 状态。
     */
    public function update(Request $request, string $code): \Illuminate\Http\JsonResponse
    {
        $discountCode = $this->resolveCode($code);
        if (! $discountCode) {
            return $this->error('优惠码不存在', 404);
        }

        $data = $request->validate([
            'discount' => ['sometimes', 'numeric', 'gt:0', 'lte:1'],
            'max_uses' => ['sometimes', 'integer', 'min:1'],
            'max_uses_per_user' => ['sometimes', 'integer', 'min:1'],
            'expires_at' => ['sometimes', 'nullable', 'date'],
            'note' => ['sometimes', 'nullable', 'string', 'max:255'],
            'status' => ['sometimes', 'in:active,used_up,expired,refreshed'],
            'plan_id' => ['sometimes', 'nullable', 'integer', 'exists:plans,id'],
        ]);

        $discountCode->fill($data)->save();

        return $this->success(null, '保存成功');
    }

    /**
     * 删除。
     */
    public function destroy(string $code): \Illuminate\Http\JsonResponse
    {
        $discountCode = $this->resolveCode($code);
        if (! $discountCode) {
            return $this->error('优惠码不存在', 404);
        }

        $discountCode->delete();
        return $this->success(null, '已删除');
    }

    /**
     * 读邀请机制配置（含优惠码校验限流）。
     *
     * 限流 5 项刻意回「生效值」而不是原始 SiteConfig 值：没配过时回 config/panel.php 的默认值，
     * 页面输入框就直接显示当前真正在用的阈值，不会出现「框是空的、实际却在按 10 次/分拦」的错觉。
     */
    public function inviteSettings(): \Illuminate\Http\JsonResponse
    {
        $data = [
            'invite_discount' => SiteConfig::getValue('invite_discount', '0.90'),
            'invite_refresh_days' => SiteConfig::getValue('invite_refresh_days', '30'),
            'invite_enabled' => SiteConfig::getValue('invite_enabled', '1'),
            'redeem_discount' => SiteConfig::getValue('redeem_discount', '0.90'),
        ];

        foreach (self::RATE_KEYS as $key) {
            $stored = SiteConfig::getValue($key, '');
            $data[$key] = ($stored === '')
                ? (string) config('panel.ratelimit.' . $key, 0)
                : $stored;
        }

        return $this->success($data);
    }

    /**
     * 改邀请机制配置（折扣、刷新周期、开关、优惠码校验限流）。
     */
    public function updateInviteSettings(Request $request): \Illuminate\Http\JsonResponse
    {
        $rules = [
            'invite_discount' => ['sometimes', 'numeric', 'gt:0', 'lte:1'],
            'invite_refresh_days' => ['sometimes', 'integer', 'min:1'],
            'invite_enabled' => ['sometimes', 'boolean'],
            'redeem_discount' => ['sometimes', 'numeric', 'gt:0', 'lte:1'],
        ];

        // 限流项：非负整数，0 = 不限，空值 = 回落到 config/panel.php 默认值
        foreach (self::RATE_KEYS as $key) {
            $rules[$key] = ['sometimes', 'nullable', 'integer', 'min:0'];
        }

        $data = $request->validate($rules);

        foreach (self::INVITE_KEYS as $key) {
            if (! array_key_exists($key, $data)) {
                continue;
            }
            // 开关统一存 '1' / '0'，其它值按字符串存
            // （限流项传 null 时 (string) 后是空串，正好等于「没配过、用 panel 默认值」）
            $value = $key === 'invite_enabled'
                ? ($request->boolean('invite_enabled') ? '1' : '0')
                : (string) $data[$key];

            SiteConfig::setValue($key, $value);
        }

        return $this->success(null, '保存成功');
    }

    /**
     * 路由参数 {code} 既支持主键 id 也支持码本身。
     */
    private function resolveCode(string $code): ?DiscountCode
    {
        return DiscountCode::where('id', $code)
            ->orWhere('code', strtoupper(trim($code)))
            ->first();
    }
}
