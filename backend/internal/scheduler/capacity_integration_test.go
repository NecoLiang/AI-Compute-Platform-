package scheduler

import (
	"strings"
	"testing"
)

func TestAdviseUsesProductTransactionUnits(t *testing.T) {
	db, svc := setupSchedDB(t)
	db.MustExec("INSERT INTO products(id,supplier_id,product_type,card_count,machine_count) VALUES(1,10,'outright',40,5)")
	db.MustExec("INSERT INTO orders(order_no,product_id,quantity,status) VALUES('ORD-CAPACITY',1,2,'paid')")
	small, keySmall, err := svc.RegisterNode(10, 1, "small", 8)
	if err != nil {
		t.Fatal(err)
	}
	large, keyLarge, err := svc.RegisterNode(10, 1, "large", 32)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Heartbeat(small.ID, keySmall, 8, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = svc.Heartbeat(large.ID, keyLarge, 32, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind                  string
		cards, machines, want int
	}{
		{"card_rental", 40, 5, 2}, {"outright", 40, 5, 16}, {"center", 40, 5, 16},
		{"outright", 0, 5, 0}, {"center", 41, 5, 0}, {"outright", 40, 0, 0}, {"colocation", 40, 5, 0},
	} {
		db.MustExec("UPDATE products SET product_type=?,card_count=?,machine_count=? WHERE id=1", tc.kind, tc.cards, tc.machines)
		advice, err := svc.Advise("ORD-CAPACITY", 10)
		if tc.want == 0 {
			if err == nil {
				t.Fatalf("ambiguous or unsupported capacity must not recommend: kind=%s cards=%d machines=%d", tc.kind, tc.cards, tc.machines)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if advice.NeedCards != tc.want {
			t.Fatalf("%s: want %d cards, got %d", tc.kind, tc.want, advice.NeedCards)
		}
		if tc.want > 8 && (advice.Nodes[0].NodeID != large.ID || advice.Nodes[1].Verdict != "unavailable") {
			t.Fatal("insufficient node recommended for machine order")
		}
	}
	db.MustExec("UPDATE products SET card_count=NULL, machine_count=NULL, product_type='card_rental' WHERE id=1")
	if advice, err := svc.Advise("ORD-CAPACITY", 10); err != nil || advice.NeedCards != 2 {
		t.Fatal("card-rental quantity must work with nullable optional product specifications")
	}
	for _, kind := range []string{"outright", "center"} {
		db.MustExec("UPDATE products SET product_type=? WHERE id=1", kind)
		if _, err := svc.Advise("ORD-CAPACITY", 10); err == nil || !strings.Contains(err.Error(), "无法确定每台卡数") {
			t.Fatalf("NULL specifications must return an actionable capacity error, got %v", err)
		}
	}
	if _, err := svc.Advise("ORD-CAPACITY", 99); err == nil {
		t.Fatal("foreign supplier must be rejected")
	}
}
