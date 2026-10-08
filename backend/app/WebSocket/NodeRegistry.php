<?php

namespace App\WebSocket;

use Workerman\Connection\TcpConnection;

/**
 * xray 节点 WS 连接注册表。
 *
 * 单进程内存表（Worker count=1）：nodeId → TcpConnection。
 * 将来若需多进程，这里换成静态内存 + Redis 路由即可，接口不变。
 *
 * 连接上挂的运行期数据（放 context，避免 PHP 8.2+ 动态属性弃用告警）：
 *   context->nodeId          鉴权通过后的节点 ID（0 = 未鉴权）
 *   context->lastSeen        最近收到任何消息的时间戳（僵死连接判定）
 *   context->notifiedVersion 已通知过的 config 版本（config.changed 去重）
 */
class NodeRegistry
{
    /** @var array<int, TcpConnection> */
    private static array $connections = [];

    /** 注册连接（同一节点重复连接：踢掉旧连接，新连接优先 —— agent 重启场景）。 */
    public static function add(int $nodeId, TcpConnection $conn): void
    {
        if (isset(self::$connections[$nodeId])) {
            $old = self::$connections[$nodeId];
            if ($old !== $conn) {
                try {
                    $old->close();
                } catch (\Throwable) {
                }
            }
        }
        self::$connections[$nodeId] = $conn;
    }

    public static function remove(int $nodeId, TcpConnection $conn): void
    {
        if (isset(self::$connections[$nodeId]) && self::$connections[$nodeId] === $conn) {
            unset(self::$connections[$nodeId]);
        }
    }

    public static function get(int $nodeId): ?TcpConnection
    {
        return self::$connections[$nodeId] ?? null;
    }

    public static function count(): int
    {
        return count(self::$connections);
    }

    /** @return array<int, TcpConnection> */
    public static function all(): array
    {
        return self::$connections;
    }

    /** @return int[] */
    public static function connectedNodeIds(): array
    {
        return array_keys(self::$connections);
    }
}
