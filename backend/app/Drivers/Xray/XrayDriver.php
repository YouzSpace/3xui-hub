<?php

namespace App\Drivers\Xray;

use App\Drivers\Capability;
use App\Drivers\Contracts\PanelDriverInterface;
use App\Models\Node;
use App\Models\NodeApiCommand;
use App\Models\TrafficSnapshot;
use App\Models\User;
use App\Models\XrayInbound;

/**
 * xray 裸内核节点驱动（双通道）。
 *
 * - 用户级操作 → 入队 NodeApiCommand（adu/rmu），agent 经 WS 推送 / HTTP 长轮询
 *   取走本地执行（秒级生效，不等回执）；
 * - 全量 config 只在结构变更时由 agent 拉取（XrayConfigService 渲染）；
 * - 流量来自 agent 主动推送（WS report / HTTP push），本驱动不做面板侧轮询。
 *
 * 多入站口径（入站结构化后）：
 * - adu/rmu 按「用户协议 == 入站协议」的启用入站逐条下发；
 * - 订阅链接按协议匹配的入站逐条生成；
 * - shadowsocks 入站为单凭据模式，不经用户级指令通道。
 *
 * 状态口径：
 * - getClient 从 users 表解析（email = ch_user_{id}），代表「该用户应在此节点上」；
 *   内核侧实际状态由指令队列推进（P1 不做强一致对账，P3 对账任务负责）。
 * - enable=false 的语义 = 内核移除该用户（rmu，秒级断连）；启用 = adu 重加。
 */
class XrayDriver implements PanelDriverInterface
{
    private const CAPABILITIES = [
        Capability::CLIENT_CREATE,
        Capability::CLIENT_READ,
        Capability::CLIENT_UPDATE,
        Capability::CLIENT_DELETE,
        Capability::CLIENT_LIST,
        Capability::CLIENT_TOGGLE,
        Capability::TRAFFIC_SYNC,
        Capability::TRAFFIC_RESET,
        Capability::SUBSCRIPTION_LINKS,
        Capability::HEALTH_CHECK,
    ];

    /** 心跳新鲜阈值（秒）：超过视为离线（agent 心跳周期 30s 的 4 倍）。 */
    private const HEARTBEAT_FRESH_SECONDS = 120;

    public function __construct(
        private readonly Node $node,
    ) {}

    private function config(): XrayConfigService
    {
        return app(XrayConfigService::class);
    }

    // ── DriverInterface ──

    public function name(): string
    {
        return 'xray';
    }

    public function version(): string
    {
        return (string) ($this->node->driver_config['xray_version'] ?? 'v26.3.27');
    }

    public function supports(string $capability): bool
    {
        return in_array(Capability::tryFrom($capability), self::CAPABILITIES);
    }

    public function capabilities(): array
    {
        return array_map(fn (Capability $c) => $c->value, self::CAPABILITIES);
    }

    // ── 客户管理（指令通道：入队即返回，agent 秒级取走执行） ──

    public function createClient(array $data, array $inboundIds = []): ?array
    {
        $email = $data['email'] ?? null;
        if (! is_string($email) || $email === '') {
            return null;
        }

        $uuid = (string) (($data['uuid'] ?? null) ?: ($data['id'] ?? null) ?: '');

        $user = $this->userForEmail($email);
        if ($user !== null && (string) $user->uuid !== '') {
            $uuid = (string) $user->uuid; // users 表是事实源
        }
        if ($uuid === '') {
            return null;
        }

        $this->enqueueAdd($email, $uuid);

        return ['uuid' => $uuid, 'email' => $email];
    }

    public function updateClient(string $identifier, array $data, ?int $inboundId = null): bool
    {
        // enable 键即开关指令：false → rmu（内核移除，秒级断连）；true/缺省 → adu（重加/更新）
        if (array_key_exists('enable', $data) && ! $data['enable']) {
            return $this->enqueueRemove($identifier);
        }

        $user = $this->userForEmail($identifier);
        $uuid = (string) ((($data['uuid'] ?? null) ?: ($data['id'] ?? null)) ?: ($user !== null ? (string) $user->uuid : ''));
        if ($uuid === '') {
            return false;
        }

        $this->enqueueAdd($identifier, $uuid);

        return true;
    }

    public function toggleClient(string $identifier, bool $enable): bool
    {
        if (! $enable) {
            return $this->enqueueRemove($identifier);
        }

        $user = $this->userForEmail($identifier);
        if ($user === null || (string) $user->uuid === '') {
            return false;
        }

        $this->enqueueAdd($identifier, (string) $user->uuid);

        return true;
    }

    public function deleteClient(string $identifier, bool $keepTraffic = false, ?int $inboundId = null): bool
    {
        return $this->enqueueRemove($identifier);
    }

    public function getClient(string $identifier): ?array
    {
        $user = $this->userForEmail($identifier);
        if ($user === null) {
            return null;
        }

        return [
            'id' => (string) $user->uuid,
            'uuid' => (string) $user->uuid,
            'email' => $identifier,
            'enable' => (bool) ($user->plan_id !== null && $user->enabled),
            'totalGB' => (int) ($user->traffic_limit ?? 0),
            'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
        ];
    }

