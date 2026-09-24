<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Models\Node;
use App\Models\SiteConfig;
use App\Models\User;
use Illuminate\Support\Str;

/**
 * 订阅生成服务（M6）。
 * 流程：校验用户 → 取在线节点 → 各节点 getClientLinks(ch_user_{id}) → 合并 → 格式化。
 * 支持格式：Base64（默认）、Clash YAML、Sing-box JSON。
 */
class SubscriptionService
{
    public function __construct(private NodeDriverFactory $driverFactory)
    {
    }

    /**
     * 生成订阅内容。校验失败抛 SubscriptionException。
     *
     * @param User $user 用户
     * @param string $format 格式：base64|clash|singbox
     * @return string 订阅内容
     */
    public function generate(User $user, string $format = 'base64'): string
    {
        $this->ensureUsable($user);

        $links = $this->getLinks($user);

        if (empty($links)) {
            return '';
        }

        return match ($format) {
            'clash' => $this->generateClash($user, $links),
            'singbox' => $this->generateSingbox($user, $links),
            default => $this->generateBase64($user, $links),
        };
    }

    /**
     * 获取用户的所有链接。
     */
    private function getLinks(User $user): array
    {
        $nodes = Node::where('enabled', true)
            ->where('status', 'online')
            ->whereHas('inbounds', function ($q) use ($user) {
                $q->where('protocol', $user->protocol);
            })
            ->get();

        $links = [];
        $email = $user->clientEmail();

        foreach ($nodes as $node) {
            $inboundIds = $node->inbounds()
                ->where('protocol', $user->protocol)
                ->pluck('inbound_id')
                ->toArray();

            if (empty($inboundIds)) {
                continue;
            }

            try {
                $driver = $this->driverFactory->make($node);

                // 取各配置入站的端口，用于过滤
                $configuredPorts = [];
                foreach ($inboundIds as $inboundId) {
                    $inbound = $driver->getInbound($inboundId);
                    if (is_array($inbound) && isset($inbound['port'])) {
                        $configuredPorts[] = (int) $inbound['port'];
                    }
                }

                foreach ($driver->getClientLinks($email) as $link) {
                    if (!is_string($link) || $link === '') {
                        continue;
                    }
                    // 只保留配置了的入站端口
                    if (!empty($configuredPorts) && !$this->linkMatchesAnyPort($link, $configuredPorts)) {
                        continue;
                    }
                    if (!in_array($link, $links, true)) {
                        $links[] = $link;
                    }
                }
            } catch (\Throwable $e) {
                report($e);
            }
        }

        return $links;
    }

    /**
     * 生成 Base64 格式。
     */
    private function generateBase64(User $user, array $links): string
    {
        $regionCounters = []; // 按地区计数
        $outLinks = [];

        // 信息条目排在真实节点之前
        foreach ($this->createInfoLinks($user) as $infoLink) {
            $outLinks[] = $infoLink;
        }

        foreach ($links as $link) {
            try {
                $outLinks[] = $this->applyNameToLink($link, $regionCounters);
            } catch (\Throwable $e) {
                // 名称处理失败：保留原链接，不毁掉整份订阅
                $outLinks[] = $link;
            }
        }

        return base64_encode(implode("\n", $outLinks));
    }

    /**
     * 对链接 #fragment（名称段）应用名称处理，链接其余部分原样保留。
     */
    private function applyNameToLink(string $link, array &$regionCounters): string
    {
        $hashPos = strrpos($link, '#');
        if ($hashPos === false) {
            return $link;
        }

        $rawName = urldecode(substr($link, $hashPos + 1));
        $newName = $this->processNodeName($rawName, $regionCounters);

        return substr($link, 0, $hashPos) . '#' . urlencode($newName);
    }

    /**
     * 统一节点名称处理：用户 ID 清理 → 正则重命名 → 旗帜。
     */
    private function processNodeName(string $name, array &$regionCounters): string
    {
        if (SiteConfig::getValue('sub_show_userid', '0') !== '1') {
            $name = $this->stripUserIdFromName($name);
        }

        $name = $this->renameNode($name);

        $showFlag = SiteConfig::getValue('sub_show_flag', '0');
        if ($showFlag === '1') {
            $name = $this->addFlagToName($name, $regionCounters);
        }

        return $name;
    }

    /**
     * 清理节点名中的面板用户标识（ch_user_N）及残留孤立分隔符。
     */
    private function stripUserIdFromName(string $name): string
    {
        $name = preg_replace('/ch_user_\d+/u', '', $name) ?? $name;
        // 残留空括号
        $name = preg_replace('/\[\s*\]/u', '', $name) ?? $name;
        // 连续空格
        $name = preg_replace('/\s{2,}/u', ' ', $name) ?? $name;
        // 多余的连续 -
        $name = preg_replace('/(?:\s*-\s*){2,}/u', '-', $name) ?? $name;
        // 首尾孤立分隔符
        $name = preg_replace('/^[\s\-\[\]()_]+|[\s\-\[\]()_]+$/u', '', $name) ?? $name;

        return $name;
    }

