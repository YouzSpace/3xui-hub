<?php

return [
    /*
     | 3x-ui 面板 API 超时（秒）。
     |
     | 跨境节点（HK 实测 RTT 699~903ms，间歇 TLS 卡死 10s+）下，原先写死的 2 秒
     | connect_timeout 会让节点初始化批量失败、流量同步误报，故全部改为可 env 覆盖。
     | 仅调超时，不动重试与并发行为。
     */

    // 第三方订阅拉取的 CA bundle（HTTPS 证书校验用）。
    // 默认空 = 走 PHP/curl 系统自带 CA（绝大多数 Linux 生产服务器自带，行为不变）。
    // 部署侧拉机场订阅若报 cURL error 60（SSL 校验拿不到根证书，常见于 Windows 开发机 /
    // 精简镜像缺 CA bundle），把路径指到一个 CA bundle（openssl 的 ca-bundle.crt /
    // ca-certificates.crt 等），可用 env 覆盖；指向的文件不存在则自动回退系统校验。
    'third_party_ca_bundle' => env('PANEL_THIRD_PARTY_CA_BUNDLE', ''),

    // 常规 API 请求总超时（ThreeXUiClient 构造 Guzzle 客户端时的默认值）
    'api_timeout' => (float) env('PANEL_API_TIMEOUT', 15),

    // 常规 API 请求连接超时
    'connect_timeout' => (float) env('PANEL_CONNECT_TIMEOUT', 5),

    // 健康检查（/panel/api/server/status）总超时
    'healthcheck_timeout' => (float) env('PANEL_HEALTHCHECK_TIMEOUT', 8),

    // 健康检查连接超时
    'healthcheck_connect_timeout' => (float) env('PANEL_HEALTHCHECK_CONNECT_TIMEOUT', 5),

    // 健康检查并发数：同时探测的节点数上限（同进程 Guzzle 并发，默认 10）
    'healthcheck_concurrency' => (int) env('PANEL_HEALTHCHECK_CONCURRENCY', 10),

    /*
     | 订阅生成的并发数：同时拉取的 3x-ui 节点数上限（同进程 Guzzle 并发，默认 10）。
     |
     | 每个节点固定 2 次请求（listInbounds 拿端口 + 取该用户链接），之前是逐节点串行、
     | 且每个入站再单独 getInbound 一次 —— 节点一多，用户等订阅的时间就是各节点耗时之和。
     | 只有 api_key（Bearer）节点走并发；cookie 模式要维护登录会话，仍逐个串行。
     | <= 0 视为 1（退化成每批一个，不并发）。
     */
    'subscription_concurrency' => (int) env('PANEL_SUBSCRIPTION_CONCURRENCY', 10),

    /*
     | agent 资产清单（manifest.json）路径。
     |
     | 空 = 用面板自带的 storage/app/xray/manifest.json（随仓库分发，含 agent 版本 + sha256）。
     | 只有需要把清单放到别处（多面板共用一份资产 / 测试）时才覆盖。
     */
    'agent_manifest_path' => (string) env('PANEL_AGENT_MANIFEST_PATH', ''),

    /*
     | php-fpm 并发（pm.max_children）调优参数。
     |
     | 用途：`php artisan fpm:tune`（已装机器一键调，不重装）+ install.sh 的 tune_php_fpm（新装机器）。
     | 发行版默认 www.conf 是 pm.max_children=5，面板一有并发就排队，是用户量上来后第一个卡的环节。
     |
     | 公式：内存 × memory_ratio ÷ process_memory_mb，与 核数 × cpu_multiplier 取小，夹到 [min,max]。
     | 单进程内存用固定估值：安装时 php-fpm 刚起来量不到真实 RSS，且这个值只用于定一个安全起点。
     |
     | ⚠️ install.sh 里有一份等价的 shell 实现（安装早期 artisan 还不可用），改公式时两边一起改。
     */
    'fpm' => [
        'process_memory_mb' => (int) env('PANEL_FPM_PROCESS_MEMORY_MB', 50),
        'memory_ratio'      => (float) env('PANEL_FPM_MEMORY_RATIO', 0.5),
        'cpu_multiplier'    => (int) env('PANEL_FPM_CPU_MULTIPLIER', 4),
        'min_children'      => (int) env('PANEL_FPM_MIN_CHILDREN', 10),
        'max_children'      => (int) env('PANEL_FPM_MAX_CHILDREN', 100),

        // 预制档位：后台若做「档位」按钮，只允许执行这些固定值（不做自由输入）
        'presets' => [
            'small'  => 10,
            'medium' => 40,
            'large'  => 100,
        ],

        // 池配置 ↔ systemd 服务名（按顺序探测，取第一个存在的）
        'targets' => [
            ['conf' => '/etc/php/8.4/fpm/pool.d/www.conf', 'service' => 'php8.4-fpm'],
            ['conf' => '/etc/php-fpm.d/www.conf', 'service' => 'php-fpm'],
            ['conf' => '/etc/opt/remi/php84/php-fpm.d/www.conf', 'service' => 'php84-php-fpm'],
        ],
    ],

    /*
     | cookie 模式登录态缓存（秒）。
     |
     | 未配 api_key 的节点每次操作都要先 POST /login + GET /csrf-token；Web 请求与队列
     | Job 是两个进程，各自的实例内标记救不了对方，于是同一个面板会被反复登录。
     | 这里把登录后的 cookie + csrf token 按「面板指纹」缓存起来跨进程共用，
     | TTL 内同一个面板只登录一次。设为 0 表示关闭缓存（回退为每次登录）。
     */
    'auth_cache_ttl' => (int) env('PANEL_AUTH_CACHE_TTL', 300),

    /*
     | 登录 / 取 CSRF 的独立超时（秒）。
     |
     | 这两个请求是为了「拿到业务请求的门票」，不该继承业务请求 15 秒的上限：
     | 面板不可达时应尽快失败并给出人话提示，而不是把 15 秒耗在一个登录上。
     */
    'login_timeout' => (float) env('PANEL_LOGIN_TIMEOUT', 5),
    'login_connect_timeout' => (float) env('PANEL_LOGIN_CONNECT_TIMEOUT', 5),
    'csrf_timeout' => (float) env('PANEL_CSRF_TIMEOUT', 2),
    'csrf_connect_timeout' => (float) env('PANEL_CSRF_CONNECT_TIMEOUT', 2),

    /*
     | 节点类 Job（NodeInboundScanJob / NodeInboundSyncJob / NodeClientsCleanupJob /
     | NodeInitUserJob）投递到的队列名。
     |
     | **默认值必须是 'default'**，与引入本开关之前的行为逐字一致。
     | 理由：Job 被派到一条没有任何 worker 监听的队列 = 任务永远不执行，日志页会一直停在
     | pending，直到 async-task-timeout 把它判失败 —— 这比「节点任务和定时任务抢同一条队列」
     | 严重得多，而且是静默的。所以队列名只能由部署侧显式打开：
     | 在 .env 里设 PANEL_NODE_OPS_QUEUE=node-ops，并另起一个 worker 监听该队列
     | （php artisan queue:work --queue=node-ops ...），两边都到位才生效。
     | 只改一边（改了 .env 没起 worker，或起了 worker 没改 .env）时，默认值保证不会把任务派丢。
     */
    'node_ops_queue' => env('PANEL_NODE_OPS_QUEUE') ?: 'default',

    /*
     | 邮件 Job（SendMailJob）的队列名。**默认值必须是 'default'**，与引入本开关之前
     | 的行为逐字一致；只有部署侧两件事都到位才生效：.env 设 PANEL_MAIL_QUEUE=mail，
     | 且另起一个 worker 监听该队列（install.sh 会一并装好 3xui-hub-queue-mail.service）。
     |
     | 为什么需要：邮件任务（自动通知 / 批量群发 / 每月定时）与控制类定时任务
     | （封禁检查、健康检查、通知扫描）共用 default 队列时，一条 queue:work 进程
     | 同一时刻只跑一个 Job —— 一次 500 人群发（限速默认关闭）会让 worker 连续忙
     | 十几分钟，期间所有 5 分钟定时任务只能排队，封禁/健康检查随之延迟。
     | 拆成两条队列后各排各的，与 node_ops_queue 同一套规矩。
     */
    'mail_queue' => env('PANEL_MAIL_QUEUE') ?: 'default',

    /*
     | Reality 目标探测（入站表单「检测目标」）同步等待节点回执的上限（秒）。
     |
     | 面板入队 reality_scan 指令后轮询等回执；节点侧并发探测，正常 5 秒内出结果。
     | 必须留在 PHP max_execution_time（常见 30s）之内，故默认 18。
     | <= 0 表示不等待（直接返回 pending），仅供测试。
     */
    'reality_scan_wait' => (float) env('PANEL_REALITY_SCAN_WAIT', 18),

    /*
     | 用户端接口限流（防刷）。
     |
     | 这几个值管理员可以在后台「邮箱配置 → 安全限制」页自己调：改的是 SiteConfig 里的同名
     | rate_* 键，**SiteConfig 有非空值就覆盖这里的默认值**；这里只是兜底（站点还没配置过、
     | 配置被清空、或部署侧想用 env 一把锁死默认值）。任何一项 <= 0 都表示该项不限。
     |
     | 规则见 App\Services\RateGuardService（5 个用户端接口共用一个服务）。
     */
    'ratelimit' => [
        // 同一邮箱两次收验证码的最小间隔（秒）
        'rate_code_interval'       => (int) env('PANEL_RATE_CODE_INTERVAL', 60),
        // 同一邮箱 24 小时内最多收验证码（次）
        'rate_code_per_day'        => (int) env('PANEL_RATE_CODE_PER_DAY', 10),
        // 同一 IP 每小时最多请求发码接口（次，两个发码接口合计）
        'rate_code_ip_hourly'      => (int) env('PANEL_RATE_CODE_IP_HOURLY', 20),
        // 同一邮箱+IP 验证码最多试错（次）
        'rate_verify_max_attempts' => (int) env('PANEL_RATE_VERIFY_MAX_ATTEMPTS', 10),
        // 试错超限后锁定（秒）
        'rate_verify_lock_seconds' => (int) env('PANEL_RATE_VERIFY_LOCK_SECONDS', 300),
        // 邮箱密码登录失败（次）触发锁定
        'rate_login_max_attempts'  => (int) env('PANEL_RATE_LOGIN_MAX_ATTEMPTS', 5),
        // 登录锁定时长（秒）
        'rate_login_lock_seconds'  => (int) env('PANEL_RATE_LOGIN_LOCK_SECONDS', 300),
        // 同一 IP 每小时最多注册（个）
        'rate_register_ip_hourly'  => (int) env('PANEL_RATE_REGISTER_IP_HOURLY', 10),

        // 邀请码/优惠码校验：单用户每分钟请求上限
        'rate_discount_per_minute'      => (int) env('PANEL_RATE_DISCOUNT_PER_MINUTE', 10),
        // 邀请码/优惠码校验：单用户每日失败上限
        'rate_discount_fail_per_day'    => (int) env('PANEL_RATE_DISCOUNT_FAIL_PER_DAY', 50),
        // 邀请码/优惠码校验：单 IP 每小时失败上限
        'rate_discount_ip_fail_hourly'  => (int) env('PANEL_RATE_DISCOUNT_IP_FAIL_HOURLY', 100),
        // 连续失败多少次触发锁定
        'rate_discount_max_attempts'    => (int) env('PANEL_RATE_DISCOUNT_MAX_ATTEMPTS', 10),
        // 锁定时长（秒）
        'rate_discount_lock_seconds'    => (int) env('PANEL_RATE_DISCOUNT_LOCK_SECONDS', 900),
    ],
];
