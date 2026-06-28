<?php

namespace App\Drivers;

use App\Drivers\Contracts\PanelDriverInterface;
use App\Drivers\ThreeXUi\ThreeXUiDriver;
use App\Models\Node;
use App\Services\ThreeXUi\ThreeXUiClient;
use RuntimeException;

/**
 * 节点驱动工厂 —— 根据 Node 的 driver_type 创建对应的 PanelDriver。
 *
 * 每次调用 make() 都返回新实例（因为不同节点配置不同）。
 */
class NodeDriverFactory
{
    public function __construct(
        private readonly DriverRegistry $registry,
    ) {}

    /** 根据节点创建面板驱动 */
    public function make(Node $node): PanelDriverInterface
    {
        $type = $node->driver_type ?? '3x-ui';

        return match ($type) {
            '3x-ui' => new ThreeXUiDriver(ThreeXUiClient::fromNode($node)),
            default  => throw new RuntimeException("Unsupported node driver: {$type}"),
        };
    }

    /** 根据节点获取已注册的通用驱动 */
    public function getRegistered(string $driverType): PanelDriverInterface
    {
        return $this->registry->panel($driverType);
    }
}
