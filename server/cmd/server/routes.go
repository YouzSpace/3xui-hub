package main

// registerRoutes 集中注册所有路由。四段路由与 PHP 逐条对齐：
//   - 用户端   /api/*         （公开 + api.auth + 支付 + 补充端点）
//   - 订阅     /api/sub/:token
//   - 节点端   /api/node-api/*（node.auth）+ 资产 /node-*
//   - 管理端   /admin-api/*   （admin.auth + 认证 + 补充端点）
//
// 依赖全部从 httpx.App 的 DB/Cfg 构造；store 用各包已有的 New*SQLStore。

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/cache"
	"controlhub-server/internal/collector"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/handler/api"
	"controlhub-server/internal/httpx"
	"controlhub-server/internal/metrics"
	"controlhub-server/internal/nodeapi"
	"controlhub-server/internal/scheduler"
	"controlhub-server/internal/subscription"
	"controlhub-server/internal/task"
	"controlhub-server/internal/tgbot"
	"controlhub-server/internal/web"
	"controlhub-server/internal/ws"

	"github.com/gin-gonic/gin"
)

func registerRoutes(r *gin.Engine, app *httpx.App, col *collector.Collector, tgSender *tgbot.Sender, cacheClient *cache.Client) (*ws.Server, *admin.MailDomain) {
	logger := log.Default()
	db := app.DB
	cfg := app.Cfg

	// 安全响应头：挂在最前面，保证后面所有响应（含 NoRoute 的 index.html、真实 404、
	// API JSON）都带上 HSTS / XFO / nosniff / Referrer-Policy 与 Report-Only CSP。
	r.Use(httpx.SecurityHeaders())

	// 操作审计中间件（RecordOperationLog）：挂在全局，内部按路径过滤
	opLog := httpx.NewOperationLogMiddleware(db, logger)
	r.Use(opLog.Handler())

	// ── 用户端 /api/* ──
	apiStore := api.NewSQLStore(db)
	apiCache := api.NewDBCache(db, "controlhub")
	apiMailer := api.NewSMTPMailer(apiStore, cfg, logger)
	apiSwitcher := api.NewPanelSwitcher(apiStore, cfg, logger)

	// 异步队列（P9 task.Queue）：建号后异步下发 client
	asyncSvc := task.NewService(db, nil)
	queue := task.NewQueue(asyncSvc, task.QueueOptions{
		Logf: logger.Printf,
		// 单个单元整链 5 分钟封顶（含退避等待）：超时即收终态、worker 立刻回池。
		// 缺省就有 5 分钟，这里显式写出来是为了让线上调参时一眼找到入口。
		JobTimeout: queueJobTimeout(),
	})
	registerProvisionHandler(queue, db, cfg.AppKey, logger)
	registerDomainHandler(queue, asyncSvc, db, logger)
	// 节点类 Job（初始化 / 入站同步 / 扫描 / 删除清理）。不注册的话节点域派出的
	// async_tasks 会被队列按「不支持的任务类型」丢掉，任务永远 pending。
	registerNodeJobs(queue, db, cfg.AppKey, asyncSvc, logger)
	// 用户节点下发（换套餐 / 支付完成）走后台任务：接口只改库，下发与失败原因进
	// async_tasks，管理员在「异步任务」页能看到哪一步失败。
	userOps := NewUserPanelOps(col, logger)
	userDelivery := NewUserDelivery(queue, asyncSvc, admin.NewAdminUserSQLStore(db), userOps, logger)
	registerUserDelivery(queue, userDelivery)
	// 流量同步 item 的执行体。不注册的话 /sync-traffic 派出的 SyncNodeTrafficJob
	// 会被队列按「不支持的任务类型」直接 failItem，按钮等于白点。
	task.RegisterTrafficSync(queue, asyncSvc,
		task.NewCollectorSyncer(col, db, cfg.AppKey), &task.SQLNodeGate{DB: db})
	queue.Start()

	provisioner := NewQueueProvisioner(db, queue, cfg.AppKey, logger)

	// 用户端下行消息通道：WS 端点与「重置订阅」等管理动作共用同一实例。
	userHub := api.NewUserHub()

	apiHandler := api.New(api.Deps{
		Store:          apiStore,
		Cache:          apiCache,
		Config:         cfg,
		Mailer:         apiMailer,
		Provisioner:    provisioner,
		ProtocolSwitch: apiSwitcher,
		UserHub:        userHub,
		Logger:         logger,
	})
	apiHandler.Register(r)

	// ── 地区调度器（#地区功能-14）──
	// 一个实例覆盖两处用到的地方：支付成功后的自动分配（下面的 AssignUserNodes）
	// 与管理端的迁移引擎（NewSchedulerEngine）。两者都不持有状态，可以共享。
	//
	// 锁的选择：装了 Redis 就用跨实例分布式锁（cache.Client 的 Lock 与 scheduler.Locker
	// 签名一致）；没装则退回进程内锁 —— 单实例部署功能不降级，多实例部署必须装 Redis。
	var assignLocker scheduler.Locker = cacheClient
	if cacheClient.Disabled() {
		assignLocker = newLocalLocker()
	}
	sched := scheduler.New(db, assignLocker)

	// 支付（P7：独立 RegisterPayment，公开 + 鉴权两段）
	payStore := api.NewPaymentSQLStore(db)

	// 管理端 Store 在支付设置 / Telegram / 备份 / 调度四个域间共享（都是 site_configs 读写）。
	adminStore := admin.NewSQLStore(db)
	// 管理端剩余路由（教程 / 优惠码 / 站点配置）仍走 admin.Handler.Register。
	// 代理商城与 Webshare 上游产品线已下线，对应的 handler 与路由已整体移除。
	adminHandler := admin.New(admin.Deps{
		Store:  adminStore,
		Logger: logger,
	})
	paySvc := api.NewPaymentService(payStore, api.PaymentServiceOptions{
		Panel:       NewPaymentPanelSyncer(col, admin.NewAdminUserSQLStore(db), userDelivery, logger),
		TGSender:    tgSender,
		SiteConfigs: apiStore,
		RateGuard:   api.NewDiscountRateGuard(apiCache, apiStore),
		// 套餐生效后按地区给用户绑节点。错误在 PaymentService 内部只记日志，
		// 绝不会把支付回调打回 FAIL（钱已收到）。
		AssignUserNodes: func(ctx context.Context, userID uint64) error {
			_, err := sched.Assign(ctx, userID)
			return err
		},
		// 换套餐后先清掉新套餐不允许地区的绑定，再由上面这步按新套餐补齐（#地区功能-15）。
		PruneUserNodes: sched.PrunePlanRegions,
	}, logger)
	payHandlers := api.NewPaymentHandlers(apiHandler, payStore, paySvc)
	payHandlers.RegisterPayment(r)

	// 用户端补充端点（sync-traffic / invite / discount）
	// sync-traffic 同样接 asyncSvc：用户端也会建 traffic_sync 任务，手搓 INSERT 一样永远 pending。
	userExtras := api.NewUserExtras(apiHandler, db, asyncSvc, payStore, paySvc).
		// 重置订阅后重建节点 client（3x-ui + 裸内核都覆盖）：复用管理员端同一套下发实现。
		WithNodeRebuild(NewUserPanelOps(col, logger), admin.NewAdminUserSQLStore(db))
	extrasAuth := r.Group("", apiHandler.AuthMiddleware())
	userExtras.Register(extrasAuth)

	// ── 订阅 /api/sub/:token ──
	subRepo := subscription.NewSQLRepository(db, cfg.AppKey)
	subHandler := subscription.NewHandler(subRepo)
	subHandler.Register(r)

	// ── 节点端 /api/node-api/* + 资产 ──
	nodeRepo := nodeapi.NewRepo(db)
	nodeHandler := nodeapi.NewHandler(nodeRepo, cfg.AppKey)
	nodeHandler.RegisterRoutes(r)

	// 节点资产（公开，无鉴权）。默认路径对齐 NewAssetHandler 的空默认（../assets）：
	// 资产自持在发布仓根目录 assets/（Xray 内核 zip + agent 二进制 + 安装脚本），
	// 相对 server/ 工作目录一层 .. 即可定位。历史默认 storage/app/node-bin 是错的（少了 xray 且多套 node-bin）。
	assetRoot := envOr("NODE_ASSET_ROOT", filepath.Join("..", "assets"))
	geoRoot := envOr("NODE_GEO_ROOT", filepath.Join("..", "assets", "geo"))
	assetHandler := nodeapi.NewAssetHandler(assetRoot, geoRoot)
	assetHandler.RegisterRoutes(r)

	// ── 节点 WS 长连接通道（agent 实时通道；PHP 是独立 node:ws-serve 进程监听 8091，
	// Go 侧内置到主进程：ServeHTTP 挂 /node-ws，Run 起轮询/心跳节拍由 main 启动）──
	wsServer := ws.NewServer(ws.Options{
		Backend:  nodeRepo,
		Outbound: &ws.SqOutboundStore{DB: db},
		AppKey:   cfg.AppKey,
		Logf:     logger.Printf,
	})
	r.GET("/node-ws", func(c *gin.Context) {
		wsServer.ServeHTTP(c.Writer, c.Request)
	})

	// ── 管理端 /admin-api/* ──
	adminGroup := r.Group("/admin-api")
	adminGroup.Use(app.AdminAuth(func(format string, args ...any) {
		logger.Printf(format, args...)
	}))

	// 管理端认证（login 在 AdminAuth 之外）
	adminAuth := admin.NewAdminAuthRoutes(app, db)
	adminAuth.RegisterLogin(r) // POST /admin-api/login
	adminAuth.RegisterAuthed(adminGroup)

	// 教程 / 优惠码 / 站点配置（代理商城与 Webshare 上游已下线，不再挂载）。
	adminHandler.Register(adminGroup)

	// 邮件域：必须在用户域之前构造 —— 续费/重置流量要用它解锁自动通知的防重标记，
	// collector 的流量用尽通知也要走同一个邮件队列。
	mailDomain := admin.NewMailDomain(admin.MailDeps{
		Store:  admin.NewMailSQLStoreWithKey(db, cfg.AppKey),
		AppKey: cfg.AppKey,
		Logger: logger,
	})
	mailDomain.Register(adminGroup)

	// 验证码发信（注册 / 找回密码）统一走账号池：把邮件域注入给 api 侧的发信器。
	// 必须在 mailDomain 构造之后、apiHandler 已存在之后调用（两处都在本函数内，顺序安全）。
	apiMailer.SetPoolMailer(mailDomain)

	// 单用户通知（流量用尽通知 + 解锁防重标记）：collector 与用户域共用一份实现。
	sceneNotifier := task.NewSceneNotifier(task.CommandDeps{
		DB:        db,
		Logf:      logger.Printf,
		MailQueue: &taskMailSender{q: mailDomain.MailQueue()},
	})
	if col != nil {
		col.SetMailNotifier(sceneNotifier)
	}

	// 用户/套餐/订单/统计域（P8a）
	userDomain := admin.NewUserDomain(admin.UserDeps{
		Users:   admin.NewAdminUserSQLStore(db),
		Plans:   admin.NewPlanSQLStore(db),
		Orders:  admin.NewOrderSQLStore(db),
		Stats:   admin.NewStatsSQLStore(db),
		Traffic: admin.NewTrafficSQLStore(db),
		Config: admin.UserConfig{
			BcryptRounds:     cfg.BcryptRounds,
			AppTimezone:      "UTC",
			BusinessTimezone: "Asia/Shanghai",
		},
		Ops:      userOps,
		Delivery: userDelivery,
		Notifier: api.NewUserNotifier(userHub, logger),
		Alerts:   alertRestorer{notifier: sceneNotifier},
		Logger:   logger,
	})
	userDomain.Register(adminGroup)

	// 节点域（P8b）
	nodeStore := admin.NewNodeSQLStore(db, cfg.AppKey)
	// 地区域要先于节点域构造：入站列表要回显归属地区名（#入站级分组），
	// NodeDomain.Regions 直接复用同一个无状态 Store。
	regionStore := admin.NewRegionSQLStore(db)
	nodeDomain := admin.NewNodeDomain(admin.NodeDeps{
		Store:   nodeStore,
		Regions: regionStore,
		Ops:     admin.NewRealNodeOps(admin.NodeConfig{AppKey: cfg.AppKey, PanelURL: os.Getenv("APP_URL")}, nodeStore, logger),
		// 节点任务（初始化 / 入站同步 / 删除清理）必须派发，否则永远 pending。
		Tasks: newNodeTaskDispatcher(asyncSvc, queue),
		Config: admin.NodeConfig{
			AppKey:     cfg.AppKey,
			PanelURL:   os.Getenv("APP_URL"),
			PanelURLV6: os.Getenv("APP_URL_V6"),
			// 端口跳跃首次可能要先装 iptables，等待上限比 reality_scan 长。
			HopWait:       envInt("PANEL_HOP_WAIT", 0),
			CertIssueWait: envInt("PANEL_CERT_ISSUE_WAIT", 0),
			// sysctl / BBR 下发：写 conf + modprobe + sysctl -p + 读回，正常 1-2 秒。
			SysctlWait: envInt("PANEL_SYSCTL_WAIT", 0),
		},
		Logger: logger,
	})
	nodeDomain.Register(adminGroup)

	// 地区域（#地区功能）：地区 CRUD + 节点归属/容量 + 调度看板总览。
	// 只做管理与查询；迁移/均衡/自动分配在各自的域，不在这里。
	//
	// Metrics 只给 GET /nodes/:node/capacity-recommend 读 node_metrics 用：
	// Store 无状态（只包一个连接池），与 main.go 的采样器实例共用同一张表，
	// 各自 new 一个不影响采样副作用。
	regionDomain := admin.NewRegionDomain(admin.RegionDeps{
		Store:   regionStore,
		Metrics: metrics.NewStore(db),
		Logger:  logger,
	})
	regionDomain.Register(adminGroup)

	// 配置模板（#地区功能-19）：标准入站/出站/路由模板 CRUD + 一键套用到 xray 节点。
	admin.NewXrayTemplateDomain(admin.XrayTemplateDeps{DB: db, AppKey: cfg.AppKey, Logger: logger}).Register(adminGroup)

	// 迁移与调度（#地区功能-10）：故障迁移 / 批量均衡 / 迁移历史 / 撤销 / 后台任务进度。
	// locker 与支付分配共用同一个（Redis 有就用 Redis，没有就是进程内锁）；迁移/均衡
	// 本身不抢分配锁，传进去是为了让 Assign/AssignRegion 在任何入口都能用同一套互斥。
	admin.NewMigrationDomain(admin.MigrationDeps{DB: db, Engine: admin.NewSchedulerEngine(db, assignLocker), Logger: logger}).Register(adminGroup)

	// 支付配置域
	paymentDomain := admin.NewPaymentDomain(admin.PaymentDeps{
		Store: admin.NewPaymentSQLStore(db),
		// 全局支付设置（订单支付有效期）读写 site_configs，与自动备份开关同一套存储。
		Settings: adminStore,
	})
	paymentDomain.Register(adminGroup)

	// 余额域（兑换码 + 用户余额管理）
	balanceDomain := admin.NewBalanceDomain(admin.BalanceDeps{
		Store:  admin.NewBalanceSQLStore(db),
		Logger: logger,
	})
	balanceDomain.Register(adminGroup)

	// 邀请返佣域（只读：返佣流水列表）
	affiliateDomain := admin.NewAffiliateDomain(admin.AffiliateDeps{
		Store:  admin.NewAffiliateSQLStore(db),
		Logger: logger,
	})
	affiliateDomain.Register(adminGroup)

	// Telegram 机器人（通知渠道：节点离线/支付到账/备份推送）
	telegramDomain := admin.NewTelegramDomain(admin.TelegramDeps{
		Store:  adminStore,
		AppKey: cfg.AppKey,
		Logger: logger,
	})
	telegramDomain.Register(adminGroup)

	// 域名域
	domainDomain := admin.NewDomainsDomain(admin.DomainDeps{
		Store:  admin.NewDomainSQLStore(db),
		Tasks:  NewDomainTaskSubmitter(asyncSvc),
		Logger: logger,
	})
	domainDomain.Register(adminGroup)

	// 备份域
	backupDomain := admin.NewBackupDomain(admin.BackupDeps{
		Store: admin.NewBackupSQLStore(db),
		Tools: admin.NewOSBackupTools(),
		Zip:   admin.NewOSBackupZip(),
		Files: admin.NewOSBackupFiles(),
		// 自动备份开关读写 site_configs（键 sched_db-backup_enabled，与调度器共用）。
		Settings: adminStore,
		DB: admin.BackupDBConfig{
			Host:     cfg.DB.Host,
			Port:     cfg.DB.Port,
			Database: cfg.DB.Database,
			Username: cfg.DB.Username,
			Password: cfg.DB.Password,
		},
		EnvPath: envOr("CONFIG_ENV", filepath.Join("..", ".env")),
		Logger:  logger,
	})
	backupDomain.Register(adminGroup)

	// 系统域
	// geo 目录必须与上面 assetHandler 的 geoRoot 同一个，否则「更新写 A、分发读 B」，
	// 节点下载到的文件与指令里的 sha256 对不上，每次同步都失败。
	geoService := admin.NewGeodataService(db, envOr("GEODATA_DIR", filepath.Join("..", "assets", "geo")), nil)
	// 系统状态页要与调度器共用同一套 when 条件（条件不满足的任务不跑、也不显示）。
	taskLookups := &task.SQLLookups{DB: db}
	systemDomain := admin.NewSystemDomain(admin.SystemDeps{
		Store:     admin.NewSystemSQLStore(db),
		Geo:       geoService,
		MailQueue: mailDomain,                // 监控页「排队中/运行中/失败」读内存队列水位，不读恒 0 的 jobs 表
		TaskQueue: taskQueueStater{q: queue}, // 节点任务队列同理：这条队列也不落表
		Condition: task.ConditionChecker(taskLookups),
		Logger:    logger,
	})
	systemDomain.Register(adminGroup)
	systemDomain.RegisterGeodata(adminGroup)

	// 管理端补充端点（monitor / sync-traffic / async-tasks）
	// sync-traffic 必须接 asyncSvc（已挂 queue）：只有 FindOrCreateTrafficSync 会拆 items
	// 并派发 SyncNodeTrafficJob，手搓 INSERT 的任务永远 pending。
	adminExtras := admin.NewAdminExtras(db, asyncSvc)
	adminExtras.Register(adminGroup)

	// 前端静态服务：生产由 go:embed 打进二进制（-tags=embed），本地开发从磁盘读 dist。
	// dist 为空（只有 .gitkeep 占位、前端没构建）时返回 nil，下面的 NoRoute 退回 API 口径 404。
	frontend := web.FrontendServer()
	// 静态资源分支的包装器只构造一次：它在 NoRoute 里按请求复用，
	// 不必每次请求都重新包一层（闭包分配虽小，但每个扫描器请求都会走到）。
	var staticFrontend http.Handler
	if frontend != nil {
		staticFrontend = httpx.SPAFallbackNotFound(frontend)
	}

	// 管理端 404 兜底（PHP bootstrap/app.php 口径）
	//
	// 三类路径分开处理（2026-11-06 修「任意路径都回 200 + index.html」）：
	//   1. /admin-api/*           → 管理端 JSON 口径 404
	//   2. /api、/node-* 等后端路径 → API 口径 404（见 web.IsAPIPath）
	//   3. 其它：只有**真正的前端路由**才回 index.html；静态资源类路径（带扩展名、
	//      隐藏文件/目录）交给前端处理器后把 index.html 兜底改写成真实 404。
	//      这样 /robots.txt、/.env、/backup.zip、/.git/config 不再伪装成 200。
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if len(p) >= 11 && p[:11] == "/admin-api/" {
			writeAPI404(c)
			return
		}
		// /api、/admin-api、/node-* 是后端路径，不参与 SPA fallback
		if frontend == nil || web.IsAPIPath(p) {
			writeAPI404(c)
			return
		}
		if httpx.IsFrontendPath(p) {
			// 真正的前端路由：回 index.html，交给 Vue Router
			// （刷新 /admin/users、/dashboard/tutorial/3 这类深链不再 404）
			frontend.ServeHTTP(c.Writer, c.Request)
			return
		}
		// 静态资源类路径：dist 里有就照常发，没有就是真实 404
		staticFrontend.ServeHTTP(c.Writer, c.Request)
	})

	return wsServer, mailDomain
}

// queueJobTimeout 读 QUEUE_JOB_TIMEOUT_MINUTES（分钟，缺省 5）。
//
// 为什么要它：旧实现的重试退避是 worker 内 sleep，合计可达 81 分钟，4 个卡住的单元
// 就能占满默认 4 个 worker、把后续任务全部饿死（2026-10-31 实测：裸内核同步子项把
// worker 占满后，用户手动同步的 7 个子项一次都没跑上，全被清扫成「任务超时未完成」）。
// 现在整条重试链按此值封顶，到点收终态并释放 worker。
func queueJobTimeout() time.Duration {
	const def = 5 * time.Minute
	raw := strings.TrimSpace(os.Getenv("QUEUE_JOB_TIMEOUT_MINUTES"))
	if raw == "" {
		return def
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		return def
	}
	return time.Duration(minutes) * time.Minute
}

// writeAPI404 输出 API 口径的 404（HTTP 200 + code 404）。
func writeAPI404(c *gin.Context) {
	c.JSON(200, gin.H{"code": 404, "msg": "接口不存在", "data": nil})
}
