<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\Plan;
use App\Models\User;
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
    public function __construct(private NodeDriverFactory $driverFactory)
    {
    }

    /**
     * 切换套餐时重新计算用户的流量限额和到期时间。
     * 无套餐 → 清零限额、永不过期。
     * 切换套餐 → 重置已用流量，直接使用新套餐额度。
     */
    public function applyPlan(User $user): void
    {
        $plan = $user->plan;

        if (!$plan) {
            $user->forceFill([
                'traffic_limit' => 0,
                'traffic_used' => 0,
                'monthly_traffic_used' => 0,
                'monthly_traffic_limit' => 0,
                'expired_at' => null,
            ])->save();
            return;
        }

        if ($plan->isPeriod()) {
            $user->forceFill([
                'traffic_limit' => $plan->period_traffic ?? 0,
                'traffic_used' => 0,
                'monthly_traffic_used' => 0,
                'monthly_traffic_limit' => $plan->monthly_traffic ?? 0,
                'expired_at' => Carbon::now()->addMonths($plan->months)->toDateString(),
            ])->save();
        } else {
            $user->forceFill([
                'traffic_limit' => $plan->total_traffic ?? 0,
                'traffic_used' => 0,
                'monthly_traffic_used' => 0,
                'monthly_traffic_limit' => 0,
                'expired_at' => null,
            ])->save();
        }

        // 重置 3x-ui 流量统计
        $this->resetClientOnNodes($user);
    }

    /**
     * 创建用户（token/uuid 自动生成）+ 在各 enabled 节点上 provision 3x-ui client。
     * 支持套餐绑定：传 plan_id 时自动填充 traffic_limit/expired_at。
     */
    public function create(array $data): User
    {
        // 套餐联动：自动填充流量和到期时间
        $trafficLimit = (int) ($data['traffic_limit'] ?? 0);
        $monthlyTrafficLimit = (int) ($data['monthly_traffic_limit'] ?? 0);
        $expiredAt = $data['expired_at'] ?? null;

        if (!empty($data['plan_id'])) {
            $plan = Plan::find($data['plan_id']);
            if ($plan) {
                if ($plan->isPeriod()) {
                    $trafficLimit = $plan->period_traffic ?? 0;
                    $monthlyTrafficLimit = $plan->monthly_traffic ?? 0;
                    $expiredAt = Carbon::now()->addMonths($plan->months)->toDateString();
                } else {
                    $trafficLimit = $plan->total_traffic ?? 0;
                    $monthlyTrafficLimit = 0;
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
            'monthly_traffic_limit' => $monthlyTrafficLimit,
            'expired_at' => $expiredAt,
            'enabled' => (bool) ($data['enabled'] ?? true),
        ]);

        $this->provisionClient($user);

        return $user;
    }

    /**
     * 在各 enabled 节点上同步 3x-ui client（ch_user_{id}）。
     * client 存在则更新配置，不存在则创建。
     */
    public function provisionClient(User $user): void
    {
        $email = $user->clientEmail();
        $clientData = [
            'email' => $email,
            'enable' => $user->plan_id ? (bool) $user->enabled : false,  // 无套餐禁用
            'totalGB' => $user->traffic_limit > 0 ? (int) $user->traffic_limit : 0,
            'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
            'limitIp' => 0,
        ];

        foreach (Node::where('enabled', true)->get() as $node) {
            $inboundIds = $node->inboundIdsFor($user->protocol);
            if (empty($inboundIds)) {
                continue;
            }

            $client = $this->driverFactory->make($node);
            try {
                // 先检查 client 是否存在
                $existing = $client->getClient($email);
                if ($existing) {
                    // 已存在：更新配置
                    $clientData['id'] = $existing['id'] ?? null;
                    foreach ($inboundIds as $inboundId) {
                        try {
                            $client->updateClient($email, $clientData, $inboundId);
                        } catch (\Throwable $e) {
                            report($e);
                        }
                    }
                } else {
                    // 不存在：创建新 client
                    $created = $client->addClient($clientData, $inboundIds);
                    if (!empty($created['uuid']) && !$user->uuid) {
                        $user->forceFill(['uuid' => $created['uuid']])->save();
                    }
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
            $inboundIds = $node->inboundIdsFor($user->protocol);
            if (empty($inboundIds)) {
                continue;
            }

            try {
                $this->driverFactory->make($node)->createClient([
                    'email' => $user->clientEmail(),
                    'enable' => (bool) $user->enabled,
                    'totalGB' => $user->traffic_limit > 0 ? (int) $user->traffic_limit : 0,
                    'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
                    'limitIp' => 0,
                ], $inboundIds);
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
     * 重置用户当月流量：清零 monthly_traffic_used + 总流量加月流量额度 + 同步 3x-ui + 打开连接。
     */
    public function resetTraffic(User $user): void
    {
        if ($user->isPeriodPlan()) {
            $addTraffic = $user->monthly_traffic_limit ?? 0;
            $user->forceFill([
                'monthly_traffic_used' => 0,
                'traffic_limit' => ($user->traffic_limit ?? 0) + $addTraffic,
            ])->save();
        } else {
            $user->forceFill(['traffic_used' => 0])->save();
        }

        // 同步新的流量限制到 3x-ui + 重置流量计数 + 打开连接
        $user->refresh();
        $this->provisionClient($user);
        $this->resetClientOnNodes($user);
        app(BanService::class)->toggleClient($user, true);
    }

    public function resetClientOnNodes(User $user): void
    {
        $email = $user->clientEmail();
        foreach (Node::where('enabled', true)->get() as $node) {
            try {
                $this->driverFactory->make($node)->resetClientTraffic($email);
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }
}
