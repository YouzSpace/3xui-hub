<?php

namespace App\Services\ThreeXUi;

use GuzzleHttp\Client;
use GuzzleHttp\Cookie\CookieJar;
use GuzzleHttp\Exception\ConnectException;
use GuzzleHttp\Exception\RequestException;
use GuzzleHttp\Exception\TransferException;
use GuzzleHttp\HandlerStack;
use GuzzleHttp\Promise\Create;
use GuzzleHttp\Promise\PromiseInterface;
use Illuminate\Contracts\Cache\Repository as CacheRepository;
use Illuminate\Support\Facades\Log;

/**
 * 3x-ui v3.x HTTP API 客户端（M5 核心）。
 *
 * 鉴权：优先 Bearer API Token（nodes.api_key），Bearer 跳过 CSRF、无状态。
 * 仅当未提供 api_key 时回退 cookie+CSRF 登录流程（/login + /panel/api/csrf-token）。
 * 该登录态按「面板指纹」缓存（config panel.auth_cache_ttl），Web 请求与队列 Job
 * 跨进程共用，TTL 内同一个面板只登录一次；会话失效（401/403）自动清缓存重登并重试一次。
 *
 * 路径前缀：baseURL = "{scheme}://{host}:{port}{web_base_path}"，webBasePath 可空。
 * API 命名空间：/panel/api/*。
 *
 * 统一响应：{success:bool, msg:string, obj:any}。success===true 取 obj，否则抛 ThreeXUiException。
 * 「可能不存在」的查询（getClient/getClientTraffic）在 not-found 时返回 null 而非抛异常。
 *
 * client 主键 = email（ControlHub 用 ch_user_{user.id}），uuid/password 由 3x-ui 在 add 时生成。
 * 参见 docs/pre-research-3xui-api.md（真机验证）与 system-design.md §3。
 */
class ThreeXUiClient
{
    // ===== 端点路径常量（集中管理，/panel/api/* 前缀）=====

    // Clients
    public const EP_CLIENTS_LIST = '/panel/api/clients/list';
    public const EP_CLIENTS_GET = '/panel/api/clients/get/';          // + {email}
    public const EP_CLIENTS_ADD = '/panel/api/clients/add';
    public const EP_CLIENTS_UPDATE = '/panel/api/clients/update/';     // + {email}
    public const EP_CLIENTS_DEL = '/panel/api/clients/del/';            // + {email}
    public const EP_CLIENTS_ATTACH = '/panel/api/clients/';             // + {email}/attach
    public const EP_CLIENTS_DETACH = '/panel/api/clients/';             // + {email}/detach
    public const EP_CLIENTS_RESET = '/panel/api/clients/resetTraffic/';// + {email}
    public const EP_CLIENTS_LINKS = '/panel/api/clients/links/';        // + {email}
    public const EP_CLIENTS_TRAFFIC = '/panel/api/clients/traffic/';   // + {email}
    public const EP_CLIENTS_ONLINES = '/panel/api/clients/onlines';

    // Inbounds
    public const EP_INBOUNDS_LIST = '/panel/api/inbounds/list';
    public const EP_INBOUNDS_OPTIONS = '/panel/api/inbounds/options';
    public const EP_INBOUNDS_GET = '/panel/api/inbounds/get/';         // + {id}

    // Server
    public const EP_SERVER_STATUS = '/panel/api/server/status';
    public const EP_SERVER_NEW_UUID = '/panel/api/server/getNewUUID';

    // Cookie 登录（兜底）
    private const EP_LOGIN = '/login';
    private const EP_CSRF_TOKEN = '/panel/api/csrf-token';

    protected Client $client;
    protected CookieJar $cookieJar;

    /** client 是否由外部注入（测试 mock）—— 注入的实例不该被 useSharedTransport 换掉 */
    private bool $clientInjected = false;
    protected ?string $baseUrl;
    protected ?string $apiKey;
    protected string $username;
    protected string $password;
    protected bool $verify;

    // 面板指纹成分（登录态缓存键用；客户端拿不到 node id，故不依赖它）
    protected string $scheme;
    protected string $host;
    protected int $port;
    protected string $basePath;

    /** cookie 模式登录态 */
    protected bool $authenticated = false;
    protected ?string $csrfToken = null;

