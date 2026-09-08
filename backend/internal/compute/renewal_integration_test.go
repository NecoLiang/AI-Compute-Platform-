package compute_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"tokenfactory/internal/compute"
	"tokenfactory/internal/payment"
)

func activeRenewalOrder(t *testing.T, db *sqlx.DB) (*compute.Service, *payment.Service, string, int64) {
	t.Helper()
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{
		"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1",
	}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	payments := payment.NewService(payment.NewRepository(db), db, &testPaymentGateway{})
	pay, err := payments.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := payments.HandleCallback(payment.CallbackReq{OrderNo: no, TxID: pay.TxID, Status: "success", Amount: 13800}); err != nil {
		t.Fatal(err)
	}
	service := compute.NewService(compute.NewRepository(db), db, strings.Repeat("12", 32))
	if _, err := service.DeliverWithAccess(101, no, compute.DeliverInfo{IpAddress: "192.0.2.10"}, false); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfirmDelivery(102, no); err != nil {
		t.Fatal(err)
	}
	return service, payments, no, id
}

func TestRenewalPaymentExtendsLeaseAndCredentialExactlyOnce(t *testing.T) {
	db := setupTradeDB(t)
	service, payments, parentNo, productID := activeRenewalOrder(t, db)
	buyer := tradeRouter(db, 102)
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	created := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", renewalInput(quote, "a7a78a5a-ae4b-466d-87b3-b238512c1570"), 0).Data.(map[string]any)
	childNo := created["order_no"].(string)
	pay, err := payments.Pay(102, payment.PayReq{OrderNo: childNo, Channel: "wechat"})
	if err != nil {
		t.Fatal(err)
	}
	callback := payment.CallbackReq{OrderNo: childNo, TxID: pay.TxID, Status: "success", Amount: 9200}
	wrong := callback
	wrong.Amount++
	if err := payments.HandleCallback(wrong); err == nil {
		t.Fatal("wrong amount accepted")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- payments.HandleCallback(callback) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.CompleteExpiredLeases(); err != nil {
		t.Fatal(err)
	}
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
	order := detail["order"].(map[string]any)
	if order["lease_end_at"] != quote["renewed_until"] || order["status"] != "active" || order["total_amount"] != float64(13800) {
		t.Fatalf("lease not extended once: %v", detail)
	}
	delivery := detail["delivery"].(map[string]any)
	if delivery["access_expires_at"] != quote["renewed_until"] || delivery["access_status"] != "delivered" {
		t.Fatalf("credential expiry differs: %v", delivery)
	}
	child := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+childNo, nil, 0).Data.(map[string]any)
	if child["order"].(map[string]any)["status"] != "completed" || child["renewal"].(map[string]any)["applied_at"] == nil {
		t.Fatalf("renewal not fulfilled: %v", child)
	}
	product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", productID), nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if product["stock"] != float64(6) {
		t.Fatalf("renewal changed stock: %v", product)
	}
}

func renewalInput(quote map[string]any, requestID string) map[string]any {
	return map[string]any{
		"duration": quote["duration"], "request_id": requestID,
		"compliance_agreed": true, "compliance_version": "2026-09-06.1",
		"expected_lease_end_at": quote["lease_end_at"], "expected_renewed_until": quote["renewed_until"],
		"expected_total_amount": quote["total_amount"], "expected_platform_fee": quote["platform_fee"],
	}
}

func TestRenewalCancellationAndParentFreezePreventPaymentWithoutReturningParentStock(t *testing.T) {
	for _, action := range []string{"cancel", "freeze"} {
		t.Run(action, func(t *testing.T) {
			db := setupTradeDB(t)
			service, payments, parentNo, productID := activeRenewalOrder(t, db)
			buyer := tradeRouter(db, 102)
			quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
			created := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", renewalInput(quote, "db8227b9-4ec7-4fca-95c1-4646f4a96d0e"), 0).Data.(map[string]any)
			childNo := created["order_no"].(string)
			pay, err := payments.Pay(102, payment.PayReq{OrderNo: childNo, Channel: "wechat"})
			if err != nil {
				t.Fatal(err)
			}
			if action == "cancel" {
				tradeRequest(t, tradeRouter(db, 103), "POST", "/api/v1/orders/"+childNo+"/cancel", nil, 40400)
				tradeRequest(t, buyer, "POST", "/api/v1/orders/"+childNo+"/cancel", nil, 0)
				tradeRequest(t, buyer, "POST", "/api/v1/orders/"+childNo+"/cancel", nil, 0)
			} else if err := service.FreezeOrder(parentNo); err != nil {
				t.Fatal(err)
			}
			if err := payments.HandleCallback(payment.CallbackReq{OrderNo: childNo, TxID: pay.TxID, Status: "success", Amount: 9200}); err == nil {
				t.Fatal("closed renewal was paid")
			}
			child := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+childNo, nil, 0).Data.(map[string]any)
			if child["order"].(map[string]any)["status"] != "cancelled" {
				t.Fatalf("renewal remains payable: %v", child)
			}
			product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", productID), nil, 0).Data.(map[string]any)["product"].(map[string]any)
			if product["stock"] != float64(6) {
				t.Fatalf("parent inventory inflated: %v", product)
			}
		})
	}
}

