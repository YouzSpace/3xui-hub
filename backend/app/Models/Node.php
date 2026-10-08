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
    // NULL = 继承 site_configs.default_node_multiplier；有值 = 该节点手动指定
    'traffic_multiplier',
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
            'driver_config' => 'array',
            // float cast 对 null 保留 null —— 「继承」与「手动设成 1.0」必须能区分开，
            // 所以这里绝不能写 ?? 1.0 之类在 cast 层兜底。
            'traffic_multiplier' => 'float',
        ];
    }

    public function inbounds(): HasMany
    {
        return $this->hasMany(NodeInbound::class);
    }

    /** xray 节点自建入站（xray_inbounds）。 */
    public function xrayInbounds(): HasMany
    {
        return $this->hasMany(XrayInbound::class);
    }

    /** xray 节点自定义出站（xray_outbounds）。 */
    public function xrayOutbounds(): HasMany
    {
        return $this->hasMany(XrayOutbound::class);
    }

    /** xray 节点路由规则（xray_routing_rules）。 */
    public function xrayRoutingRules(): HasMany
    {
        return $this->hasMany(XrayRoutingRule::class);
    }

    /** WARP 账户（每节点一份）。 */
    public function warpAccount(): \Illuminate\Database\Eloquent\Relations\HasOne
    {
        return $this->hasOne(NodeWarpAccount::class);
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

    // ───────────────────────────────────────────────
    // xray 节点（driver_type = 'xray'）专用
    // driver_config 结构：
    //   {
    //     "node_secret":   "…32B随机（加密入库）",
    //     "reality": { "sni","public_key","private_key","short_ids":[..], "listen_ports":[..] },
    //     "config_version": 1,          // 全量 config 单调递增（ETag）
    //     "pull_interval": 30,
    //     "xray_version": "v26.3.27"
    //   }
    // ───────────────────────────────────────────────

    public function isXray(): bool
    {
        return ($this->driver_type ?? '3x-ui') === 'xray';
    }

    /** 取/设 node_secret（加密存取，明文仅在 installCommand 与鉴权校验时出现）。 */
    public function nodeSecret(): ?string
    {
        $raw = $this->driver_config['node_secret'] ?? null;
        return $raw === null ? null : Crypt::decryptString($raw);
    }

    public function setNodeSecret(string $secret): void
    {
        $cfg = $this->driver_config ?? [];
        $cfg['node_secret'] = Crypt::encryptString($secret);
        $this->driver_config = $cfg;
    }

    /** 生成全新 32B 随机节点密钥并写入 driver_config。 */
    public function regenerateNodeSecret(): string
    {
        $secret = \Illuminate\Support\Str::random(32);
        $this->setNodeSecret($secret);
        $this->save();
        return $secret;
    }

    /** 取 Reality 配置（已废弃：Reality 密钥随入站迁移至 xray_inbounds；保留供老数据读取）。 */
    public function xrayReality(): ?array
    {
        return $this->driver_config['reality'] ?? null;
    }

    /** 当前全量 config 版本号（结构变更时 bump）。 */
    public function xrayConfigVersion(): int
    {
        return (int) ($this->driver_config['config_version'] ?? 0);
    }

    public function bumpXrayConfigVersion(): int
    {
        $cfg = $this->driver_config ?? [];
        $cfg['config_version'] = $this->xrayConfigVersion() + 1;
        // 变更时刻：alive 上报落后版本时，节点页据此显示「配置滞后 N 秒」
        $cfg['config_changed_at'] = now()->toIso8601String();
        $this->driver_config = $cfg;
        $this->save();
        return $cfg['config_version'];
    }

    /**
     * 拼安装命令（指向面板自己的地址，GitHub 零依赖）。
     * $panelBase 形如 https://your-hub.com，节点侧全程只连面板。
     */
    public function installCommand(string $panelBase): string
    {
        $panelBase = rtrim($panelBase, '/');
        $name = rawurlencode((string) $this->name);
        $secret = $this->nodeSecret();
        if ($secret === null) {
            $secret = \Illuminate\Support\Str::random(32);
            $this->setNodeSecret($secret);
            $this->save();
        }
        return "curl -fsSL {$panelBase}/node-install.sh | sudo bash -s -- --hub {$panelBase} --node {$name} --secret {$secret}";
    }
}