    /**
     * @param array $config scheme|host|port|web_base_path|api_key|username|password
     *                      另可传 http_client（Guzzle Client）注入用于测试 mock。
     */
    public function __construct(array $config)
    {
        $scheme = $config['scheme'] ?? 'https';
        $host = $config['host'] ?? '';
        $port = $config['port'] ?? 443;
        $this->apiKey = ($config['api_key'] ?? null) ?: null;
        $this->username = $config['username'] ?? '';
        $this->password = $config['password'] ?? '';

        $basePath = (string) ($config['web_base_path'] ?? '');
        $basePath = rtrim($basePath, '/');
        if ($basePath !== '' && !str_starts_with($basePath, '/')) {
            $basePath = '/' . $basePath;
        }

        $this->scheme = $scheme;
        $this->host = (string) $host;
        $this->port = (int) $port;
        $this->basePath = $basePath;

        $this->baseUrl = sprintf('%s://%s:%d%s', $scheme, $host, $port, $basePath);
        $this->cookieJar = new CookieJar();

        // verify 默认 true（生产安全）；dev 联调可传 false 绕过自签/不完整证书链
        $this->verify = $config['verify'] ?? true;

        if (isset($config['http_client'])) {
            $this->clientInjected = true;
            $this->client = $config['http_client'];
        } else {
            $this->client = self::makeHttpClient($this->baseUrl, $this->verify);
        }
    }

    /** 按实例配置新建 Guzzle Client；$transport 非空时以它作传输层（见 useSharedTransport）。 */
    private static function makeHttpClient(string $baseUrl, bool $verify, ?callable $transport = null): Client
    {
        $options = [
            'base_uri' => $baseUrl,
            'http_errors' => true,
            'timeout' => self::panelConfig('api_timeout', 15.0),
            'connect_timeout' => self::panelConfig('connect_timeout', 5.0),
            'verify' => $verify,
        ];

        if ($transport !== null) {
            $options['handler'] = $transport;
        }

        return new Client($options);
    }

    /**
     * 新建一个可供多个 client 共用的传输层（并发探测专用，见 useSharedTransport）。
     *
     * 用 Guzzle 默认的 handler 栈：底层是同一个 curl_multi 事件循环，
     * 无 curl 扩展时自动降级，不影响可用性（只是并发收益消失）。
     */
    public static function newSharedTransport(): HandlerStack
    {
        return HandlerStack::create();
    }

    /**
     * 让本实例改用共用传输层 —— **并发探测的前提**。
     *
     * 为什么必须共用：Guzzle 的 CurlMultiHandler 把 curl_multi 句柄藏在实例里，它的 wait()
     * 会一直跑到「自己名下」的请求全部结束。每个节点各建一个 Client 就各有一个 multi 句柄，
     * 逐批 settle 时只能一个接一个 wait —— 表面并发、实际串行（实测 4 节点 × 1s = 4s）。
     * 整批共用一个传输层，请求才会进同一个 curl_multi 句柄真正并行走。
     *
     * 共享的只是「传输层事件循环」；base_uri / verify / headers / cookie jar 等请求级配置仍各自独立，
     * 且默认不开连接共享（transport_sharing=NONE），节点之间不串状态。
     *
     * 注入了 http_client（测试 mock）的实例原样返回，避免把 mock 换掉后误发真实请求。
     */
    public function useSharedTransport(callable $transport): static
    {
        if ($this->clientInjected) {
            return $this;
        }

        $this->client = self::makeHttpClient($this->baseUrl, $this->verify, $transport);

        return $this;
    }

    /**
     * 读 config/panel.php 的超时配置（秒）。
     *
     * ThreeXUiClientTest 属于不引导 Laravel 应用的纯单元测试（无 config 绑定），
     * 此时回落到与 config/panel.php 一致的默认值，保证客户端可脱离应用单独使用。
     */
    private static function panelConfig(string $key, float $default): float
    {
        if (!function_exists('config') || !function_exists('app') || !app()->bound('config')) {
            return $default;
        }

        return (float) config('panel.' . $key, $default);
    }

    /**
     * 由 Node 模型构造（鸭子类型，避免与 M4 硬耦合）。
     * Node 落地后其属性访问器会返回解密后的 api_key/password 明文。
     */
    public static function fromNode(object $node): static
    {
        return new static([
            'scheme' => $node->scheme ?? 'https',
            'host' => $node->host ?? '',
            'port' => $node->port ?? 443,
            'web_base_path' => $node->web_base_path ?? '',
            'api_key' => $node->api_key ?? null,
            'username' => $node->username ?? '',
            'password' => $node->password ?? '',
            'verify' => $node->verify_ssl ?? false,
        ]);
    }

    /** 注入 Guzzle 客户端（测试用）。 */
    public function setClient(Client $client): static
    {
        $this->client = $client;
        $this->clientInjected = true;

        return $this;
    }

    public function getBaseUrl(): string
    {
        return $this->baseUrl;
    }

    // ===== Clients =====

    /** GET /panel/api/clients/list → 全部 client 数组。 */
    public function listClients(): array
    {
        $obj = $this->request('GET', self::EP_CLIENTS_LIST);

        return is_array($obj) ? $obj : [];
    }

