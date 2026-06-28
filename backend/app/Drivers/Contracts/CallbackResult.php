<?php

namespace App\Drivers\Contracts;

/**
 * 支付回调处理结果。
 */
class CallbackResult
{
    public function __construct(
        public readonly bool $success,
        public readonly ?string $orderNo = null,
        public readonly ?string $tradeNo = null,
        public readonly ?float $amount = null,
        public readonly ?string $error = null,
        public readonly array $raw = [],
    ) {}

    public static function paid(string $orderNo, string $tradeNo, float $amount): self
    {
        return new self(success: true, orderNo: $orderNo, tradeNo: $tradeNo, amount: $amount);
    }

    public static function fail(string $error): self
    {
        return new self(success: false, error: $error);
    }
}
