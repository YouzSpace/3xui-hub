<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Attributes\Hidden;
use Illuminate\Database\Eloquent\Casts\Attribute;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;
use Illuminate\Support\Facades\Crypt;
use Illuminate\Support\Facades\DB;

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

    /** xray 节点出站订阅（服务器池，xray_outbound_subscriptions）。 */
    public function xrayOutboundSubscriptions(): HasMany
    {
        return $this->hasMany(XrayOutboundSubscription::class);
    }

    /** xray 节点路由负载均衡器（xray_balancers）。 */
    public function xrayBalancers(): HasMany
    {
        return $this->hasMany(XrayBalancer::class);
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

    /**
     * 递增全量 config 版本号（结构变更时调用）。
     *
     * 行锁内重读再递增：driver_config 是整块 JSON 列，若用内存快照 +1 后整块回写，
     * 与 alive/register 的心跳回写并发时会互相覆盖（丢版本号 → 结构变更不重启内核，
     * 或版本回退 → 指纹回退、agent 误判成新配置再重启一次）。
     */
    public function bumpXrayConfigVersion(): int
    {
        return DB::transaction(function () {
            $fresh = static::whereKey($this->getKey())->lockForUpdate()->first();
            $cfg = $fresh->driver_config ?? [];
            $cfg['config_version'] = (int) ($cfg['config_version'] ?? 0) + 1;
            // 变更时刻：alive 上报落后版本时，节点页据此显示「配置滞后 N 秒」
            $cfg['config_changed_at'] = now()->toIso8601String();
            $fresh->forceFill(['driver_config' => $cfg])->save();
            $this->setRawAttributes($fresh->getAttributes(), true);

            return $cfg['config_version'];
        });
    }

    /**
     * 只合并 driver_config 的指定键（行锁内重读最新值再合并）。
     *
     * 心跳（alive）与注册（register）都会写 driver_config；用内存快照整块回写会把
     * 并发 bumpXrayConfigVersion() 刚递增的 config_version 覆盖回旧值。凡「只更新
     * 部分键」的写入都必须走这里，config_version 只由 bumpXrayConfigVersion 推进。
     */
    public function mergeDriverConfig(array $patch): void
    {
        DB::transaction(function () use ($patch) {
            $fresh = static::whereKey($this->getKey())->lockForUpdate()->first();
            $cfg = $fresh->driver_config ?? [];
            $fresh->forceFill(['driver_config' => array_replace_recursive($cfg, $patch)])->save();
            $this->setRawAttributes($fresh->getAttributes(), true);
        });
    }

    /**
     * xray 路由基础设置：{domainStrategy, domainMatcher}。
     *
     * 存在 driver_config.xray_routing（JSON 列，免建表）。未设时返回默认口径
     * （AsIs / 空 —— 与旧渲染的硬编码 'AsIs' 行为一致，老节点升级后配置不变）。
     *
     * @return array{domainStrategy: string, domainMatcher: string}
     */
    public function xrayRoutingSettings(): array
    {
        $s = $this->driver_config['xray_routing'] ?? [];

        return [
            'domainStrategy' => (string) ($s['domainStrategy'] ?? 'AsIs'),
            'domainMatcher' => (string) ($s['domainMatcher'] ?? ''),
        ];
    }

    /** 写入 xray 路由基础设置并落库（调用方随后自行 bump 版本）。 */
    public function setXrayRoutingSettings(string $domainStrategy, string $domainMatcher): void
    {
        $cfg = $this->driver_config ?? [];
        $cfg['xray_routing'] = [
            'domainStrategy' => $domainStrategy,
            'domainMatcher' => $domainMatcher,
        ];
        $this->driver_config = $cfg;
        $this->save();
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
        // 先落盘再执行，不用 curl|bash 流式：脚本较长，管道中断会执行到半截。
        // 命令里不提权：脚本自己按身份决定（root 直跑 → sudo → su）。
        // 写死 sudo 会让「root 直登但没装 sudo」的机器（Debian 基础镜像常见：sudo 是
        // optional 包、连 /etc/sudoers 都没有）死在 `bash: sudo: command not found`；
        // 错在 shell 解析阶段，脚本一行都跑不到，脚本内也兜不住。
        return "curl -fsSL {$panelBase}/node-install.sh -o /tmp/controlhub-node-install.sh && bash /tmp/controlhub-node-install.sh --hub {$panelBase} --node {$name} --secret {$secret}";
    }
}
