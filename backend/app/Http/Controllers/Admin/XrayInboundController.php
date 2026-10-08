<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\RealityKeyService;
use App\Drivers\Xray\XrayConfigService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\User;
use App\Models\XrayInbound;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Collection;
use Illuminate\Validation\ValidationException;

/**
 * Admin xray 节点入站管理（/admin-api/nodes/{node}/xray-inbounds）。
 *
 * 入站协议面板自建：vless / vmess / trojan / shadowsocks
 * （含 tcp/ws 传输、none/tls/reality 安全层；reality 仅 vless）。
 * Reality 密钥面板生成（X25519，与 xray x25519 对拍兼容）。
 *
 * 任何变更后 bump 该节点 config_version → WS 服务推送 config.changed → 节点秒级拉取。
 */
class XrayInboundController extends Controller
{
    use ApiResponse;

    public function __construct(
        private readonly XrayConfigService $config,
    ) {}

    /** GET /admin-api/nodes/{node}/xray-inbounds */
    public function index(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $inbounds = $node->xrayInbounds()->orderBy('sort')->orderBy('id')->get();
        $users = $this->config->eligibleUsers();

        return $this->success([
            'inbounds' => $inbounds->map(fn (XrayInbound $in) => $this->present($in, $users))->values()->all(),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** POST /admin-api/nodes/{node}/xray-inbounds */
    public function store(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);
        $data = $this->validatePayload($request);

        $inbound = new XrayInbound();
        $inbound->node_id = $node->id;
        $this->fillFromPayload($inbound, $data, $node, null);
        $inbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'inbound' => $this->present($inbound->fresh(), $this->config->eligibleUsers()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-inbounds/{inbound} */
    public function update(Request $request, Node $node, XrayInbound $inbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $inbound);

        $data = $this->validatePayload($request, $inbound);
        $this->fillFromPayload($inbound, $data, $node, $inbound);
        $inbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'inbound' => $this->present($inbound->fresh(), $this->config->eligibleUsers()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-inbounds/{inbound} */
    public function destroy(Node $node, XrayInbound $inbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $inbound);

        $inbound->delete();
        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-inbounds/reality-keypair
     * 生成一组 Reality 密钥材料（新建/轮换表单预填；与 xray x25519 格式一致）。
     */
    public function realityKeypair(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        [$privateKey, $publicKey] = RealityKeyService::generateKeypair();

        return $this->success([
            'private_key' => $privateKey,
            'public_key' => $publicKey,
            'short_id' => RealityKeyService::shortId(),
            'dest' => XrayConfigService::DEFAULT_DEST,
            'server_name' => XrayConfigService::DEFAULT_SNI,
        ]);
    }

    // ── 内部 ─────────────────────────────────────────────

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function ensureOwned(Node $node, XrayInbound $inbound): void
    {
        if ((int) $inbound->node_id !== (int) $node->id) {
            abort(404);
        }
    }

    private function validatePayload(Request $request, ?XrayInbound $existing = null): array
    {
        return $request->validate([
            'protocol' => ['required', 'in:vless,vmess,trojan,shadowsocks'],
            'tag' => ['nullable', 'string', 'max:64', 'regex:/^[A-Za-z0-9_-]+$/'],
            'port' => ['required', 'integer', 'between:1,65535'],
            'listen' => ['nullable', 'string', 'max:64'],
            'network' => ['nullable', 'in:tcp,ws,grpc'],
            'security' => ['nullable', 'in:none,tls,reality'],
            // reality 参数
            'dest' => ['nullable', 'string', 'max:255'],
            'server_names' => ['nullable', 'array', 'max:8'],
            'server_names.*' => ['string', 'max:255'],
            'short_ids' => ['nullable', 'array', 'max:8'],
            'short_ids.*' => ['string', 'max:32'],
            'private_key' => ['nullable', 'string', 'max:255'],
            'public_key' => ['nullable', 'string', 'max:255'],
            // ws / tls 参数
            'ws_path' => ['nullable', 'string', 'max:255'],
            'ws_host' => ['nullable', 'string', 'max:255'],
            'tls_server_name' => ['nullable', 'string', 'max:255'],
            'tls_certificate' => ['nullable', 'string', 'max:65535'],
            'tls_key' => ['nullable', 'string', 'max:65535'],
            // shadowsocks
            'ss_method' => ['nullable', 'in:aes-256-gcm,aes-128-gcm,chacha20-poly1305,xchacha20-poly1305,2022-blake3-aes-256-gcm,2022-blake3-aes-128-gcm'],
            'ss_password' => ['nullable', 'string', 'max:128'],
            // 开关
            'enabled' => ['nullable', 'boolean'],
            'sort' => ['nullable', 'integer', 'min:0', 'max:65535'],
        ]);
    }

    /** 表单载荷 → 入站模型（settings / stream_settings / 密钥）。 */
    private function fillFromPayload(XrayInbound $inbound, array $data, Node $node, ?XrayInbound $existing): void
    {
        $protocol = $data['protocol'];
        $network = $data['network'] ?? 'tcp';
        $security = $data['security'] ?? 'none';

        // 组合合法性（前后端同守）
        if ($security === 'reality' && $protocol !== 'vless') {
            throw ValidationException::withMessages(['security' => 'Reality 仅支持 VLESS 协议']);
        }
        if ($protocol === 'shadowsocks' && ($security !== 'none' || $network !== 'tcp')) {
            throw ValidationException::withMessages(['protocol' => 'Shadowsocks 仅支持 tcp + 无附加安全层']);
        }

        // tag：缺省自动建议；唯一性预检（跨节点内唯一）
        $tag = (string) ($data['tag'] ?? '');
        if ($tag === '') {
            $tag = $this->suggestTag($node, $protocol, $security, $network, $existing?->id);
        }
        $dup = $node->xrayInbounds()->where('tag', $tag)
            ->when($existing, fn ($q) => $q->where('id', '!=', $existing->id))
            ->exists();
        if ($dup) {
            throw ValidationException::withMessages(['tag' => "标识 {$tag} 已存在"]);
        }

        // 端口冲突：同节点启用入站不可复用端口（xray 会启动失败）
        $portConflict = $node->xrayInbounds()
            ->where('port', (int) $data['port'])
            ->where('enabled', true)
            ->when($existing, fn ($q) => $q->where('id', '!=', $existing->id))
            ->exists();
        $willEnable = array_key_exists('enabled', $data) ? (bool) $data['enabled'] : ($existing?->enabled ?? true);
        if ($willEnable && $portConflict) {
            throw ValidationException::withMessages(['port' => '端口已被该节点其他启用入站占用']);
        }

        // settings
        $settings = match ($protocol) {
            'vless' => ['decryption' => 'none'],
            'shadowsocks' => array_filter([
                'method' => $data['ss_method'] ?? 'aes-256-gcm',
                'password' => $data['ss_password'] ?? null,
            ], fn ($v) => $v !== null),
            default => [],
        };
        if ($protocol === 'shadowsocks' && empty($settings['password'])) {
            throw ValidationException::withMessages(['ss_password' => 'Shadowsocks 必须设置密码']);
        }

        // stream_settings
        $stream = ['network' => $network, 'security' => $security];

        if ($security === 'reality') {
            $privateKey = (string) ($data['private_key'] ?? '');
            $publicKey = (string) ($data['public_key'] ?? '');

            if ($privateKey === '') {
                // 新建且未提供材料 → 面板自动生成完整一套
                [$privateKey, $publicKey] = RealityKeyService::generateKeypair();
            }
            if ($publicKey === '') {
                $publicKey = $existing !== null
                    ? (string) (($existing->stream_settings['realitySettings']['publicKey'] ?? ''))
                    : '';
            }

            $stream['realitySettings'] = [
                'dest' => (string) ($data['dest'] ?? '') !== '' ? (string) $data['dest'] : XrayConfigService::DEFAULT_DEST,
                'serverNames' => $this->stringList($data['server_names'] ?? [], XrayConfigService::DEFAULT_SNI),
                'shortIds' => $this->stringList($data['short_ids'] ?? [], null) ?: [RealityKeyService::shortId()],
                'publicKey' => $publicKey,
            ];
            $inbound->setRealityPrivateKey($privateKey);
        } else {
            $inbound->setRealityPrivateKey(null);
        }

        if ($network === 'ws') {
            $ws = array_filter([
                'path' => (string) ($data['ws_path'] ?? '') !== '' ? (string) $data['ws_path'] : '/',
                'host' => (string) ($data['ws_host'] ?? ''),
            ], fn ($v) => $v !== '');
            $stream['wsSettings'] = $ws;
        }

        if ($security === 'tls') {
            $tls = [];
            if (! empty($data['tls_server_name'])) {
                $tls['serverName'] = (string) $data['tls_server_name'];
            }
            if (! empty($data['tls_certificate']) && ! empty($data['tls_key'])) {
                $tls['certificates'] = [[
                    'certificate' => [(string) $data['tls_certificate']],
                    'key' => [(string) $data['tls_key']],
                ]];
            }
            $stream['tlsSettings'] = $tls;
        }

        $inbound->tag = $tag;
        $inbound->protocol = $protocol;
        $inbound->port = (int) $data['port'];
        $inbound->listen = (string) ($data['listen'] ?? '') !== '' ? (string) $data['listen'] : '0.0.0.0';
        $inbound->settings = $settings;
        $inbound->stream_settings = $stream;
        $inbound->sniffing = ['enabled' => true, 'destOverride' => ['http', 'tls', 'quic']];
        if (array_key_exists('enabled', $data)) {
            $inbound->enabled = (bool) $data['enabled'];
        }
        if (array_key_exists('sort', $data)) {
            $inbound->sort = (int) $data['sort'];
        }
    }

    /** 非空字符串列表（空则用默认单元素）。 */
    private function stringList(array $values, ?string $default): array
    {
        $out = array_values(array_filter(array_map('strval', $values), fn ($v) => trim($v) !== ''));
        if ($out === [] && $default !== null) {
            $out = [$default];
        }

        return $out;
    }

    /** 建议 tag（协议-安全-传输，冲突自动加序号）。 */
    private function suggestTag(Node $node, string $protocol, string $security, string $network, ?int $excludeId): string
    {
        $base = $protocol;
        if ($security === 'reality') {
            $base .= '-reality';
        } elseif ($security === 'tls' && $network !== 'tcp') {
            $base .= '-ws-tls';
        } elseif ($network !== 'tcp') {
            $base .= '-' . $network;
        }
        $base = preg_replace('/[^A-Za-z0-9_-]/', '', $base) ?: 'inbound';

        $existing = $node->xrayInbounds()
            ->when($excludeId, fn ($q) => $q->where('id', '!=', $excludeId))
            ->pluck('tag')
            ->all();

        $tag = $base;
        $i = 2;
        while (in_array($tag, $existing, true)) {
            $tag = $base . '-' . $i;
            $i++;
        }

        return $tag;
    }

    /** 入站展示结构（不吐私钥；public_key 保留供界面/订阅）。 */
    private function present(XrayInbound $inbound, Collection $users): array
    {
        $stream = $inbound->stream_settings ?? [];
        $reality = $stream['realitySettings'] ?? [];
        $settings = $inbound->settings ?? [];

        $clientCount = $inbound->protocol === 'shadowsocks'
            ? 0
            : $users->where('protocol', $inbound->protocol)->count();

        return [
            'id' => $inbound->id,
            'tag' => $inbound->tag,
            'protocol' => $inbound->protocol,
            'port' => $inbound->port,
            'listen' => $inbound->listen,
            'network' => $stream['network'] ?? 'tcp',
            'security' => $stream['security'] ?? 'none',
            'dest' => $reality['dest'] ?? null,
            'server_names' => array_values((array) ($reality['serverNames'] ?? [])),
            'short_ids' => array_values((array) ($reality['shortIds'] ?? [])),
            'public_key' => $reality['publicKey'] ?? null,
            'has_private_key' => $inbound->reality_private_key !== null,
            'ws_path' => $stream['wsSettings']['path'] ?? null,
            'ws_host' => $stream['wsSettings']['host'] ?? null,
            'tls_server_name' => $stream['tlsSettings']['serverName'] ?? null,
            'has_tls_cert' => ! empty($stream['tlsSettings']['certificates']),
            'ss_method' => $inbound->protocol === 'shadowsocks' ? ($settings['method'] ?? null) : null,
            'ss_password_set' => $inbound->protocol === 'shadowsocks' && ! empty($settings['password']),
            'enabled' => $inbound->enabled,
            'sort' => $inbound->sort,
            'client_count' => $clientCount,
            'created_at' => $inbound->created_at?->toIso8601String(),
        ];
    }
}
