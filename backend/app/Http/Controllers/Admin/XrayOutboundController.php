<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\OutboundLinkParser;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\NodeApiCommand;
use App\Models\XrayOutbound;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;
use Illuminate\Validation\ValidationException;

/**
 * Admin xray 节点出站管理（/admin-api/nodes/{node}/xray-outbounds）。
 *
 * - 结构化 CRUD（协议/服务器/凭据/传输 + 高级 JSON 透传兜底）
 * - URI 一键导入（vless/vmess/trojan/ss/socks/http 分享链接）
 * - 连通测试（下发 test_outbound 指令，由节点侧 TCP 探活回执）
 * - 列表附出站流量（agent 上报的 xray_outbound_traffic 覆盖写快照）
 *
 * 任何变更后 bump 该节点 config_version → 节点秒级拉取生效。
 */
class XrayOutboundController extends Controller
{
    use ApiResponse;

    /** 系统保留 tag（与 XrayConfigService 内置出站一致，不可占用）。 */
    private const RESERVED_TAGS = ['direct', 'blocked', 'api'];

    /** GET /admin-api/nodes/{node}/xray-outbounds */
    public function index(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $traffic = DB::table('xray_outbound_traffic')
            ->where('node_id', $node->id)
            ->get()
            ->keyBy('tag');

        $outbounds = $node->xrayOutbounds()->orderBy('sort')->orderBy('id')->get();

        return $this->success([
            'outbounds' => $outbounds->map(fn (XrayOutbound $o) => $this->present($o, $traffic->get($o->tag)))->values()->all(),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** POST /admin-api/nodes/{node}/xray-outbounds */
    public function store(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);
        $data = $this->validatePayload($request);

        $outbound = new XrayOutbound();
        $outbound->node_id = $node->id;
        $this->fillFromPayload($outbound, $data, $node, null);
        $outbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'outbound' => $this->present($outbound->fresh(), null),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-outbounds/{outbound} */
    public function update(Request $request, Node $node, XrayOutbound $outbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $outbound);

        $data = $this->validatePayload($request);
        $this->fillFromPayload($outbound, $data, $node, $outbound);
        $outbound->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'outbound' => $this->present($outbound->fresh(), null),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-outbounds/{outbound} */
    public function destroy(Node $node, XrayOutbound $outbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $outbound);

        if ($outbound->tag === 'warp') {
            throw ValidationException::withMessages(['tag' => 'WARP 出站由 WARP 功能管理，请到 WARP 面板操作']);
        }

        $outbound->delete();
        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-outbounds/import
     * body: {link: "vless://..."} → 解析为出站结构（不落库，返回预填结构供表单确认后提交）。
     */
    public function import(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate(['link' => ['required', 'string', 'max:4096']]);

        try {
            $parsed = OutboundLinkParser::parse($data['link']);
        } catch (\InvalidArgumentException $e) {
            return $this->error($e->getMessage());
        }

        return $this->success(['parsed' => $parsed]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-outbounds/{outbound}/test
     * 下发连通测试指令并同步等待回执（≤8s：WS 通道秒级执行，HTTP 降级 2s 节拍内也够）。
     */
    public function test(Node $node, XrayOutbound $outbound): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $outbound);

        [$host, $port] = $this->targetOf($outbound);
        if ($host === '') {
            return $this->error('该出站没有可测试的目标地址');
        }

        $command = NodeApiCommand::create([
            'node_id' => $node->id,
            'user_id' => null,
            'action' => 'test_outbound',
            'payload' => ['host' => $host, 'port' => $port],
            'status' => NodeApiCommand::STATUS_PENDING,
        ]);

        // 同步等待执行回执（节点在线时 2-3 秒内必达；离线超时返回 pending）
        $deadline = microtime(true) + 8;
        while (microtime(true) < $deadline) {
            $fresh = $command->fresh();
            if ($fresh->status === NodeApiCommand::STATUS_SUCCESS || $fresh->status === NodeApiCommand::STATUS_FAILED) {
                return $this->success([
                    'success' => $fresh->status === NodeApiCommand::STATUS_SUCCESS,
                    'output' => (string) $fresh->result,
                ]);
            }
            usleep(500_000);
        }

        return $this->success(['pending' => true, 'message' => '节点未在 8 秒内回执（可能离线），稍后可重试']);
    }

    // ── 内部 ─────────────────────────────────────────────

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function ensureOwned(Node $node, XrayOutbound $outbound): void
    {
        if ((int) $outbound->node_id !== (int) $node->id) {
            abort(404);
        }
    }

    private function validatePayload(Request $request): array
    {
        return $request->validate([
            'tag' => ['required', 'string', 'max:64', 'regex:/^[A-Za-z0-9_.-]+$/'],
            'protocol' => ['required', 'in:freedom,blackhole,vmess,vless,trojan,shadowsocks,socks,http,wireguard'],
            'remark' => ['nullable', 'string', 'max:128'],
            'enabled' => ['nullable', 'boolean'],
            'sort' => ['nullable', 'integer', 'min:0', 'max:65535'],
            // 通用连接参数（按协议取用）
            'address' => ['nullable', 'string', 'max:255'],
            'port' => ['nullable', 'integer', 'between:1,65535'],
            'user_id' => ['nullable', 'string', 'max:255'], // vless/vmess uuid
            'password' => ['nullable', 'string', 'max:255'], // trojan/ss
            'flow' => ['nullable', 'string', 'max:64'],
            'encryption' => ['nullable', 'string', 'max:64'],
            'method' => ['nullable', 'string', 'max:64'], // ss 加密方式
            'username' => ['nullable', 'string', 'max:255'], // socks/http 用户名
            // 传输
            'network' => ['nullable', 'in:tcp,ws,grpc'],
            'security' => ['nullable', 'in:none,tls'],
            'ws_path' => ['nullable', 'string', 'max:255'],
            'ws_host' => ['nullable', 'string', 'max:255'],
            'tls_server_name' => ['nullable', 'string', 'max:255'],
            // wireguard（高级透传兜底；WARP 功能外的手工 wireguard）
            'wg_secret_key' => ['nullable', 'string', 'max:255'],
            'wg_address' => ['nullable', 'string', 'max:512'], // 逗号分隔
            'wg_peer_public_key' => ['nullable', 'string', 'max:255'],
            'wg_peer_endpoint' => ['nullable', 'string', 'max:255'],
            'wg_reserved' => ['nullable', 'string', 'max:64'], // "0,0,0"
            // 高级：直接编辑 JSON（非空时优先，UI 未覆盖的协议用）
            'advanced_settings' => ['nullable', 'string', 'max:65535'],
            'advanced_stream' => ['nullable', 'string', 'max:65535'],
        ]);
    }

    private function fillFromPayload(XrayOutbound $outbound, array $data, Node $node, ?XrayOutbound $existing): void
    {
        $protocol = $data['protocol'];
        $tag = (string) $data['tag'];

        if (in_array(strtolower($tag), self::RESERVED_TAGS, true)) {
            throw ValidationException::withMessages(['tag' => "标识 {$tag} 为系统保留"]);
        }
        if (strtolower($tag) === 'warp' && $protocol !== 'wireguard') {
            throw ValidationException::withMessages(['tag' => 'warp 标识预留给 WARP 出站']);
        }

        $dup = $node->xrayOutbounds()->where('tag', $tag)
            ->when($existing, fn ($q) => $q->where('id', '!=', $existing->id))
            ->exists();
        if ($dup) {
            throw ValidationException::withMessages(['tag' => "标识 {$tag} 已存在"]);
        }

        // 高级 JSON 透传（优先）
        $settings = $this->parseAdvanced($data['advanced_settings'] ?? null, 'advanced_settings');
        $stream = $this->parseAdvanced($data['advanced_stream'] ?? null, 'advanced_stream');

        if ($settings === null) {
            $settings = $this->buildSettings($protocol, $data);
        }
        if ($stream === null) {
            $stream = $this->buildStream($protocol, $data);
        }

        // wireguard 私钥分离存储（加密列）
        if ($protocol === 'wireguard' && ! empty($data['wg_secret_key'])) {
            $outbound->setSecretKey((string) $data['wg_secret_key']);
        }

        $outbound->tag = $tag;
        $outbound->protocol = $protocol;
        $outbound->settings = $settings;
        $outbound->stream_settings = $stream;
        $outbound->remark = (string) ($data['remark'] ?? '') !== '' ? (string) $data['remark'] : null;
        if (array_key_exists('enabled', $data)) {
            $outbound->enabled = (bool) $data['enabled'];
        }
        if (array_key_exists('sort', $data)) {
            $outbound->sort = (int) $data['sort'];
        }
    }

    /** 按协议组装 settings（结构化字段路径）。 */
    private function buildSettings(string $protocol, array $data): array|\stdClass
    {
        $address = (string) ($data['address'] ?? '');
        $port = (int) ($data['port'] ?? 0);

        return match ($protocol) {
            'freedom' => ['domainStrategy' => 'AsIs'],
            'blackhole' => new \stdClass(),
            'vmess' => [
                'vnext' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'users' => [[
                        'id' => $this->requireField((string) ($data['user_id'] ?? ''), 'user_id'),
                        'alterId' => 0,
                        'security' => (string) ($data['encryption'] ?? 'auto') ?: 'auto',
                    ]],
                ]],
            ],
            'vless' => [
                'vnext' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'users' => [array_filter([
                        'id' => $this->requireField((string) ($data['user_id'] ?? ''), 'user_id'),
                        'encryption' => 'none',
                        'flow' => (string) ($data['flow'] ?? ''),
                    ], fn ($v) => $v !== '')],
                ]],
            ],
            'trojan' => [
                'servers' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'password' => $this->requireField((string) ($data['password'] ?? ''), 'password'),
                ]],
            ],
            'shadowsocks' => [
                'servers' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'method' => $this->requireField((string) ($data['method'] ?? ''), 'method'),
                    'password' => $this->requireField((string) ($data['password'] ?? ''), 'password'),
                ]],
            ],
            'socks', 'http' => [
                'servers' => [[
                    'address' => $this->requireField($address, 'address'),
                    'port' => $this->requirePort($port),
                    'users' => [[
                        'user' => (string) ($data['username'] ?? ''),
                        'pass' => (string) ($data['password'] ?? ''),
                    ]],
                ]],
            ],
            'wireguard' => array_filter([
                // 注：privateKey（secretKey）不落 settings —— 由 secret_key 加密列单独保存，
                // 渲染时统一注入（见 XrayConfigService::outboundSettings）
                'address' => (string) ($data['wg_address'] ?? '') !== ''
                    ? array_map('trim', explode(',', (string) $data['wg_address']))
                    : null,
                'peers' => (string) ($data['wg_peer_public_key'] ?? '') !== '' ? [[
                    'publicKey' => (string) $data['wg_peer_public_key'],
                    'endpoint' => (string) ($data['wg_peer_endpoint'] ?? ''),
                ]] : null,
                'reserved' => (string) ($data['wg_reserved'] ?? '') !== ''
                    ? array_map(fn ($v) => (int) trim($v), explode(',', (string) $data['wg_reserved']))
                    : null,
            ], fn ($v) => $v !== null),
            default => new \stdClass(),
        };
    }

    /**
     * wireguard 特殊处理：settings 里的 secretKey 占位符在生产渲染时由加密列注入。
     * 为了让「高级 JSON 透传」与「表单组装」统一，这里把 secretKey 从 settings 中剥离，
     * 渲染层（XrayConfigService::outboundSettings）统一从 secret_key 列注入。
     */
    private function buildStream(string $protocol, array $data): array
    {
        if (in_array($protocol, ['freedom', 'blackhole'], true)) {
            return [];
        }

        $network = (string) ($data['network'] ?? 'tcp');
        $security = (string) ($data['security'] ?? 'none');

        $stream = ['network' => $network, 'security' => $security];

        if ($network === 'ws') {
            $ws = array_filter([
                'path' => (string) ($data['ws_path'] ?? '') !== '' ? (string) $data['ws_path'] : '/',
                'headers' => (string) ($data['ws_host'] ?? '') !== '' ? ['Host' => (string) $data['ws_host']] : null,
            ], fn ($v) => $v !== null);
            $stream['wsSettings'] = $ws;
        }

        if ($security === 'tls') {
            $stream['tlsSettings'] = array_filter([
                'serverName' => (string) ($data['tls_server_name'] ?? ''),
            ], fn ($v) => $v !== '');
        }

        return $stream;
    }

    private function requireField(string $value, string $field): string
    {
        if (trim($value) === '') {
            throw ValidationException::withMessages([$field => '该字段必填']);
        }

        return trim($value);
    }

    private function requirePort(int $port): int
    {
        if ($port < 1 || $port > 65535) {
            throw ValidationException::withMessages(['port' => '端口必填（1-65535）']);
        }

        return $port;
    }

    private function parseAdvanced(?string $json, string $fieldName): ?array
    {
        if ($json === null || trim($json) === '') {
            return null;
        }
        $decoded = json_decode($json, true);
        if (! is_array($decoded)) {
            throw ValidationException::withMessages([$fieldName => 'JSON 格式不正确']);
        }

        return $decoded;
    }

    /** 从出站配置提取测试目标（服务器地址 + 端口）。 */
    private function targetOf(XrayOutbound $outbound): array
    {
        $settings = $outbound->settings ?? [];

        foreach (['vnext', 'servers'] as $key) {
            $first = $settings[$key][0] ?? null;
            if (is_array($first) && ! empty($first['address'])) {
                return [(string) $first['address'], (int) ($first['port'] ?? 443)];
            }
        }

        if ($outbound->protocol === 'wireguard') {
            $endpoint = (string) ($settings['peers'][0]['endpoint'] ?? '');
            if ($endpoint !== '' && str_contains($endpoint, ':')) {
                [$h, $p] = explode(':', $endpoint, 2);

                return [$h, (int) $p];
            }
        }

        return ['', 0];
    }

    /** 出站展示结构（不吐凭据明文；流量来自快照表）。 */
    private function present(XrayOutbound $outbound, ?object $traffic): array
    {
        $settings = $outbound->settings ?? [];
        $stream = $outbound->stream_settings ?? [];

        $first = $settings['vnext'][0] ?? $settings['servers'][0] ?? null;

        return [
            'id' => $outbound->id,
            'tag' => $outbound->tag,
            'protocol' => $outbound->protocol,
            'remark' => $outbound->remark,
            'address' => is_array($first) ? ($first['address'] ?? null) : null,
            'port' => is_array($first) ? ($first['port'] ?? null) : null,
            'network' => $stream['network'] ?? null,
            'security' => $stream['security'] ?? null,
            'enabled' => $outbound->enabled,
            'sort' => $outbound->sort,
            'settings' => $settings,
            'stream_settings' => $stream,
            'has_secret_key' => $outbound->secret_key !== null,
            'upload' => (int) ($traffic->upload ?? 0),
            'download' => (int) ($traffic->download ?? 0),
            'traffic_updated_at' => isset($traffic->updated_at) ? (string) $traffic->updated_at : null,
        ];
    }
}
