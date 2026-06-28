<?php

namespace App\Drivers;

/**
 * 能力枚举 —— 业务层只判断能力，不判断驱动类型。
 *
 * 使用方式：
 *   if ($driver->supports(Capability::CLIENT_CREATE->value)) { ... }
 */
enum Capability: string
{
    // 客户管理
    case CLIENT_CREATE = 'client.create';
    case CLIENT_READ   = 'client.read';
    case CLIENT_UPDATE = 'client.update';
    case CLIENT_DELETE = 'client.delete';
    case CLIENT_LIST   = 'client.list';
    case CLIENT_TOGGLE = 'client.toggle';

    // 流量
    case TRAFFIC_SYNC   = 'traffic.sync';
    case TRAFFIC_RESET  = 'traffic.reset';

    // 入站
    case INBOUND_LIST  = 'inbound.list';
    case INBOUND_GET   = 'inbound.get';

    // 协议
    case PROTOCOL_ATTACH = 'protocol.attach';
    case PROTOCOL_DETACH = 'protocol.detach';

    // 订阅
    case SUBSCRIPTION_LINKS = 'subscription.links';
    case SUBSCRIPTION_GEN   = 'subscription.generate';

    // 健康
    case HEALTH_CHECK = 'health.check';

    // 支付
    case PAYMENT_PAY      = 'payment.pay';
    case PAYMENT_CALLBACK = 'payment.callback';
    case PAYMENT_QUERY    = 'payment.query';
    case PAYMENT_REFUND   = 'payment.refund';
}
