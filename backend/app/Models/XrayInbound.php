<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;
use Illuminate\Support\Facades\Crypt;

/**
 * xray 节点自建入站（协议不写死；渲染 config 时动态组装）。
 *
 * - settings / stream_settings 存协议公开参数；clients 用户列表渲染时注入
 * - reality_private_key：Crypt 加密的 X25519 私钥（xray x25519 同格式）
 */
#[Fillable([
    'node_id',
    'tag',
    'protocol',
    'port',
    'listen',
    'settings',
    'stream_settings',
    'sniffing',
    'reality_private_key',
    'enabled',
    'sort',
])]
class XrayInbound extends Model
{
    public const PROTOCOLS = ['vless', 'vmess', 'trojan', 'shadowsocks'];

    protected function casts(): array
    {
        return [
            'settings' => 'array',
            'stream_settings' => 'array',
            'sniffing' => 'array',
            'enabled' => 'boolean',
            'port' => 'integer',
            'sort' => 'integer',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }

    /** 加密写入私钥（null 清除）。 */
    public function setRealityPrivateKey(?string $plain): void
    {
        $this->reality_private_key = ($plain === null || $plain === '')
            ? null
            : Crypt::encryptString($plain);
    }

    /** 解密读取私钥（兼容明文存量/手工库改）。 */
    public function realityPrivateKey(): ?string
    {
        $raw = $this->reality_private_key;
        if (! is_string($raw) || $raw === '') {
            return null;
        }
        try {
            return Crypt::decryptString($raw);
        } catch (\Throwable) {
            return $raw;
        }
    }

    /** 该入站是否 reality 安全层。 */
    public function isReality(): bool
    {
        return ($this->stream_settings['security'] ?? '') === 'reality';
    }

    /** 渲染用：全部启用入站（节点维度，按 sort 排序）。 */
    public static function enabledFor(Node $node): \Illuminate\Database\Eloquent\Collection
    {
        return static::where('node_id', $node->id)
            ->where('enabled', true)
            ->orderBy('sort')
            ->orderBy('id')
            ->get();
    }
}