func TestRenewalAfterExpiryReservesAvailableStockAndRequiresFreshDelivery(t *testing.T) {
	db := setupTradeDB(t)
	service, payments, parentNo, productID := activeRenewalOrder(t, db)
	buyer := tradeRouter(db, 102)
	oldProof, err := service.BuildDeliveryAttestPayload(parentNo)
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec("UPDATE orders SET lease_end_at=DATE_SUB(NOW(),INTERVAL 1 HOUR) WHERE order_no=?", parentNo)
	if _, err := service.CompleteExpiredLeases(); err != nil {
		t.Fatal(err)
	}
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	if quote["mode"] != "restart" || quote["renewed_until"] != nil {
		t.Fatalf("expired lease promised uninterrupted access: %v", quote)
	}
	created := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", renewalInput(quote, "cd8c0319-225b-4d23-b11d-80f2525fb108"), 0).Data.(map[string]any)
	childNo := created["order_no"].(string)
	pay, err := payments.Pay(102, payment.PayReq{OrderNo: childNo, Channel: "wechat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := payments.HandleCallback(payment.CallbackReq{OrderNo: childNo, TxID: pay.TxID, Status: "success", Amount: 9200}); err != nil {
		t.Fatal(err)
	}
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
	if detail["order"].(map[string]any)["status"] != "paid" || detail["actions"].(map[string]any)["can_view_credential"] != false {
		t.Fatalf("old access revived: %v", detail)
	}
	if _, err := service.DeliverWithAccess(101, parentNo, compute.DeliverInfo{IpAddress: "192.0.2.11"}, false); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfirmDelivery(102, parentNo); err != nil {
		t.Fatal(err)
	}
	detail = tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
	order := detail["order"].(map[string]any)
	start, _ := time.Parse(time.RFC3339, order["lease_start_at"].(string))
	end, _ := time.Parse(time.RFC3339, order["lease_end_at"].(string))
	if end.Sub(start) != 2*time.Hour || order["duration"] != float64(3) || order["total_amount"] != float64(13800) {
		t.Fatalf("restart uses old duration or reprices original: %v", order)
	}
	if detail["delivery"].(map[string]any)["access_expires_at"] != order["lease_end_at"] {
		t.Fatalf("credential expiry differs: %v", detail)
	}
	proof, err := service.BuildDeliveryAttestPayload(parentNo)
	if err != nil || proof != oldProof {
		t.Fatalf("original delivery proof changed: %v %v", proof, err)
	}
	if _, err := service.BuildDeliveryAttestPayload(childNo); err != nil {
		t.Fatalf("new delivery has no proof: %v", err)
	}
	product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", productID), nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if product["stock"] != float64(6) {
		t.Fatalf("restart stock ownership wrong: %v", product)
	}
}

func TestRenewalRefundRemovesOnlyUnusedExtensionAndNeverDuplicatesStock(t *testing.T) {
	db := setupTradeDB(t)
	service, payments, parentNo, productID := activeRenewalOrder(t, db)
	buyer := tradeRouter(db, 102)
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	created := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", renewalInput(quote, "75f0334a-0d08-4a0e-bf0a-0d0cd56d1e09"), 0).Data.(map[string]any)
	childNo := created["order_no"].(string)
	pay, err := payments.Pay(102, payment.PayReq{OrderNo: childNo, Channel: "wechat"})
	if err != nil {
		t.Fatal(err)
	}
	callback := payment.CallbackReq{OrderNo: childNo, TxID: pay.TxID, Status: "success", Amount: 9200}
	if err := payments.HandleCallback(callback); err != nil {
		t.Fatal(err)
	}
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+childNo+"/refund", nil, 0)
	for range 2 {
		if err := service.CompleteRefund(childNo); err != nil {
			t.Fatal(err)
		}
	}
	if err := payments.HandleCallback(callback); err != nil {
		t.Fatal(err)
	}
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
	if detail["order"].(map[string]any)["lease_end_at"] != quote["lease_end_at"] || detail["delivery"].(map[string]any)["access_expires_at"] != quote["lease_end_at"] {
		t.Fatalf("refunded extension remains: %v", detail)
	}
	product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", productID), nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if product["stock"] != float64(6) {
		t.Fatalf("refund returned parent stock: %v", product)
	}
}

