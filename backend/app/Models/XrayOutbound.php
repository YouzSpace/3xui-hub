<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;
use Illuminate\Support\Facades\Crypt;

/**
 * xray 节点自定义出站（中转/出口）。
 *
 * - settings：协议参数；wireguard 私钥单独进 secret_key（加密）
 * - tag 同节点内唯一；路由规则按 tag 引用
 */
#[Fillable([
    'node_id',
    'tag',
    'protocol',
    'settings',
    'stream_settings',
    'secret_key',
    'enabled',
    'sort',
    'remark',
])]
class XrayOutbound extends Model
{
    public const PROTOCOLS = [
        'freedom', 'blackhole', 'vmess', 'vless', 'trojan', 'shadowsocks', 'socks', 'http', 'wireguard',
    ];

    protected function casts(): array
    {
        return [
            'settings' => 'array',
            'stream_settings' => 'array',
            'enabled' => 'boolean',
            'sort' => 'integer',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }

    public function setSecretKey(?string $plain): void
    {
        $this->secret_key = ($plain === null || $plain === '')
            ? null
            : Crypt::encryptString($plain);
    }

    public function secretKey(): ?string
    {
        $raw = $this->secret_key;
        if (! is_string($raw) || $raw === '') {
            return null;
        }
        try {
            return Crypt::decryptString($raw);
        } catch (\Throwable) {
            return $raw;
        }
    }

    /** 渲染用：全部启用出站（节点维度，按 sort 排序）。 */
    public static function enabledFor(Node $node): \Illuminate\Database\Eloquent\Collection
    {
        return static::where('node_id', $node->id)
            ->where('enabled', true)
            ->orderBy('sort')
            ->orderBy('id')
            ->get();
    }
}