    /**
     * base64 / base64url 解码（允许缺 padding）。失败返回 null。
     */
    private function decodeBase64Url(string $value): ?string
    {
        $value = strtr($value, '-_', '+/');
        $pad = strlen($value) % 4;
        if ($pad === 1) {
            return null;
        }
        if ($pad > 0) {
            $value .= str_repeat('=', 4 - $pad);
        }
        $decoded = base64_decode($value, true);

        return $decoded === false ? null : $decoded;
    }

    /**
     * 解析 ss userinfo（base64(method:password)）为 [method, password]。失败返回 null。
     */
    private function parseSsCredentials(string $userInfo): ?array
    {
        $decoded = $this->decodeBase64Url($userInfo);
        if ($decoded === null || strpos($decoded, ':') === false) {
            return null;
        }
        [$method, $password] = explode(':', $decoded, 2);
        if ($method === '' || $password === '') {
            return null;
        }

        return [$method, $password];
    }

    /**
     * 生成 Clash YAML 格式。
     */
    private function generateClash(User $user, array $links): string
    {
        $proxies = [];
        $proxyNames = [];
        $regionCounters = []; // 按地区计数

        // 添加流量信息节点
        $infoProxies = $this->createInfoProxies($user);
        foreach ($infoProxies as $infoProxy) {
            $proxies[] = $infoProxy;
            // 流量信息节点不添加到 proxyNames（不加入代理组）
        }

        foreach ($links as $link) {
            $proxy = $this->parseLinkToClashProxy($link, $regionCounters);
            if ($proxy) {
                $proxies[] = $proxy;
                $proxyNames[] = $proxy['name'];
            }
        }

        if (empty($proxies)) {
            return '';
        }

        $yaml = "proxies:\n";
        foreach ($proxies as $proxy) {
            $yaml .= "  - name: \"{$proxy['name']}\"\n";
            $yaml .= "    type: {$proxy['type']}\n";
            $yaml .= "    server: {$proxy['server']}\n";
            $yaml .= "    port: {$proxy['port']}\n";

            if (isset($proxy['uuid'])) {
                $yaml .= "    uuid: {$proxy['uuid']}\n";
            }
            if (isset($proxy['cipher'])) {
                $yaml .= "    cipher: {$proxy['cipher']}\n";
            }
            if (isset($proxy['password'])) {
                $yaml .= "    password: {$proxy['password']}\n";
            }
            if (isset($proxy['encryption'])) {
                $yaml .= "    encryption: {$proxy['encryption']}\n";
            }
            if (isset($proxy['flow'])) {
                $yaml .= "    flow: {$proxy['flow']}\n";
            }
            if (isset($proxy['udp'])) {
                $yaml .= "    udp: " . ($proxy['udp'] ? 'true' : 'false') . "\n";
            }
            if (isset($proxy['tls'])) {
                $yaml .= "    tls: " . ($proxy['tls'] ? 'true' : 'false') . "\n";
            }
            if (isset($proxy['sni'])) {
                $yaml .= "    servername: {$proxy['sni']}\n";
            }
            if (isset($proxy['servername'])) {
                $yaml .= "    servername: {$proxy['servername']}\n";
            }
            if (isset($proxy['client-fingerprint'])) {
                $yaml .= "    client-fingerprint: {$proxy['client-fingerprint']}\n";
            }
            if (isset($proxy['reality-opts'])) {
                $yaml .= "    reality-opts:\n";
                if (isset($proxy['reality-opts']['public-key'])) {
                    $yaml .= "      public-key: {$proxy['reality-opts']['public-key']}\n";
                }
                if (isset($proxy['reality-opts']['short-id'])) {
                    $yaml .= "      short-id: {$proxy['reality-opts']['short-id']}\n";
                }
            }
            if (isset($proxy['network'])) {
                $yaml .= "    network: {$proxy['network']}\n";
            }
            if (isset($proxy['ws-opts'])) {
                $yaml .= "    ws-opts:\n";
                $yaml .= "      path: \"{$proxy['ws-opts']['path']}\"\n";
                if (isset($proxy['ws-opts']['headers'])) {
                    $yaml .= "      headers:\n";
                    foreach ($proxy['ws-opts']['headers'] as $key => $value) {
                        $yaml .= "        {$key}: \"{$value}\"\n";
                    }
                }
            }
            if (isset($proxy['xhttp-opts'])) {
                $yaml .= "    xhttp-opts:\n";
                $yaml .= "      path: {$proxy['xhttp-opts']['path']}\n";
                $yaml .= "      mode: {$proxy['xhttp-opts']['mode']}\n";
                if (isset($proxy['xhttp-opts']['x-padding-bytes'])) {
                    $yaml .= "      x-padding-bytes: {$proxy['xhttp-opts']['x-padding-bytes']}\n";
                }
            }
        }

        $yaml .= "\nproxy-groups:\n";
        $yaml .= "  - name: \"Proxy\"\n";
        $yaml .= "    type: select\n";
        $yaml .= "    proxies:\n";
        foreach ($proxyNames as $name) {
            $yaml .= "      - \"{$name}\"\n";
        }

        $yaml .= "\nrules:\n";
        $yaml .= "  - MATCH,Proxy\n";

        return $yaml;
    }

