<?php

namespace App\Services;

use App\Models\Plan;
use App\Models\ThirdPartySub;
use App\Models\User;
use GuzzleHttp\Client;
use GuzzleHttp\RequestOptions;

/**
 * 第三方订阅（外部机场）接入。
 *
 * 职责：
 *  1. 拉取：定时从机场订阅 URL 拉取节点链接（明文 / 整段 base64），解析后缓存到
 *     third_party_subs.cached_links。拉取失败保留上次成功缓存，页面标红原因。
 *  2. 保密改名：第三方节点注入用户订阅前，把机场原名（常带机场品牌词）替换成
 *     中性的「地区+序号」（香港1、日本2…），用户端零机场痕迹。
 *  3. 组装：按套餐勾选的 third_party_sub_ids + 用户第三方有效期，返回该用户要注入的链接。
 *
 * 规则（产品铁律）：第三方「不管流量、只管时间」。
 *  - 用户第三方未到期 且 套餐勾选了该订阅 → 注入
 *  - 到期 / 未开通 / 总开关关 / 订阅停用 → 不注入；客户端里已导入的旧节点不处理。
 */
class ThirdPartyService
{
    /** 拉取超时（秒）。机场在境外，跨境 RTT 可能较高，给足。 */
    private const FETCH_TIMEOUT = 15;
    private const CONNECT_TIMEOUT = 6;
    private const MAX_NODES = 400; // 单订阅缓存上限，防异常响应撑爆

    /**
     * 第三方节点在用户端的中性地区词表（用于识别机场节点名里的地区）。
     * 命中 → 「地区+序号」；不命中 → 「线路+序号」（绝不带机场原名，保证保密）。
     */
    private const REGION_LABELS = [
        ['label' => '香港',   'keys' => ['香港', 'HK', 'HONG KONG', 'HKG']],
        ['label' => '台湾',   'keys' => ['台湾', 'TW', 'TAIWAN', 'TWN']],
        ['label' => '日本',   'keys' => ['日本', 'JP', 'JAPAN', 'JPN']],
        ['label' => '韩国',   'keys' => ['韩国', 'KR', 'KOREA', 'KOR']],
        ['label' => '新加坡', 'keys' => ['新加坡', 'SG', 'SINGAPORE', 'SGP']],
        ['label' => '美国',   'keys' => ['美国', 'US', 'USA', 'AMERICA']],
        ['label' => '英国',   'keys' => ['英国', 'UK', 'BRITAIN', 'GREAT BRITAIN']],
        ['label' => '德国',   'keys' => ['德国', 'DE', 'GERMANY', 'DEU']],
        ['label' => '法国',   'keys' => ['法国', 'FR', 'FRANCE', 'FRA']],
        ['label' => '加拿大', 'keys' => ['加拿大', 'CA', 'CANADA', 'CAN']],
        ['label' => '澳大利亚', 'keys' => ['澳大利亚', '澳洲', 'AU', 'AUSTRALIA', 'AUS']],
        ['label' => '泰国',   'keys' => ['泰国', 'TH', 'THAILAND', 'THA']],
        ['label' => '马来西亚', 'keys' => ['马来西亚', 'MY', 'MALAYSIA', 'MYS']],
        ['label' => '越南',   'keys' => ['越南', 'VN', 'VIETNAM', 'VNM']],
        ['label' => '俄罗斯', 'keys' => ['俄罗斯', 'RU', 'RUSSIA', 'RUS']],
        ['label' => '土耳其', 'keys' => ['土耳其', 'TR', 'TURKEY', 'TUR']],
        ['label' => '印度',   'keys' => ['印度', 'IN', 'INDIA', 'IND']],
    ];

    /**
     * 拉取某条第三方订阅：抓取 → 解析 → 存缓存。
     * 成功：刷新 cached_links / node_count / last_status=success。
     * 失败：仅记 last_status=failed + last_error，保留上次成功缓存（不清空）。
     */
    public function fetchSub(ThirdPartySub $sub): void
    {
        try {
            $links = $this->fetchAndParse($sub->url);
            if (empty($links)) {
                throw new \RuntimeException('拉取成功但未解析到任何节点链接');
            }

            $sub->forceFill([
                'cached_links' => implode("\n", $links),
                'node_count' => count($links),
                'last_status' => 'success',
                'last_fetched_at' => now(),
                'last_error' => null,
            ])->save();
        } catch (\Throwable $e) {
            $sub->forceFill([
                'last_status' => 'failed',
                'last_fetched_at' => now(),
                'last_error' => mb_substr($e->getMessage(), 0, 250),
            ])->save();
        }
    }

    /** 拉取所有启用的订阅（容错：单条失败不影响其它）。 */
    public function fetchAllEnabled(): void
    {
        ThirdPartySub::where('enabled', true)
            ->orderBy('id')
            ->each(function (ThirdPartySub $sub) {
                $this->fetchSub($sub);
            });
    }

