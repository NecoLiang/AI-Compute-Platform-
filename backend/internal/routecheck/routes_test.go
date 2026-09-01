package routecheck

// 按 main.go 的完全相同方式装配全部路由, 确认 gin 路由树无冲突、无重复。
// 路由冲突只在注册时 panic, go build 查不出来, 因此需要这层运行时校验。
// 新增模块后请同步更新本文件, 使其与 main.go 保持一致。

import (
	"sort"
	"testing"

	"tokenfactory/internal/admin"
	"tokenfactory/internal/agentsearch"
	"tokenfactory/internal/auth"
	"tokenfactory/internal/blockchain"
	"tokenfactory/internal/compute"
	"tokenfactory/internal/equipment"
	"tokenfactory/internal/intermediary"
	"tokenfactory/internal/invoice"
	"tokenfactory/internal/notification"
	"tokenfactory/internal/payment"
	"tokenfactory/internal/scheduler"
	"tokenfactory/internal/ticket"
	"tokenfactory/internal/user"

	"github.com/gin-gonic/gin"
)

func TestAllRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("路由注册 panic (存在冲突): %v", r)
		}
	}()

	computeH := compute.NewHandler(nil)
	paymentH := payment.NewHandler(nil)
	intermediaryH := intermediary.NewHandler(nil)
	equipmentH := equipment.NewHandler(nil)
	invoiceH := invoice.NewHandler(nil)
	ticketH := ticket.NewHandler(nil)
	notificationH := notification.NewHandler(nil)
	collateralH := intermediary.NewCollateralHandler(nil)
	adminH := admin.NewHandler(nil)
	userH := user.NewHandler(nil)
	blockchainH := blockchain.NewHandler(nil)
	agentSearchH := agentsearch.NewHandler(nil)
	schedulerH := scheduler.NewHandler(nil)
	authH := auth.NewHandler(nil, nil)

	r := gin.New()

	public := r.Group("/api/v1")
	authH.RegisterPublicRoutes(public)
	computeH.RegisterPublicRoutes(public)
	intermediaryH.RegisterPublicRoutes(public)
	equipmentH.RegisterPublicRoutes(public)
	collateralH.RegisterPublicRoutes(public)
	blockchainH.RegisterRoutes(public)
	schedulerH.RegisterNodeRoutes(public)

	protected := r.Group("/api/v1")
	authH.RegisterProtectedRoutes(protected)
	userH.RegisterRoutes(protected)

	buyer := r.Group("/api/v1")
	computeH.RegisterBuyerRoutes(buyer)
	agentSearchH.RegisterBuyerRoutes(buyer)
	invoiceH.RegisterBuyerRoutes(buyer)
	ticketH.RegisterBuyerRoutes(buyer)
	notificationH.RegisterBuyerRoutes(buyer)
	equipmentH.RegisterBuyerRoutes(buyer)
	paymentH.RegisterBuyerRoutes(buyer)

	supplier := r.Group("/api/v1")
	computeH.RegisterSupplierRoutes(supplier)
	paymentH.RegisterSupplierRoutes(supplier)
	schedulerH.RegisterSupplierRoutes(supplier)

	vendor := r.Group("/api/v1")
	intermediaryH.RegisterVendorRoutes(vendor)
	equipmentH.RegisterVendorRoutes(vendor)

	adminRoute := r.Group("/api/v1")
	computeH.RegisterAdminRoutes(adminRoute)
	invoiceH.RegisterAdminRoutes(adminRoute)
	ticketH.RegisterAdminRoutes(adminRoute)
	paymentH.RegisterAdminRoutes(adminRoute)
	intermediaryH.RegisterAdminRoutes(adminRoute)
	equipmentH.RegisterAdminRoutes(adminRoute)
	collateralH.RegisterAdminRoutes(adminRoute)
	adminH.RegisterRoutes(adminRoute)
	blockchainH.RegisterAdminRoutes(adminRoute)
	schedulerH.RegisterAdminRoutes(adminRoute)

	paymentH.RegisterCallbackRoutes(r.Group("/api/v1"))

	routes := r.Routes()
	lines := make([]string, 0, len(routes))
	seen := map[string]bool{}
	for _, rt := range routes {
		k := rt.Method + " " + rt.Path
		if seen[k] {
			t.Errorf("重复路由: %s", k)
		}
		seen[k] = true
		lines = append(lines, k)
	}
	if seen["POST /api/v1/auth/login"] {
		t.Error("账号密码登录尚未开放，不应注册公开路由")
	}
	if !seen["POST /api/v1/auth/sms/login"] {
		t.Error("缺少手机号验证码登录路由")
	}
	sort.Strings(lines)
	t.Logf("共注册 %d 条路由:", len(lines))
	for _, l := range lines {
		t.Logf("  %s", l)
	}
}