    /**
     * 解析链接为 Clash 代理配置。解析失败或不支持时返回 null（跳过该节点）。
     */
    private function parseLinkToClashProxy(string $link, array &$regionCounters): ?array
    {
        try {
            // 解析 vless://、trojan://、ss:// 等链接
            if (!preg_match('/^(vless|trojan|ss):\/\/([^@]+)@([^:]+):(\d+)/', $link, $m)) {
                return null;
            }
            $protocol = $m[1];
            $userPart = $m[2];
            $server = $m[3];
            $port = (int) $m[4];

            // 解析参数
            $params = [];
            if (strpos($link, '?') !== false) {
                $queryString = substr($link, strpos($link, '?') + 1);
                if (strpos($queryString, '#') !== false) {
                    $queryString = substr($queryString, 0, strpos($queryString, '#'));
                }
                parse_str($queryString, $params);
            }

            // 解析名称
            $name = '';
            if (strpos($link, '#') !== false) {
                $name = urldecode(substr($link, strrpos($link, '#') + 1));
            }
            if (empty($name)) {
                $name = "{$protocol}-{$server}:{$port}";
            }

            // 应用名称处理（用户 ID 清理 / 正则重命名 / 旗帜）
            $name = $this->processNodeName($name, $regionCounters);
            if (trim($name) === '') {
                $name = "{$protocol}-{$server}:{$port}";
            }

            // ss：base64 解码凭据，输出 cipher + password
            if ($protocol === 'ss') {
                $credentials = $this->parseSsCredentials($userPart);
                if ($credentials === null) {
                    return null;
                }

                return [
                    'name' => $name,
                    'type' => 'ss',
                    'server' => $server,
                    'port' => $port,
                    'cipher' => $credentials[0],
                    'password' => $credentials[1],
                    'udp' => true,
                ];
            }

            $proxy = [
                'name' => $name,
                'type' => $protocol,
                'server' => $server,
                'port' => $port,
                'udp' => true,
                'client-fingerprint' => $params['fp'] ?? 'chrome',
            ];

            // trojan 用 password，vless 用 uuid
            if ($protocol === 'trojan') {
                $proxy['password'] = $userPart;
            } else {
                $proxy['uuid'] = $userPart;

                // flow 只透传不发明：链接没有 flow 就绝不输出
                if (isset($params['flow'])) {
                    $proxy['flow'] = $params['flow'];
                }

                // encryption 参数
                if (isset($params['encryption'])) {
                    $proxy['encryption'] = $params['encryption'];
                } else {
                    // VLESS 协议默认使用 none 加密
                    $proxy['encryption'] = 'none';
                }
            }

            // TLS / Reality
            if (isset($params['security'])) {
                if ($params['security'] === 'tls') {
                    $proxy['tls'] = true;
                    if (isset($params['sni'])) {
                        $proxy['sni'] = $this->cleanSni($params['sni']);
                    }
                } elseif ($params['security'] === 'reality') {
                    $proxy['tls'] = true;
                    $proxy['servername'] = $this->cleanSni($params['sni'] ?? $server);
                    if (isset($params['fp'])) {
                        $proxy['client-fingerprint'] = $params['fp'];
                    }
                    if (isset($params['pbk'])) {
                        $proxy['reality-opts'] = [
                            'public-key' => $params['pbk'],
                        ];
                        if (isset($params['sid'])) {
                            $proxy['reality-opts']['short-id'] = $params['sid'];
                        }
                    }
                }
            }

            // 传输协议
            $proxy['network'] = $params['type'] ?? 'tcp';
            if (isset($params['type'])) {
                if ($params['type'] === 'ws') {
                    $wsOpts = ['path' => $params['path'] ?? '/'];
                    if (isset($params['host'])) {
                        $wsOpts['headers'] = ['Host' => $params['host']];
                    }
                    $proxy['ws-opts'] = $wsOpts;
                } elseif ($params['type'] === 'xhttp') {
                    // clash 的 xhttp-opts 保留（mihomo 1.19.31 实测支持）
                    $xhttpOpts = [
                        'path' => $params['path'] ?? '/',
                        'mode' => $params['mode'] ?? 'auto',
                    ];
                    // 解析 extra 参数（JSON 格式）
                    if (isset($params['extra'])) {
                        $extra = json_decode($params['extra'], true);
                        if ($extra) {
                            if (isset($extra['xPaddingBytes'])) {
                                $xhttpOpts['x-padding-bytes'] = $extra['xPaddingBytes'];
                            }
                        }
                    }
                    $proxy['xhttp-opts'] = $xhttpOpts;
                }
            }

            return $proxy;
        } catch (\Throwable $e) {
            // 解析失败：跳过该节点，不毁掉整份订阅
            return null;
        }
    }