    /**
     * GET /panel/api/clients/get/{email}。
     * client 不存在或请求失败时返回 null。
     */
    public function getClient(string $email): ?array
    {
        $obj = $this->requestNullable('GET', self::EP_CLIENTS_GET . rawurlencode($email));

        return is_array($obj) ? $obj : null;
    }

    /**
     * POST /panel/api/clients/add，body {client, inboundIds}。
     * uuid/password 由 3x-ui 服务端生成，成功返回新建的 client（obj）。
     */
    public function addClient(array $client, array $inboundIds): ?array
    {
        $obj = $this->request('POST', self::EP_CLIENTS_ADD, [
            'json' => ['client' => $client, 'inboundIds' => $inboundIds],
        ]);

        if (is_array($obj)) {
            return $obj;
        }

        // 部分 3x-ui 版本 /panel/api/clients/add 成功建 client 但不回传 obj（返回 null）。
        // 此时回查一次确认 client 是否真的建上，避免上层误判"创建失败"而回滚、残留孤儿 client。
        $email = $client['email'] ?? null;

        return $email !== null ? $this->getClient($email) : null;
    }

    /** POST /panel/api/clients/update/{email}，body = 完整 client（替换非 patch）。可指定 inboundIds。 */
    public function updateClient(string $email, array $client, ?int $inboundId = null): bool
    {
        $options = ['json' => $client];
        if ($inboundId !== null) {
            $options['query'] = ['inboundIds' => (string) $inboundId];
        }
        $this->request('POST', self::EP_CLIENTS_UPDATE . rawurlencode($email), $options);

        return true;
    }

    /** POST /panel/api/clients/del/{email}?keepTraffic=0|1，全量删除该 email。 */
    public function deleteClient(string $email, bool $keepTraffic = false, ?int $inboundId = null): bool
    {
        $this->request('POST', self::EP_CLIENTS_DEL . rawurlencode($email), [
            'query' => ['keepTraffic' => $keepTraffic ? '1' : '0'],
        ]);

        return true;
    }

    /** POST /panel/api/clients/{email}/attach，body {inboundIds}（协议切换：挂新）。 */
    public function attachClient(string $email, array $inboundIds): bool
    {
        $this->request('POST', self::EP_CLIENTS_ATTACH . rawurlencode($email) . '/attach', [
            'json' => ['inboundIds' => $inboundIds],
        ]);

        return true;
    }

    /** POST /panel/api/clients/{email}/detach，body {inboundIds}（协议切换：卸旧）。 */
    public function detachClient(string $email, array $inboundIds): bool
    {
        $this->request('POST', self::EP_CLIENTS_DETACH . rawurlencode($email) . '/detach', [
            'json' => ['inboundIds' => $inboundIds],
        ]);

        return true;
    }

    /** POST /panel/api/clients/resetTraffic/{email}。 */
    public function resetClientTraffic(string $email): bool
    {
        $this->request('POST', self::EP_CLIENTS_RESET . rawurlencode($email));

        return true;
    }

    /**
     * GET /panel/api/clients/links/{email} → ["vless://...", ...]（3x-ui 生成完整链接）。
     *
     * 「该 email 在这个面板上不存在」是合法状态（节点重接 / 初始化未建号期间就是缺 client），
     * 降级为 debug 日志并返回空数组，不抛不 report —— 订阅少一个节点链接即可，
     * 不该每次拉订阅都刷一条 ERROR + 全堆栈。
     * 真正的 API 错误（网络/鉴权/5xx/响应格式非法）仍照原样抛出，交由调用方 report。
     */
    public function getClientLinks(string $email): array
    {
        try {
            $obj = $this->request('GET', self::EP_CLIENTS_LINKS . rawurlencode($email));
        } catch (ThreeXUiException $e) {
            if (!self::isClientNotFound($e)) {
                throw $e;
            }

            self::debugLog('3x-ui client 不存在，跳过其订阅链接', [
                'email' => $email,
                'msg' => $e->getMessage(),
            ]);

            return [];
        }

        return is_array($obj) ? array_values(array_filter($obj, 'is_string')) : [];
    }

    /**
     * 判定异常是否为「client 不存在」。
     *
     * 3x-ui 对不存在的 email 返回的是 HTTP 200 + {success:false, msg:"Obtain (record not found)"}，
     * 与普通业务失败的响应结构完全一致，只能按文案识别；各版本措辞不一，故用关键字兜底匹配。
     *
     * 带传输层前缀的异常一律【不算】not-found：网络超时/鉴权失败/5xx/非法响应分别由
     * send()、ensureAuthenticated()、request() 加前缀抛出，这些必须照常上抛。
     */
    private static function isClientNotFound(ThreeXUiException $e): bool
    {
        $msg = $e->getMessage();

        foreach (['3x-ui request failed:', '3x-ui login failed:', 'invalid 3x-ui response:'] as $prefix) {
            if (str_starts_with($msg, $prefix)) {
                return false;
            }
        }

        return (bool) preg_match('/record not found|not found|not exist|不存在/i', $msg);
    }

