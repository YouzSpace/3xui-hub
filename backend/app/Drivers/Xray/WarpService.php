<?php

namespace App\Drivers\Xray;

use App\Models\Node;
use App\Models\NodeWarpAccount;
use App\Models\XrayOutbound;
use Illuminate\Support\Facades\Http;

/**
 * Cloudflare WARP 集成（面板侧注册；wireguard 私钥随 config 下发节点）。
 *
 * 异机架构正好复用现有通道：注册/换 IP 是纯 HTTP（调 CF API），
 * 生成的 wireguard 出站由「面板渲染 config → 节点拉取」链路下发生效。
 *
 * 流程（对齐 3x-ui warp.go 的 CF API 口径）：
 *   register → 生成 wireguard keypair → POST /reg → 存 device/token/peer/reserved
 *   apply    → 组装 wireguard 出站（tag=warp）→ 写入 xray_outbounds → bump config
 *   rotate   → 重新注册设备（换 IP）→ 申请回原 license → 重组出站 → bump
 */
class WarpService
{
    /** CF WARP 注册 API（与 3x-ui 同源同版本头）。 */
    private const API_BASE = 'https://api.cloudflareclient.com/v0a4005';
    private const CLIENT_VER = 'a-6.30-3596';

    public function __construct(
        private readonly Node $node,
    ) {}

    /** 生成 wireguard 密钥对（标准 base64；clamp 私钥）。 */
    public static function generateWireguardKeypair(): array
    {
        $keypair = \ParagonIE_Sodium_Compat::crypto_box_keypair();
        $sk = \ParagonIE_Sodium_Compat::crypto_box_secretkey($keypair);
        $pk = \ParagonIE_Sodium_Compat::crypto_box_publickey($keypair);

        $sk[0] = chr(ord($sk[0]) & 248);
        $sk[31] = chr((ord($sk[31]) & 127) | 64);

        return [base64_encode($sk), base64_encode($pk)];
    }

    /**
     * 注册（或重新注册）WARP 设备并覆盖写入账户；原 license 自动保留并尝试回挂。
     */
    public function register(): NodeWarpAccount
    {
        $existingLicense = $this->node->warpAccount?->license_key;

        [$privateKey, $publicKey] = self::generateWireguardKeypair();

        $resp = Http::timeout(20)
            ->connectTimeout(8)
            ->withHeaders([
                'CF-Client-Version' => self::CLIENT_VER,
                'Content-Type' => 'application/json',
            ])
            ->post(self::API_BASE . '/reg', [
                'key' => $publicKey,
                'tos' => now()->utc()->format('Y-m-d\TH:i:s.000\Z'),
                'type' => 'PC',
                'model' => 'controlhub',
                'name' => mb_substr((string) ($this->node->name ?: 'controlhub'), 0, 64),
            ]);

        if ($resp->failed()) {
            throw new \RuntimeException('WARP 注册失败：CF API 返回 ' . $resp->status());
        }

        $json = $resp->json();
        $deviceId = (string) ($json['id'] ?? '');
        $token = (string) ($json['token'] ?? '');
        if ($deviceId === '' || $token === '') {
            throw new \RuntimeException('WARP 注册失败：响应缺少 device id / token');
        }

        $config = is_array($json['config'] ?? null) ? $json['config'] : [];
        $peer = is_array($config['peers'][0] ?? null) ? $config['peers'][0] : [];
        $iface = is_array($config['interface'] ?? null) ? $config['interface'] : [];
        $addrs = is_array($iface['addresses'] ?? null) ? $iface['addresses'] : [];

        $addresses = [];
        if (! empty($addrs['v4'])) {
            $addresses[] = $addrs['v4'] . '/32';
        }
        if (! empty($addrs['v6'])) {
            $addresses[] = $addrs['v6'] . '/128';
        }

        $clientId = (string) ($config['client_id'] ?? '');
        $reserved = [];
        if ($clientId !== '') {
            $decoded = base64_decode($clientId, true);
            if (is_string($decoded)) {
                foreach (str_split($decoded) as $byte) {
                    $reserved[] = ord($byte);
                }
            }
        }

        $account = $this->node->warpAccount ?? new NodeWarpAccount(['node_id' => $this->node->id]);
        $account->device_id = $deviceId;
        $account->setAccessToken($token);
        $account->setPrivateKey($privateKey);
        $account->public_key = $publicKey;
        $account->peer_public_key = (string) ($peer['public_key'] ?? '');
        $account->peer_endpoint = (string) ($peer['endpoint']['host'] ?? '');
        $account->addresses = $addresses;
        $account->reserved = $reserved;
        $account->client_id = $clientId;
        $account->license_key = (string) ($json['account']['license'] ?? '') !== ''
            ? (string) $json['account']['license']
            : $existingLicense;

        // 有原 license（WARP+）则回挂新设备（失败不阻断注册）
        if ($existingLicense !== null && strlen($existingLicense) >= 26) {
            try {
                $this->setLicense($existingLicense, $account);
            } catch (\Throwable) {
                // 忽略：免费账户仍可用
            }
        }

        $account->save();

        return $account->fresh();
    }