    /**
     * 生成 Sing-box JSON 格式。
     */
    private function generateSingbox(User $user, array $links): string
    {
        $regionCounters = []; // 按地区计数

        // 添加流量信息节点
        $infoOutbounds = $this->createInfoOutbounds($user);

        $realOutbounds = [];
        foreach ($links as $link) {
            $outbound = $this->parseLinkToSingboxOutbound($link, $regionCounters);
            if ($outbound) {
                $realOutbounds[] = $outbound;
            }
        }

        // 有链接但全部被跳过（无真实节点出站）时注入提示条目，排 direct 之后第一位
        // $links 本身为空保持现状（返回空串），不加提示
        $hintOutbounds = [];
        if (!empty($links) && empty($realOutbounds)) {
            $hintOutbounds[] = [
                'type' => 'shadowsocks',
                'tag' => '不支持Sing-box请用Clash或Base64',
                'server' => '127.0.0.1',
                'server_port' => 1,
                'method' => 'aes-256-gcm',
                'password' => 'info',
            ];
        }

        $outbounds = array_merge($hintOutbounds, $infoOutbounds, $realOutbounds);

        if (empty($outbounds)) {
            return '';
        }

        $config = [
            'outbounds' => array_merge(
                [['type' => 'direct', 'tag' => 'direct']],
                $outbounds
            ),
        ];

        return json_encode($config, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES);
    }

    /**
     * 解析链接为 Sing-box outbound 配置。解析失败或不支持时返回 null（跳过该节点）。
     */
    private function parseLinkToSingboxOutbound(string $link, array &$regionCounters): ?array
    {
        try {
            // 解析 vless://、trojan://、ss:// 等链接
            if (!preg_match('/^(vless|trojan|ss):\/\/([^@]+)@([^:]+):(\d+)/', $link, $m)) {
                return null;
            }
            $protocol = $m[1];
            $userPart = $m[2];
            $server = $m[3];
            $port = (int) $m[4];

            // 解析参数
            $params = [];
            if (strpos($link, '?') !== false) {
                $queryString = substr($link, strpos($link, '?') + 1);
                if (strpos($queryString, '#') !== false) {
                    $queryString = substr($queryString, 0, strpos($queryString, '#'));
                }
                parse_str($queryString, $params);
            }

            // 解析名称
            $name = '';
            if (strpos($link, '#') !== false) {
                $name = urldecode(substr($link, strrpos($link, '#') + 1));
            }
            if (empty($name)) {
                $name = "{$protocol}-{$server}:{$port}";
            }

            // 应用名称处理（用户 ID 清理 / 正则重命名 / 旗帜）
            $name = $this->processNodeName($name, $regionCounters);
            if (trim($name) === '') {
                $name = "{$protocol}-{$server}:{$port}";
            }

            // ss 的 type 用 shadowsocks
            $outbound = [
                'type' => $protocol === 'ss' ? 'shadowsocks' : $protocol,
                'tag' => $name,
                'server' => $server,
                'server_port' => $port,
            ];

            // UUID 或 password
            if ($protocol === 'vless') {
                $outbound['uuid'] = $userPart;
                // flow 只透传不发明：链接没有 flow 就绝不输出
                if (isset($params['flow'])) {
                    $outbound['flow'] = $params['flow'];
                }
                // 注意：sing-box 的 vless 不输出 encryption 字段
            } elseif ($protocol === 'trojan') {
                $outbound['password'] = $userPart;
            } elseif ($protocol === 'ss') {
                // base64 解码凭据拆出 method / password
                $credentials = $this->parseSsCredentials($userPart);
                if ($credentials === null) {
                    return null;
                }
                $outbound['method'] = $credentials[0];
                $outbound['password'] = $credentials[1];
            }

            // TLS / Reality
            if (isset($params['security'])) {
                if ($params['security'] === 'tls') {
                    $outbound['tls'] = [
                        'enabled' => true,
                        'server_name' => $this->cleanSni($params['sni'] ?? $server),
                    ];
                } elseif ($params['security'] === 'reality') {
                    $tlsConfig = [
                        'enabled' => true,
                        'server_name' => $this->cleanSni($params['sni'] ?? $server),
                        'reality' => [
                            'enabled' => true,
                            'public_key' => $params['pbk'] ?? '',
                            'short_id' => $params['sid'] ?? '',
                        ],
                    ];
                    if (isset($params['fp'])) {
                        $tlsConfig['utls'] = [
                            'enabled' => true,
                            'fingerprint' => $params['fp'],
                        ];
                    }
                    $outbound['tls'] = $tlsConfig;
                }
            }

            // 传输协议
            $network = $params['type'] ?? 'tcp';
            if ($network && !in_array($network, ['tcp', 'none'], true)) {
                // sing-box 1.14.1 不支持 xhttp 等传输：跳过该节点
                if (!in_array($network, ['ws', 'grpc', 'http', 'httpupgrade'], true)) {
                    return null;
                }
                $transport = ['type' => $network];
                if (isset($params['path'])) {
                    $transport['path'] = $params['path'];
                }
                if (isset($params['host'])) {
                    $transport['headers'] = ['Host' => $params['host']];
                }
                $outbound['transport'] = $transport;
            }

            return $outbound;
        } catch (\Throwable $e) {
            // 解析失败：跳过该节点，不毁掉整份订阅
            return null;
        }
    }