    /**
     * GET /panel/api/clients/traffic/{email} → {up,down,total,expiryTime,enable,...}。
     * 不存在时返回 null。
     */
    public function getClientTraffic(string $email): ?array
    {
        $obj = $this->requestNullable('GET', self::EP_CLIENTS_TRAFFIC . rawurlencode($email));

        return is_array($obj) ? $obj : null;
    }

    /** GET /panel/api/clients/onlines → 在线 client。 */
    public function getOnlineClients(): array
    {
        $obj = $this->requestNullable('GET', self::EP_CLIENTS_ONLINES);

        return is_array($obj) ? $obj : [];
    }

    // ===== Inbounds =====

    /** GET /panel/api/inbounds/list → 全量（含 clientStats）。 */
    public function listInbounds(): array
    {
        $obj = $this->request('GET', self::EP_INBOUNDS_LIST);

        return is_array($obj) ? $obj : [];
    }

    /**
     * listInbounds() 的异步版（resolve 出 inbound 数组）。
     *
     * 供订阅生成在同进程内并发拉取多个节点（见 SubscriptionService::fetchNodeLinksConcurrently）。
     * **仅支持 api_key（Bearer）模式**：无状态、只加 Authorization 头，可安全并发；
     * cookie 模式要维护登录会话（/login + CSRF），调用方应回退同步 listInbounds()。
     * api_key 为空时返回 rejected promise。
     */
    public function listInboundsAsync(): PromiseInterface
    {
        return $this->getAsyncJson(
            self::EP_INBOUNDS_LIST,
            fn ($obj) => is_array($obj) ? $obj : [],
        );
    }

    /**
     * getClientLinks() 的异步版，语义与同步版逐条对齐：
     * 「该 email 在此面板不存在」→ resolve 成空数组（不抛）；其它错误 reject。
     */
    public function clientLinksAsync(string $email): PromiseInterface
    {
        return $this->getAsyncJson(
            self::EP_CLIENTS_LINKS . rawurlencode($email),
            fn ($obj) => is_array($obj) ? array_values(array_filter($obj, 'is_string')) : [],
            $email,
        );
    }

    /**
     * 异步 GET + {success,obj} 解包（Bearer 模式专用）。
     *
     * 判定口径与同步 request() 完全一致：非 JSON / 缺 success 键 / success=false 都算失败；
     * 区别只是「等这一次往返」换成 promise，由调用方统一 settle，避免逐节点串行等待。
     * 传输层异常统一包成 ThreeXUiException（文案与同步版一致），业务层只需认一种异常。
     *
     * @param  callable  $map  把 obj 映射成调用方要的值
     * @param  ?string  $notFoundEmail  传入时，把该 email 的 not-found 降级为 []（同 getClientLinks）
     */
    private function getAsyncJson(string $path, callable $map, ?string $notFoundEmail = null): PromiseInterface
    {
        if (! $this->apiKey) {
            return Create::rejectionFor(
                new ThreeXUiException('异步请求仅支持 api_key（Bearer）模式，cookie 模式请用同步方法')
            );
        }

        $promise = $this->client->getAsync($this->baseUrl . $path, [
            'headers' => [
                'Accept' => 'application/json',
                'Authorization' => 'Bearer ' . $this->apiKey,
            ],
            'verify' => $this->verify,
            'timeout' => self::panelConfig('api_timeout', 15.0),
            'connect_timeout' => self::panelConfig('connect_timeout', 5.0),
        ]);

        return $promise->then(function ($response) use ($path, $map) {
            $body = (string) $response->getBody();
            $json = json_decode($body, true);

            if (! is_array($json) || ! array_key_exists('success', $json)) {
                throw new ThreeXUiException('invalid 3x-ui response: ' . substr($body, 0, 200));
            }

            if (! $json['success']) {
                throw new ThreeXUiException($json['msg'] ?? '3x-ui request failed');
            }

            return $map($json['obj'] ?? null);
        })->otherwise(function (\Throwable $e) use ($notFoundEmail) {
            // not-found 是合法状态（节点重接 / 初始化未建号期间就是缺 client）：与同步版一样降级为空
            if ($notFoundEmail !== null && $e instanceof ThreeXUiException && self::isClientNotFound($e)) {
                self::debugLog('3x-ui client 不存在，跳过其订阅链接', [
                    'email' => $notFoundEmail,
                    'msg' => $e->getMessage(),
                ]);

                return [];
            }

            if ($e instanceof TransferException) {
                throw new ThreeXUiException('3x-ui request failed: ' . $e->getMessage(), 0, $e);
            }

            throw $e;
        });
    }