    public function listClients(): array
    {
        return User::query()
            ->whereNotNull('plan_id')
            ->get()
            ->map(fn (User $user) => [
                'email' => $user->clientEmail(),
                'uuid' => (string) $user->uuid,
                'inboundIds' => [],
            ])
            ->all();
    }

    // ── 流量 ──

    public function getClientTraffic(string $identifier): ?array
    {
        $userId = $this->userIdFromEmail($identifier);
        if ($userId === null) {
            return null;
        }

        $snapshot = TrafficSnapshot::where('user_id', $userId)
            ->where('node_id', $this->node->id)
            ->first();

        if ($snapshot === null) {
            return null;
        }

        return [
            'up' => (int) $snapshot->upload,
            'down' => (int) $snapshot->download,
        ];
    }

    /**
     * 重置内核流量计数：no-op。
     *
     * 「绝对值快照 + 增量落账」口径下，清零 users.traffic_used 后下一轮推送的
     * delta = now - last 会继续正常累计（快照自带面板侧重置分支），无需动内核计数；
     * 内核侧计数清理由 P3 的强制断连链路统一处理。
     */
    public function resetClientTraffic(string $identifier): bool
    {
        return true;
    }

    // ── 入站（入站列表在 xray_inbounds 自建；驱动不暴露面板式入站挂载） ──

    public function listInbounds(): array
    {
        return [];
    }

    public function getInbound(int $id): ?array
    {
        return null;
    }

    public function getClientStatsGroupedByInbound(): array
    {
        return [];
    }

    /** 面板侧轮询不提供流量（agent push 通道负责）——返回空，轮询路径自然跳过。 */
    public function getClientStatsByEmail(): array
    {
        return [];
    }

    /** 订阅链接：按「用户协议 == 入站协议」的启用入站逐条生成。 */
    public function getClientLinks(string $identifier): array
    {
        $user = $this->userForEmail($identifier);
        if ($user === null || (string) $user->uuid === '') {
            return [];
        }

        return $this->config()->subscriptionUris($this->node, $user);
    }

    /**
     * attach/detach：xray 多入站按协议自动匹配用户，无面板式入站挂载概念 ——
     * 协议切换链路（ProtocolSwitchService）只对「有 3x-ui 入站记录」的节点生效，
     * xray 节点不会被触达。保持 no-op。
     */
    public function attachClient(string $identifier, array $inboundIds): bool
    {
        return true;
    }

    public function detachClient(string $identifier, array $inboundIds): bool
    {
        return true;
    }

    // ── 健康检查（心跳新鲜度，不做面板请求） ──

    public function healthCheck(): array
    {
        $last = $this->node->last_check_at;
        if ($last === null) {
            return ['ok' => false, 'latencyMs' => 0, 'error' => '节点尚未注册（无心跳）'];
        }

        $age = (int) $last->diffInSeconds(now());
        if ($age > self::HEARTBEAT_FRESH_SECONDS) {
            return ['ok' => false, 'latencyMs' => 0, 'error' => "心跳超时（{$age}s 未上报）"];
        }

        return ['ok' => true, 'latencyMs' => 0];
    }

    // ── 指令入队（多入站：每协议匹配的启用入站一条指令） ──────────

    private function enqueueAdd(string $email, string $uuid): void
    {
        $user = $this->userForEmail($email);
        if ($user === null) {
            return; // 面板用户必须已入库（调用方先建 users 行）
        }
        if ($uuid !== '' && (string) $user->uuid === '') {
            $user->uuid = $uuid; // 极端情况下用户表缺 uuid，用传入值渲染
        }

        foreach ($this->inboundsFor($user) as $inbound) {
            NodeApiCommand::create([
                'node_id' => $this->node->id,
                'user_id' => $user->id,
                'action' => NodeApiCommand::ACTION_ADD_USER,
                'payload' => ['inbounds' => [$this->config()->aduInboundFragment($inbound, $user)]],
                'status' => NodeApiCommand::STATUS_PENDING,
            ]);
        }
    }

    private function enqueueRemove(string $email): bool
    {
        $user = $this->userForEmail($email);
        $inbounds = $user !== null ? $this->inboundsFor($user) : collect();

        foreach ($inbounds as $inbound) {
            NodeApiCommand::create([
                'node_id' => $this->node->id,
                'user_id' => $user->id,
                'action' => NodeApiCommand::ACTION_REMOVE_USER,
                'payload' => [
                    'tag' => $inbound->tag,
                    'emails' => [$email],
                ],
                'status' => NodeApiCommand::STATUS_PENDING,
            ]);
        }

        return true;
    }

    /**
     * 用户协议匹配的启用入站（ss 单凭据模式排除 —— 不经用户级指令通道）。
     *
     * @return \Illuminate\Support\Collection<int, XrayInbound>
     */
    private function inboundsFor(User $user): \Illuminate\Support\Collection
    {
        return XrayInbound::enabledFor($this->node)
            ->filter(fn (XrayInbound $in) => $in->protocol === (string) $user->protocol
                && $in->protocol !== 'shadowsocks');
    }

    private function userIdFromEmail(string $email): ?int
    {
        return preg_match('/^ch_user_(\d+)$/', $email, $m) ? (int) $m[1] : null;
    }

    private function userForEmail(string $email): ?User
    {
        $userId = $this->userIdFromEmail($email);

        return $userId === null ? null : User::find($userId);
    }
}
