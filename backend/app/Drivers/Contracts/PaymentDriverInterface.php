<?php

namespace App\Drivers\Contracts;

use App\Models\Order;
use App\Models\PaymentConfig;

/**
 * 支付驱动 —— 对接支付网关。
 *
 * 适用范围：wwspay、Stripe、支付宝、微信支付等。
 * 新增支付方式只需实现此接口并在 Registry 注册。
 */
interface PaymentDriverInterface extends DriverInterface
{
    /** 下单，返回支付链接或 client_secret */
    public function pay(Order $order, PaymentConfig $config): PaymentResult;

    /** 验签 */
    public function verifySignature(array $data, PaymentConfig $config): bool;

    /** 处理回调，返回标准化结果 */
    public function handleCallback(array $data, PaymentConfig $config): CallbackResult;

    /** 查询订单状态 */
    public function query(Order $order, PaymentConfig $config): ?array;

    /** 退款（如支持） */
    public function refund(Order $order, PaymentConfig $config, ?float $amount = null): bool;
}