    /** 应用/更新到节点：组装 wireguard 出站写入 xray_outbounds 并 bump 版本。 */
    public function apply(): XrayOutbound
    {
        $account = $this->node->warpAccount;
        if ($account === null) {
            throw new \RuntimeException('尚未注册 WARP，请先注册');
        }
        $privateKey = $account->privateKey();
        if ($privateKey === null || $privateKey === '') {
            throw new \RuntimeException('WARP 账户缺少私钥，请重新注册');
        }

        $settings = array_filter([
            'address' => array_values((array) ($account->addresses ?? [])),
            'peers' => [[
                'publicKey' => (string) $account->peer_public_key,
                'endpoint' => (string) $account->peer_endpoint,
            ]],
            'reserved' => array_values((array) ($account->reserved ?? [])),
            'mtu' => 1280,
        ], fn ($v) => $v !== [] && $v !== [''] && $v !== null);

        $outbound = $this->node->xrayOutbounds()->where('tag', 'warp')->first();
        if ($outbound === null) {
            $outbound = new XrayOutbound([
                'node_id' => $this->node->id,
                'tag' => 'warp',
            ]);
        }
        $outbound->protocol = 'wireguard';
        $outbound->settings = $settings;
        $outbound->setSecretKey($privateKey);
        $outbound->enabled = true;
        $outbound->remark = 'Cloudflare WARP';
        $outbound->save();

        $account->enabled = true;
        $account->save();

        $this->node->bumpXrayConfigVersion();

        return $outbound->fresh();
    }

    /** 换 IP：重新注册设备（得到新出口），已应用则同步重组出站。 */
    public function rotate(): NodeWarpAccount
    {
        $wasEnabled = (bool) ($this->node->warpAccount?->enabled);

        $account = $this->register();

        if ($wasEnabled) {
            $this->apply();
        }

        $account->last_rotate_at = now();
        $account->save();

        return $account->fresh();
    }

    /** 设置 license（WARP+；PUT /reg/{device}/account）。 */
    public function setLicense(string $license, ?NodeWarpAccount $account = null): void
    {
        $account = $account ?? $this->node->warpAccount;
        if ($account === null) {
            throw new \RuntimeException('尚未注册 WARP，请先注册');
        }

        $resp = Http::timeout(20)
            ->connectTimeout(8)
            ->withHeaders([
                'Authorization' => 'Bearer ' . (string) $account->accessToken(),
                'Content-Type' => 'application/json',
            ])
            ->put(self::API_BASE . '/reg/' . $account->device_id . '/account', [
                'license' => $license,
            ]);

        if ($resp->failed() || ! is_string($resp->json('id') ?? null)) {
            throw new \RuntimeException('license 设置失败：CF API 返回 ' . $resp->status());
        }

        $account->license_key = $license;
        $account->save();
    }

    /** 删除 WARP：清账户 + 移除 warp 出站 + bump。 */
    public function remove(): void
    {
        $this->node->warpAccount?->delete();

        $outbound = $this->node->xrayOutbounds()->where('tag', 'warp')->first();
        if ($outbound !== null) {
            $outbound->delete();
            $this->node->bumpXrayConfigVersion();
        }
    }
}
