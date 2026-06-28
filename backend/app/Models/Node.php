<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Attributes\Hidden;
use Illuminate\Database\Eloquent\Casts\Attribute;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;
use Illuminate\Support\Facades\Crypt;

/**
 * ControlHub 节点（system-design §5.1）。
 * password / api_key 用 Laravel encrypt() 加密入库，accessor 解密出明文。
 * ThreeXUiClient::fromNode() 读 $node->api_key 等属性得到明文。
 */
#[Fillable([
    'name', 'host', 'port', 'scheme', 'web_base_path',
    'username', 'password', 'api_key',
    'enabled', 'verify_ssl', 'status', 'latency', 'last_check_at',
    'driver_type', 'driver_version', 'driver_config',
])]
#[Hidden(['password', 'api_key'])]
class Node extends Model
{
    protected function casts(): array
    {
        return [
            'enabled' => 'boolean',
            'verify_ssl' => 'boolean',
            'port' => 'integer',
            'latency' => 'integer',
            'last_check_at' => 'datetime',
        ];
    }

    public function inbounds(): HasMany
    {
        return $this->hasMany(NodeInbound::class);
    }

    /** 取某协议的第一个 inbound_id，无则 null。 */
    public function inboundIdFor(string $protocol): ?int
    {
        $row = $this->inbounds()->where('protocol', $protocol)->first();

        return $row?->inbound_id;
    }

    /** 取某协议的全部 inbound_id。 */
    public function inboundIdsFor(string $protocol): array
    {
        return $this->inbounds()
            ->where('protocol', $protocol)
            ->pluck('inbound_id')
            ->toArray();
    }

    protected function password(): Attribute
    {
        return Attribute::make(
            get: fn ($value) => $value === null ? null : Crypt::decryptString($value),
            set: fn ($value) => $value === null || $value === '' ? null : Crypt::encryptString($value),
        );
    }

    protected function apiKey(): Attribute
    {
        return Attribute::make(
            get: fn ($value) => $value === null ? null : Crypt::decryptString($value),
            set: fn ($value) => $value === null || $value === '' ? null : Crypt::encryptString($value),
        );
    }
}
