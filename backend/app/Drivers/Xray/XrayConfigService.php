<?php

namespace App\Drivers\Xray;

use App\Models\Node;
use App\Models\User;
use App\Models\XrayInbound;
use App\Models\XrayOutbound;
use App\Models\XrayRoutingRule;
use Illuminate\Support\Collection;

/**
 * xray 节点全量配置生成（双通道的「全量通道」事实源）。
 *
 * 配置内容全部由面板侧管理：
 *   - 入站：xray_inbounds（协议/端口/传输/安全 + 加密 Reality 私钥）
 *   - 出站：内置 direct/blocked + xray_outbounds（含 WARP 生成的 wireguard 出站）
 *   - 路由：内置基础规则（阻私有 / 阻 BT）+ xray_routing_rules
 *   - 用户：按「用户协议 == 入站协议」注入 clients（ss 为单凭据模式，不注入）
 *
 * agent 以 ETag 拉取（无变化 304），仅结构变更时内容才变 —— 用户级操作走
 * adu/rmu 指令通道（NodeApiCommand），不改全量 config。
 *
 * 内核口径（v26.3.27 真机 + 本地 -test 验证）：
 * - freedom 挂 finalRules（block geoip:private + allow）：不依赖 dat 的私网出站兜底
 * - routing 基础规则与 3x-ui 默认一致：geoip:private → blocked、bittorrent → blocked
 * - levels 的数字键必须强转 object（PHP 数字键 json_encode 会变 []，xray 解析报错）
 * - system 段开入/出站统计（出站流量功能的数据源）
 */
class XrayConfigService
{
    /** xray 内核 API（本地回环）监听端口（v26 CLI 必须显式 --server 指向这里）。 */
    public const API_PORT = 10085;

    /** VLESS+Reality 传输流控（v26 校验要求该值，旧写法 xtls-rpra-vision 校验不过）。 */
    public const FLOW = 'xtls-rprx-vision';

    /** 默认 Reality dest / SNI。dest 不能用 TLS 记录 >8192 字节的站（P1 真机踩坑记录）。 */
    public const DEFAULT_DEST = 'www.cloudflare.com:443';
    public const DEFAULT_SNI = 'www.cloudflare.com';

    // ── 全量渲染 ─────────────────────────────────────────

    /**
     * 渲染全量 config.json（数组形态；stats 用空对象占位，保证 json_encode 出 {}）。
     */
    public function render(Node $node): array
    {
        $users = $this->eligibleUsers();

        $inbounds = [];
        foreach (XrayInbound::enabledFor($node) as $inbound) {
            $inbounds[] = $this->renderInbound($inbound, $users);
        }

        return [
            'log' => ['loglevel' => 'warning'],
            'api' => [
                'tag' => 'api',
                'listen' => '127.0.0.1:' . self::API_PORT,
                'services' => ['HandlerService', 'StatsService'],
            ],
            'stats' => new \stdClass(),
            'policy' => $this->policyConfig(),
            'outbounds' => $this->renderOutbounds($node),
            'routing' => $this->renderRouting($node),
            'inbounds' => $inbounds,
        ];
    }

    /** 渲染并编码为 JSON 字符串（ETag 与响应体的唯一编码口径）。 */
    public function renderJson(Node $node): string
    {
        return json_encode(
            $this->render($node),
            JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE
        );
    }

    /** 全量 config 的 ETag（sha1，agent 用 If-None-Match 对比）。 */
    public function etag(Node $node): string
    {
        return '"' . sha1($this->renderJson($node)) . '"';
    }

    /** 策略段：用户级统计（levels.0）+ 入/出站级统计（system，出站流量数据源）。 */
    public function policyConfig(): array
    {
        return [
            'levels' => (object) [
                '0' => (object) [
                    'statsUserUplink' => true,
                    'statsUserDownlink' => true,
                ],
            ],
            'system' => (object) [
                'statsInboundUplink' => true,
                'statsInboundDownlink' => true,
                'statsOutboundUplink' => true,
                'statsOutboundDownlink' => true,
            ],
        ];
    }

    // ── 入站渲染 ─────────────────────────────────────────

