<?php

namespace App\Drivers;

use App\Drivers\Contracts\DriverInterface;
use App\Drivers\Contracts\PanelDriverInterface;
use App\Drivers\Contracts\PaymentDriverInterface;
use App\Drivers\Contracts\SubscriptionDriverInterface;
use RuntimeException;

/**
 * 驱动注册中心 —— 唯一入口。
 *
 * 所有驱动在此注册，业务层通过 get() 获取，不关心具体实现。
 */
class DriverRegistry
{
    /** @var array<string, DriverInterface> */
    private array $drivers = [];

    public function register(DriverInterface $driver): void
    {
        $this->drivers[$driver->name()] = $driver;
    }

    public function get(string $name): DriverInterface
    {
        return $this->drivers[$name]
            ?? throw new RuntimeException("Driver [{$name}] not registered");
    }

    /** 获取面板驱动 */
    public function panel(string $name): PanelDriverInterface
    {
        $driver = $this->get($name);
        if (! $driver instanceof PanelDriverInterface) {
            throw new RuntimeException("Driver [{$name}] is not a Panel driver");
        }
        return $driver;
    }

    /** 获取支付驱动 */
    public function payment(string $name): PaymentDriverInterface
    {
        $driver = $this->get($name);
        if (! $driver instanceof PaymentDriverInterface) {
            throw new RuntimeException("Driver [{$name}] is not a Payment driver");
        }
        return $driver;
    }

    /** 获取订阅驱动 */
    public function subscription(string $name): SubscriptionDriverInterface
    {
        $driver = $this->get($name);
        if (! $driver instanceof SubscriptionDriverInterface) {
            throw new RuntimeException("Driver [{$name}] is not a Subscription driver");
        }
        return $driver;
    }

    /** 判断某驱动是否支持某能力 */
    public function supports(string $name, string $capability): bool
    {
        return $this->get($name)->supports($capability);
    }

    /** 列出所有已注册驱动名 */
    public function names(): array
    {
        return array_keys($this->drivers);
    }
}
