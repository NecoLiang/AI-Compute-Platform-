package compute_test

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"tokenfactory/internal/admin"
	"tokenfactory/internal/compute"
	"tokenfactory/internal/notification"
)

func TestQualificationSubmissionPreservesExpiry(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	router := tradeRouter(db, 101)
	input := map[string]any{"qual_type": "idc_license", "cert_name": "IDC license", "cert_number": "TEST-EXPIRY", "cert_url": "https://example.test/license.pdf", "expires_at": "2099-12-31"}
	created := tradeRequest(t, router, "POST", "/api/v1/supplier/qualifications", input, 0)
	id := created.Data.(map[string]any)["id"]
	list := tradeRequest(t, router, "GET", "/api/v1/supplier/qualifications", nil, 0)
	found := false
	for _, value := range list.Data.([]any) {
		q := value.(map[string]any)
		if q["id"] == id {
			found = true
			expiry, _ := q["expires_at"].(string)
			if !strings.HasPrefix(expiry, "2099-12-31") {
				t.Fatalf("expiry lost: %v", q)
			}
		}
	}
	if !found {
		t.Fatal("submitted qualification missing")
	}
	for _, invalid := range []string{"2026-02-30", "tomorrow", "2026-09-09T00:00:00Z", "2000-01-01"} {
		input["expires_at"] = invalid
		tradeRequest(t, router, "POST", "/api/v1/supplier/qualifications", input, 40001)
	}
}

func TestQualificationExpiryJob(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, adminRouter := tradeRouter(db, 101), tradeRouter(db, 104)
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	productID := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, adminRouter, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", productID), nil, 0)
	// Seed calendar boundaries; exercise the actual job and public read APIs.
	for i, days := range []int{31, 30, 0, -1} {
		db.MustExec("INSERT INTO supplier_qualifications (user_id,qual_type,cert_name,status,expires_at) VALUES (101,?,?, 'verified', DATE_ADD(CURRENT_DATE,INTERVAL ? DAY))", fmt.Sprintf("boundary-%d", i), fmt.Sprintf("License %d", days), days)
	}
	db.MustExec("INSERT INTO supplier_qualifications (user_id,qual_type,cert_name,status,expires_at) VALUES (102,'pending','Pending','pending',CURRENT_DATE),(102,'rejected','Rejected','rejected',CURRENT_DATE)")
	svc := compute.NewService(compute.NewRepository(db), db)
	if n, err := svc.ProcessQualificationExpiries(); err != nil || n != 3 {
		t.Fatalf("first sweep: n=%d err=%v", n, err)
	}
	ns := notification.NewService(notification.NewRepository(db))
	list, total, _, _, err := ns.List(101, "system", 1, 100)
	if err != nil || total != 3 {
		t.Fatalf("notifications: total=%d err=%v", total, err)
	}
	audits, _, err := admin.NewService(admin.NewRepository(db)).ListAuditLogs(1, 100)
	if err != nil {
		t.Fatal(err)
	}
	events := 0
	for _, event := range audits {
		if strings.HasPrefix(event.Action, "qualification_expir") {
			events++
			if event.TargetType != "supplier_qualification" || event.OperatorID != 0 || len(event.AfterVal) != 10 {
				t.Fatalf("invalid expiry evidence: %+v", event)
			}
		}
	}
	if events != 3 {
		t.Fatalf("missing expiry audit evidence: %d", events)
	}
	for _, n := range list {
		if n.UserID != 101 || !strings.HasPrefix(n.Link, "/console/supplier/qualifications#qualification-") {
			t.Fatalf("wrong recipient/link: %+v", n)
		}
	}
	_, total, _, _, err = ns.List(102, "", 1, 100)
	if err != nil || total != 0 {
		t.Fatalf("other user notified: %d %v", total, err)
	}
	quals := tradeRequest(t, supplier, "GET", "/api/v1/supplier/qualifications", nil, 0)
	for _, item := range quals.Data.([]any) {
		q := item.(map[string]any)
		if q["cert_name"] == "License -1" && q["status"] != "expired" {
			t.Fatalf("not expired: %v", q)
		}
		if q["cert_name"] == "License 0" && (q["status"] != "verified" || q["expires_in_days"] != float64(0)) {
			t.Fatalf("expiry day invalid: %v", q)
		}
	}
	products := tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
	if products.Data.([]any)[0].(map[string]any)["status"] != "frozen" {
		t.Fatalf("product not frozen: %v", products.Data)
	}
	for _, n := range list {
		if err := ns.Delete(101, n.ID); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := svc.ProcessQualificationExpiries(); err != nil || n != 0 {
				t.Errorf("repeat: %d %v", n, err)
			}
		}()
	}
	wg.Wait()
	_, total, _, _, err = ns.List(101, "", 1, 100)
	if err != nil || total != 0 {
		t.Fatalf("deleted notifications recreated: %d %v", total, err)
	}
	// A corrected validity date must be evaluated anew, not the original warning date.
	db.MustExec("UPDATE supplier_qualifications SET expires_at=DATE_ADD(CURRENT_DATE,INTERVAL 15 DAY) WHERE cert_name='License 30'")
	if n, err := svc.ProcessQualificationExpiries(); err != nil || n != 1 {
		t.Fatalf("changed validity: %d %v", n, err)
	}
}