    /**
     * 根据节点名称添加旗帜并简化格式。
     * 自动识别地区关键词，按地区分组编号，显示为"旗帜+地区+编号"格式。
     */
    private function addFlagToName(string $name, array &$regionCounters): string
    {
        // 地区关键词到旗帜和简称的映射
        $regionMap = [
            '香港' => ['flag' => '🇭🇰', 'code' => 'HK'],
            'HK' => ['flag' => '🇭🇰', 'code' => 'HK'],
            'Hong Kong' => ['flag' => '🇭🇰', 'code' => 'HK'],
            '台湾' => ['flag' => '🇹🇼', 'code' => 'TW'],
            'TW' => ['flag' => '🇹🇼', 'code' => 'TW'],
            'Taiwan' => ['flag' => '🇹🇼', 'code' => 'TW'],
            '日本' => ['flag' => '🇯🇵', 'code' => 'JP'],
            'JP' => ['flag' => '🇯🇵', 'code' => 'JP'],
            'Japan' => ['flag' => '🇯🇵', 'code' => 'JP'],
            '韩国' => ['flag' => '🇰🇷', 'code' => 'KR'],
            'KR' => ['flag' => '🇰🇷', 'code' => 'KR'],
            'Korea' => ['flag' => '🇰🇷', 'code' => 'KR'],
            '新加坡' => ['flag' => '🇸🇬', 'code' => 'SG'],
            'SG' => ['flag' => '🇸🇬', 'code' => 'SG'],
            'Singapore' => ['flag' => '🇸🇬', 'code' => 'SG'],
            '美国' => ['flag' => '🇺🇸', 'code' => 'US'],
            'US' => ['flag' => '🇺🇸', 'code' => 'US'],
            'USA' => ['flag' => '🇺🇸', 'code' => 'US'],
            'America' => ['flag' => '🇺🇸', 'code' => 'US'],
            '英国' => ['flag' => '🇬🇧', 'code' => 'UK'],
            'UK' => ['flag' => '🇬🇧', 'code' => 'UK'],
            'Britain' => ['flag' => '🇬🇧', 'code' => 'UK'],
            '德国' => ['flag' => '🇩🇪', 'code' => 'DE'],
            'DE' => ['flag' => '🇩🇪', 'code' => 'DE'],
            'Germany' => ['flag' => '🇩🇪', 'code' => 'DE'],
            '法国' => ['flag' => '🇫🇷', 'code' => 'FR'],
            'FR' => ['flag' => '🇫🇷', 'code' => 'FR'],
            'France' => ['flag' => '🇫🇷', 'code' => 'FR'],
            '加拿大' => ['flag' => '🇨🇦', 'code' => 'CA'],
            'CA' => ['flag' => '🇨🇦', 'code' => 'CA'],
            'Canada' => ['flag' => '🇨🇦', 'code' => 'CA'],
            '澳大利亚' => ['flag' => '🇦🇺', 'code' => 'AU'],
            'AU' => ['flag' => '🇦🇺', 'code' => 'AU'],
            'Australia' => ['flag' => '🇦🇺', 'code' => 'AU'],
            '印度' => ['flag' => '🇮🇳', 'code' => 'IN'],
            'IN' => ['flag' => '🇮🇳', 'code' => 'IN'],
            'India' => ['flag' => '🇮🇳', 'code' => 'IN'],
            '泰国' => ['flag' => '🇹🇭', 'code' => 'TH'],
            'TH' => ['flag' => '🇹🇭', 'code' => 'TH'],
            'Thailand' => ['flag' => '🇹🇭', 'code' => 'TH'],
            '马来西亚' => ['flag' => '🇲🇾', 'code' => 'MY'],
            'MY' => ['flag' => '🇲🇾', 'code' => 'MY'],
            'Malaysia' => ['flag' => '🇲🇾', 'code' => 'MY'],
            '越南' => ['flag' => '🇻🇳', 'code' => 'VN'],
            'VN' => ['flag' => '🇻🇳', 'code' => 'VN'],
            'Vietnam' => ['flag' => '🇻🇳', 'code' => 'VN'],
            '俄罗斯' => ['flag' => '🇷🇺', 'code' => 'RU'],
            'RU' => ['flag' => '🇷🇺', 'code' => 'RU'],
            'Russia' => ['flag' => '🇷🇺', 'code' => 'RU'],
            '巴西' => ['flag' => '🇧🇷', 'code' => 'BR'],
            'BR' => ['flag' => '🇧🇷', 'code' => 'BR'],
            'Brazil' => ['flag' => '🇧🇷', 'code' => 'BR'],
            '土耳其' => ['flag' => '🇹🇷', 'code' => 'TR'],
            'TR' => ['flag' => '🇹🇷', 'code' => 'TR'],
            'Turkey' => ['flag' => '🇹🇷', 'code' => 'TR'],
            '荷兰' => ['flag' => '🇳🇱', 'code' => 'NL'],
            'NL' => ['flag' => '🇳🇱', 'code' => 'NL'],
            'Netherlands' => ['flag' => '🇳🇱', 'code' => 'NL'],
            '中国' => ['flag' => '🇨🇳', 'code' => 'CN'],
            'CN' => ['flag' => '🇨🇳', 'code' => 'CN'],
            'China' => ['flag' => '🇨🇳', 'code' => 'CN'],
        ];

        // 检查名称是否已经有旗帜，如果有则移除后再处理
        if (preg_match('/[\x{1F1E0}-\x{1F1FF}]{2}/u', $name)) {
            // 移除已有的旗帜emoji
            $name = preg_replace('/[\x{1F1E0}-\x{1F1FF}]{2}\s*/u', '', $name);
        }

        // 根据关键词识别地区
        $upperName = strtoupper($name);
        $region = null;
        foreach ($regionMap as $keyword => $info) {
            if (stripos($upperName, strtoupper($keyword)) !== false) {
                $region = $info;
                break;
            }
        }

        if (!$region) {
            return $name;
        }

        // 按地区计数
        $regionCode = $region['code'];
        if (!isset($regionCounters[$regionCode])) {
            $regionCounters[$regionCode] = 0;
        }
        $regionCounters[$regionCode]++;
        $number = str_pad($regionCounters[$regionCode], 2, '0', STR_PAD_LEFT);

        // 组合为"旗帜+地区+编号"格式
        return $region['flag'] . ' ' . $region['code'] . $number;
    }

