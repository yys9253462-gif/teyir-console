package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nodeops/server/internal/auth"
	"nodeops/server/internal/collector"
	"nodeops/server/internal/config"
	"nodeops/server/internal/executor"
	"nodeops/server/internal/geo"
	apihttp "nodeops/server/internal/httpapi"
	"nodeops/server/internal/ippurity"
	"nodeops/server/internal/secrets"
	"nodeops/server/internal/speedtest"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + port + "/api/health")
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	box, err := secrets.NewBox(cfg.CredentialKey)
	if err != nil {
		logger.Error("credential box unavailable", "error", err)
		os.Exit(1)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		logger.Error("database configuration failed", "error", err)
		os.Exit(1)
	}
	applyPoolDefaults(poolCfg, cfg.DatabaseURL)
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		logger.Error("database configuration failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := pool.Ping(pingCtx); err != nil {
		pingCancel()
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	pingCancel()

	seedCtx, seedCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := seedAdminUser(seedCtx, pool, logger, cfg.SeedAdminEmail, cfg.SeedAdminPassword); err != nil {
		logger.Warn("seed admin user failed", "error", err)
	}
	seedCancel()

	// IP 检测密钥：环境变量是初始值，系统设置里填的会覆盖（handler.LoadPurityKeys）。
	purityKeys := ippurity.NewKeyStore(map[string]string{
		ippurity.KeyIPAPIIs: cfg.PurityAPIKey,
		ippurity.KeyIPQS:    cfg.PurityIPQSKey,
	})
	handler := apihttp.New(logger, cfg.AllowedOrigins, pool, apihttp.Options{
		ViewerRestrictSubscriptions: cfg.ViewerRestrictSubscriptions,
		TrustedProxyCIDRs:           cfg.TrustedProxyCIDRs,
		JWTSecret:                   cfg.JWTSecret,
		JWTTTL:                      time.Duration(cfg.JWTTTLMinutes) * time.Minute,
		CredentialBox:               box,
		CollectorSeconds:            cfg.CollectorSeconds,
		ExecutorSeconds:             cfg.ExecutorSeconds,
		Geo:                         buildGeoResolver(cfg, logger),
		Purity:                      buildPurityResolver(cfg, logger, purityKeys),
		PurityKeys:                  purityKeys,
		Prober:                      speedtest.NewProber(logger, pool, box),
	})
	// 节点账号对账试跑：只列出会被删除的残留，不改任何东西，打印 JSON 后退出。
	//   docker compose run --rm --no-deps api --reconcile-dry-run
	if len(os.Args) == 2 && os.Args[1] == "--reconcile-dry-run" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		out, _ := json.MarshalIndent(handler.ReconcileNodeAccounts(ctx, true), "", "  ")
		fmt.Println(string(out))
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--gapfill-dry-run" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, _ := json.MarshalIndent(handler.ProvisioningGapReport(ctx), "", "  ")
		fmt.Println(string(out))
		return
	}
	server := &http.Server{
		Addr:              ":" + itoa(cfg.Port),
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// ReadTimeout 覆盖整个请求体读取；最大的 JSON 请求体是 1MB，30 秒足够。
		// 终端 WebSocket 经 gorilla Upgrade 劫持连接后会清掉截止时间，不受影响。
		// 不设 WriteTimeout：安装/下发等长请求要持续写响应。
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 告警通知 goroutine 在按静默期睡眠；进程退出时若不通知它们，
	// 这些睡眠中的告警会无声丢失。用一个通道让 handler 能感知关闭。
	handler.SetShutdownNotify(shutdownCtx.Done())
	var metricsCollector *collector.Collector
	var collectorCancel context.CancelFunc = func() {}
	if cfg.EnableCollector {
		var collectorCtx context.Context
		collectorCtx, collectorCancel = context.WithCancel(context.Background())
		defer collectorCancel()
		metricsCollector = collector.NewCollector(logger, pool, cfg.CollectorSeconds, box)
		metricsCollector.Start(collectorCtx)
		logger.Info("collector started", "interval_seconds", cfg.CollectorSeconds)
	}
	// Alert reconciliation is a periodic state sync, not a request handler, so it
	// runs on its own timer independent of whether metric collection is enabled.
	// Doing it in the GET handler meant every browser poll wrote to the database.
	alertCtx, alertCancel := context.WithCancel(context.Background())
	defer alertCancel()
	go runAlertReconciler(alertCtx, logger, handler, time.Duration(cfg.CollectorSeconds)*time.Second)
	executorCtx, executorCancel := context.WithCancel(context.Background())
	defer executorCancel()
	taskExecutor := executor.New(logger, pool, cfg.ExecutorSeconds, box)
	go taskExecutor.Run(executorCtx)
	logger.Info("executor started", "interval_seconds", cfg.ExecutorSeconds)
	// Subscription expiry is a wall-clock event, not a request. The sweeper
	// lapses and purges on a timer so a shop-sold subscription stops working at
	// its expiry even if nobody opens the panel.
	//
	// ⚠️ 为什么从 1 小时改成 2 分钟（2026-10-04 真实连接实测）：
	//   原值 time.Hour 意味着**过了到期时间后最长 1 小时内，买家手里那份
	//   已经下载好的配置依然能连** —— 因为回收动作只在这个定时器里做。
	//   实测：把 expires_at 改成过去之后连续取样 **182 秒**，
	//   节点账号仍在、**真实连接仍是 204**。对「到期就不能用」的要求来说，
	//   这是真实的收入漏洞（每次都能白用最多一小时）。
	//
	//   为什么 2 分钟：
	//     * 一次 sweep 只是几条 SQL，且**只在确实有过期项时**才去调节点；
	//       空跑几乎零成本，所以轮询可以密一些。
	//     * 2 分钟把「到期→断线」窗口压到远小于客户端自己的订阅刷新周期。
	//     * 真正兜底的是 subscriptionFeed 里的**即时回收**（见 store.go），
	//       本定时器只负责「没人来拉订阅」的那种情况。
	subscriptionCtx, subscriptionCancel := context.WithCancel(context.Background())
	defer subscriptionCancel()
	handler.StartSubscriptionSweeper(subscriptionCtx, 2*time.Minute)
	handler.StartSubscriptionSync(subscriptionCtx)
	handler.StartHostJobJournal(subscriptionCtx)
	// 每日节点账号对账：删掉节点上已没有订阅对应的 sub_ 账号（见 node_account_reconcile.go）。
	handler.StartNodeAccountReconcile(subscriptionCtx)
	// 跨节点统一设备上限：合并各节点在线 IP（第一阶段只观察，见 devicelimit.go）。
	handler.StartDeviceLimitWatch(subscriptionCtx)
	logger.Info("subscription sweeper started", "interval", "2m")

	// 节点整机流量计量 + 月度限额（超额停服务）+ 内核版本刷新。见 nodetraffic.go。
	trafficCtx, trafficCancel := context.WithCancel(context.Background())
	defer trafficCancel()
	handler.StartNodeTrafficMeter(trafficCtx)
	purityLoadCtx, purityLoadCancel := context.WithTimeout(context.Background(), 15*time.Second)
	handler.LoadPurityKeys(purityLoadCtx)
	purityLoadCancel()
	handler.BackfillIPNative(trafficCtx)
	handler.StartBlockChecker(trafficCtx)
	handler.StartTelegramBot(trafficCtx)
	handler.StartScheduler(trafficCtx)
	logger.Info("node traffic meter started", "interval", "1m")

	// 时序采样表保留 30 天（见 retention.go）。
	handler.StartSampleRetention(trafficCtx)

	// 节点监控：网络延迟探测 + 大陆三网测速。周期与采集器/执行器都不同
	// （三网线路质量是慢变量），所以单独一个调度器与它自己的开关。
	monitoringCtx, monitoringCancel := context.WithCancel(context.Background())
	defer monitoringCancel()
	handler.StartMonitoringScheduler(monitoringCtx, time.Duration(cfg.SpeedtestIntervalMinutes)*time.Minute)
	// Shutdown 一调用 ListenAndServe 就立即返回 ErrServerClosed；主函数必须
	// 等 Shutdown 把在途请求排空后再退出，否则 10 秒排空窗口形同虚设。
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-shutdownCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("api server starting", "port", cfg.Port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("api server failed", "error", err)
		os.Exit(1)
	}
	<-shutdownDone
	// HTTP 排空后立即取消全部后台周期任务，避免进程关闭前继续发起新任务或在连接池关闭瞬间打断落盘。
	alertCancel()
	subscriptionCancel()
	trafficCancel()
	monitoringCancel()
	collectorCancel()
	// 优雅停止执行器与采集器并限时等待在途写回完成，避免进程退出打断正在落盘的任务结果。
	executorCancel()
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		taskExecutor.Wait()
		if metricsCollector != nil {
			metricsCollector.Wait()
		}
	}()
	select {
	case <-drainDone:
	case <-time.After(8 * time.Second):
		logger.Warn("background worker shutdown drain timed out")
	}
	logger.Info("api server stopped")
}

