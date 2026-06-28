<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\User;

/**
 * 封禁服务（M8 + 套餐适配）。
 *
 * 到期/超限/无套餐：不禁用用户，只关闭 3x-ui 流量。
 * 仅管理员手动封禁才设置 enabled=false。
 */
class BanService
{
    public function __construct(private NodeDriverFactory $driverFactory)
    {
    }

    /**
     * 判断用户是否应被关闭流量。
     * 返回 'disable'（需要关闭 3x-ui 流量）、false（正常）。
     * 永不返回 'ban'，只有管理员才能封禁用户。
     */
    public function banReason(User $user): string|false
    {
        // 到期检查 → 关闭流量
        if ($user->expired_at !== null && $user->expired_at->isPast()) {
            return 'disable';
        }

        // 周期套餐：当月流量超限 → 关闭流量
        if ($user->plan_id && $user->monthly_traffic_limit > 0 && $user->monthly_traffic_used >= $user->monthly_traffic_limit) {
            return 'disable';
        }

        // 周期/总量套餐：总流量超限 → 关闭流量
        if ($user->plan_id && $user->traffic_limit > 0 && $user->traffic_used >= $user->traffic_limit) {
            return 'disable';
        }

        return false;
    }

    /** @deprecated 使用 banReason() 替代 */
    public function isBannable(User $user): bool
    {
        return $this->banReason($user) !== false;
    }

    /**
     * 封禁用户（仅管理员手动操作）。
     * 设置 enabled=false + 关闭 3x-ui 流量。
     */
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
     * 流量同步后内联检查（M8.4）：满足条件则关闭 3x-ui 流量。
     * 不禁用用户，用户仍能登录续费。
     */
    public function checkAfterSync(User $user): void
    {
        if (!$user) return;

        $reason = $this->banReason($user);
        if ($reason !== false) {
            $this->toggleClient($user, false);
        }
    }

    public function toggleClient(User $user, bool $enable): void
    {
        $email = $user->clientEmail();

        foreach (Node::where('enabled', true)->get() as $node) {
            try {
                $driver = $this->driverFactory->make($node);
                // 逐个入站更新 enable 状态
                $inboundIds = $node->inboundIdsFor($user->protocol);
                foreach ($inboundIds as $inboundId) {
                    try {
                        $resp = $driver->getClient($email);
                        if ($resp === null) continue;
                        $clientData = $resp['client'] ?? $resp;
                        $clientData['enable'] = $enable;
                        if (isset($clientData['id'])) {
                            $clientData['id'] = (string) $clientData['id'];
                        }
                        $driver->updateClient($email, $clientData, $inboundId);
                    } catch (\Throwable) {
                        // 入站不存在或 client 不存在，忽略
                    }
                }
                // 兜底不带 inboundId 再更新一次
                try {
                    $resp = $driver->getClient($email);
                    if ($resp !== null) {
                        $clientData = $resp['client'] ?? $resp;
                        $clientData['enable'] = $enable;
                        if (isset($clientData['id'])) {
                            $clientData['id'] = (string) $clientData['id'];
                        }
                        $driver->updateClient($email, $clientData);
                    }
                } catch (\Throwable) {}
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }
}
