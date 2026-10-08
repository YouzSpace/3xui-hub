<?php

namespace App\Drivers\Xray;

/**
 * 出站分享链接解析（vless / vmess / trojan / ss / socks / http → xray outbound 结构）。
 *
 * 对齐 Sub-Store / 3x-ui 的 URI 口径（我们订阅生成是同一套字段的逆过程）。
 */
class OutboundLinkParser
{
    /**
     * @return array{protocol:string, settings:array, stream_settings:array, tag:string}
     */
    public static function parse(string $link): array
    {
        $link = trim($link);
        $scheme = strtolower((string) parse_url($link, PHP_URL_SCHEME));

        return match ($scheme) {
            'vmess' => self::parseVmess($link),
            'vless' => self::parseVless($link),
            'trojan' => self::parseTrojan($link),
            'ss' => self::parseShadowsocks($link),
            'socks', 'socks5' => self::parseSocksLike($link),
            'http', 'https' => self::parseHttp($link),
            default => throw new \InvalidArgumentException('不支持的链接协议：' . ($scheme ?: '（空）')),
        };
    }

    private static function parseVmess(string $link): array
    {
        $b64 = substr($link, strlen('vmess://'));
        $json = self::b64decodeLoose($b64);
        $d = json_decode((string) $json, true);
        if (! is_array($d) || empty($d['add']) || empty($d['port']) || empty($d['id'])) {
            throw new \InvalidArgumentException('vmess 链接内容不完整');
        }

        $net = (string) ($d['net'] ?? 'tcp');
        $tls = strtolower((string) ($d['tls'] ?? '')) === 'tls';

        $stream = ['network' => $net, 'security' => $tls ? 'tls' : 'none'];
        if ($net === 'ws') {
            $ws = ['path' => (string) ($d['path'] ?? '/')];
            if (! empty($d['host'])) {
                $ws['headers'] = ['Host' => (string) $d['host']];
            }
            $stream['wsSettings'] = $ws;
        }
        if ($tls) {
            $tlsSettings = [];
            $sni = (string) ($d['sni'] ?? $d['host'] ?? '');
            if ($sni !== '') {
                $tlsSettings['serverName'] = $sni;
            }
            $stream['tlsSettings'] = $tlsSettings;
        }

        return [
            'protocol' => 'vmess',
            'settings' => [
                'vnext' => [[
                    'address' => (string) $d['add'],
                    'port' => (int) $d['port'],
                    'users' => [[
                        'id' => (string) $d['id'],
                        'alterId' => (int) ($d['aid'] ?? 0),
                        'security' => (string) ($d['scy'] ?? 'auto'),
                    ]],
                ]],
            ],
            'stream_settings' => $stream,
            'tag' => (string) ($d['ps'] ?? $d['add']),
        ];
    }

    private static function parseVless(string $link): array
    {
        $parts = parse_url($link);
        if ($parts === false || empty($parts['host']) || empty($parts['user'])) {
            throw new \InvalidArgumentException('vless 链接解析失败');
        }
        $query = [];
        parse_str((string) ($parts['query'] ?? ''), $query);

        $security = (string) ($query['security'] ?? 'none');
        $network = (string) ($query['type'] ?? 'tcp');

        $stream = ['network' => $network, 'security' => $security];
        if ($security === 'reality') {
            $stream['realitySettings'] = array_filter([
                'serverName' => (string) ($query['sni'] ?? ''),
                'publicKey' => (string) ($query['pbk'] ?? ''),
                'shortId' => (string) ($query['sid'] ?? ''),
                'fingerprint' => (string) ($query['fp'] ?? ''),
            ], fn ($v) => $v !== '');
        } elseif ($security === 'tls') {
            $stream['tlsSettings'] = array_filter([
                'serverName' => (string) ($query['sni'] ?? $parts['host']),
            ], fn ($v) => $v !== '');
        }
        if ($network === 'ws') {
            $ws = ['path' => (string) ($query['path'] ?? '/')];
            if (! empty($query['host'])) {
                $ws['headers'] = ['Host' => (string) $query['host']];
            }
            $stream['wsSettings'] = $ws;
        }

        return [
            'protocol' => 'vless',
            'settings' => [
                'vnext' => [[
                    'address' => (string) $parts['host'],
                    'port' => (int) ($parts['port'] ?? 443),
                    'users' => [array_filter([
                        'id' => rawurldecode((string) $parts['user']),
                        'encryption' => 'none',
                        'flow' => (string) ($query['flow'] ?? ''),
                    ], fn ($v) => $v !== '')],
                ]],
            ],
            'stream_settings' => $stream,
            'tag' => self::fragment($link) ?: (string) $parts['host'],
        ];
    }