func TestQualificationExpiryRollbackAndReplacement(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, admin, buyer := tradeRouter(db, 101), tradeRouter(db, 104), tradeRouter(db, 102)
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, admin, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	db.MustExec("UPDATE supplier_qualifications SET expires_at=DATE_SUB(CURRENT_DATE,INTERVAL 1 DAY) WHERE user_id=101")
	svc := compute.NewService(compute.NewRepository(db), db)
	ns := notification.NewService(notification.NewRepository(db))
	// A delivery failure must roll back the state and product freeze, then retry.
	db.MustExec("CREATE TRIGGER fail_expiry_notification BEFORE INSERT ON notifications FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='simulated delivery failure'")
	if _, err := svc.ProcessQualificationExpiries(); err == nil {
		t.Fatal("expected notification failure")
	}
	quals := tradeRequest(t, supplier, "GET", "/api/v1/supplier/qualifications", nil, 0)
	if quals.Data.([]any)[0].(map[string]any)["status"] != "verified" {
		t.Fatalf("failed job changed qualification: %v", quals.Data)
	}
	products := tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
	if products.Data.([]any)[0].(map[string]any)["status"] != "active" {
		t.Fatalf("failed job froze product: %v", products.Data)
	}
	// Expired admission is blocked even before a successful sweep.
	tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 40001)
	tradeRequest(t, buyer, "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 1, "duration": 1, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 40001)
	db.MustExec("DROP TRIGGER fail_expiry_notification")
	var wg sync.WaitGroup
	var sent atomic.Int64
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := svc.ProcessQualificationExpiries()
			if err != nil {
				t.Error(err)
			}
			sent.Add(int64(n))
		}()
	}
	wg.Wait()
	if sent.Load() != 1 {
		t.Fatalf("concurrent sweeps sent %d events", sent.Load())
	}
	_, total, _, _, err := ns.List(101, "system", 1, 100)
	if err != nil || total != 1 {
		t.Fatalf("retry notification: %d %v", total, err)
	}
	// Replacement is a real submission/review, not a supplier editing verified data.
	input := map[string]any{"qual_type": "idc", "cert_name": "Replacement license", "cert_number": "RENEWED", "cert_url": "https://example.test/new.pdf", "expires_at": "2099-12-31"}
	replacement := tradeRequest(t, supplier, "POST", "/api/v1/supplier/qualifications", input, 0)
	replacementID := int64(replacement.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 40001)
	reviewPath := fmt.Sprintf("/api/v1/admin/audits/qualifications/%d/approve", replacementID)
	// An application that expired while waiting for review cannot be approved.
	db.MustExec("UPDATE supplier_qualifications SET expires_at=DATE_SUB(CURRENT_DATE,INTERVAL 1 DAY) WHERE id=?", replacementID)
	tradeRequest(t, admin, "POST", reviewPath, nil, 40001)
	db.MustExec("UPDATE supplier_qualifications SET expires_at=DATE_ADD(CURRENT_DATE,INTERVAL 30 DAY) WHERE id=?", replacementID)
	tradeRequest(t, admin, "POST", reviewPath, nil, 0)
	updated := tradeRequest(t, supplier, "GET", "/api/v1/supplier/qualifications", nil, 0)
	for _, value := range updated.Data.([]any) {
		q := value.(map[string]any)
		if q["id"] != float64(replacementID) && q["superseded"] != true {
			t.Fatalf("old certificate not superseded: %v", q)
		}
	}
	tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	if n, err := svc.ProcessQualificationExpiries(); err != nil || n != 1 {
		t.Fatalf("replacement warning: %d %v", n, err)
	}
	products = tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
	for _, value := range products.Data.([]any) {
		p := value.(map[string]any)
		if p["id"] == float64(id) && p["status"] != "frozen" {
			t.Fatalf("old product automatically unfrozen: %v", p)
		}
	}
}

func TestQualificationExpiryBlocksRenewalBeforeSweep(t *testing.T) {
	db := setupTradeDB(t)
	_, _, no, _ := activeRenewalOrder(t, db)
	buyer := tradeRouter(db, 102)
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	db.MustExec("UPDATE supplier_qualifications SET expires_at=DATE_SUB(CURRENT_DATE,INTERVAL 1 DAY) WHERE user_id=101")
	tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no+"/renewal-quote?duration=2", nil, 40001)
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+no+"/renew", renewalInput(quote, "e77e2f1c-b18d-40ac-9545-6bd69d8bb53d"), 40001)
}

func TestQualificationExpiryRecheckedAfterProductReviewWait(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier := tradeRouter(db, 101)
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tx.MustExec("UPDATE products SET stock=stock WHERE id=?", id)
	var connectionID int64
	if err := tx.Get(&connectionID, "SELECT CONNECTION_ID()"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	svc := compute.NewService(compute.NewRepository(db), db)
	go func() { done <- svc.ApproveProduct(id) }()
	waitForRenewalLock(t, db, connectionID)
	// Expiry changes while approval waits, as when the task runs across midnight.
	db.MustExec("UPDATE supplier_qualifications SET expires_at=DATE_SUB(CURRENT_DATE,INTERVAL 1 DAY),status='expired' WHERE user_id=101")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("stale qualification check allowed product approval")
	}
	products := tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
	if products.Data.([]any)[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("expired supplier listed a product: %v", products.Data)
	}
}
