<?php

namespace App\Services;

use App\Models\Node;

/**
 * 节点健康检查服务（M9）。
 * healthCheck → 更新 status/latency/last_check_at。
 * maintenance 态由管理员手动设置，本服务不改写。
 */
class HealthCheckService
{
    public function __construct(private ThreeXUiClientFactory $clientFactory)
    {
    }

    public function check(Node $node): array
    {
        if ($node->status === 'maintenance') {
            return ['ok' => false, 'status' => 'maintenance', 'latency' => $node->latency, 'skipped' => true];
        }

        try {
            $client = $this->clientFactory->forNode($node);
            $health = $client->healthCheck();
        } catch (\Throwable $e) {
            $this->apply($node, 'offline', 0);
            return ['ok' => false, 'status' => 'offline', 'latency' => 0, 'error' => $e->getMessage()];
        }

        $ok = (bool) $health['ok'];
        $this->apply($node, $ok ? 'online' : 'offline', (int) ($health['latencyMs'] ?? 0));

        return [
            'ok' => $ok,
            'status' => $ok ? 'online' : 'offline',
            'latency' => (int) ($health['latencyMs'] ?? 0),
        ];
    }

    private function apply(Node $node, string $status, int $latency): void
    {
        $node->forceFill([
            'status' => $status,
            'latency' => $latency,
            'last_check_at' => now(),
        ])->save();
    }
}