    /**
     * 拉取 URL 并解析成节点链接数组（明文逐行 / 整段 base64）。
     * 机场对 UA 不敏感，返回多为 base64 或明文；两种都兼容。
     *
     * @return string[]
     */
    private function fetchAndParse(string $url): array
    {
        // SSL 证书校验：默认走 PHP/curl 系统 CA（绝大多数 Linux 生产服务器自带，行为不变）。
        // 缺系统 CA 的部署（Windows 开发机 / 精简镜像）拉机场 https 会报 cURL error 60，
        // 此时在 config/panel.php 或 env PANEL_THIRD_PARTY_CA_BUNDLE 指一个 CA bundle，
        // 指向的文件存在则用它校验，不存在则自动回退系统校验。
        $verify = true;
        $caBundle = (string) config('panel.third_party_ca_bundle', '');
        if ($caBundle !== '' && is_file($caBundle)) {
            $verify = $caBundle;
        }

        $client = new Client([
            RequestOptions::TIMEOUT => self::FETCH_TIMEOUT,
            RequestOptions::CONNECT_TIMEOUT => self::CONNECT_TIMEOUT,
            RequestOptions::ALLOW_REDIRECTS => true,
            RequestOptions::VERIFY => $verify,
            RequestOptions::HEADERS => [
                'User-Agent' => 'Mozilla/5.0 (compatible; ControlHub/1.0)',
                'Accept' => '*/*',
            ],
        ]);

        $resp = $client->get($url);
        $body = (string) $resp->getBody();
        $body = trim($body);
        if ($body === '') {
            throw new \RuntimeException('拉取内容为空');
        }

        // 整段 base64 判定：含 base64 特征且解码后出现节点链接
        $decoded = $this->tryBase64Decode($body);
        $content = ($decoded !== null && $this->looksLikeLinkList($decoded)) ? $decoded : $body;

        $links = $this->extractLinks($content);

        return array_slice($links, 0, self::MAX_NODES);
    }

    /** 从多行文本中提取代理节点链接（ss/vless/vmess/trojan/hysteria2/hysteria/tuic）。 */
    private function extractLinks(string $content): array
    {
        $lines = preg_split('/\r\n|\r|\n/', $content);
        $links = [];
        foreach ($lines as $line) {
            $line = trim($line);
            if ($line === '') {
                continue;
            }
            if (preg_match('/^(ss|vless|vmess|trojan|hysteria2|hysteria|tuic):\/\//', $line)) {
                $links[] = $line;
            }
        }

        return $links;
    }

    /** base64 宽松解码（允许 URL-safe、缺 padding）。失败返回 null。 */
    private function tryBase64Decode(string $value): ?string
    {
        $value = str_replace(["\r", "\n", ' '], '', $value);
        $value = strtr($value, '-_', '+/');
        $pad = strlen($value) % 4;
        if ($pad === 1) {
            return null;
        }
        if ($pad > 0) {
            $value .= str_repeat('=', 4 - $pad);
        }
        if (!preg_match('/^[A-Za-z0-9+\/=]+$/', $value)) {
            return null;
        }

        $decoded = base64_decode($value, true);

        return $decoded === false ? null : $decoded;
    }

    /** 判断字符串是否像「多行节点链接清单」。 */
    private function looksLikeLinkList(string $s): bool
    {
        return (bool) preg_match('/^ss:\/\/|^vless:\/\/|^vmess:\/\/|^trojan:\/\/|^hysteria2:\/\//mi', $s);
    }

    /**
     * 某用户要注入的第三方节点链接（已过滤 + 保密改名）。
     *
     * 单一来源：套餐勾选的 third_party_sub_ids —— 账号有效期内自动包含（到期随账号一起没）。
     * 开与不开由「套餐配置 + 总开关」控制，用户层面不做单独控制。
     *
     * 返回的每条是原机场链接但 #fragment 名字已替换成「地区+序号」：
     * base64 格式原样透传；Clash / Sing-box 交给解析器把 #fragment 当节点名。
     *
     * @return string[]
     */
    public function linksForUser(User $user): array
    {
        // 总开关（SiteConfig：默认 0=关闭，一键从所有订阅摘掉第三方）
        if (\App\Models\SiteConfig::getValue('third_party_enabled', '0') !== '1') {
            return [];
        }

        $ids = [];

        // 唯一来源：套餐勾选（用户买了带第三方的套餐才注入；无套餐 → 无）
        if ($user->plan) {
            $ids = $user->plan->thirdPartySubIdList();
        }

        if (empty($ids)) {
            return [];
        }

        $subs = ThirdPartySub::where('enabled', true)
            ->whereIn('id', $ids)
            ->orderBy('id')
            ->get();

        $regionCounters = [];
        $links = [];
        foreach ($subs as $sub) {
            foreach ($sub->getCachedLinks() as $rawLink) {
                $links[] = $this->renameToNeutral($rawLink, $regionCounters);
            }
        }

        // 去重（同地区序号唯一，正常不会重，兜底）
        $seen = [];
        $out = [];
        foreach ($links as $l) {
            if (!isset($seen[$l])) {
                $seen[$l] = true;
                $out[] = $l;
            }
        }

        return $out;
    }

    /**
     * 把第三方链接的 #fragment（机场原名）替换成中性「地区+序号」。
     * 无 #fragment 的链接（极少）补一个。绝不保留机场品牌词。
     */
    private function renameToNeutral(string $link, array &$regionCounters): string
    {
        $hashPos = strrpos($link, '#');
        $rawName = ($hashPos !== false) ? urldecode(substr($link, $hashPos + 1)) : '';
        $neutral = $this->neutralName($rawName, $regionCounters);

        if ($hashPos === false) {
            return $link . '#' . urlencode($neutral);
        }

        return substr($link, 0, $hashPos) . '#' . urlencode($neutral);
    }

    /** 由机场节点名生成中性名「地区+序号」。 */
    private function neutralName(string $rawName, array &$counters): string
    {
        $upper = strtoupper($rawName);
        $label = '线路'; // 兜底：识别不到地区也不带机场名

        foreach (self::REGION_LABELS as $region) {
            foreach ($region['keys'] as $key) {
                if (mb_strpos($upper, strtoupper($key)) !== false) {
                    $label = $region['label'];
                    break 2;
                }
            }
        }

        if (!isset($counters[$label])) {
            $counters[$label] = 0;
        }
        $counters[$label]++;

        return $label . $counters[$label]; // 如「香港1」「日本2」「线路3」
    }
}