    private static function parseTrojan(string $link): array
    {
        $parts = parse_url($link);
        if ($parts === false || empty($parts['host']) || empty($parts['pass'])) {
            throw new \InvalidArgumentException('trojan 链接解析失败');
        }
        $query = [];
        parse_str((string) ($parts['query'] ?? ''), $query);

        $security = (string) ($query['security'] ?? 'tls');
        $network = (string) ($query['type'] ?? 'tcp');

        $stream = ['network' => $network, 'security' => $security];
        if ($security === 'tls') {
            $stream['tlsSettings'] = array_filter([
                'serverName' => (string) ($query['sni'] ?? $parts['host']),
            ], fn ($v) => $v !== '');
        }
        if ($network === 'ws') {
            $stream['wsSettings'] = ['path' => (string) ($query['path'] ?? '/')];
        }

        return [
            'protocol' => 'trojan',
            'settings' => [
                'servers' => [[
                    'address' => (string) $parts['host'],
                    'port' => (int) ($parts['port'] ?? 443),
                    'password' => rawurldecode((string) $parts['pass']),
                ]],
            ],
            'stream_settings' => $stream,
            'tag' => self::fragment($link) ?: (string) $parts['host'],
        ];
    }

    private static function parseShadowsocks(string $link): array
    {
        $rest = substr($link, strlen('ss://'));
        $fragment = self::fragment($link);
        $rest = explode('#', $rest, 2)[0];

        $method = '';
        $password = '';
        $host = '';
        $port = 0;

        if (str_contains($rest, '@')) {
            // SIP002: ss://base64(method:password)@host:port
            [$userinfo, $hostport] = explode('@', $rest, 2);
            $decoded = self::b64decodeLoose($userinfo);
            if ($decoded === false || ! str_contains((string) $decoded, ':')) {
                throw new \InvalidArgumentException('ss 链接 userinfo 解析失败');
            }
            [$method, $password] = explode(':', (string) $decoded, 2);
            $clean = explode('?', $hostport, 2)[0]; // 丢弃 plugin 等 query
            $parsed = parse_url('scheme://' . $clean);
            $host = (string) ($parsed['host'] ?? '');
            $port = (int) ($parsed['port'] ?? 0);
        } else {
            // 旧格式：ss://base64(method:password@host:port)
            $decoded = self::b64decodeLoose($rest);
            if ($decoded === false) {
                throw new \InvalidArgumentException('ss 链接解析失败');
            }
            if (! preg_match('/^(.+?):(.+)@(.+):(\d+)$/', (string) $decoded, $m)) {
                throw new \InvalidArgumentException('ss 链接内容不完整');
            }
            [, $method, $password, $host, $port] = $m;
            $port = (int) $port;
        }

        if ($host === '' || $port <= 0 || $method === '') {
            throw new \InvalidArgumentException('ss 链接内容不完整');
        }

        return [
            'protocol' => 'shadowsocks',
            'settings' => [
                'servers' => [[
                    'address' => $host,
                    'port' => $port,
                    'method' => $method,
                    'password' => $password,
                ]],
            ],
            'stream_settings' => [],
            'tag' => $fragment !== '' ? $fragment : $host,
        ];
    }

    private static function parseSocksLike(string $link): array
    {
        $parts = parse_url($link);
        if ($parts === false || empty($parts['host'])) {
            throw new \InvalidArgumentException('socks 链接解析失败');
        }

        return [
            'protocol' => 'socks',
            'settings' => [
                'servers' => [[
                    'address' => (string) $parts['host'],
                    'port' => (int) ($parts['port'] ?? 1080),
                    'users' => [[
                        'user' => rawurldecode((string) ($parts['user'] ?? '')),
                        'pass' => rawurldecode((string) ($parts['pass'] ?? '')),
                    ]],
                ]],
            ],
            'stream_settings' => [],
            'tag' => self::fragment($link) ?: (string) $parts['host'],
        ];
    }

    private static function parseHttp(string $link): array
    {
        $parts = parse_url($link);
        if ($parts === false || empty($parts['host'])) {
            throw new \InvalidArgumentException('http 链接解析失败');
        }

        return [
            'protocol' => 'http',
            'settings' => [
                'servers' => [[
                    'address' => (string) $parts['host'],
                    'port' => (int) ($parts['port'] ?? 8080),
                    'users' => [[
                        'user' => rawurldecode((string) ($parts['user'] ?? '')),
                        'pass' => rawurldecode((string) ($parts['pass'] ?? '')),
                    ]],
                ]],
            ],
            'stream_settings' => [],
            'tag' => self::fragment($link) ?: (string) $parts['host'],
        ];
    }

    /** URL fragment（节点名），兼容 URL-safe 与百分号编码。 */
    private static function fragment(string $link): string
    {
        $frag = parse_url($link, PHP_URL_FRAGMENT);
        if (! is_string($frag) || $frag === '') {
            return '';
        }

        return rawurldecode($frag);
    }

    /** 宽容 base64 解码（标准/URL-safe、有无填充都收）。 */
    private static function b64decodeLoose(string $input): string|false
    {
        $input = trim($input);
        $normalized = strtr($input, '-_', '+/');
        $pad = strlen($normalized) % 4;
        if ($pad > 0) {
            $normalized .= str_repeat('=', 4 - $pad);
        }

        return base64_decode($normalized, true);
    }
}
