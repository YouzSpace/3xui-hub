<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;
use Illuminate\Support\Facades\Crypt;

/**
 * Cloudflare WARP 账户（每节点一份；注册在面板，私钥随 config 下发给节点）。
 */
#[Fillable([
    'node_id',
    'device_id',
    'access_token',
    'private_key',
    'public_key',
    'peer_public_key',
    'peer_endpoint',
    'addresses',
    'reserved',
    'license_key',
    'client_id',
    'auto_rotate_hours',
    'last_rotate_at',
    'enabled',
])]
class NodeWarpAccount extends Model
{
    protected function casts(): array
    {
        return [
            'addresses' => 'array',
            'reserved' => 'array',
            'enabled' => 'boolean',
            'auto_rotate_hours' => 'integer',
            'last_rotate_at' => 'datetime',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }

    public function setAccessToken(?string $plain): void
    {
        $this->access_token = ($plain === null || $plain === '')
            ? null
            : Crypt::encryptString($plain);
    }

    public function accessToken(): ?string
    {
        return $this->decryptField($this->access_token);
    }

    public function setPrivateKey(?string $plain): void
    {
        $this->private_key = ($plain === null || $plain === '')
            ? null
            : Crypt::encryptString($plain);
    }

    public function privateKey(): ?string
    {
        return $this->decryptField($this->private_key);
    }

    private function decryptField(?string $raw): ?string
    {
        if (! is_string($raw) || $raw === '') {
            return null;
        }
        try {
            return Crypt::decryptString($raw);
        } catch (\Throwable) {
            return $raw;
        }
    }
}