func TestExpiredRenewalCancelTimeoutAndRefundReleaseOnlyTheReservedCards(t *testing.T) {
	for _, action := range []string{"cancel", "timeout", "refund"} {
		t.Run(action, func(t *testing.T) {
			db := setupTradeDB(t)
			service, payments, parentNo, productID := activeRenewalOrder(t, db)
			buyer := tradeRouter(db, 102)
			db.MustExec("UPDATE orders SET lease_end_at=DATE_SUB(NOW(),INTERVAL 1 HOUR) WHERE order_no=?", parentNo)
			if _, err := service.CompleteExpiredLeases(); err != nil {
				t.Fatal(err)
			}
			quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
			created := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", renewalInput(quote, "f4c4b7c7-0db5-4cfb-813b-8ba7f79ad75d"), 0).Data.(map[string]any)
			childNo := created["order_no"].(string)
			pay, err := payments.Pay(102, payment.PayReq{OrderNo: childNo, Channel: "wechat"})
			if err != nil {
				t.Fatal(err)
			}
			callback := payment.CallbackReq{OrderNo: childNo, TxID: pay.TxID, Status: "success", Amount: 9200}
			switch action {
			case "cancel":
				for range 2 {
					tradeRequest(t, buyer, "POST", "/api/v1/orders/"+childNo+"/cancel", nil, 0)
				}
			case "timeout":
				db.MustExec("UPDATE orders SET payment_expires_at=DATE_SUB(NOW(),INTERVAL 1 MINUTE) WHERE order_no=?", childNo)
				for range 2 {
					if _, err := service.CloseExpiredUnpaidOrders(); err != nil {
						t.Fatal(err)
					}
				}
			case "refund":
				if err := payments.HandleCallback(callback); err != nil {
					t.Fatal(err)
				}
				tradeRequest(t, buyer, "POST", "/api/v1/orders/"+childNo+"/refund", nil, 0)
				for range 2 {
					if err := service.CompleteRefund(childNo); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := payments.HandleCallback(callback); action != "refund" && err == nil {
				t.Fatal("late callback accepted")
			}
			product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", productID), nil, 0).Data.(map[string]any)["product"].(map[string]any)
			if product["stock"] != float64(8) {
				t.Fatalf("stock not returned exactly once: %v", product)
			}
			parent := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
			if parent["order"].(map[string]any)["status"] != "completed" {
				t.Fatalf("lease left active: %v", parent)
			}
		})
	}
}

func TestRenewalRequiresCurrentQuoteAndConsentAndCannotBeFulfilledByAdminStatus(t *testing.T) {
	db := setupTradeDB(t)
	service, _, parentNo, productID := activeRenewalOrder(t, db)
	buyer := tradeRouter(db, 102)
	tradeRequest(t, tradeRouter(db, 103), "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 40300)
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	input := renewalInput(quote, "a9a2234b-f1e5-44a8-9725-b6f25866de35")
	input["compliance_agreed"] = false
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", input, 40001)
	input["compliance_agreed"] = true
	db.MustExec("UPDATE products SET unit_price=3000 WHERE id=?", productID)
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", input, 40900)
	quote = tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	created := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", renewalInput(quote, "a9a2234b-f1e5-44a8-9725-b6f25866de35"), 0).Data.(map[string]any)
	no := created["order_no"].(string)
	for _, status := range []string{"paid", "provisioning", "active", "completed"} {
		if err := service.AdminUpdateOrderStatus(no, status); err == nil {
			t.Fatalf("admin bypassed payment with %s", status)
		}
	}
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", renewalInput(quote, "88a05d6d-cead-401e-bf58-e5aa810cabbd"), 40900)
	child := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	if child["order"].(map[string]any)["status"] != "pending_payment" {
		t.Fatalf("admin changed renewal: %v", child)
	}
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+no+"/cancel", nil, 0)
	db.MustExec("UPDATE order_deliveries SET access_expires_at=DATE_SUB(NOW(),INTERVAL 1 MINUTE) WHERE order_id=(SELECT id FROM orders WHERE order_no=?)", parentNo)
	tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 40900)
}

func TestRenewalConcurrentSubmissionsReturnTheSameOrder(t *testing.T) {
	db := setupTradeDB(t)
	service, _, parentNo, _ := activeRenewalOrder(t, db)
	q, err := service.GetRenewalQuote(102, parentNo, 2)
	if err != nil {
		t.Fatal(err)
	}
	req := compute.RenewOrderReq{Duration: 2, RequestID: "88db33f5-d89a-4197-93f8-f9e1962e9e52", ComplianceAgreed: true,
		ComplianceVersion: "2026-09-06.1", ExpectedLeaseEndAt: q.LeaseEndAt, ExpectedRenewedUntil: q.RenewedUntil,
		ExpectedTotalAmount: 9200, ExpectedPlatformFee: 460}
	var wg sync.WaitGroup
	orders := make(chan string, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := service.RenewOrder(102, parentNo, req)
			if err != nil {
				errs <- err
				return
			}
			orders <- o.OrderNo
		}()
	}
	wg.Wait()
	close(orders)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	first := <-orders
	if first == "" {
		t.Fatal("no renewal created")
	}
	for no := range orders {
		if no != first {
			t.Fatalf("duplicate renewal: %s %s", first, no)
		}
	}
	list := tradeRequest(t, tradeRouter(db, 102), "GET", "/api/v1/orders", nil, 0).Data.(map[string]any)
	if list["total"] != float64(2) {
		t.Fatalf("duplicate order created: %v", list)
	}
}

