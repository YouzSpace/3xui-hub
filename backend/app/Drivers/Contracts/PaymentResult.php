<?php

namespace App\Drivers\Contracts;

/**
 * 支付下单结果。
 */
class PaymentResult
{
    public function __construct(
        public readonly bool $success,
        public readonly ?string $payUrl = null,
        public readonly ?string $clientSecret = null,
        public readonly ?string $error = null,
        public readonly array $raw = [],
    ) {}

    public static function ok(string $payUrl): self
    {
        return new self(success: true, payUrl: $payUrl);
    }

    public static function fail(string $error): self
    {
        return new self(success: false, error: $error);
    }
}
