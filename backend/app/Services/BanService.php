<?php

namespace App\Services;

use App\Models\Node;
use App\Models\User;

/**
 * 封禁服务（M8 + 套餐适配）。
 *
 * 周期套餐：
 *   当月流量 monthly_traffic_used >= plan.monthly_traffic → 禁用（管理员可重置）
 *   总流量 traffic_used >= plan.period_traffic → 封禁（永久）
 *   到期 → 封禁
 *
 * 总量套餐：
 *   总流量 traffic_used >= plan.total_traffic → 封禁（永久，不可重置）
 *   永不过期
 *
 * 无套餐：
 *   直接封禁（没有流量 = 不可用）
 */
class BanService
{
    public function __construct(private ThreeXUiClientFactory $clientFactory)
    {
    }

    /**
     * 判断用户是否应被禁用/封禁。
     * 返回 'ban'（永久封禁）、'disable'（临时禁用，可重置）、false（正常）。
     */
    public function banReason(User $user): string|false
    {
        // 到期检查（周期套餐和无套餐）
        if ($user->expired_at !== null && $user->expired_at->isPast()) {
            return 'ban';
        }

        $plan = $user->plan;

        if ($plan && $plan->isPeriod()) {
            // 周期套餐：当月流量超限 → 临时禁用
            if ($plan->monthly_traffic > 0 && $user->monthly_traffic_used >= $plan->monthly_traffic) {
                return 'disable';
            }
            // 周期套餐：总流量超限 → 永久封禁
            if ($plan->period_traffic > 0 && $user->traffic_used >= $plan->period_traffic) {
                return 'ban';
            }
        } elseif ($plan && $plan->isTotal()) {
            // 总量套餐：总流量超限 → 永久封禁
            if ($plan->total_traffic > 0 && $user->traffic_used >= $plan->total_traffic) {
                return 'ban';
            }
        } else {
            // 无套餐：没有流量，直接封禁
            return 'ban';
        }

        return false;
    }

    /** @deprecated 使用 banReason() 替代 */
    public function isBannable(User $user): bool
    {
        return $this->banReason($user) !== false;
    }

    public function ban(User $user): void
    {
        $user->forceFill(['enabled' => false])->save();
        $this->toggleClient($user, false);
    }

    public function unban(User $user): void
    {
        $user->forceFill(['enabled' => true])->save();
        $this->toggleClient($user, true);
    }

    /**
     * 流量同步后内联检查（M8.4）：满足禁用条件则 ban/disable。
     */
    public function checkAfterSync(User $user): void
    {
        if (!$user) return;

        $reason = $this->banReason($user);
        if ($reason !== false) {
            $this->ban($user);
        }
    }

    public function toggleClient(User $user, bool $enable): void
    {
        $email = $user->clientEmail();

        foreach (Node::where('enabled', true)->get() as $node) {
            try {
                $client = $this->clientFactory->forNode($node);
                // 逐个入站更新 enable 状态
                $inboundIds = $node->inboundIdsFor($user->protocol);
                foreach ($inboundIds as $inboundId) {
                    try {
                        $resp = $client->getClient($email);
                        if ($resp === null) continue;
                        $clientData = $resp['client'] ?? $resp;
                        $clientData['enable'] = $enable;
                        if (isset($clientData['id'])) {
                            $clientData['id'] = (string) $clientData['id'];
                        }
                        $client->updateClient($email, $clientData, $inboundId);
                    } catch (\Throwable) {
                        // 入站不存在或 client 不存在，忽略
                    }
                }
                // 兜底不带 inboundId 再更新一次
                try {
                    $resp = $client->getClient($email);
                    if ($resp !== null) {
                        $clientData = $resp['client'] ?? $resp;
                        $clientData['enable'] = $enable;
                        if (isset($clientData['id'])) {
                            $clientData['id'] = (string) $clientData['id'];
                        }
                        $client->updateClient($email, $clientData);
                    }
                } catch (\Throwable) {}
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }
}
