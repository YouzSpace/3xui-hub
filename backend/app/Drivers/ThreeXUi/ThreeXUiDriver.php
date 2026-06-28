<?php

namespace App\Drivers\ThreeXUi;

use App\Drivers\Capability;
use App\Drivers\Contracts\PanelDriverInterface;
use App\Services\ThreeXUi\ThreeXUiClient;

/**
 * 3x-ui 面板驱动。
 *
 * 把现有 ThreeXUiClient 包进 PanelDriverInterface，业务层不直接依赖 ThreeXUiClient。
 */
class ThreeXUiDriver implements PanelDriverInterface
{
    private const CAPABILITIES = [
        Capability::CLIENT_CREATE,
        Capability::CLIENT_READ,
        Capability::CLIENT_UPDATE,
        Capability::CLIENT_DELETE,
        Capability::CLIENT_LIST,
        Capability::CLIENT_TOGGLE,
        Capability::TRAFFIC_SYNC,
        Capability::TRAFFIC_RESET,
        Capability::INBOUND_LIST,
        Capability::INBOUND_GET,
        Capability::PROTOCOL_ATTACH,
        Capability::PROTOCOL_DETACH,
        Capability::SUBSCRIPTION_LINKS,
        Capability::HEALTH_CHECK,
    ];

    public function __construct(
        private readonly ThreeXUiClient $client,
    ) {}

    // ── DriverInterface ──

    public function name(): string    { return '3x-ui'; }
    public function version(): string { return '3.x'; }

    public function supports(string $capability): bool
    {
        return in_array(Capability::tryFrom($capability), self::CAPABILITIES);
    }

    public function capabilities(): array
    {
        return array_map(fn (Capability $c) => $c->value, self::CAPABILITIES);
    }

    // ── PanelDriverInterface ──

    public function listClients(): array
    {
        return $this->client->listClients();
    }

    public function getClient(string $identifier): ?array
    {
        return $this->client->getClient($identifier);
    }

    public function createClient(array $data, array $inboundIds = []): ?array
    {
        return $this->client->addClient($data, $inboundIds);
    }

    public function updateClient(string $identifier, array $data, ?int $inboundId = null): bool
    {
        return $this->client->updateClient($identifier, $data, $inboundId);
    }

    public function deleteClient(string $identifier, bool $keepTraffic = false, ?int $inboundId = null): bool
    {
        return $this->client->deleteClient($identifier, $keepTraffic, $inboundId);
    }

    public function toggleClient(string $identifier, bool $enable): bool
    {
        return $this->client->updateClient($identifier, ['enable' => $enable]);
    }

    public function getClientTraffic(string $identifier): ?array
    {
        return $this->client->getClientTraffic($identifier);
    }

    public function resetClientTraffic(string $identifier): bool
    {
        return $this->client->resetClientTraffic($identifier);
    }

    public function listInbounds(): array
    {
        return $this->client->listInbounds();
    }

    public function getInbound(int $id): ?array
    {
        return $this->client->getInbound($id);
    }

    public function getClientStatsGroupedByInbound(): array
    {
        return $this->client->getClientStatsGroupedByInbound();
    }

    public function getClientLinks(string $identifier): array
    {
        return $this->client->getClientLinks($identifier);
    }

    public function attachClient(string $identifier, array $inboundIds): bool
    {
        return $this->client->attachClient($identifier, $inboundIds);
    }

    public function detachClient(string $identifier, array $inboundIds): bool
    {
        return $this->client->detachClient($identifier, $inboundIds);
    }

    public function healthCheck(): array
    {
        return $this->client->healthCheck();
    }
}
