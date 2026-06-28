<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\User;
use Illuminate\Support\Str;

/**
 * 订阅生成服务（M6）。
 * 流程：校验用户 → 取在线节点 → 各节点 getClientLinks(ch_user_{id}) → 合并 → Base64。
 * links 由面板驱动直接生成完整链接，ControlHub 只聚合 + Base64。
 */
class SubscriptionService
{
    public function __construct(private NodeDriverFactory $driverFactory)
    {
    }

    /**
     * 生成订阅 Base64 文本。校验失败抛 SubscriptionException。
     */
    public function generate(User $user): string
    {
        $this->ensureUsable($user);

        $nodes = Node::where('enabled', true)
            ->where('status', 'online')
            ->whereHas('inbounds', function ($q) use ($user) {
                $q->where('protocol', $user->protocol);
            })
            ->get();

        $links = [];
        $email = $user->clientEmail();

        foreach ($nodes as $node) {
            $inboundIds = $node->inbounds()
                ->where('protocol', $user->protocol)
                ->pluck('inbound_id')
                ->toArray();

            if (empty($inboundIds)) {
                continue;
            }

            try {
                $driver = $this->driverFactory->make($node);

                // 取各配置入站的端口，用于过滤
                $configuredPorts = [];
                foreach ($inboundIds as $inboundId) {
                    $inbound = $driver->getInbound($inboundId);
                    if (is_array($inbound) && isset($inbound['port'])) {
                        $configuredPorts[] = (int) $inbound['port'];
                    }
                }

                foreach ($driver->getClientLinks($email) as $link) {
                    if (!is_string($link) || $link === '') {
                        continue;
                    }
                    // 只保留配置了的入站端口
                    if (!empty($configuredPorts) && !$this->linkMatchesAnyPort($link, $configuredPorts)) {
                        continue;
                    }
                    if (!in_array($link, $links, true)) {
                        $links[] = $link;
                    }
                }
            } catch (\Throwable $e) {
                report($e);
            }
        }

        return base64_encode(implode("\n", $links));
    }

    private function linkMatchesPort(string $link, int $port): bool
    {
        if (preg_match('#@[^/:@\]]+:(\d+)#', $link, $m)) {
            return (int) $m[1] === $port;
        }

        return false;
    }

    private function linkMatchesAnyPort(string $link, array $ports): bool
    {
        if (preg_match('#@[^/:@\]]+:(\d+)#', $link, $m)) {
            return in_array((int) $m[1], $ports, true);
        }

        return false;
    }

    /**
     * 校验用户可用：enabled + 未过期 + 未超量。
     */
    public function ensureUsable(User $user): void
    {
        if (!$user->enabled) {
            throw new SubscriptionException('账号已禁用', 403);
        }

        if (!$user->plan_id) {
            throw new SubscriptionException('无套餐，暂无流量', 403);
        }

        if ($user->expired_at !== null && $user->expired_at->isPast()) {
            throw new SubscriptionException('账号已过期', 403);
        }

        if ($user->traffic_limit > 0 && $user->traffic_used >= $user->traffic_limit) {
            throw new SubscriptionException('流量已耗尽', 403);
        }
    }
}
