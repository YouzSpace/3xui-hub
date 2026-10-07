<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Plan;
use App\Models\SiteConfig;
use App\Models\ThirdPartySub;
use App\Services\ThirdPartyService;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * Admin 第三方订阅管理。
 *
 * 第三方「不管流量、只管时间」：
 *  - 套餐勾选的订阅：账号有效期内自动注入用户订阅，到期随账号失效
 * 开与不开由「套餐配置 + 总开关」控制，用户层面不做单独控制。
 * 节点在用户端统一命名「地区+序号」，机场原名/品牌词不外露（保密）。
 */
class ThirdPartyController extends Controller
{
    use ApiResponse;

    public function __construct(
        private ThirdPartyService $service,
    ) {}

    /** 列表 + 总开关状态。链接打码展示（含机场 token，敏感）。 */
    public function index(): JsonResponse
    {
        $subs = ThirdPartySub::orderByDesc('id')->get();

        return $this->success([
            'items' => $subs->map(fn (ThirdPartySub $s) => $this->present($s))->values(),
            'enabled' => SiteConfig::getValue('third_party_enabled', '0') === '1',
        ]);
    }

    public function store(Request $request): JsonResponse
    {
        $data = $request->validate([
            'name' => ['required', 'string', 'max:64'],
            'url' => ['required', 'string', 'max:2048', 'url'],
            'enabled' => ['sometimes', 'boolean'],
        ]);

        $sub = ThirdPartySub::create([
            'name' => $data['name'],
            'url' => $data['url'],
            'enabled' => $data['enabled'] ?? true,
            'last_status' => 'pending',
        ]);

        // 新建后立刻拉一次：失败保留 pending 状态 + 记原因，不阻塞创建
        $this->service->fetchSub($sub);

        return $this->success($this->present($sub->fresh()), '已创建');
    }

    public function update(Request $request, ThirdPartySub $sub): JsonResponse
    {
        $data = $request->validate([
            'name' => ['sometimes', 'string', 'max:64'],
            'url' => ['sometimes', 'string', 'max:2048', 'url'],
            'enabled' => ['sometimes', 'boolean'],
        ]);

        $sub->forceFill($data)->save();

        // URL 变了立刻重拉，避免缓存还是旧源的数据
        if (array_key_exists('url', $data)) {
            $this->service->fetchSub($sub->fresh());
        }

        return $this->success($this->present($sub->fresh()), '已更新');
    }

    public function destroy(ThirdPartySub $sub): JsonResponse
    {
        $id = $sub->id;
        $sub->delete();

        // 清理套餐里的引用（JSON 数组），避免套餐配了个已删除的订阅
        foreach (Plan::all() as $plan) {
            $ids = array_values(array_filter(array_map('intval', (array) $plan->third_party_sub_ids)));
            if (in_array($id, $ids, true)) {
                $ids = array_values(array_filter($ids, fn ($x) => $x !== $id));
                $plan->forceFill(['third_party_sub_ids' => $ids])->save();
            }
        }

        return $this->success(null, '已删除');
    }

    /** 立即拉取（同步等机场响应，前端放宽超时）。 */
    public function refresh(ThirdPartySub $sub): JsonResponse
    {
        $this->service->fetchSub($sub);
        $sub->refresh();

        return $this->success($this->present($sub), $sub->last_status === 'success' ? '拉取成功' : '拉取失败（已保留上次成功缓存）');
    }

    /** 总开关：一键从所有订阅里摘掉第三方。 */
    public function switch(Request $request): JsonResponse
    {
        $data = $request->validate([
            'enabled' => ['required', 'boolean'],
        ]);

        SiteConfig::setValue('third_party_enabled', $data['enabled'] ? '1' : '0');

        return $this->success(['enabled' => $data['enabled']], $data['enabled'] ? '已开启' : '已关闭');
    }

    private function present(ThirdPartySub $s): array
    {
        $url = $s->url;
        $masked = mb_strlen($url) > 20
            ? mb_substr($url, 0, 12) . '…' . mb_substr($url, -6)
            : $url;

        return [
            'id' => $s->id,
            'name' => $s->name,
            'url' => $url, // 仅管理端接口返回完整链接
            'url_masked' => $masked,
            'enabled' => (bool) $s->enabled,
            'node_count' => (int) $s->node_count,
            'last_fetched_at' => $s->last_fetched_at?->toIso8601String(),
            'last_status' => $s->last_status,
            'last_error' => $s->last_error,
        ];
    }
}