    /**
     * 正则重命名节点名称。
     */
    private function renameNode(string $name): string
    {
        $enabled = SiteConfig::getValue('sub_rename_enabled', '0');
        if ($enabled !== '1') {
            return $name;
        }

        $regex = SiteConfig::getValue('sub_rename_regex', '');
        $replacement = SiteConfig::getValue('sub_rename_replacement', '');

        if (empty($regex) || empty($replacement)) {
            return $name;
        }

        try {
            $result = preg_replace($regex, $replacement, $name);
            return $result !== null ? $result : $name;
        } catch (\Throwable $e) {
            // 正则表达式错误，返回原名称
            return $name;
        }
    }

    /**
     * 创建流量信息节点（Clash 格式）。
     */
    private function createInfoProxies(User $user): array
    {
        $proxies = [];
        $showExpire = SiteConfig::getValue('sub_show_expire', '0') === '1';
        $showTraffic = SiteConfig::getValue('sub_show_traffic', '0') === '1';

        // 判断是否显示重置时间（只有周期套餐且月数 > 1 才显示）
        $showReset = false;
        if ($user->plan && $user->plan->isPeriod() && $user->plan->months > 1) {
            $showReset = true;
        }

        // 到期时间
        if ($showExpire) {
            $expire = $user->expired_at ? $user->expired_at->timestamp : 0;
            $expireStr = $expire > 0 ? date('Y-m-d', $expire) : '永久';
            $proxies[] = [
                'name' => "到期: {$expireStr}",
                'type' => 'ss',
                'server' => '127.0.0.1',
                'port' => 1,
                'cipher' => 'aes-256-gcm',
                'password' => 'info',
                'udp' => false,
            ];
        }

        // 当月流量（只有周期套餐才显示）
        if ($showTraffic && $user->plan && $user->plan->isPeriod()) {
            $monthlyUsed = $user->monthly_traffic_used ?? 0;
            $monthlyLimit = $user->monthly_traffic_limit ?? 0;
            $usedStr = $this->formatBytes($monthlyUsed);
            $limitStr = $this->formatBytes($monthlyLimit);
            $proxies[] = [
                'name' => "当月: {$usedStr} / {$limitStr}",
                'type' => 'ss',
                'server' => '127.0.0.1',
                'port' => 1,
                'cipher' => 'aes-256-gcm',
                'password' => 'info',
                'udp' => false,
            ];
        }

        // 重置时间（只有周期套餐且月数 > 1 才显示）
        if ($showReset) {
            $resetAt = $user->next_traffic_reset_at ? $user->next_traffic_reset_at->timestamp : 0;
            $resetDays = $resetAt > 0 ? max(0, ceil(($resetAt - time()) / 86400)) : 0;
            $proxies[] = [
                'name' => "重置: {$resetDays}天后",
                'type' => 'ss',
                'server' => '127.0.0.1',
                'port' => 1,
                'cipher' => 'aes-256-gcm',
                'password' => 'info',
                'udp' => false,
            ];
        }

        // 自定义显示（信息条目）
        foreach ($this->customInfoLines() as $line) {
            $proxies[] = [
                'name' => $line,
                'type' => 'ss',
                'server' => '127.0.0.1',
                'port' => 1,
                'cipher' => 'aes-256-gcm',
                'password' => 'info',
                'udp' => false,
            ];
        }

        return $proxies;
    }

