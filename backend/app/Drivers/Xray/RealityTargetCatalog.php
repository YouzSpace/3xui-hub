<?php

namespace App\Drivers\Xray;

/**
 * Reality 目标候选清单（面板侧维护，agent 只当「哑探测器」）。
 *
 * 候选目录放在面板侧的好处：调整候选域名不需要重新给节点发 agent 二进制。
 * 清单口径参考 3x-ui 的内置候选——都是同时满足 TLS 1.3 + h2 + 证书链受信任的大厂站点。
 */
class RealityTargetCatalog
{
    /** 单次扫描候选上限（与 agent 侧 realityScanMaxTargets 一致，双保险）。 */
    public const MAX_TARGETS = 20;

    /**
     * 内置常用候选（留空即用它）。
     *
     * @return array<int, string>
     */
    public static function defaults(): array
    {
        return [
            'www.cloudflare.com:443',
            'www.samsung.com:443',
            'www.nvidia.com:443',
            'www.apple.com:443',
            'aws.amazon.com:443',
            'www.microsoft.com:443',
            'www.icloud.com:443',
            'www.tesla.com:443',
            'dl.google.com:443',
            'www.yahoo.com:443',
            'www.bing.com:443',
            'www.amazon.com:443',
        ];
    }

    /**
     * 规整管理员输入的目标（去空白、去重、截上限）；空入参返回空数组（由调用方决定是否回落内置候选）。
     *
     * @param  array<int|string, mixed>  $targets
     * @return array<int, string>
     */
    public static function sanitize(array $targets): array
    {
        $out = [];
        foreach ($targets as $raw) {
            $target = trim((string) $raw);
            if ($target === '' || in_array($target, $out, true)) {
                continue;
            }
            $out[] = $target;
            if (count($out) >= self::MAX_TARGETS) {
                break;
            }
        }

        return $out;
    }
}
