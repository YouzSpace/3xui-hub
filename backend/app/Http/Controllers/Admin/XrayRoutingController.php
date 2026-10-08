<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\XrayConfigService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\XrayOutbound;
use App\Models\XrayRoutingRule;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Validation\ValidationException;

/**
 * Admin xray 节点路由管理（/admin-api/nodes/{node}/xray-routing）。
 *
 * - 规则 CRUD（域名/IP/端口/网络/协议/入站 → 目标出站）
 * - 路由测试器：输入域名或 IP，按渲染口径（内置基础规则 + 自定义规则）
 *   逐条模拟匹配，返回命中的规则与最终出站
 *
 * 任何变更后 bump 该节点 config_version → 节点秒级拉取生效。
 */
class XrayRoutingController extends Controller
{
    use ApiResponse;

    public function __construct(
        private readonly XrayConfigService $config,
    ) {}

    /** GET /admin-api/nodes/{node}/xray-routing */
    public function index(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $rules = $node->xrayRoutingRules()->orderBy('sort')->orderBy('id')->get();

        return $this->success([
            'rules' => $rules->map(fn (XrayRoutingRule $r) => $this->present($r))->values()->all(),
            'outbound_tags' => $this->outboundTags($node),
            'inbound_tags' => $node->xrayInbounds()->pluck('tag')->values()->all(),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** POST /admin-api/nodes/{node}/xray-routing */
    public function store(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);
        $data = $this->validatePayload($request, $node);

        $rule = new XrayRoutingRule();
        $rule->node_id = $node->id;
        $this->fillFromPayload($rule, $data);
        $rule->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'rule' => $this->present($rule->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/xray-routing/{rule} */
    public function update(Request $request, Node $node, XrayRoutingRule $rule): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $rule);

        $data = $this->validatePayload($request, $node);
        $this->fillFromPayload($rule, $data);
        $rule->save();

        $node->bumpXrayConfigVersion();

        return $this->success([
            'rule' => $this->present($rule->fresh()),
            'config_version' => $node->xrayConfigVersion(),
        ]);
    }

    /** DELETE /admin-api/nodes/{node}/xray-routing/{rule} */
    public function destroy(Node $node, XrayRoutingRule $rule): JsonResponse
    {
        $this->ensureXray($node);
        $this->ensureOwned($node, $rule);

        $rule->delete();
        $node->bumpXrayConfigVersion();

        return $this->success(['config_version' => $node->xrayConfigVersion()]);
    }

    /**
     * POST /admin-api/nodes/{node}/xray-routing/test
     * body: {domain?: "...", ip?: "..."} → 按渲染口径逐条模拟匹配。
     */
    public function test(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate([
            'domain' => ['nullable', 'string', 'max:255'],
            'ip' => ['nullable', 'string', 'max:64'],
        ]);

        $domain = trim((string) ($data['domain'] ?? ''));
        $ip = trim((string) ($data['ip'] ?? ''));
        if ($domain === '' && $ip === '') {
            return $this->error('请输入域名或 IP');
        }

        // 渲染口径的完整规则链（内置基础规则 + 自定义）
        $rules = $this->config->renderRouting($node)['rules'];
        $matched = null;

        foreach ($rules as $idx => $rule) {
            $hit = false;
            $notes = [];

            if ($domain !== '' && ! empty($rule['domain'])) {
                foreach ((array) $rule['domain'] as $pattern) {
                    $result = $this->matchDomain((string) $pattern, $domain);
                    if ($result === true) {
                        $hit = true;
                        $notes[] = "域名命中 {$pattern}";
                        break;
                    }
                    if ($result === null) {
                        $notes[] = "规则含 {$pattern}（需内核 geo 数据，无法在面板精确判定）";
                    }
                }
            }

            if (! $hit && $ip !== '' && ! empty($rule['ip'])) {
                foreach ((array) $rule['ip'] as $pattern) {
                    $result = $this->matchIp((string) $pattern, $ip);
                    if ($result === true) {
                        $hit = true;
                        $notes[] = "IP 命中 {$pattern}";
                        break;
                    }
                    if ($result === null) {
                        $notes[] = "规则含 {$pattern}（需内核 geo 数据，无法在面板精确判定）";
                    }
                }
            }

            if ($hit) {
                $matched = [
                    'index' => $idx,
                    'rule' => $rule,
                    'outbound' => $rule['outboundTag'],
                    'notes' => $notes,
                ];
                break;
            }
        }

        return $this->success([
            'matched' => $matched,
            // 未命中任何规则：走第一条出站（xray 默认行为 = direct）
            'outbound' => $matched['outbound'] ?? 'direct',
            'rules_checked' => count($rules),
        ]);
    }

    // ── 内部 ─────────────────────────────────────────────

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function ensureOwned(Node $node, XrayRoutingRule $rule): void
    {
        if ((int) $rule->node_id !== (int) $node->id) {
            abort(404);
        }
    }

    /** 可选目标出站：内置 + 自定义 + warp。 */
    private function outboundTags(Node $node): array
    {
        $tags = [
            ['tag' => 'direct', 'label' => '直连（内置）'],
            ['tag' => 'blocked', 'label' => '拦截（内置）'],
        ];
        foreach (XrayOutbound::enabledFor($node) as $outbound) {
            $tags[] = [
                'tag' => $outbound->tag,
                'label' => $outbound->remark ?: $outbound->protocol,
            ];
        }

        return $tags;
    }

    private function validatePayload(Request $request, Node $node): array
    {
        $data = $request->validate([
            'domains' => ['nullable', 'array', 'max:200'],
            'domains.*' => ['string', 'max:512'],
            'ips' => ['nullable', 'array', 'max:200'],
            'ips.*' => ['string', 'max:512'],
            'port' => ['nullable', 'string', 'max:64'],
            'network' => ['nullable', 'in:tcp,udp'],
            'protocol' => ['nullable', 'in:http,tls,bittorrent'],
            'inbound_tag' => ['nullable', 'string', 'max:64'],
            'outbound_tag' => ['required', 'string', 'max:64'],
            'remark' => ['nullable', 'string', 'max:128'],
            'enabled' => ['nullable', 'boolean'],
            'sort' => ['nullable', 'integer', 'min:0', 'max:65535'],
        ]);

        // 至少一个匹配条件，否则会命中全部流量（渲染层也会跳过，这里直接拒绝）
        if (empty(array_filter($data['domains'] ?? [])) && empty(array_filter($data['ips'] ?? []))
            && empty($data['port']) && empty($data['network']) && empty($data['protocol']) && empty($data['inbound_tag'])) {
            throw ValidationException::withMessages(['domains' => '至少需要一个匹配条件（域名/IP/端口/网络/协议/入站）']);
        }

        // 目标出站必须存在（内置或已配置）
        $valid = array_column($this->outboundTags($node), 'tag');
        if (! in_array($data['outbound_tag'], $valid, true)) {
            throw ValidationException::withMessages(['outbound_tag' => '目标出站不存在']);
        }

        return $data;
    }

    private function fillFromPayload(XrayRoutingRule $rule, array $data): void
    {
        $rule->domains = array_values(array_filter(array_map('trim', $data['domains'] ?? []), fn ($v) => $v !== '')) ?: null;
        $rule->ips = array_values(array_filter(array_map('trim', $data['ips'] ?? []), fn ($v) => $v !== '')) ?: null;
        $rule->port = $data['port'] ?? null;
        $rule->network = $data['network'] ?? null;
        $rule->protocol = $data['protocol'] ?? null;
        $rule->inbound_tag = $data['inbound_tag'] ?? null;
        $rule->outbound_tag = (string) $data['outbound_tag'];
        $rule->remark = (string) ($data['remark'] ?? '') !== '' ? (string) $data['remark'] : null;
        if (array_key_exists('enabled', $data)) {
            $rule->enabled = (bool) $data['enabled'];
        }
        if (array_key_exists('sort', $data)) {
            $rule->sort = (int) $data['sort'];
        }
    }

    /**
     * 域名匹配模拟。
     * @return bool|null true=命中；false=不命中；null=依赖 geo 数据无法精确判定
     */
    private function matchDomain(string $pattern, string $domain): ?bool
    {
        $domain = strtolower($domain);
        $pattern = trim($pattern);
        if ($pattern === '') {
            return false;
        }

        if (str_starts_with($pattern, 'geosite:')) {
            return null; // 需要内核 geosite 数据
        }
        if (str_starts_with($pattern, 'full:')) {
            return $domain === strtolower(substr($pattern, 5));
        }
        if (str_starts_with($pattern, 'domain:')) {
            $d = strtolower(substr($pattern, 7));

            return $domain === $d || str_ends_with($domain, '.' . $d);
        }
        if (str_starts_with($pattern, 'keyword:')) {
            return str_contains($domain, strtolower(substr($pattern, 8)));
        }
        if (str_starts_with($pattern, 'regexp:')) {
            return @preg_match('/' . str_replace('/', '\/', substr($pattern, 7)) . '/i', $domain) === 1;
        }
        if (str_starts_with($pattern, 'ext:')) {
            return str_ends_with($domain, '.' . strtolower(substr($pattern, 4)));
        }

        // 裸域名等价 domain:
        $d = strtolower($pattern);

        return $domain === $d || str_ends_with($domain, '.' . $d);
    }

    /**
     * IP 匹配模拟。
     * @return bool|null true=命中；false=不命中；null=依赖 geo 数据无法精确判定
     */
    private function matchIp(string $pattern, string $ip): ?bool
    {
        $pattern = trim($pattern);
        if ($pattern === '') {
            return false;
        }

        if (str_starts_with($pattern, 'geoip:')) {
            return null; // 需要内核 geoip 数据
        }

        if (! str_contains($pattern, '/')) {
            return $ip === $pattern;
        }

        // CIDR 判断
        [$subnet, $bits] = explode('/', $pattern, 2);
        $bits = (int) $bits;
        $ipBin = @inet_pton($ip);
        $subnetBin = @inet_pton($subnet);
        if ($ipBin === false || $subnetBin === false || strlen($ipBin) !== strlen($subnetBin)) {
            return false;
        }

        $fullBytes = intdiv($bits, 8);
        $remBits = $bits % 8;

        if (substr($ipBin, 0, $fullBytes) !== substr($subnetBin, 0, $fullBytes)) {
            return false;
        }
        if ($remBits === 0) {
            return true;
        }

        $mask = ~((1 << (8 - $remBits)) - 1) & 0xFF;

        return (ord($ipBin[$fullBytes]) & $mask) === (ord($subnetBin[$fullBytes]) & $mask);
    }

    private function present(XrayRoutingRule $rule): array
    {
        return [
            'id' => $rule->id,
            'domains' => array_values((array) ($rule->domains ?? [])),
            'ips' => array_values((array) ($rule->ips ?? [])),
            'port' => $rule->port,
            'network' => $rule->network,
            'protocol' => $rule->protocol,
            'inbound_tag' => $rule->inbound_tag,
            'outbound_tag' => $rule->outbound_tag,
            'remark' => $rule->remark,
            'enabled' => $rule->enabled,
            'sort' => $rule->sort,
        ];
    }
}
