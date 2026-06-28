<?php

namespace App\Drivers\Contracts;

/**
 * 订阅生成驱动 —— 把用户/节点数据转成客户端订阅格式。
 *
 * 适用范围：Clash、V2Ray、Sing-box 等订阅格式。
 * 输入：用户数据 + 节点列表
 * 输出：Base64 编码的订阅文本
 */
interface SubscriptionDriverInterface extends DriverInterface
{
    /** 生成订阅内容 */
    public function generate(array $links): string;

    /** 输出格式名称 */
    public function format(): string;
}
