package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"tokenfactory/internal/admin"
	"tokenfactory/internal/auth"
	"tokenfactory/internal/blockchain"
	"tokenfactory/internal/compute"
	"tokenfactory/internal/equipment"
	"tokenfactory/internal/intermediary"
	"tokenfactory/internal/payment"
	"tokenfactory/internal/sms"
	"tokenfactory/internal/user"
	"tokenfactory/pkg/config"
	"tokenfactory/pkg/db"
	mw "tokenfactory/pkg/middleware"
	"tokenfactory/pkg/redis"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Database
	sqlDB, err := db.NewMySQL(cfg.Database.DSN)
	if err != nil {
		slog.Error("connect mysql", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	// Redis
	rdb := redis.NewRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	defer rdb.Close()
	redisCtx, cancelRedisPing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRedisPing()
	if err := rdb.Ping(redisCtx).Err(); err != nil {
		slog.Error("connect redis", "error", err)
		os.Exit(1)
	}

	// Repositories
	authRepo := auth.NewRepository(sqlDB)
	userRepo := user.NewRepository(sqlDB)

	// Services
	var smsSender auth.SMSSender
	if cfg.SMS.LocalPreview {
		smsSender = sms.NewPreviewSender()
		slog.Warn("local SMS preview is enabled")
	} else if cfg.SMS.Enabled {
		smsSender, err = sms.NewAliyunSender(cfg.SMS.SignName, cfg.SMS.LoginTemplateCode, cfg.SMS.RegisterTemplateCode, cfg.SMS.Endpoint)
		if err != nil {
			slog.Error("configure Alibaba Cloud SMS", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Warn("SMS login is disabled; configure SMS sign and templates to enable it")
	}
	authSvc := auth.NewService(authRepo, userRepo, rdb, smsSender, time.Duration(cfg.SMS.CodeTTL)*time.Second, cfg.JWT.AccessSecret, cfg.JWT.RefreshSecret, cfg.JWT.AccessTTL, cfg.JWT.RefreshTTL)
	capVerifier := auth.NewCapVerifier(cfg.Security.CapSiteVerifyURL, cfg.Security.CapSecret, cfg.Security.CapTestToken)
	userSvc := user.NewService(userRepo)
	computeRepo := compute.NewRepository(sqlDB)
	computeSvc := compute.NewService(computeRepo, sqlDB, cfg.Security.CredentialKey)
	if cfg.Security.CredentialKey == "" {
		slog.Warn("security.credential_key 未配置: 交付访问凭证(C-06)将返回明确错误而非降级存明文",
			"补充方式", "config.yaml 配置 64 位 hex(32字节)密钥, 生产环境应从 KMS 注入")
	}
	paymentRepo := payment.NewRepository(sqlDB)
	paymentSvc := payment.NewService(paymentRepo, sqlDB)
	intermediaryRepo := intermediary.NewRepository(sqlDB)
	intermediarySvc := intermediary.NewService(intermediaryRepo)
	equipmentRepo := equipment.NewRepository(sqlDB)
	equipmentSvc := equipment.NewService(equipmentRepo)
	collateralRepo := intermediary.NewCollateralRepository(sqlDB)
	collateralSvc := intermediary.NewCollateralService(collateralRepo)
	adminRepo := admin.NewRepository(sqlDB)
	adminSvc := admin.NewService(adminRepo)
	blockchainRepo := blockchain.NewRepository(sqlDB)
	blockchainSvc := blockchain.NewService(blockchainRepo, rdb)

	// Router
	r := gin.New()
	r.Use(mw.RequestID(), mw.Logger(), gin.Recovery())

	// Health
	r.GET("/health", func(c *gin.Context) {
		response.Success(c, gin.H{"status": "ok"})
	})

	// Public API
	authHandler := auth.NewHandler(authSvc, capVerifier)
	public := r.Group("/api/v1")
	authHandler.RegisterPublicRoutes(public)
	compute.NewHandler(computeSvc).RegisterPublicRoutes(public)
	intermediary.NewHandler(intermediarySvc).RegisterPublicRoutes(public)
	equipment.NewHandler(equipmentSvc).RegisterPublicRoutes(public)
	intermediary.NewCollateralHandler(collateralSvc).RegisterPublicRoutes(public)
	blockchain.NewHandler(blockchainSvc).RegisterRoutes(public)

	// Authenticated API
	protected := r.Group("/api/v1")
	protected.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb))
	authHandler.RegisterProtectedRoutes(protected)
	user.NewHandler(userSvc).RegisterRoutes(protected)

	// Buyer API
	buyer := r.Group("/api/v1")
	buyer.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb))
	compute.NewHandler(computeSvc).RegisterBuyerRoutes(buyer)
	equipment.NewHandler(equipmentSvc).RegisterBuyerRoutes(buyer)
	payment.NewHandler(paymentSvc).RegisterBuyerRoutes(buyer)
	if cfg.Server.Mode != "release" {
		dev := r.Group("/api/v1/dev")
		dev.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb))
		compute.NewHandler(computeSvc).RegisterDevRoutes(dev)
	}

	// Supplier API
	supplier := r.Group("/api/v1")
	supplier.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb), mw.RBAC("supplier"))
	compute.NewHandler(computeSvc).RegisterSupplierRoutes(supplier)
	payment.NewHandler(paymentSvc).RegisterSupplierRoutes(supplier)

	// Vendor API
	vendor := r.Group("/api/v1")
	vendor.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb), mw.RBAC("vendor"))
	intermediary.NewHandler(intermediarySvc).RegisterVendorRoutes(vendor)
	equipment.NewHandler(equipmentSvc).RegisterVendorRoutes(vendor)

	// Admin API
	adminRoute := r.Group("/api/v1")
	adminRoute.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb), mw.RBAC("operator", "admin"))
	compute.NewHandler(computeSvc).RegisterAdminRoutes(adminRoute)
	payment.NewHandler(paymentSvc).RegisterAdminRoutes(adminRoute)
	intermediary.NewHandler(intermediarySvc).RegisterAdminRoutes(adminRoute)
	equipment.NewHandler(equipmentSvc).RegisterAdminRoutes(adminRoute)
	intermediary.NewCollateralHandler(collateralSvc).RegisterAdminRoutes(adminRoute)
	admin.NewHandler(adminSvc).RegisterRoutes(adminRoute)

	// Payment callback (no auth, signature verification)
	payment.NewHandler(paymentSvc).RegisterCallbackRoutes(r.Group("/api/v1"))

	// Server
	srv := &http.Server{
		Addr:    ":" + cfg.Server.Port,
		Handler: r,
	}

	// 定时任务。只定义方法不接线等于没做, 因此这里统一装配:
	//   C-06      吊销过期访问凭证        —— 否则凭证永不过期
	//   REQ-A-023 关闭超时未支付订单并释放余量 —— 否则余量被永久锁死
	//   REQ-A-043 租期到期置完成并释放余量     —— 否则卡永远不回到可售池
	stopJobs := make(chan struct{})
	runJobs := func() {
		if n, err := computeSvc.RevokeExpiredAccess(); err != nil {
			slog.Error("吊销过期访问凭证失败", "error", err)
		} else if n > 0 {
			slog.Info("已吊销过期访问凭证", "count", n)
		}
		if n, err := computeSvc.CloseExpiredUnpaidOrders(); err != nil {
			slog.Error("关闭超时未支付订单失败", "error", err)
		} else if n > 0 {
			slog.Info("已关闭超时未支付订单并释放余量", "count", n)
		}
		if n, err := computeSvc.CompleteExpiredLeases(); err != nil {
			slog.Error("处理到期租约失败", "error", err)
		} else if n > 0 {
			slog.Info("已完成到期租约并释放余量", "count", n)
		}
	}
	go func() {
		// 支付超时为 15 分钟, 用 5 分钟粒度扫描, 保证超时后最迟 5 分钟内释放余量。
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		runJobs() // 启动即跑一次: 进程重启期间到期的订单不至于要等一个周期
		for {
			select {
			case <-stopJobs:
				return
			case <-ticker.C:
				runJobs()
			}
		}
	}()

	go func() {
		slog.Info("server starting", "port", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down...")
	close(stopJobs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