    /**
     * 创建流量信息节点（Sing-box 格式）。
     */
    private function createInfoOutbounds(User $user): array
    {
        $outbounds = [];
        $showExpire = SiteConfig::getValue('sub_show_expire', '0') === '1';
        $showTraffic = SiteConfig::getValue('sub_show_traffic', '0') === '1';

        // 判断是否显示重置时间（只有周期套餐且月数 > 1 才显示）
        $showReset = false;
        if ($user->plan && $user->plan->isPeriod() && $user->plan->months > 1) {
            $showReset = true;
        }

        // 到期时间
        if ($showExpire) {
            $expire = $user->expired_at ? $user->expired_at->timestamp : 0;
            $expireStr = $expire > 0 ? date('Y-m-d', $expire) : '永久';
            $outbounds[] = [
                'type' => 'shadowsocks',
                'tag' => "到期: {$expireStr}",
                'server' => '127.0.0.1',
                'server_port' => 1,
                'method' => 'aes-256-gcm',
                'password' => 'info',
            ];
        }

        // 当月流量（只有周期套餐才显示）
        if ($showTraffic && $user->plan && $user->plan->isPeriod()) {
            $monthlyUsed = $user->monthly_traffic_used ?? 0;
            $monthlyLimit = $user->monthly_traffic_limit ?? 0;
            $usedStr = $this->formatBytes($monthlyUsed);
            $limitStr = $this->formatBytes($monthlyLimit);
            $outbounds[] = [
                'type' => 'shadowsocks',
                'tag' => "当月: {$usedStr} / {$limitStr}",
                'server' => '127.0.0.1',
                'server_port' => 1,
                'method' => 'aes-256-gcm',
                'password' => 'info',
            ];
        }

        // 重置时间（只有周期套餐且月数 > 1 才显示）
        if ($showReset) {
            $resetAt = $user->next_traffic_reset_at ? $user->next_traffic_reset_at->timestamp : 0;
            $resetDays = $resetAt > 0 ? max(0, ceil(($resetAt - time()) / 86400)) : 0;
            $outbounds[] = [
                'type' => 'shadowsocks',
                'tag' => "重置: {$resetDays}天后",
                'server' => '127.0.0.1',
                'server_port' => 1,
                'method' => 'aes-256-gcm',
                'password' => 'info',
            ];
        }

        // 自定义显示（信息条目）
        foreach ($this->customInfoLines() as $line) {
            $outbounds[] = [
                'type' => 'shadowsocks',
                'tag' => $line,
                'server' => '127.0.0.1',
                'server_port' => 1,
                'method' => 'aes-256-gcm',
                'password' => 'info',
            ];
        }

        return $outbounds;
    }

    /**
     * 格式化字节数。
     */
    private function formatBytes(int $bytes): string
    {
        if ($bytes === 0) return '0 B';

        $units = ['B', 'KB', 'MB', 'GB', 'TB'];
        $i = 0;
        $bytes = abs($bytes);

        while ($bytes >= 1024 && $i < count($units) - 1) {
            $bytes /= 1024;
            $i++;
        }

        return round($bytes, 2) . ' ' . $units[$i];
    }