    /**
     * 一次拉取所有 inbound 的 client 流量统计。
     * 返回 [inboundId => [clientEmail => ['up'=>int, 'down'=>int], ...], ...]
     *
     * 用于批量流量同步，每个节点只需1次HTTP请求。
     */
    public function getClientStatsGroupedByInbound(): array
    {
        $inbounds = $this->listInbounds();
        $result = [];

        foreach ($inbounds as $inbound) {
            $inboundId = $inbound['id'] ?? null;
            if ($inboundId === null) continue;

            $stats = [];
            foreach ($inbound['clientStats'] ?? [] as $stat) {
                $email = $stat['email'] ?? '';
                if ($email === '') continue;
                $stats[$email] = [
                    'up' => (int) ($stat['up'] ?? 0),
                    'down' => (int) ($stat['down'] ?? 0),
                ];
            }
            $result[$inboundId] = $stats;
        }

        return $result;
    }

    /**
     * 单节点 client 流量，按 email 去重后返回。
     * 返回 [clientEmail => ['up'=>int, 'down'=>int], ...]
     *
     * 去重依据（已查实 3x-ui / Xray 官方源码）：
     * - Xray 按 user>>>email 计数，一个 email 只有一个 counter，跨其所有 inbound 聚合；
     * - 3x-ui 库 client_traffics 表 Email 唯一，节点 API 每个 email 只返回一行。
     * 故同一 email 出现在多个 inbound 下时，携带的是【同一份全局累计值】，
     * 只需取首次出现的值（对齐 3x-ui 内部的 emailTrafficMap 写法），
     * 绝不可跨 inbound 累加（那会让挂 N 个入站的用户被记成 N 倍）。
     * 每个节点只需 1 次 HTTP 请求（listInbounds）。
     *
     * 这是流量同步唯一的去重入口：所有同步路径（cron / 队列 / 管理员点同步 / 用户端同步）
     * 都消费本方法，避免各自重复实现去重而漏改某一处。
     */
    public function getClientStatsByEmail(): array
    {
        $inbounds = $this->listInbounds();
        $result = [];

        foreach ($inbounds as $inbound) {
            foreach ($inbound['clientStats'] ?? [] as $stat) {
                $email = $stat['email'] ?? '';
                if ($email === '') continue;
                if (isset($result[$email])) continue; // 同 email 跨入站重复，只取首次
                $result[$email] = [
                    'up' => (int) ($stat['up'] ?? 0),
                    'down' => (int) ($stat['down'] ?? 0),
                ];
            }
        }

        return $result;
    }

    /** GET /panel/api/inbounds/options → 轻量 picker [{id,protocol,port,tag,remark,tlsFlowCapable}]。 */
    public function inboundOptions(): array
    {
        $obj = $this->request('GET', self::EP_INBOUNDS_OPTIONS);

        return is_array($obj) ? $obj : [];
    }

    /** GET /panel/api/inbounds/get/{id}。 */
    public function getInbound(int $id): ?array
    {
        $obj = $this->requestNullable('GET', self::EP_INBOUNDS_GET . $id);

        return is_array($obj) ? $obj : null;
    }

    // ===== Server =====

    /**
     * GET /panel/api/server/status → 归一化健康结果。
     * 返回 ['ok', 'latencyMs', 'cpu', 'mem', 'xrayState', 'error'(失败时)]。
     * 请求异常不抛，返回 ok=false。
     */
    public function healthCheck(): array
    {
        $start = microtime(true);

        try {
            $result = $this->send('GET', self::EP_SERVER_STATUS, [
                'timeout' => self::panelConfig('healthcheck_timeout', 8.0),
                'connect_timeout' => self::panelConfig('healthcheck_connect_timeout', 5.0),
            ]);
        } catch (\Throwable $e) {
            return $this->unhealthy(0, $e->getMessage());
        }

        $latencyMs = (int) round((microtime(true) - $start) * 1000);
        $json = json_decode($result['body'], true);

        if (!is_array($json) || !($json['success'] ?? false)) {
            return $this->unhealthy($latencyMs, $json['msg'] ?? 'unhealthy');
        }

        $obj = $json['obj'] ?? [];

        return [
            'ok' => true,
            'latencyMs' => $latencyMs,
            'cpu' => $obj['cpu'] ?? null,
            'mem' => $obj['mem'] ?? null,
            'xrayState' => $obj['xray']['state'] ?? null,
        ];
    }

