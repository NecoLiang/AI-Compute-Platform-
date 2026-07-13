package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"tokenfactory/internal/auth"
	"tokenfactory/internal/user"
	"tokenfactory/internal/compute"
	"tokenfactory/internal/payment"
	"tokenfactory/internal/intermediary"
	"tokenfactory/internal/admin"
	"tokenfactory/internal/blockchain"
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

	// Repositories
	authRepo := auth.NewRepository(sqlDB)
	userRepo := user.NewRepository(sqlDB)

	// Services
	authSvc := auth.NewService(authRepo, userRepo, rdb, cfg.JWT.AccessSecret, cfg.JWT.RefreshSecret, cfg.JWT.AccessTTL, cfg.JWT.RefreshTTL)
	userSvc := user.NewService(userRepo)
	computeRepo := compute.NewRepository(sqlDB)
	computeSvc := compute.NewService(computeRepo, sqlDB)
	paymentRepo := payment.NewRepository(sqlDB)
	paymentSvc := payment.NewService(paymentRepo, sqlDB)
	intermediaryRepo := intermediary.NewRepository(sqlDB)
	intermediarySvc := intermediary.NewService(intermediaryRepo)
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
	public := r.Group("/api/v1")
	auth.NewHandler(authSvc).RegisterRoutes(public, cfg.JWT.AccessSecret, rdb)
	compute.NewHandler(computeSvc).RegisterPublicRoutes(public)
	intermediary.NewHandler(intermediarySvc).RegisterPublicRoutes(public)
	blockchain.NewHandler(blockchainSvc).RegisterRoutes(public)

	// Authenticated API
	protected := r.Group("/api/v1")
	protected.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb))
	user.NewHandler(userSvc).RegisterRoutes(protected)

	// Buyer API
	buyer := r.Group("/api/v1")
	buyer.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb))
	compute.NewHandler(computeSvc).RegisterBuyerRoutes(buyer)
	payment.NewHandler(paymentSvc).RegisterBuyerRoutes(buyer)

	// Supplier API
	supplier := r.Group("/api/v1")
	supplier.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb), mw.RBAC("supplier"))
	compute.NewHandler(computeSvc).RegisterSupplierRoutes(supplier)
	payment.NewHandler(paymentSvc).RegisterSupplierRoutes(supplier)

	// Vendor API
	vendor := r.Group("/api/v1")
	vendor.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb), mw.RBAC("vendor"))
	intermediary.NewHandler(intermediarySvc).RegisterVendorRoutes(vendor)

	// Admin API
	adminRoute := r.Group("/api/v1")
	adminRoute.Use(mw.AuthRequired(cfg.JWT.AccessSecret, rdb), mw.RBAC("operator", "admin"))
	compute.NewHandler(computeSvc).RegisterAdminRoutes(adminRoute)
	payment.NewHandler(paymentSvc).RegisterAdminRoutes(adminRoute)
	intermediary.NewHandler(intermediarySvc).RegisterAdminRoutes(adminRoute)
	admin.NewHandler(adminSvc).RegisterRoutes(adminRoute)

	// Payment callback (no auth, signature verification)
	payment.NewHandler(paymentSvc).RegisterCallbackRoutes(r.Group("/api/v1"))

	// Server
	srv := &http.Server{
		Addr:    ":" + cfg.Server.Port,
		Handler: r,
	}

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