    /**
     * 渲染单个入站片段。
     *
     * @param  Collection  $users  全量候选用户（按协议自动过滤注入 clients）
     * @param  bool  $withStreamSettings  adu 指令载荷不需要 streamSettings（入站已存在）
     */
    public function renderInbound(XrayInbound $inbound, Collection $users, bool $withStreamSettings = true): array
    {
        $settings = $inbound->settings ?? [];

        $clients = $this->clientsFor($inbound, $users);
        if ($clients !== null) {
            $settings['clients'] = $clients;
        }

        $frag = [
            'tag' => $inbound->tag,
            'listen' => $inbound->listen ?: '0.0.0.0',
            'port' => $inbound->port,
            'protocol' => $inbound->protocol,
            'settings' => $settings === [] ? new \stdClass() : $settings,
        ];

        if ($withStreamSettings) {
            $stream = $inbound->stream_settings ?? [];
            if (($stream['security'] ?? '') === 'reality') {
                $stream['realitySettings'] = $this->realitySettingsFor($inbound);
            }
            if ($stream !== []) {
                $frag['streamSettings'] = $stream;
            }

            $frag['sniffing'] = $inbound->sniffing ?: [
                'enabled' => true,
                'destOverride' => ['http', 'tls', 'quic'],
            ];
        }

        return $frag;
    }

    /**
     * adu 指令用的入站片段（单用户；不含 streamSettings/私钥 —— 指令会入库）。
     * 格式沿用 P1 真机验证过的契约：{"inbounds":[{完整入站片段, clients:[单用户]}]}。
     */
    public function aduInboundFragment(XrayInbound $inbound, User $user): array
    {
        return $this->renderInbound($inbound, collect([$user]), false);
    }

    /** Reality 安全参数（公开参数来自入站 stream_settings；私钥解密注入）。 */
    public function realitySettingsFor(XrayInbound $inbound): array
    {
        $public = $inbound->stream_settings['realitySettings'] ?? [];

        $settings = [
            'dest' => (string) ($public['dest'] ?? self::DEFAULT_DEST),
            'serverNames' => array_values(array_filter(
                (array) ($public['serverNames'] ?? [self::DEFAULT_SNI]),
                'is_string'
            )),
            'shortIds' => array_values(array_filter(
                (array) ($public['shortIds'] ?? []),
                'is_string'
            )),
        ];

        $privateKey = $inbound->realityPrivateKey();
        if ($privateKey !== null && $privateKey !== '') {
            $settings['privateKey'] = $privateKey;
        }

        return $settings;
    }

    /**
     * 按协议生成 clients 数组；shadowsocks 单凭据模式返回 null（不注入 clients）。
     *
     * @return array<int, array>|null
     */
    public function clientsFor(XrayInbound $inbound, Collection $users): ?array
    {
        if ($inbound->protocol === 'shadowsocks') {
            return null;
        }

        $clients = [];
        foreach ($users as $user) {
            if ((string) $user->protocol !== $inbound->protocol) {
                continue;
            }
            $client = match ($inbound->protocol) {
                'vless' => $this->vlessClient($inbound, $user),
                'vmess' => $this->vmessClient($user),
                'trojan' => $this->trojanClient($user),
                default => null,
            };
            if ($client !== null) {
                $clients[] = $client;
            }
        }

        return $clients;
    }

    private function vlessClient(XrayInbound $inbound, User $user): array
    {
        $client = [
            'id' => (string) $user->uuid,
            'email' => $user->clientEmail(),
            'totalGB' => (int) ($user->traffic_limit ?? 0),
            'expiryTime' => $user->expired_at ? $user->expired_at->toIso8601ZuluString() : 0,
        ];
        if ($this->flowApplies($inbound)) {
            $client['flow'] = self::FLOW;
        }

        return $client;
    }

    private function vmessClient(User $user): array
    {
        return [
            'id' => (string) $user->uuid,
            'email' => $user->clientEmail(),
            'totalGB' => (int) ($user->traffic_limit ?? 0),
            'expiryTime' => $user->expired_at ? $user->expired_at->toIso8601ZuluString() : 0,
        ];
    }

    private function trojanClient(User $user): array
    {
        // trojan 以密码定位用户：统一用 uuid 作密码（面板侧保证唯一）
        return [
            'password' => (string) $user->uuid,
            'email' => $user->clientEmail(),
            'totalGB' => (int) ($user->traffic_limit ?? 0),
            'expiryTime' => $user->expired_at ? $user->expired_at->toIso8601ZuluString() : 0,
        ];
    }

    /** flow（xtls-rprx-vision）仅适用于 vless + tcp + reality/tls。 */
    public function flowApplies(XrayInbound $inbound): bool
    {
        if ($inbound->protocol !== 'vless') {
            return false;
        }
        $stream = $inbound->stream_settings ?? [];

        return ($stream['network'] ?? 'tcp') === 'tcp'
            && in_array($stream['security'] ?? 'none', ['reality', 'tls'], true);
    }