    /**
     * healthCheck() 的异步版：GET /panel/api/server/status，返回 promise 而不阻塞。
     *
     * 供 HealthCheckService::checkMany() 在同进程内并发探测多个节点用。判定规则、超时值、
     * 返回结构均与同步 healthCheck() 完全一致，只是把「等这一次请求」变成「先全发出去再统一等」。
     *
     * **仅支持 api_key（Bearer）模式**：该模式无状态（只加 Authorization 头，不碰 cookie/CSRF/
     * 登录缓存），每个节点经工厂拿到的又是独立 client 实例（独立 Guzzle Client / CookieJar），
     * 天然可并发；cookie 模式要维护登录会话，不适合并发，调用方应回退同步 healthCheck()。
     * api_key 为空时返回 rejected promise（reason 为带说明的 ThreeXUiException），由调用方兜底。
     *
     * 归一化保证：请求失败（网络异常 / 5xx / 解析失败 / success=false）一律 resolve 成
     * ok=false 的结果，**不会把 reject 泄漏给调用方** —— checkMany 只需处理数组，无需 try。
     *
     * @return PromiseInterface resolved 时值为与 healthCheck() 同构的数组
     */
    public function healthCheckAsync(): PromiseInterface
    {
        if (!$this->apiKey) {
            return Create::rejectionFor(
                new ThreeXUiException('healthCheckAsync 仅支持 api_key 模式，cookie 模式请用同步 healthCheck()')
            );
        }

        // 计时点与 healthCheck() 一致：发起请求之前
        $start = microtime(true);

        try {
            $promise = $this->client->getAsync($this->baseUrl . self::EP_SERVER_STATUS, [
                'headers' => [
                    'Accept' => 'application/json',
                    'Authorization' => 'Bearer ' . $this->apiKey,
                ],
                'verify' => $this->verify,
                'timeout' => self::panelConfig('healthcheck_timeout', 8.0),
                'connect_timeout' => self::panelConfig('healthcheck_connect_timeout', 5.0),
            ]);
        } catch (\Throwable $e) {
            // 少数 handler 会在「发起阶段」就同步抛（如 base_uri 非法），此处也归一化为 ok=false
            return Create::promiseFor($this->unhealthy(0, $e->getMessage()));
        }

        return $promise
            ->then(function ($response) use ($start) {
                $latencyMs = (int) round((microtime(true) - $start) * 1000);
                $json = json_decode((string) $response->getBody(), true);

                if (!is_array($json) || !($json['success'] ?? false)) {
                    return $this->unhealthy($latencyMs, $json['msg'] ?? 'unhealthy');
                }

                $obj = $json['obj'] ?? [];

                return [
                    'ok' => true,
                    'latencyMs' => $latencyMs,
                    'cpu' => $obj['cpu'] ?? null,
                    'mem' => $obj['mem'] ?? null,
                    'xrayState' => $obj['xray']['state'] ?? null,
                ];
            })
            // otherwise 兜住两类失败：promise 自身 reject（网络/5xx），以及 then 回调里抛出的异常。
            // 网络失败与同步 healthCheck() 的 catch 分支对齐：latencyMs 记 0。
            ->otherwise(function ($reason) {
                return $this->unhealthy(
                    0,
                    $reason instanceof \Throwable ? $reason->getMessage() : (string) $reason
                );
            });
    }

    /** GET /panel/api/server/getNewUUID → UUID 字符串。 */
    public function newUuid(): string
    {
        $obj = $this->request('GET', self::EP_SERVER_NEW_UUID);

        return is_string($obj) ? $obj : (string) ($obj ?? '');
    }

    // ===== HTTP 底层 =====

    /**
     * 发起请求并解析 {success,obj}。success===true 返回 obj，否则抛 ThreeXUiException(msg)。
     *
     * @return mixed obj
     */
    protected function request(string $method, string $path, array $options = []): mixed
    {
        $result = $this->send($method, $path, $options);
        $json = json_decode($result['body'], true);

        if (!is_array($json) || !array_key_exists('success', $json)) {
            throw new ThreeXUiException('invalid 3x-ui response: ' . substr((string) $result['body'], 0, 200));
        }

        if (!$json['success']) {
            throw new ThreeXUiException($json['msg'] ?? '3x-ui request failed');
        }

        return $json['obj'] ?? null;
    }

    /**
     * 同 request，但 client 不存在 / not-found 时返回 null 而非抛异常。
     * 其它真正的业务错误（success=false 但非 not-found）也返回 null —— 调用方无法区分，
     * 因为 3x-ui 对不存在 client 的返回结构与普通业务失败一致。
     */
    protected function requestNullable(string $method, string $path, array $options = []): mixed
    {
        try {
            $result = $this->send($method, $path, $options);
        } catch (ThreeXUiException $e) {
            return null;
        }

        $json = json_decode($result['body'], true);

        if (!is_array($json)) {
            return null;
        }

        if (!($json['success'] ?? false)) {
            return null;
        }

        return $json['obj'] ?? null;
    }

