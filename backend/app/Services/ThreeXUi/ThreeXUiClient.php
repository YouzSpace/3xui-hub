<?php

namespace App\Services\ThreeXUi;

use GuzzleHttp\Client;
use GuzzleHttp\Cookie\CookieJar;
use GuzzleHttp\Exception\RequestException;

/**
 * 3x-ui v3.x HTTP API 客户端（M5 核心）。
 *
 * 鉴权：优先 Bearer API Token（nodes.api_key），Bearer 跳过 CSRF、无状态。
 * 仅当未提供 api_key 时回退 cookie+CSRF 登录流程（/login + /panel/api/csrf-token）。
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
    protected ?string $baseUrl;
    protected ?string $apiKey;
    protected string $username;
    protected string $password;
    protected bool $verify;

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

        $this->baseUrl = sprintf('%s://%s:%d%s', $scheme, $host, $port, $basePath);
        $this->cookieJar = new CookieJar();

        // verify 默认 true（生产安全）；dev 联调可传 false 绕过自签/不完整证书链
        $this->verify = $config['verify'] ?? true;

        $this->client = $config['http_client'] ?? new Client([
            'base_uri' => $this->baseUrl,
            'http_errors' => true,
            'timeout' => 10.0,
            'verify' => $this->verify,
        ]);
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

        return is_array($obj) ? $obj : null;
    }

    /** POST /panel/api/clients/update/{email}，body = 完整 client（替换非 patch）。 */
    public function updateClient(string $email, array $client): bool
    {
        $this->request('POST', self::EP_CLIENTS_UPDATE . rawurlencode($email), [
            'json' => $client,
        ]);

        return true;
    }

    /** POST /panel/api/clients/del/{email}?keepTraffic=0|1。 */
    public function deleteClient(string $email, bool $keepTraffic = false): bool
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

    /** GET /panel/api/clients/links/{email} → ["vless://...", ...]（3x-ui 生成完整链接）。 */
    public function getClientLinks(string $email): array
    {
        $obj = $this->request('GET', self::EP_CLIENTS_LINKS . rawurlencode($email));

        return is_array($obj) ? array_values(array_filter($obj, 'is_string')) : [];
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
            $result = $this->send('GET', self::EP_SERVER_STATUS);
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
     * 执行一次 HTTP 请求，返回原始 ['body'=>string, 'status'=>int, 'latencyMs'=>int]。
     * 负责鉴权头、cookie、CSRF 兜底、Guzzle 异常包装。
     */
    protected function send(string $method, string $path, array $options = []): array
    {
        $this->ensureAuthenticated();

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
            throw new ThreeXUiException('3x-ui request failed: ' . $e->getMessage(), 0, $e);
        }

        return [
            'status' => $response->getStatusCode(),
            'body' => (string) $response->getBody(),
        ];
    }

    /**
     * Bearer 模式（api_key 非空）→ 直接通过，无状态。
     * cookie 模式 → /login JSON + 取 cookie，再 GET /panel/api/csrf-token 取 CSRF token。
     */
    protected function ensureAuthenticated(): void
    {
        if ($this->apiKey || $this->authenticated) {
            return;
        }

        try {
            $resp = $this->client->request('POST', $this->baseUrl . self::EP_LOGIN, [
                'json' => ['username' => $this->username, 'password' => $this->password],
                'cookies' => $this->cookieJar,
                'verify' => $this->verify,
                'headers' => ['Accept' => 'application/json'],
            ]);
        } catch (RequestException $e) {
            throw new ThreeXUiException('3x-ui login failed: ' . $e->getMessage(), 0, $e);
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
            ]);
            $csrf = trim((string) $csrfResp->getBody(), "\" \r\n");
            $this->csrfToken = $csrf !== '' ? $csrf : null;
        } catch (RequestException $e) {
            // CSRF 获取失败不致命（部分版本无此端点），继续尝试
            $this->csrfToken = null;
        }

        $this->authenticated = true;
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
