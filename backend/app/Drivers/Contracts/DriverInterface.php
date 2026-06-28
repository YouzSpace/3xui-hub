<?php

namespace App\Drivers\Contracts;

/**
 * 所有驱动必须实现的最小接口。
 * 业务层通过 Capability 判断驱动能做什么，不依赖具体类型。
 */
interface DriverInterface
{
    /** 驱动名称，如 "3x-ui"、"marzban"、"wwspay" */
    public function name(): string;

    /** 驱动版本 */
    public function version(): string;

    /** 查询是否支持某个能力 */
    public function supports(string $capability): bool;

    /** 列出支持的所有能力 */
    public function capabilities(): array;
}