    /**
     * 执行一次 HTTP 请求，返回原始 ['body'=>string, 'status'=>int]。
     * 负责鉴权头、cookie、CSRF 兜底、Guzzle 异常包装。
     *
     * 会话失效重试：cookie 模式下业务请求回 401/403（登录态被顶掉/过期/面板重启）时，
     * 清掉缓存 → 重新登录一次 → **只重试该请求一次**。Bearer 模式不参与（token 无效就该报错）。
     * 只有 401/403 走这条路，其它状态码/网络错误的重试行为与拆分前完全一致（不重试）。
     */
    protected function send(string $method, string $path, array $options = []): array
    {
        $this->ensureAuthenticated();

        try {
            return $this->execute($method, $path, $options);
        } catch (PanelAuthExpiredException) {
            $this->forgetAuth();
            $this->ensureAuthenticated();

            return $this->execute($method, $path, $options, true);
        }
    }

    /**
     * 单次 HTTP 往返（不含重试）。$isRetry=true 时 401/403 不再触发二次重登，
     * 而是按普通请求失败抛出 —— 避免「重登后仍然 401」被上层当成 not-found 静默吞掉。
     */
    private function execute(string $method, string $path, array $options, bool $isRetry = false): array
    {
        $headers = [
            'Accept' => 'application/json',
        ];

        if ($this->apiKey) {
            $headers['Authorization'] = 'Bearer ' . $this->apiKey;
        }

        // cookie 模式：POST 带 X-CSRF-Token，GET 不需要
        if ($this->csrfToken && strcasecmp($method, 'POST') === 0) {
            $headers['X-CSRF-Token'] = $this->csrfToken;
        }

        $merged = array_merge([
            'headers' => $headers,
            'verify' => $this->verify,
        ], $options);

        if (!$this->apiKey) {
            // cookie 模式共享 cookie jar
            $merged['cookies'] = $this->cookieJar;
        }

        try {
            $response = $this->client->request($method, $this->baseUrl . $path, $merged);
        } catch (RequestException $e) {
            if (!$isRetry && $this->isSessionExpired($e)) {
                throw new PanelAuthExpiredException('3x-ui session expired: ' . $e->getMessage(), 0, $e);
            }

            throw new ThreeXUiException('3x-ui request failed: ' . $e->getMessage(), 0, $e);
        }

        return [
            'status' => $response->getStatusCode(),
            'body' => (string) $response->getBody(),
        ];
    }

    /** cookie 模式下 401/403 = 会话失效（Bearer 模式不适用，token 无效没有「重登」一说）。 */
    private function isSessionExpired(RequestException $e): bool
    {
        if ($this->apiKey) {
            return false;
        }

        $status = $e->getResponse()?->getStatusCode();

        return $status === 401 || $status === 403;
    }

    /**
     * 鉴权入口。
     *
     * Bearer 模式（api_key 非空）→ 直接通过，无状态、**不碰缓存**。
     * cookie 模式 → 优先复用跨进程缓存里的登录态（cookie + csrf），
     * 缓存没有/不可用才真的 POST /login + GET /csrf-token，成功后写回缓存。
     */
    protected function ensureAuthenticated(): void
    {
        if ($this->apiKey || $this->authenticated) {
            return;
        }

        if ($this->restoreAuthFromCache()) {
            $this->authenticated = true;

            return;
        }

        $this->login();
        $this->authenticated = true;
        $this->storeAuthToCache();
    }

    /**
     * 真登录：POST /login（JSON）→ 成功后 GET /panel/api/csrf-token。
     *
     * 两个请求走独立的短超时（config/panel.php），不继承业务请求的 15 秒。
     * catch 的是 TransferException 而非 RequestException —— Guzzle 的 ConnectException
     * （连接超时/拒绝/DNS 失败）继承自 TransferException 而非 RequestException，
     * 用后者接会把它漏给上层，管理员只看到一段英文的 cURL 报错。
     */
    protected function login(): void
    {
        try {
            $resp = $this->client->request('POST', $this->baseUrl . self::EP_LOGIN, [
                'json' => ['username' => $this->username, 'password' => $this->password],
                'cookies' => $this->cookieJar,
                'verify' => $this->verify,
                'headers' => ['Accept' => 'application/json'],
                'timeout' => self::panelConfig('login_timeout', 5.0),
                'connect_timeout' => self::panelConfig('login_connect_timeout', 5.0),
            ]);
        } catch (TransferException $e) {
            throw new ThreeXUiException($this->loginErrorMessage($e), 0, $e);
        }

        $body = json_decode((string) $resp->getBody(), true);

        if (!is_array($body) || !($body['success'] ?? false)) {
            throw new ThreeXUiException('3x-ui login failed: ' . ($body['msg'] ?? 'invalid credentials'));
        }

        // 取 CSRF token（响应体可能是带引号字符串）
        try {
            $csrfResp = $this->client->request('GET', $this->baseUrl . self::EP_CSRF_TOKEN, [
                'cookies' => $this->cookieJar,
                'verify' => $this->verify,
                'headers' => ['Accept' => 'application/json'],
                'timeout' => self::panelConfig('csrf_timeout', 2.0),
                'connect_timeout' => self::panelConfig('csrf_connect_timeout', 2.0),
            ]);
            $csrf = trim((string) $csrfResp->getBody(), "\" \r\n");
            $this->csrfToken = $csrf !== '' ? $csrf : null;
        } catch (TransferException $e) {
            // CSRF 获取失败不致命（部分版本无此端点），继续尝试
            $this->csrfToken = null;
        }
    }