func TestLeaseExpiryRechecksDeadlineAfterWaitingForTheOrderLock(t *testing.T) {
	db := setupTradeDB(t)
	service, _, parentNo, productID := activeRenewalOrder(t, db)
	db.MustExec("UPDATE orders SET lease_end_at=DATE_SUB(NOW(),INTERVAL 1 HOUR) WHERE order_no=?", parentNo)
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tx.MustExec("UPDATE orders SET lease_end_at=lease_end_at WHERE order_no=?", parentNo)
	var connectionID int64
	if err := tx.Get(&connectionID, "SELECT CONNECTION_ID()"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := service.CompleteExpiredLeases(); finished <- err }()
	// Coordinate a concurrent lease update through MySQL's real row lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := db.Get(&waiting, `SELECT COUNT(*) FROM performance_schema.data_lock_waits w
			JOIN information_schema.innodb_trx t ON t.trx_id=w.BLOCKING_ENGINE_TRANSACTION_ID WHERE t.trx_mysql_thread_id=?`, connectionID); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expiry worker did not wait for the lease lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tx.MustExec("UPDATE orders SET lease_end_at=DATE_ADD(NOW(),INTERVAL 2 HOUR) WHERE order_no=?", parentNo)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	buyer := tradeRouter(db, 102)
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
	if detail["order"].(map[string]any)["status"] != "active" {
		t.Fatalf("stale expiry closed renewed lease: %v", detail)
	}
	product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", productID), nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if product["stock"] != float64(6) {
		t.Fatalf("stale expiry returned held cards: %v", product)
	}
}

func TestRenewalCreatesOneLinkedOrderWithoutExtendingOrReservingTwice(t *testing.T) {
	db := setupTradeDB(t)
	_, _, parentNo, productID := activeRenewalOrder(t, db)
	buyer := tradeRouter(db, 102)
	before := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
	db.MustExec("UPDATE products SET unit_price=3000 WHERE id=?", productID)
	db.MustExec("UPDATE system_config SET config_value='650' WHERE config_key='fee_rate'")
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	if quote["mode"] != "extend" || quote["total_amount"] != float64(12000) || quote["platform_fee"] != float64(780) {
		t.Fatalf("renewal must quote current offer without repricing old order: %v", quote)
	}
	oldEnd := before["order"].(map[string]any)["lease_end_at"].(string)
	parsedEnd, err := time.Parse(time.RFC3339, oldEnd)
	if err != nil {
		t.Fatal(err)
	}
	newEnd, err := time.Parse(time.RFC3339, quote["renewed_until"].(string))
	if err != nil || !newEnd.Equal(parsedEnd.Add(2*time.Hour)) {
		t.Fatalf("wrong extension: %v %v", quote, err)
	}
	input := renewalInput(quote, "cdd36d72-79ab-4a0a-9d2d-56a25f89c877")
	created := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", input, 0).Data.(map[string]any)
	retried := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+parentNo+"/renew", input, 0).Data.(map[string]any)
	if created["order_no"] != retried["order_no"] {
		t.Fatalf("retry created another order: %v %v", created, retried)
	}
	child := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+created["order_no"].(string), nil, 0).Data.(map[string]any)
	if child["order"].(map[string]any)["parent_order_no"] != parentNo || child["actions"].(map[string]any)["can_renew"] != false {
		t.Fatalf("renewal relationship or actions missing: %v", child)
	}
	after := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+parentNo, nil, 0).Data.(map[string]any)
	old := after["order"].(map[string]any)
	if old["lease_end_at"] != oldEnd || old["total_amount"] != float64(13800) || old["platform_fee"] != float64(690) {
		t.Fatalf("creating renewal changed existing lease or price: %v", old)
	}
	if after["pending_renewal_order_no"] != created["order_no"] {
		t.Fatalf("pending renewal cannot be resumed: %v", after)
	}
	product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", productID), nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if product["stock"] != float64(6) {
		t.Fatalf("renewal reserved the same cards twice: %v", product)
	}
}
