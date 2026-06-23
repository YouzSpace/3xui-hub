<?php

namespace App\Services;

use App\Models\Node;
use App\Models\Plan;
use App\Models\User;
use App\Services\ThreeXUi\ThreeXUiClient;
use Illuminate\Support\Carbon;
use Illuminate\Support\Str;

/**
 * 用户管理服务（M3 + M13 套餐联动）。
 * - 创建用户：自动生成 token(sub_+32)、uuid(v4)，在各节点 provision
 * - provisionAllUsersToNode：新节点创建后，把已有用户同步过去
 * - resetTraffic / renew：流量重置和续费
 */
class UserAdminService
{
    public function __construct(private ThreeXUiClientFactory $clientFactory)
    {
    }

    /**
     * 切换套餐时重新计算用户的流量限额和到期时间。
     * 无套餐 → 清零限额、永不过期。
     */
    public function applyPlan(User $user): void
    {
        $plan = $user->plan;

        if (!$plan) {
            $user->forceFill([
                'traffic_limit' => 0,
                'traffic_used' => 0,
                'monthly_traffic_used' => 0,
                'expired_at' => null,
            ])->save();
            return;
        }

        if ($plan->isPeriod()) {
            $user->forceFill([
                'traffic_limit' => $plan->period_traffic ?? 0,
                'monthly_traffic_used' => 0,
                'expired_at' => Carbon::now()->addMonths($plan->months)->toDateString(),
            ])->save();
        } else {
            $user->forceFill([
                'traffic_limit' => $plan->total_traffic ?? 0,
                'monthly_traffic_used' => 0,
                'expired_at' => null,
            ])->save();
        }
    }

    /**
     * 创建用户（token/uuid 自动生成）+ 在各 enabled 节点上 provision 3x-ui client。
     * 支持套餐绑定：传 plan_id 时自动填充 traffic_limit/expired_at。
     */
    public function create(array $data): User
    {
        // 套餐联动：自动填充流量和到期时间
        $trafficLimit = (int) ($data['traffic_limit'] ?? 0);
        $expiredAt = $data['expired_at'] ?? null;

        if (!empty($data['plan_id'])) {
            $plan = Plan::find($data['plan_id']);
            if ($plan) {
                if ($plan->isPeriod()) {
                    $trafficLimit = $plan->period_traffic ?? 0;
                    $expiredAt = Carbon::now()->addMonths($plan->months)->toDateString();
                } else {
                    $trafficLimit = $plan->total_traffic ?? 0;
                    $expiredAt = null;
                }
            }
        }

        $user = User::create([
            'email' => $data['email'] ?? null,
            'token' => $data['token'] ?? ('sub_' . Str::random(32)),
            'uuid' => $data['uuid'] ?? Str::uuid()->toString(),
            'protocol' => $data['protocol'] ?? 'vless',
            'plan_id' => $data['plan_id'] ?? null,
            'traffic_limit' => $trafficLimit,
            'traffic_used' => 0,
            'monthly_traffic_used' => 0,
            'expired_at' => $expiredAt,
            'enabled' => (bool) ($data['enabled'] ?? true),
        ]);

        $this->provisionClient($user);

        return $user;
    }

    /**
     * 在各 enabled 节点上创建 3x-ui client（ch_user_{id}），并回读 uuid 存库。
     */
    public function provisionClient(User $user): void
    {
        $email = $user->clientEmail();
        $client = [
            'email' => $email,
            'enable' => (bool) $user->enabled,
            'totalGB' => $user->traffic_limit > 0 ? (int) $user->traffic_limit : 0,
            'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
            'limitIp' => 0,
        ];

        foreach (Node::where('enabled', true)->get() as $node) {
            $inboundId = $node->inboundIdFor($user->protocol);
            if ($inboundId === null) {
                continue;
            }

            try {
                $created = $this->clientFactory->forNode($node)->addClient($client, [$inboundId]);
                if (!empty($created['uuid']) && !$user->uuid) {
                    $user->forceFill(['uuid' => $created['uuid']])->save();
                }
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }

    /**
     * 新节点创建后，把所有已启用用户同步到该节点上。
     * 单用户失败不阻断。
     */
    public function provisionAllUsersToNode(Node $node): void
    {
        $users = User::whereNotNull('plan_id')->get();

        foreach ($users as $user) {
            $inboundId = $node->inboundIdFor($user->protocol);
            if ($inboundId === null) {
                continue;
            }

            try {
                $this->clientFactory->forNode($node)->addClient([
                    'email' => $user->clientEmail(),
                    'enable' => (bool) $user->enabled,
                    'totalGB' => $user->traffic_limit > 0 ? (int) $user->traffic_limit : 0,
                    'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
                    'limitIp' => 0,
                ], [$inboundId]);
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }

    /**
     * 总量套餐续费：重置总流量 + 重新启用 + 3x-ui 重置。
     */
    public function renew(User $user): void
    {
        $user->forceFill([
            'traffic_used' => 0,
            'monthly_traffic_used' => 0,
            'enabled' => true,
        ])->save();

        $this->resetClientOnNodes($user);
    }

    /**
     * 重置用户当月流量：清零 monthly_traffic_used + 3x-ui 各 enabled 节点 resetClientTraffic。
     */
    public function resetTraffic(User $user): void
    {
        if ($user->isPeriodPlan()) {
            $user->forceFill(['monthly_traffic_used' => 0])->save();
        } else {
            $user->forceFill(['traffic_used' => 0])->save();
        }

        $this->resetClientOnNodes($user);
    }

    private function resetClientOnNodes(User $user): void
    {
        $email = $user->clientEmail();
        foreach (Node::where('enabled', true)->get() as $node) {
            try {
                $this->clientFactory->forNode($node)->resetClientTraffic($email);
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }
}
