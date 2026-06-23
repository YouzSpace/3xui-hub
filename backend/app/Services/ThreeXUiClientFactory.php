<?php

namespace App\Services;

use App\Models\Node;
use App\Services\ThreeXUi\ThreeXUiClient;

/**
 * ThreeXUiClient 工厂：由 Node 构造 client（M5 fromNode 鸭子类型）。
 * 集中此逻辑便于在 Service 层 mock（测试时绑定假工厂）。
 */
class ThreeXUiClientFactory
{
    public function forNode(Node $node): ThreeXUiClient
    {
        return ThreeXUiClient::fromNode($node);
    }
}
