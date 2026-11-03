// Command server 是 ControlHub 面板的 Go 后端入口。
// 与 PHP 后端并存期监听独立端口；前端/MySQL/API 契约不变。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/config"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/httpx"
	"controlhub-server/internal/store"
	"controlhub-server/internal/task"
	"controlhub-server/internal/tgbot"
	"controlhub-server/internal/ws"

	"github.com/gin-gonic/gin"
)

// Version 是构建版本号，由发布流程通过 -ldflags "-X main.Version=vX.Y.Z" 注入；
// 本地/开发环境未注入时保持 "dev"。用于启动日志与 --version 排查「服务器跑的哪一版」。
var Version = "dev"

func main() {
	// --version：打印构建版本后退出（不连数据库）。供 3hub update 读「当前跑的哪一版」，
	// 版本号由发布流程 -ldflags "-X main.Version=vX.Y.Z" 注入，本地/开发是 "dev"。
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(Version)
		os.Exit(0)
	}

	app, db, err := bootstrap()
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	if db != nil {
		defer db.Close()
	}

	// Telegram 通知（节点离线/支付到账/备份推送，全局共享一个实例）
	tgSender := tgbot.NewSender(db, app.Cfg.AppKey, log.Printf)

	// collector（P6 采集层 + P8a 用户域下发的驱动构造，全局共享一个实例）
	col, colErr := collector.New(collector.Options{
		DB:       db,
		AppKey:   app.Cfg.AppKey,
		Now:      time.Now,
		Logf:     log.Printf,
		TGSender: tgSender,
	})
	if colErr != nil {
		log.Fatalf("初始化 collector 失败: %v", colErr)
	}

	addr := envOr("LISTEN_ADDR", ":8080")
	router, wsServer, mailDomain := newRouter(app, col, tgSender)
	srv := &http.Server{
		Handler:           router,
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 优雅退出：收到 SIGINT/SIGTERM 后停止接新连接，给在途请求 15 秒收尾。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 启动调度器（P9 常驻 goroutine，随服务启动/优雅退出一起关闭）
	startScheduler(ctx, app, db, col, mailDomain, tgSender)

	// 启动节点 WS 通道（轮询/心跳节拍；握手由 /node-ws 路由 ServeHTTP 处理）
	if wsServer != nil {
		go wsServer.Run(ctx)
	}

	go func() {
		log.Printf("ControlHub Go 后端启动（版本 %s），监听 %s", Version, addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭服务…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("服务关闭异常: %v", err)
	}
}

// bootstrap 加载配置并连接数据库。
// CONFIG_ENV 指定 .env 路径，缺省复用仓库根 .env（含 APP_KEY 与 DB_*）。
func bootstrap() (*httpx.App, *sql.DB, error) {
	cfg, err := config.Load(envOr("CONFIG_ENV", config.DefaultEnvPath))
	if err != nil {
		return nil, nil, err
	}

	db, err := store.Open(cfg.DB)
	if err != nil {
		return nil, nil, err
	}

	// 增量建表：老用户升级时 install.sh 跳过 schema.sql，这里幂等补齐新增表。
	if err := ensureSchema(db); err != nil {
		return nil, nil, err
	}

	return httpx.NewApp(db, cfg), db, nil
}

// newRouter 构建 gin 引擎并注册路由。单独拆出来便于单测/健康检查复用。
// 返回 gin 引擎 + 节点 WS 服务（WS 的 Run 需由 main 用 ctx 启动）。
func newRouter(app *httpx.App, col *collector.Collector, tgSender *tgbot.Sender) (*gin.Engine, *ws.Server, *admin.MailDomain) {
	if app.Cfg.AppEnv == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// 路由注册随迁移阶段逐步接入（handler/api、handler/admin）。
	// /api/ping 由 api.Handler.Register 注册（对齐 PHP 契约）。
	wsServer, mailDomain := registerRoutes(r, app, col, tgSender)

	return r, wsServer, mailDomain
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt 读整型环境变量；未设置或非法/非正数时返回 def（由调用方补默认）。
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// backupTimeoutMinutes 读 BACKUP_TIMEOUT_MINUTES（默认 30 分钟）。
// 非法值或 <=0 回落到默认，避免一次手滑把备份超时设成 0 秒。
func backupTimeoutMinutes() time.Duration {
	v := os.Getenv("BACKUP_TIMEOUT_MINUTES")
	if v == "" {
		return 30 * time.Minute
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(n) * time.Minute
}

// startScheduler 组装并启动定时调度器（P9 常驻 goroutine）。
// 队列分离默认关闭（全部进 default 队列）。
func startScheduler(ctx context.Context, app *httpx.App, db *sql.DB, col *collector.Collector, mailDomain *admin.MailDomain, tgSender *tgbot.Sender) {
	logger := log.Default()
	cfg := app.Cfg

	// 异步任务服务（async-task-timeout 用）
	asyncSvc := task.NewService(db, nil)

	// 节点 store（BumpConfigVersion 复用 Node::bumpXrayConfigVersion 语义）
	nodeStore := admin.NewNodeSQLStore(db, cfg.AppKey)

	// 节点域：定时任务里的 WARP 换 IP（xray:warp-rotate）需要它。
	// 只用到库操作与 CF 注册，不注册路由，故与 routes.go 各建一份（都是无状态包装）。
	nodeDomain := admin.NewNodeDomain(admin.NodeDeps{
		Store:  nodeStore,
		Ops:    admin.NewRealNodeOps(admin.NodeConfig{AppKey: cfg.AppKey, PanelURL: os.Getenv("APP_URL")}, nodeStore, logger),
		Config: admin.NodeConfig{AppKey: cfg.AppKey, PanelURL: os.Getenv("APP_URL"), PanelURLV6: os.Getenv("APP_URL_V6")},
		Logger: logger,
	})

	// 一次性运维入口：把「有入站引用、且不由面板机 acme.sh 维护」的证书立刻重签一遍后退出
	// （定时任务要等窗口期，这个入口给「刚接手一批手工导入证书」的场景用）：
	//
	//	controlhub-server --renew-certs-once
	//
	// 与定时任务同一套实现（同一份入库 + bump 逻辑），跑完退出，不启 HTTP 与调度。
	if len(os.Args) > 1 && os.Args[1] == "--renew-certs-once" {
		res, err := nodeDomain.RenewAllReferencedCertificates(context.Background())
		log.Printf("[certs] 一次性续签：检查=%d 处理=%d 成功=%d 失败=%d 跳过=%d bump节点=%d",
			res.Checked, res.Due, res.Renewed, res.Failed, res.Skipped, res.Bumped)
		for _, r := range res.Reasons {
			log.Printf("[certs]   %s", r)
		}
		if err != nil {
			log.Printf("[certs] 续签出错: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// 一次性运维入口：本机证书目录 → 证书库同步（acme.sh 续签后跑；内容变了才 bump 节点）：
	//
	//	controlhub-server --sync-local-certs
	if len(os.Args) > 1 && os.Args[1] == "--sync-local-certs" {
		res, err := nodeDomain.SyncLocalCertificates(context.Background())
		log.Printf("[certs] 本机目录同步：目录=%s 扫描=%d 新增=%d 更新=%d 跳过=%d bump节点=%d",
			res.Dir, res.Scanned, res.Created, res.Updated, res.Skipped, res.Bumped)
		for _, r := range res.Reasons {
			log.Printf("[certs]   %s", r)
		}
		if err != nil {
			log.Printf("[certs] 同步出错: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// 一次性运维入口：逐台比对 agent 版本并**只下发**升级指令（`3hub update` 收尾自动调用）：
	//
	//	controlhub-server --upgrade-agents
	//
	// 只入队、不等回执：节点取走指令后自己下载 → 校验 → 替换 → 重启，面板不在这儿干等
	// （离线/失败的节点由进程内的周期补跑收拾，见 startScheduler 里的 runAgentUpgradeReconciler）。
	// 输出格式刻意做成「一台一行」，好让 `3hub update` 直接透传给用户：
	//   AGENT 最新=1.2.1 检查=5 已下发=4 失败=0 跳过=1
	//   AGENT   US2    1.1.0 → 1.2.1   已下发（节点取走后自动升级）
	//   AGENT   KR01   1.2.1           跳过（已是最新 1.2.1）
	// 只有「连指令都下不去」才算失败（退出码 1），节点执行结果不在这里等。
	if len(os.Args) > 1 && os.Args[1] == "--upgrade-agents" {
		res, err := nodeDomain.UpgradeAllAgents(context.Background())
		if err != nil {
			log.Printf("AGENT 出错: %v", err)
			os.Exit(1)
		}
		log.Printf("AGENT 最新=%s 检查=%d 已下发=%d 失败=%d 跳过=%d",
			res.Latest, res.Total, res.Dispatched, res.Failed, res.Skipped)
		for _, o := range res.Outcomes {
			from := o.From
			if from == "" {
				from = "未上报"
			}
			switch o.Action {
			case "dispatched":
				log.Printf("AGENT   %-8s %s → %s   已下发（节点取走后自动升级）", o.NodeName, from, o.To)
			case "failed":
				log.Printf("AGENT   %-8s %s → %s   失败：%s", o.NodeName, from, o.To, o.Reason)
			default:
				log.Printf("AGENT   %-8s %s   跳过（%s）", o.NodeName, from, o.Reason)
			}
		}
		if res.HasProblem() {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Geo 数据服务（geodata:update 命令用）。
	// 目录必须与 assets.go 的 geoRoot 同一个（../assets/geo），
	// 否则更新写到 server/storage、分发却从 assets 读 → 节点侧 sha256 校验必失败。
	geoService := admin.NewGeodataService(db, envOr("GEODATA_DIR", filepath.Join("..", "assets", "geo")), nil)

	// 命令运行器（13 个 artisan 命令）
	cmdRunner := task.NewCommandRunner(task.CommandDeps{
		DB:            db,
		AppKey:        cfg.AppKey,
		Logf:          logger.Printf,
		Now:           time.Now,
		BackupBin:     envOr("BACKUP_MYSQLDUMP_BIN", "mysqldump"),
		BackupDir:     envOr("BACKUP_DIR", "/var/backups/3xui-hub"),
		BackupKeep:    7,
		BackupTimeout: backupTimeoutMinutes(),
		TGSender:      tgSender,
		DBHost:        cfg.DB.Host,
		DBPort:        cfg.DB.Port,
		DBName:        cfg.DB.Database,
		DBUser:        cfg.DB.Username,
		DBPassword:    cfg.DB.Password,

		BumpConfigVersion: nodeStore.BumpConfigVersion,
		Geodata:           &geodataAdapter{inner: geoService},
		MailQueue:         &taskMailSender{q: mailDomain.MailQueue()},
		RotateWarp:        nodeDomain.RotateWarp,
		ToggleClient:      col.ToggleClient,
		ReconcileNodes:    reconcileAdapter(col),
	})

	// 调度器（DB 用于写 scheduled_task_runs 运行记录与 worker 心跳）
	sched := task.NewScheduler(task.Options{
		Now:  time.Now,
		Logf: logger.Printf,
		DB:   db,
	})

	if err := sched.Register(task.DefaultJobs(task.Deps{
		Collector: col,
		Tasks:     asyncSvc,
		Commands:  cmdRunner,
		Lookups:   &task.SQLLookups{DB: db},
		// 本机证书目录同步（acme.sh 续签 → TLS 证书库）：复用节点域的实现，
		// 只用到库操作（加密写私钥）与读目录，不需要 HTTP。
		SyncLocalCerts: certSyncAdapter(nodeDomain.SyncLocalCertificates),
		// 远程证书自动续签（到期前 N 天让对应节点重签，回执 PEM 覆盖证书库并 bump 配置版本）。
		RenewNodeCerts: certRenewAdapter(nodeDomain.RenewDueCertificates),
		Logf:           logger.Printf,
	})...); err != nil {
		logger.Printf("[schedule] 注册调度任务失败: %v", err)
		return
	}

	sched.Start(ctx)
	logger.Printf("[schedule] 调度器已启动（%d 条任务）", len(sched.Entries()))

	// 节点 agent 自动补跑：离线/失败的节点上线后自动再试（同一目标版本总共 3 次机会、
	// 两次之间至少间隔 10 分钟，可经 AGENT_UPGRADE_MAX_ATTEMPTS / _RETRY_INTERVAL_SECONDS 调）。
	// 为什么用进程内 ticker 而不是调度器里的 DB 任务：现有安装的 task 表里没有这一条，
	// 加 DB 任务就得迁移；ticker 升级到即生效，而它只做「比对版本 + 入队」，开销极小。
	go runAgentUpgradeReconciler(ctx, nodeDomain, logger)
}

// runAgentUpgradeReconciler 周期补跑节点 agent 自动升级，只下发、不等回执。
func runAgentUpgradeReconciler(ctx context.Context, d *admin.NodeDomain, logger *log.Logger) {
	const interval = 5 * time.Minute
	timer := time.NewTimer(time.Minute) // 启动 1 分钟后首跑，先让心跳/清单就位
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			res, err := d.ReconcileAgentUpgrades(ctx)
			switch {
			case err != nil:
				logger.Printf("[agent] 自动补跑出错: %v", err)
			case res.Dispatched > 0 || res.Failed > 0:
				logger.Printf("[agent] 自动补跑：检查=%d 已下发=%d 失败=%d 跳过=%d（目标 %s）",
					res.Total, res.Dispatched, res.Failed, res.Skipped, res.Latest)
			}
			timer.Reset(interval)
		}
	}
}

// certSyncAdapter 把 admin 的「本机证书目录同步」结果转成调度层的口径。
//
// 为什么要绕一层：task 包不 import handler/admin（会反向依赖），调度项只认自己的小结构。
func certSyncAdapter(sync func(context.Context) (admin.LocalCertSyncResult, error)) func(context.Context) (task.LocalCertSyncSummary, error) {
	if sync == nil {
		return nil
	}
	return func(ctx context.Context) (task.LocalCertSyncSummary, error) {
		res, err := sync(ctx)
		if err != nil {
			return task.LocalCertSyncSummary{}, err
		}
		return task.LocalCertSyncSummary{
			Dir:     res.Dir,
			Scanned: res.Scanned,
			Created: res.Created,
			Updated: res.Updated,
			Skipped: res.Skipped,
			Bumped:  res.Bumped,
			Reasons: res.Reasons,
		}, nil
	}
}

// certRenewAdapter 把 admin 的「远程证书自动续签」结果转成调度层的口径。
func certRenewAdapter(renew func(context.Context) (admin.CertRenewResult, error)) func(context.Context) (task.CertRenewSummary, error) {
	if renew == nil {
		return nil
	}
	return func(ctx context.Context) (task.CertRenewSummary, error) {
		res, err := renew(ctx)
		if err != nil {
			return task.CertRenewSummary{}, err
		}
		return task.CertRenewSummary{
			Checked: res.Checked,
			Due:     res.Due,
			Renewed: res.Renewed,
			Failed:  res.Failed,
			Skipped: res.Skipped,
			Bumped:  res.Bumped,
			Reasons: res.Reasons,
		}, nil
	}
}