    /**
     * 创建流量信息链接（Base64 格式）。
     */
    private function createInfoLinks(User $user): array
    {
        $links = [];
        $showExpire = SiteConfig::getValue('sub_show_expire', '0') === '1';
        $showTraffic = SiteConfig::getValue('sub_show_traffic', '0') === '1';

        // 判断是否显示重置时间（只有周期套餐且月数 > 1 才显示）
        $showReset = false;
        if ($user->plan && $user->plan->isPeriod() && $user->plan->months > 1) {
            $showReset = true;
        }

        // 生成 SS URI（标准格式：base64(method:password)@server:port#name）
        $userInfo = base64_encode('aes-256-gcm:info');

        // 到期时间
        if ($showExpire) {
            $expire = $user->expired_at ? $user->expired_at->timestamp : 0;
            $expireStr = $expire > 0 ? date('Y-m-d', $expire) : '永久';
            $links[] = "ss://{$userInfo}@127.0.0.1:1#" . urlencode("到期: {$expireStr}");
        }

        // 当月流量（只有周期套餐才显示）
        if ($showTraffic && $user->plan && $user->plan->isPeriod()) {
            $monthlyUsed = $user->monthly_traffic_used ?? 0;
            $monthlyLimit = $user->monthly_traffic_limit ?? 0;
            $usedStr = $this->formatBytes($monthlyUsed);
            $limitStr = $this->formatBytes($monthlyLimit);
            $links[] = "ss://{$userInfo}@127.0.0.1:1#" . urlencode("当月: {$usedStr} / {$limitStr}");
        }

        // 重置时间（只有周期套餐且月数 > 1 才显示）
        if ($showReset) {
            $resetAt = $user->next_traffic_reset_at ? $user->next_traffic_reset_at->timestamp : 0;
            $resetDays = $resetAt > 0 ? max(0, ceil(($resetAt - time()) / 86400)) : 0;
            $links[] = "ss://{$userInfo}@127.0.0.1:1#" . urlencode("重置: {$resetDays}天后");
        }

        // 自定义显示（信息条目）
        foreach ($this->customInfoLines() as $line) {
            $links[] = "ss://{$userInfo}@127.0.0.1:1#" . urlencode($line);
        }

        return $links;
    }

    /**
     * 自定义显示（信息条目）文本行。空行忽略。
     */
    private function customInfoLines(): array
    {
        if (SiteConfig::getValue('sub_custom_info_enabled', '0') !== '1') {
            return [];
        }

        $text = SiteConfig::getValue('sub_custom_info_text', '');
        $lines = preg_split('/\r\n|\r|\n/', $text);
        $result = [];
        foreach ($lines as $line) {
            $line = trim($line);
            if ($line !== '') {
                $result[] = $line;
            }
        }

        return $result;
    }

    /**
     * SNI 剥端口：`apple.com:443` → `apple.com`；纯域名 / IPv6 原样返回。
     * Clash 的 servername 不容忍端口（x509 校验会挂），Xray 系容忍。
     */
    private function cleanSni(string $sni): string
    {
        // 仅剥「单冒号 + 纯数字端口」形式；含多冒号的 IPv6 不动
        $pos = strrpos($sni, ':');
        if ($pos !== false && strpos($sni, ':') === $pos && $pos > 0 && ctype_digit(substr($sni, $pos + 1))) {
            return substr($sni, 0, $pos);
        }

        return $sni;
    }

    private function linkMatchesPort(string $link, int $port): bool
    {
        if (preg_match('#@[^/@\]]+:(\d+)#', $link, $m)) {
            return (int) $m[1] === $port;
        }

        return false;
    }

    private function linkMatchesAnyPort(string $link, array $ports): bool
    {
        if (preg_match('#@[^/@\]]+:(\d+)#', $link, $m)) {
            return in_array((int) $m[1], $ports, true);
        }

        return false;
    }

    /**
     * 校验用户可用：enabled + 未过期 + 未超量。
     */
    public function ensureUsable(User $user): void
    {
        if (!$user->enabled) {
            throw new SubscriptionException('账号已禁用', 403);
        }

        if (!$user->plan_id) {
            throw new SubscriptionException('无套餐，暂无流量', 403);
        }

        if ($user->expired_at !== null && $user->expired_at->isPast()) {
            throw new SubscriptionException('账号已过期', 403);
        }

        if ($user->traffic_limit > 0 && $user->traffic_used >= $user->traffic_limit) {
            throw new SubscriptionException('流量已耗尽', 403);
        }
    }
}
