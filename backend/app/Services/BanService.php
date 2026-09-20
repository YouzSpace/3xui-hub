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

    /**
     * 全量替换 3x-ui client 前，规范化 API 不兼容的字段类型。
     * 3x-ui 的 /clients/update 是全量替换语义，回传整个 client 对象时，
     * 以下字段若类型不符会触发 Go 反序列化失败——而 toggleClient 里被
     * catch(\Throwable) 静默吞掉，导致 enable 永远改不动（"没关"的根因）：
     * - id：3x-ui 要求 string
     * - allowedIPs / tunnelAllowedIPs：Go 用 []string 反序列化，空字符串会报
     *   "cannot unmarshal string into Go struct field .allowedIPs of type []string"
     */
    private function normalizeForUpdate(array $clientData): array
    {
        // 3x-ui v3.7.0 返回的 id 是数据库自增行号，uuid 才是 Xray 用的 client id；
        // 全量替换时把行号写回 id 会把 client 的 uuid 覆盖成数字，导致用户断连。
        // 优先用 uuid，旧版 3x-ui（id 即 uuid、无 uuid 键）回退仅做 string 强转。
        if (!empty($clientData['uuid'])) {
            $clientData['id'] = $clientData['uuid'];
        } elseif (isset($clientData['id'])) {
            $clientData['id'] = (string) $clientData['id'];
        }
        foreach (['allowedIPs', 'tunnelAllowedIPs'] as $field) {
            if (isset($clientData[$field]) && $clientData[$field] === '') {
                $clientData[$field] = [];
            }
        }
        return $clientData;
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
                        $clientData = $this->normalizeForUpdate($resp['client'] ?? $resp);
                        $clientData['enable'] = $enable;
                        $driver->updateClient($email, $clientData, $inboundId);
                    } catch (\Throwable) {
                        // 入站不存在或 client 不存在，忽略
                    }
                }
                // 兜底不带 inboundId 再更新一次
                try {
                    $resp = $driver->getClient($email);
                    if ($resp !== null) {
                        $clientData = $this->normalizeForUpdate($resp['client'] ?? $resp);
                        $clientData['enable'] = $enable;
                        $driver->updateClient($email, $clientData);
                    }
                } catch (\Throwable) {}
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }
}
