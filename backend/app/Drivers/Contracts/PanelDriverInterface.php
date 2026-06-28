<?php

namespace App\Drivers\Contracts;

/**
 * 面板驱动 —— 通过面板 API 管理代理用户。
 *
 * 适用范围：3x-ui、Marzban、Hiddify 等有 REST API 的管理面板。
 *
 * 每个方法对应一个 Capability，业务层通过 supports() 判断后再调。
 */
interface PanelDriverInterface extends DriverInterface
{
    // ── 客户管理 ──
    public function listClients(): array;
    public function getClient(string $identifier): ?array;
    public function createClient(array $data, array $inboundIds = []): ?array;
    public function updateClient(string $identifier, array $data, ?int $inboundId = null): bool;
    public function deleteClient(string $identifier, bool $keepTraffic = false, ?int $inboundId = null): bool;
    public function toggleClient(string $identifier, bool $enable): bool;

    // ── 流量 ──
    public function getClientTraffic(string $identifier): ?array;
    public function resetClientTraffic(string $identifier): bool;

    // ── 入站 ──
    public function listInbounds(): array;
    public function getInbound(int $id): ?array;

    // ── 批量流量同步 ──
    public function getClientStatsGroupedByInbound(): array;

    // ── 订阅链接 ──
    public function getClientLinks(string $identifier): array;

    // ── 协议切换 ──
    public function attachClient(string $identifier, array $inboundIds): bool;
    public function detachClient(string $identifier, array $inboundIds): bool;

    // ── 健康检查 ──
    public function healthCheck(): array;
}