    /** 登录失败信息：连接超时翻译成人话，其余保持原前缀（isClientNotFound 依赖该前缀）。 */
    private function loginErrorMessage(TransferException $e): string
    {
        if ($e instanceof ConnectException) {
            return '3x-ui login failed: 节点登录超时，请检查 host/端口是否可达，或改用 api_key（' . $e->getMessage() . '）';
        }

        return '3x-ui login failed: ' . $e->getMessage();
    }

    // ===== 登录态跨进程缓存 =====

    /**
     * 缓存键：由 scheme/host/port/web_base_path/username 组成的稳定指纹。
     * 刻意不掺 node id —— 客户端（含队列 Job、probeInbounds 里临时构造的实例）拿不到它。
     * 不含 password：改密码后旧会话失效会由 401 → 清缓存 + 重登自愈。
     */
    protected function authCacheKey(): string
    {
        return 'panel-auth:' . sha1(implode("\0", [
            $this->scheme,
            $this->host,
            (string) $this->port,
            $this->basePath,
            $this->username,
        ]));
    }

    /**
     * 缓存仓储；不可用（未引导 Laravel / 未绑定 cache / driver 不支持）时返回 null。
     * 调用方一律把 null 当作「没有缓存」处理 —— 回退为每次登录，绝不因此报错。
     */
    private static function cacheRepository(): ?CacheRepository
    {
        if (!function_exists('app') || !function_exists('config') || !app()->bound('cache')) {
            return null;
        }

        try {
            return app('cache')->store();
        } catch (\Throwable) {
            return null;
        }
    }

    /** 命中缓存则把 cookie + csrf 还原到本实例，返回是否命中。任何异常都算未命中。 */
    protected function restoreAuthFromCache(): bool
    {
        $cache = self::cacheRepository();
        if ($cache === null) {
            return false;
        }

        try {
            $payload = $cache->get($this->authCacheKey());
        } catch (\Throwable) {
            return false;
        }

        if (!is_array($payload) || !is_array($payload['cookies'] ?? null) || $payload['cookies'] === []) {
            return false;
        }

        try {
            $this->cookieJar = new CookieJar(false, $payload['cookies']);
        } catch (\Throwable) {
            return false;
        }

        $csrf = $payload['csrf'] ?? null;
        $this->csrfToken = is_string($csrf) && $csrf !== '' ? $csrf : null;

        return true;
    }

    /** 登录成功后写缓存；写失败只影响下次是否重登，绝不影响本次请求。 */
    protected function storeAuthToCache(): void
    {
        $cache = self::cacheRepository();
        if ($cache === null) {
            return;
        }

        $ttl = (int) self::panelConfig('auth_cache_ttl', 300);
        if ($ttl <= 0) {
            return;
        }

        $cookies = $this->cookieJar->toArray();
        if ($cookies === []) {
            // 没拿到任何 cookie：缓存了也没用（下次照样过不了鉴权）
            return;
        }

        try {
            $cache->put($this->authCacheKey(), [
                'cookies' => $cookies,
                'csrf' => $this->csrfToken,
            ], $ttl);
        } catch (\Throwable) {
            // 静默降级：下次重新登录即可
        }
    }

    /** 清本实例登录态 + 清缓存（401/403 后调用）。 */
    protected function forgetAuth(): void
    {
        $this->authenticated = false;
        $this->csrfToken = null;
        $this->cookieJar = new CookieJar();

        $cache = self::cacheRepository();
        if ($cache === null) {
            return;
        }

        try {
            $cache->forget($this->authCacheKey());
        } catch (\Throwable) {
            // 清不掉也无妨：下次登录会覆盖
        }
    }

    /** 纯单元测试（未引导 Laravel 应用、Facade 无根）下静默跳过，保证本类可脱离框架使用。 */
    private static function debugLog(string $message, array $context = []): void
    {
        if (function_exists('app') && app()->bound('log')) {
            Log::debug($message, $context);
        }
    }

    private function unhealthy(int $latencyMs, string $error): array
    {
        return [
            'ok' => false,
            'latencyMs' => $latencyMs,
            'cpu' => null,
            'mem' => null,
            'xrayState' => null,
            'error' => $error,
        ];
    }
}