    // ── 出站渲染 ─────────────────────────────────────────

    /**
     * 出站段：内置 direct（freedom + finalRules 阻私有网段）+ blocked（blackhole），
     * 其后按 sort 追加节点自定义出站。
     */
    public function renderOutbounds(Node $node): array
    {
        $outbounds = [
            [
                'tag' => 'direct',
                'protocol' => 'freedom',
                'settings' => [
                    'finalRules' => [
                        ['action' => 'block', 'ip' => ['geoip:private']],
                        ['action' => 'allow'],
                    ],
                ],
            ],
            [
                'tag' => 'blocked',
                'protocol' => 'blackhole',
                'settings' => new \stdClass(),
            ],
        ];

        foreach (XrayOutbound::enabledFor($node) as $outbound) {
            if (in_array($outbound->tag, ['direct', 'blocked', 'api'], true)) {
                continue; // 系统保留 tag 不可占用
            }

            $frag = [
                'tag' => $outbound->tag,
                'protocol' => $outbound->protocol,
                'settings' => $this->outboundSettings($outbound),
            ];
            $stream = $outbound->stream_settings ?? [];
            if ($stream !== []) {
                $frag['streamSettings'] = $stream;
            }
            $outbounds[] = $frag;
        }

        return $outbounds;
    }

    /** 出站 settings 组装（wireguard 私钥解密注入；空 settings 出 {}）。 */
    private function outboundSettings(XrayOutbound $outbound): mixed
    {
        $settings = $outbound->settings ?? [];

        if ($outbound->protocol === 'wireguard') {
            $key = $outbound->secretKey();
            if ($key !== null && $key !== '') {
                $settings['secretKey'] = $key;
            }
        }

        return $settings === [] ? new \stdClass() : $settings;
    }

    // ── 路由渲染 ─────────────────────────────────────────

    /**
     * 路由段：内置基础规则（与 3x-ui 默认一致）打头，其后是节点自定义规则。
     */
    public function renderRouting(Node $node): array
    {
        $rules = [
            [
                'type' => 'field',
                'ip' => ['geoip:private'],
                'outboundTag' => 'blocked',
            ],
            [
                'type' => 'field',
                'protocol' => ['bittorrent'],
                'outboundTag' => 'blocked',
            ],
        ];

        foreach (XrayRoutingRule::enabledFor($node) as $rule) {
            $frag = ['type' => 'field', 'outboundTag' => $rule->outbound_tag];
            if (! empty($rule->domains)) {
                $frag['domain'] = array_values(array_filter((array) $rule->domains, 'is_string'));
            }
            if (! empty($rule->ips)) {
                $frag['ip'] = array_values(array_filter((array) $rule->ips, 'is_string'));
            }
            if (! empty($rule->port)) {
                $frag['port'] = (string) $rule->port;
            }
            if (! empty($rule->network)) {
                $frag['network'] = (string) $rule->network;
            }
            if (! empty($rule->protocol)) {
                $frag['protocol'] = [(string) $rule->protocol];
            }
            if (! empty($rule->inbound_tag)) {
                $frag['inboundTag'] = [(string) $rule->inbound_tag];
            }
            // 空规则（无任何匹配条件）会命中全部流量，跳过防误配
            if (count($frag) <= 2) {
                continue;
            }
            $rules[] = $frag;
        }

        return [
            'domainStrategy' => 'AsIs',
            'rules' => $rules,
        ];
    }

    // ── 用户口径 ─────────────────────────────────────────

    /**
     * 应该存在于节点上的用户（全量 config 的候选集）。
     * 口径与用户级 adu/rmu 一致：有套餐 + 启用 + 未被关闭流量；
     * 协议匹配由 clientsFor 按入站处理（不再限定 vless）。
     */
    public function eligibleUsers(): Collection
    {
        return User::query()
            ->whereNotNull('plan_id')
            ->where('enabled', true)
            ->whereNull('traffic_disabled_at')
            ->get();
    }

    // ── 订阅链接 ─────────────────────────────────────────

