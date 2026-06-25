<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\Plan;
use App\Models\User;
use App\Services\BanService;
use App\Services\ProtocolSwitchService;
use App\Services\ThreeXUi\ThreeXUiClient;
use App\Services\UserAdminService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * Admin 用户管理（M3.2~M3.5 + M10.4 + 套餐联动）。
 * GET/POST/PUT/DELETE /admin/users
 * POST /admin/users/{id}/reset-traffic
 * POST /admin/users/{id}/protocol  → 切换协议
 * POST /admin/users/{id}/renew     → 总量套餐续费
 */
class UserController extends Controller
{
    use ApiResponse;

    public function __construct(
        private UserAdminService $service,
        private ProtocolSwitchService $protocolService,
        private BanService $banService,
    ) {
    }

    public function index(): \Illuminate\Http\JsonResponse
    {
        $users = User::with('plan')->orderByDesc('id')->get();

        return $this->success($users->map(fn (User $u) => $this->present($u))->values());
    }

    public function show(User $user): \Illuminate\Http\JsonResponse
    {
        $user->load('plan');

        return $this->success($this->present($user, true));
    }

    public function store(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateUser($request);

        $user = $this->service->create($data);

        return $this->success($this->present($user->load('plan'), true), '创建成功');
    }

    public function update(Request $request, User $user): \Illuminate\Http\JsonResponse
    {
        $data = $this->validateUser($request, $user);

        $oldPlanId = $user->plan_id;
        $newPlanId = array_key_exists('plan_id', $data) ? $data['plan_id'] : $user->plan_id;

        $user->forceFill([
            'email' => $data['email'] ?? $user->email,
            'protocol' => $data['protocol'] ?? $user->protocol,
            'plan_id' => $newPlanId,
            'traffic_limit' => isset($data['traffic_limit']) ? (int) $data['traffic_limit'] : $user->traffic_limit,
            'traffic_used' => isset($data['traffic_used']) ? (int) $data['traffic_used'] : $user->traffic_used,
            'monthly_traffic_used' => isset($data['monthly_traffic_used']) ? (int) $data['monthly_traffic_used'] : $user->monthly_traffic_used,
            'expired_at' => array_key_exists('expired_at', $data) ? $data['expired_at'] : $user->expired_at,
            'enabled' => isset($data['enabled']) ? (bool) $data['enabled'] : $user->enabled,
        ])->save();

        // 切换套餐时：重新计算流量/到期 + 同步 3x-ui client
        if ($newPlanId !== $oldPlanId) {
            $user->load('plan');
            $this->service->applyPlan($user);
            $this->service->provisionClient($user);

            $reason = $this->banService->banReason($user);
            if ($reason !== false) {
                $this->banService->ban($user);
            } elseif (!$user->enabled) {
                $this->banService->unban($user);
            }
        }

        // 手动改 enabled 时：同步到 3x-ui 节点
        if (isset($data['enabled']) && $newPlanId === $oldPlanId) {
            $this->banService->toggleClient($user, (bool) $data['enabled']);
        }

        return $this->success($this->present($user->load('plan'), true), '更新成功');
    }

    public function destroy(User $user): \Illuminate\Http\JsonResponse
    {
        // 同步删除 3x-ui 各节点各入站上的 client
        $email = $user->clientEmail();
        Node::each(function (Node $node) use ($email, $user) {
            try {
                $client = ThreeXUiClient::fromNode($node);
                $inboundIds = $node->inboundIdsFor($user->protocol);
                foreach ($inboundIds as $inboundId) {
                    try {
                        $client->deleteClient($email, false, $inboundId);
                    } catch (\Throwable) {
                        // 已删除或不存在，忽略
                    }
                }
                // 兜底：不带 inboundId 再删一次
                try {
                    $client->deleteClient($email);
                } catch (\Throwable) {}
            } catch (\Throwable) {
                // 节点离线，忽略
            }
        });

        $user->delete();

        return $this->success(null, '已删除');
    }

    public function resetTraffic(User $user): \Illuminate\Http\JsonResponse
    {
        $this->service->resetTraffic($user);

        return $this->success($this->present($user->fresh()->load('plan'), true), '流量已重置');
    }

    /** 总量套餐续费：重置总流量 + 重新启用。 */
    public function renew(User $user): \Illuminate\Http\JsonResponse
    {
        if (!$user->isTotalPlan()) {
            return $this->error('仅总量套餐支持续费', 400);
        }

        $this->service->renew($user);

        return $this->success($this->present($user->fresh()->load('plan'), true), '续费成功');
    }

    /** M10.4 Admin 切换用户协议。 */
    public function switchProtocol(Request $request, User $user): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'protocol' => ['required', 'in:vless,trojan'],
        ]);

        try {
            $this->protocolService->switch($user, $data['protocol']);
        } catch (\DomainException $e) {
            return $this->error($e->getMessage(), 400);
        } catch (\Throwable $e) {
            report($e);
            return $this->error('协议切换失败：' . $e->getMessage(), 500);
        }

        return $this->success($this->present($user->fresh()->load('plan'), true), '切换成功');
    }

    private function validateUser(Request $request, ?User $user = null): array
    {
        $rules = [
            'email' => ['nullable', 'email'],
            'protocol' => ['sometimes', 'in:vless,trojan'],
            'plan_id' => ['nullable', 'integer', 'exists:plans,id'],
            'traffic_limit' => ['sometimes', 'integer', 'min:0'],
            'traffic_used' => ['sometimes', 'integer', 'min:0'],
            'monthly_traffic_used' => ['sometimes', 'integer', 'min:0'],
            'expired_at' => ['sometimes', 'nullable', 'date'],
            'enabled' => ['sometimes', 'boolean'],
        ];

        return $request->validate($rules);
    }

    private function present(User $u, bool $full = false): array
    {
        return [
            'id' => $u->id,
            'email' => $u->email,
            'token' => $full ? $u->token : null,
            'uuid' => $full ? $u->uuid : null,
            'protocol' => $u->protocol,
            'plan_id' => $u->plan_id,
            'plan_type' => $u->plan?->type,
            'plan_name' => $u->plan?->name,
            'traffic_limit' => (int) $u->traffic_limit,
            'traffic_used' => (int) $u->traffic_used,
            'monthly_traffic_used' => (int) $u->monthly_traffic_used,
            'monthly_traffic_limit' => $u->plan?->monthly_traffic ?? 0,
            'expired_at' => $u->expired_at?->toIso8601String(),
            'enabled' => (bool) $u->enabled,
            'created_at' => $u->created_at?->toIso8601String(),
        ];
    }
}
