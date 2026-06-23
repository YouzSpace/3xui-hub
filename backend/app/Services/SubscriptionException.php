<?php

namespace App\Services;

use RuntimeException;

/**
 * 订阅生成业务异常（用户禁用/过期/超量）。
 * Controller 捕获后转 code。
 */
class SubscriptionException extends RuntimeException
{
    public function __construct(string $message, public int $codeValue = 403)
    {
        parent::__construct($message);
    }
}
