package main

// registerRoutes 集中注册所有路由。四段路由与 PHP 逐条对齐：
//   - 用户端   /api/*         （公开 + api.auth + 支付 + 补充端点）
//   - 订阅     /api/sub/:token
//   - 节点端   /api/node-api/*（node.auth）+ 资产 /node-*
//   - 管理端   /admin-api/*   （admin.auth + 认证 + 补充端点）
//
// 依赖全部从 httpx.App 的 DB/Cfg 构造；store 用各包已有的 New*SQLStore。

import (
	"log"
	"os"
	"path/filepath"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/handler/api"
	"controlhub-server/internal/httpx"
	"controlhub-server/internal/nodeapi"
	"controlhub-server/internal/subscription"
	"controlhub-server/internal/task"
	"controlhub-server/internal/tgbot"
	"controlhub-server/internal/ws"

	"github.com/gin-gonic/gin"
)

func registerRoutes(r *gin.Engine, app *httpx.App, col *collector.Collector, tgSender *tgbot.Sender) (*ws.Server, *admin.MailDomain) {
	logger := log.Default()
	db := app.DB
	cfg := app.Cfg

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
	})
	registerProvisionHandler(queue, db, cfg.AppKey, logger)
	registerDomainHandler(queue, asyncSvc, db, logger)
	// 节点类 Job（初始化 / 入站同步 / 扫描 / 删除清理）。不注册的话节点域派出的
	// async_tasks 会被队列按「不支持的任务类型」丢掉，任务永远 pending。
	registerNodeJobs(queue, db, cfg.AppKey, asyncSvc, logger)
	// 流量同步 item 的执行体。不注册的话 /sync-traffic 派出的 SyncNodeTrafficJob
	// 会被队列按「不支持的任务类型」直接 failItem，按钮等于白点。
	task.RegisterTrafficSync(queue, asyncSvc,
		task.NewCollectorSyncer(col, db, cfg.AppKey), &task.SQLNodeGate{DB: db})
	queue.Start()

	provisioner := NewQueueProvisioner(db, queue, cfg.AppKey, logger)

	apiHandler := api.New(api.Deps{
		Store:          apiStore,
		Cache:          apiCache,
		Config:         cfg,
		Mailer:         apiMailer,
		Provisioner:    provisioner,
		ProtocolSwitch: apiSwitcher,
		Logger:         logger,
	})
	apiHandler.Register(r)

	// 支付（P7：独立 RegisterPayment，公开 + 鉴权两段）
	payStore := api.NewPaymentSQLStore(db)
	paySvc := api.NewPaymentService(payStore, api.PaymentServiceOptions{
		Panel:       NewPaymentPanelSyncer(col, admin.NewAdminUserSQLStore(db), logger),
		ProxyOrders: api.NewProxyCompleter(db, logger),
		TGSender:    tgSender,
		SiteConfigs: apiStore,
		RateGuard:   api.NewDiscountRateGuard(apiCache, apiStore),
	}, logger)
	payHandlers := api.NewPaymentHandlers(apiHandler, payStore, paySvc)
	payHandlers.RegisterPayment(r)

	// 用户端补充端点（sync-traffic / invite / discount / proxy-store）
	// sync-traffic 同样接 asyncSvc：用户端也会建 traffic_sync 任务，手搓 INSERT 一样永远 pending。
	userExtras := api.NewUserExtras(apiHandler, db, asyncSvc, payStore, paySvc)
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

	// 节点资产（公开，无鉴权）。默认路径对齐 NewAssetHandler 的空默认（../backend/storage/app/xray）：
	// 资产自持在发布仓 backend/storage/app/xray/（Xray 内核 zip + agent 二进制 + 安装脚本），
	// 相对 server/ 工作目录一层 .. 即可定位。历史默认 storage/app/node-bin 是错的（少了 xray 且多套 node-bin）。
	assetRoot := envOr("NODE_ASSET_ROOT", filepath.Join("..", "backend", "storage", "app", "xray"))
	geoRoot := envOr("NODE_GEO_ROOT", filepath.Join("..", "backend", "storage", "app", "xray", "geo"))
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

	// 商城域（P8d）
	adminStore := admin.NewSQLStore(db, cfg.AppKey)
	webshareClient := admin.NewHTTPWebshareClient(admin.Config{
		WebshareProxy:    os.Getenv("WEBSHARE_PROXY"),
		WebshareCABundle: os.Getenv("WEBSHARE_CA_BUNDLE"),
	})
	adminHandler := admin.New(admin.Deps{
		Store:    adminStore,
		Upstream: webshareClient,
		Config: admin.Config{
			WebshareProxy:    os.Getenv("WEBSHARE_PROXY"),
			WebshareCABundle: os.Getenv("WEBSHARE_CA_BUNDLE"),
		},
	})
	adminHandler.Register(adminGroup)

	// 邮件域：必须在用户域之前构造 —— 续费/重置流量要用它解锁自动通知的防重标记，
	// collector 的流量用尽通知也要走同一个邮件队列。
	mailDomain := admin.NewMailDomain(admin.MailDeps{
		Store:  admin.NewMailSQLStoreWithKey(db, cfg.AppKey),
		AppKey: cfg.AppKey,
		Logger: logger,
	})
	mailDomain.Register(adminGroup)

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
		Ops:    NewUserPanelOps(col, logger),
		Alerts: alertRestorer{notifier: sceneNotifier},
		Logger: logger,
	})
	userDomain.Register(adminGroup)

	// 节点域（P8b）
	nodeStore := admin.NewNodeSQLStore(db, cfg.AppKey)
	nodeDomain := admin.NewNodeDomain(admin.NodeDeps{
		Store: nodeStore,
		Ops:   admin.NewRealNodeOps(admin.NodeConfig{AppKey: cfg.AppKey, PanelURL: os.Getenv("APP_URL")}, nodeStore, logger),
		// 节点任务（初始化 / 入站同步 / 删除清理）必须派发，否则永远 pending。
		Tasks: newNodeTaskDispatcher(asyncSvc, queue),
		Config: admin.NodeConfig{
			AppKey:   cfg.AppKey,
			PanelURL: os.Getenv("APP_URL"),
		},
		Logger: logger,
	})
	nodeDomain.Register(adminGroup)

	// 支付配置域
	paymentDomain := admin.NewPaymentDomain(admin.PaymentDeps{
		Store: admin.NewPaymentSQLStore(db),
	})
	paymentDomain.Register(adminGroup)

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
		DB: admin.BackupDBConfig{
			Host:     cfg.DB.Host,
			Port:     cfg.DB.Port,
			Database: cfg.DB.Database,
			Username: cfg.DB.Username,
			Password: cfg.DB.Password,
		},
		EnvPath: envOr("CONFIG_ENV", "backend/.env"),
		Logger:  logger,
	})
	backupDomain.Register(adminGroup)

	// 系统域
	// geo 目录必须与上面 assetHandler 的 geoRoot 同一个，否则「更新写 A、分发读 B」，
	// 节点下载到的文件与指令里的 sha256 对不上，每次同步都失败。
	geoService := admin.NewGeodataService(db, envOr("GEODATA_DIR", filepath.Join("..", "backend", "storage", "app", "xray", "geo")), nil)
	systemDomain := admin.NewSystemDomain(admin.SystemDeps{
		Store:  admin.NewSystemSQLStore(db),
		Geo:    geoService,
		Logger: logger,
	})
	systemDomain.Register(adminGroup)
	systemDomain.RegisterGeodata(adminGroup)

	// 管理端补充端点（monitor / sync-traffic / async-tasks）
	// sync-traffic 必须接 asyncSvc（已挂 queue）：只有 FindOrCreateTrafficSync 会拆 items
	// 并派发 SyncNodeTrafficJob，手搓 INSERT 的任务永远 pending。
	adminExtras := admin.NewAdminExtras(db, asyncSvc)
	adminExtras.Register(adminGroup)

	// 管理端 404 兜底（PHP bootstrap/app.php 口径）
	r.NoRoute(func(c *gin.Context) {
		if len(c.Request.URL.Path) >= 11 && c.Request.URL.Path[:11] == "/admin-api/" {
			adminHandler.NoRouteHandler()(c)
			return
		}
		// 其他路径返回 API 口径 404
		writeAPI404(c)
	})

	return wsServer, mailDomain
}

// writeAPI404 输出 API 口径的 404（HTTP 200 + code 404）。
func writeAPI404(c *gin.Context) {
	c.JSON(200, gin.H{"code": 404, "msg": "接口不存在", "data": nil})
}