    /**
     * 生成用户在节点上的全部订阅 URI（每个协议匹配的启用入站一条）。
     *
     * @return string[]
     */
    public function subscriptionUris(Node $node, User $user): array
    {
        $host = trim((string) $node->host);
        if ($host === '' || (string) $user->uuid === '') {
            return [];
        }

        $uris = [];
        foreach (XrayInbound::enabledFor($node) as $inbound) {
            if ((string) $user->protocol !== $inbound->protocol) {
                continue;
            }
            $uri = match ($inbound->protocol) {
                'vless' => $this->vlessUri($node, $inbound, $user, $host),
                'vmess' => $this->vmessUri($node, $inbound, $user, $host),
                'trojan' => $this->trojanUri($node, $inbound, $user, $host),
                default => null, // shadowsocks 单凭据模式不跟用户走
            };
            if ($uri !== null) {
                $uris[] = $uri;
            }
        }

        return $uris;
    }

    /** 入站显示名（订阅节点名：节点名-tag 区分同节点多入站）。 */
    private function inboundLabel(Node $node, XrayInbound $inbound): string
    {
        return $node->name . '-' . $inbound->tag;
    }

    private function vlessUri(Node $node, XrayInbound $inbound, User $user, string $host): string
    {
        $stream = $inbound->stream_settings ?? [];
        $security = $stream['security'] ?? 'none';
        $network = $stream['network'] ?? 'tcp';

        $query = 'type=' . $network . '&security=' . $security . '&fp=chrome';

        if ($security === 'reality') {
            $public = $stream['realitySettings'] ?? [];
            $sni = $public['serverNames'][0] ?? self::DEFAULT_SNI;
            $query .= '&sni=' . rawurlencode((string) $sni)
                . '&pbk=' . rawurlencode((string) ($public['publicKey'] ?? ''));
            $sid = $public['shortIds'][0] ?? '';
            if ($sid !== '') {
                $query .= '&sid=' . rawurlencode((string) $sid);
            }
            if ($this->flowApplies($inbound)) {
                $query .= '&flow=' . self::FLOW;
            }
        } elseif ($security === 'tls') {
            $tls = $stream['tlsSettings'] ?? [];
            $query .= '&sni=' . rawurlencode((string) ($tls['serverName'] ?? $host));
        }

        if ($network === 'ws') {
            $ws = $stream['wsSettings'] ?? [];
            $query .= '&path=' . rawurlencode((string) ($ws['path'] ?? '/'));
            if (! empty($ws['host'])) {
                $query .= '&host=' . rawurlencode((string) $ws['host']);
            }
        }

        return sprintf(
            'vless://%s@%s:%d?%s#%s',
            $user->uuid,
            $host,
            $inbound->port,
            $query,
            rawurlencode($this->inboundLabel($node, $inbound))
        );
    }

    private function vmessUri(Node $node, XrayInbound $inbound, User $user, string $host): string
    {
        $stream = $inbound->stream_settings ?? [];
        $network = $stream['network'] ?? 'tcp';
        $security = $stream['security'] ?? 'none';
        $ws = $stream['wsSettings'] ?? [];
        $tls = $stream['tlsSettings'] ?? [];

        $payload = [
            'v' => '2',
            'ps' => $this->inboundLabel($node, $inbound),
            'add' => $host,
            'port' => (string) $inbound->port,
            'id' => (string) $user->uuid,
            'aid' => '0',
            'scy' => 'auto',
            'net' => $network,
            'type' => 'none',
            'host' => (string) ($ws['host'] ?? ''),
            'path' => (string) ($ws['path'] ?? ''),
            'tls' => $security === 'tls' ? 'tls' : '',
            'sni' => $security === 'tls' ? (string) ($tls['serverName'] ?? '') : '',
        ];

        return 'vmess://' . base64_encode(
            (string) json_encode($payload, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES)
        );
    }

    private function trojanUri(Node $node, XrayInbound $inbound, User $user, string $host): string
    {
        $stream = $inbound->stream_settings ?? [];
        $network = $stream['network'] ?? 'tcp';
        $security = $stream['security'] ?? 'none';
        $tls = $stream['tlsSettings'] ?? [];

        $query = 'security=' . ($security === 'tls' ? 'tls' : 'none') . '&type=' . $network;
        if ($security === 'tls') {
            $query .= '&sni=' . rawurlencode((string) ($tls['serverName'] ?? $host));
        }
        if ($network === 'ws') {
            $ws = $stream['wsSettings'] ?? [];
            $query .= '&path=' . rawurlencode((string) ($ws['path'] ?? '/'));
        }

        return sprintf(
            'trojan://%s@%s:%d?%s#%s',
            $user->uuid,
            $host,
            $inbound->port,
            $query,
            rawurlencode($this->inboundLabel($node, $inbound))
        );
    }
}