// runAlertReconciler drives the periodic alert state sync. It runs one pass
// immediately so a fresh boot does not show an empty alert centre, then repeats on
// the same cadence as the collector. Failures are logged and retried next tick
// rather than exiting: a transient database error must not take the API down.
func runAlertReconciler(ctx context.Context, logger *slog.Logger, handler *apihttp.Server, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := handler.ReconcileAlerts(ctx); err != nil {
			if ctx.Err() == nil {
				logger.Warn("alert reconcile failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func seedAdminUser(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, email, password string) error {
	// 新口令策略只用于首次创建，避免存量种子配置导致升级后无法启动。
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email=$1)`, email).Scan(&exists); err != nil {
		return err
	}
	if exists {
		logger.Info("seed admin user already exists, skip", "email", email)
		return nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `INSERT INTO users (email, password_hash, role) VALUES ($1, $2, 'admin') ON CONFLICT (email) DO NOTHING`, email, hash)
	if err != nil {
		return err
	}
	logger.Info("seed admin user ready", "email", email, "rows", tag.RowsAffected())
	return nil
}

// buildGeoResolver 装配「IP → 地区/商家」识别器，用于新增节点/面板时自动命名。
//
// 三条容错原则（对应 geo 包里的详细论证）：
//
//  1. **离线库加载失败绝不能让服务起不来。** 没有库只是"识别要走公网 API"，
//     而节点管理、面板对接、开户交付这些主功能一个都不依赖它。
//     一个锦上添花的功能无权拒绝服务。所以这里只 Warn。
//  2. 什么都没配（既无库又关了 API）时返回 nil，调用方据此静默跳过识别，
//     行为退回加这个功能之前的状态 —— 手填。
//  3. 返回的 Resolver 内部对"查不到"是宽容的（返回空 Info 而非错误），
//     所以调用点不需要写 if err。
func buildGeoResolver(cfg config.Config, logger *slog.Logger) *geo.Resolver {
	opts := geo.Options{
		// 缓存必要的理由：公网 API 有速率限制（ip-api 免费版 45 次/分钟），
		// 而"导入一批节点"天然是连发查询，缓存把同一台机器压成一次出网。
		Cache: geo.NewCache(24*time.Hour, 4096),
	}

	if cfg.GeoOfflineDB != "" {
		db, err := geo.LoadOfflineFile(cfg.GeoOfflineDB)
		if err != nil {
			// Warn 而不是 Fatal：见上面第 1 条。
			logger.Warn("geo offline database unavailable, will fall back to public API",
				"path", cfg.GeoOfflineDB, "error", err)
		} else {
			opts.DB = db
			logger.Info("geo offline database loaded",
				"path", cfg.GeoOfflineDB, "entries", db.Len())
		}
	}

	if cfg.GeoEnableAPI {
		provider := geo.IPAPI{BaseURL: cfg.GeoAPIBaseURL}
		opts.API = provider
		logger.Info("geo public API fallback enabled", "provider", provider.Name())
	}

	if opts.DB == nil && opts.API == nil {
		logger.Info("geo identification disabled: no offline database and GEO_ENABLE_API=false")
		return nil
	}
	return geo.NewResolver(opts)
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}

// buildPurityResolver 装配「IP → 纯净度（连接类型/双ISP/风险评分）」检测器。
//
// 三条容错原则与 buildGeoResolver 完全一致（理由见那一函数与 ippurity 包）：
//
//  1. 离线库加载失败只 Warn，绝不让服务起不来。
//  2. 既无离线库也无 API 密钥时返回 nil，纯净度功能整体静默关闭，
//     节点其余功能不受影响；端点会返回明确的「未启用」。
//  3. Resolver 内部对"查不到/出网失败"是宽容的，调用点不需要写 if err。
//
// 与 geo 的一处差别：**这里没有开关式的 GEO_ENABLE_API 对应物**。
// geo 默认开公网 API（无需密钥），而纯净度 API 要密钥才可用 ——
// 密钥本身就是开关，多一个布尔只会带来"开了但没密钥"的矛盾状态。
func buildPurityResolver(cfg config.Config, logger *slog.Logger, keys *ippurity.KeyStore) *ippurity.Resolver {
	opts := ippurity.Options{}
	if cfg.PurityOfflineDB != "" {
		db, err := ippurity.LoadOfflineFile(cfg.PurityOfflineDB)
		if err != nil {
			logger.Warn("ip purity offline database unavailable",
				"path", cfg.PurityOfflineDB, "error", err)
		} else {
			opts.DB = db
			logger.Info("ip purity offline database loaded",
				"path", cfg.PurityOfflineDB, "entries", db.Len())
		}
	}
	// provider 总是装上：密钥可能在运行时由系统设置填入，查询时才取当前值；
	// 没有密钥时 provider 返回 ErrNoKey，由 Resolver 给出可读提示。
	opts.API = ippurity.IPAPIIs{Keys: keys, BaseURL: cfg.PurityAPIBaseURL}
	opts.Score = ippurity.IPQS{Keys: keys}
	return ippurity.NewResolver(opts)
}

// defaultMaxConns replaces pgx's max(4, NumCPU) default: on the 2-core host the
// collector, alert reconcile, device-limit sweep, retention VACUUM and page
// queries all queued behind 4 connections.
const defaultMaxConns = 10

// applyPoolDefaults raises the pool ceiling unless DATABASE_URL sets
// pool_max_conns explicitly, which keeps operator overrides authoritative.
func applyPoolDefaults(c *pgxpool.Config, dsn string) {
	if strings.Contains(dsn, "pool_max_conns") {
		return
	}
	if c.MaxConns < defaultMaxConns {
		c.MaxConns = defaultMaxConns
	}
}
