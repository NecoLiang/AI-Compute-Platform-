package compute_test

import (
	"fmt"
	"testing"
)

func TestAdminOrderRejectsSyntheticPaymentAndRefundStates(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	operator := tradeRouter(db, 104)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	productID := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, operator, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", productID), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{
		"product_id": productID, "quantity": 2, "duration": 3,
		"compliance_agreed": true, "compliance_version": "2026-09-06.1",
	}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	for _, target := range []string{"paid", "provisioning", "active", "completed", "refunded", "pending_payment"} {
		t.Run(target, func(t *testing.T) {
			tradeRequest(t, operator, "PATCH", "/api/v1/admin/orders/"+no+"/status", map[string]string{"status": target}, 40001)
			var status string
			if err := db.Get(&status, "SELECT status FROM orders WHERE order_no=?", no); err != nil || status != "pending_payment" {
				t.Fatalf("operator fabricated state: status=%s err=%v", status, err)
			}
		})
	}
}

func TestAdminOrderActionsAreAtomicAuditedAndIdempotent(t *testing.T) {
	db := setupTradeDB(t)
	service, _, activeNo, productID := activeRenewalOrder(t, db)
	operator, buyer := tradeRouter(db, 104), tradeRouter(db, 102)
	placed := tradeRequest(t, buyer, "POST", "/api/v1/orders", map[string]any{
		"product_id": productID, "quantity": 1, "duration": 1,
		"compliance_agreed": true, "compliance_version": "2026-09-06.1",
	}, 0)
	pendingNo := placed.Data.(map[string]any)["order_no"].(string)
	var pendingID int64
	if err := db.Get(&pendingID, "SELECT id FROM orders WHERE order_no=?", pendingNo); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/admin/orders/%d/status", pendingID)
	db.MustExec("CREATE TRIGGER reject_admin_order_audit BEFORE INSERT ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test audit failure'")
	tradeRequest(t, operator, "PATCH", path, map[string]string{"status": "cancelled"}, 50000)
	var status string
	var stock int
	db.Get(&status, "SELECT status FROM orders WHERE id=?", pendingID)
	db.Get(&stock, "SELECT stock FROM products WHERE id=?", productID)
	if status != "pending_payment" || stock != 5 {
		t.Fatalf("failed audit did not roll back cancellation: status=%s stock=%d", status, stock)
	}
	db.MustExec("DROP TRIGGER reject_admin_order_audit")
	for range 2 {
		tradeRequest(t, operator, "PATCH", path, map[string]string{"status": "cancelled"}, 0)
	}
	var audits int
	db.Get(&audits, "SELECT COUNT(*) FROM audit_logs WHERE action='update_order_status' AND target_id=? AND operator_id=104 AND before_value='pending_payment' AND after_value='cancelled'", pendingID)
	db.Get(&stock, "SELECT stock FROM products WHERE id=?", productID)
	if audits != 1 || stock != 6 {
		t.Fatalf("repeated cancellation changed audit/stock: audits=%d stock=%d", audits, stock)
	}
	tradeRequest(t, operator, "PATCH", "/api/v1/admin/orders/"+activeNo+"/status", map[string]string{"status": "refunded"}, 40001)

	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+activeNo+"/renewal-quote?duration=1", nil, 0).Data.(map[string]any)
	renewed := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+activeNo+"/renew", renewalInput(quote, "2eaa5b0a-9001-4775-a6af-3a7b833a41fb"), 0)
	childNo := renewed.Data.(map[string]any)["order_no"].(string)
	db.MustExec("CREATE TRIGGER reject_admin_order_audit BEFORE INSERT ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test audit failure'")
	if err := service.AdminUpdateOrderStatus(104, activeNo, "frozen", "127.0.0.1"); err == nil {
		t.Fatal("freeze committed without audit")
	}
	var access string
	db.Get(&access, "SELECT access_status FROM order_deliveries WHERE order_id=(SELECT id FROM orders WHERE order_no=?)", activeNo)
	db.Get(&status, "SELECT status FROM orders WHERE order_no=?", childNo)
	if access != "delivered" || status != "pending_payment" {
		t.Fatalf("failed freeze changed credentials or renewal: access=%s child=%s", access, status)
	}
	db.MustExec("DROP TRIGGER reject_admin_order_audit")
	for range 2 {
		if err := service.AdminUpdateOrderStatus(104, activeNo, "frozen", "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	db.Get(&access, "SELECT access_status FROM order_deliveries WHERE order_id=(SELECT id FROM orders WHERE order_no=?)", activeNo)
	db.Get(&status, "SELECT status FROM orders WHERE order_no=?", childNo)
	db.Get(&audits, "SELECT COUNT(*) FROM audit_logs WHERE action='update_order_status' AND after_value='frozen' AND operator_id=104")
	db.Get(&stock, "SELECT stock FROM products WHERE id=?", productID)
	if access != "revoked" || status != "cancelled" || audits != 1 || stock != 6 {
		t.Fatalf("freeze inconsistent: access=%s child=%s audits=%d stock=%d", access, status, audits, stock)
	}
}

func TestAdminOrderCapabilitiesAndTerminalStates(t *testing.T) {
	db := setupTradeDB(t)
	_, _, no, _ := activeRenewalOrder(t, db)
	operator := tradeRouter(db, 104)
	for _, status := range []string{"completed", "cancelled", "refunded", "refunding"} {
		db.MustExec("UPDATE orders SET status=? WHERE order_no=?", status, no)
		list := tradeRequest(t, operator, "GET", "/api/v1/admin/orders?status="+status, nil, 0).Data.(map[string]any)["list"].([]any)
		if len(list) != 1 || len(list[0].(map[string]any)["allowed_actions"].([]any)) != 0 {
			t.Fatalf("terminal or refunding order exposes actions: %v", list)
		}
		tradeRequest(t, operator, "PATCH", "/api/v1/admin/orders/"+no+"/status", map[string]string{"status": "frozen"}, 40900)
	}
	db.MustExec("UPDATE orders SET status='pending_payment', stock_reserved=NULL WHERE order_no=?", no)
	tradeRequest(t, operator, "PATCH", "/api/v1/admin/orders/"+no+"/status", map[string]string{"status": "cancelled"}, 40001)
	list := tradeRequest(t, operator, "GET", "/api/v1/admin/orders", nil, 0).Data.(map[string]any)["list"].([]any)
	actions := list[0].(map[string]any)["allowed_actions"].([]any)
	if len(actions) != 1 || actions[0] != "frozen" {
		t.Fatalf("unknown historical stock must not allow cancellation: %v", actions)
	}
	tradeRequest(t, operator, "PATCH", "/api/v1/admin/orders/ORDMISSINGCHECK/status", map[string]string{"status": "frozen"}, 40400)
}
