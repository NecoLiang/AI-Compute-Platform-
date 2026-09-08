package compute

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"tokenfactory/internal/legal"
	"tokenfactory/internal/trading"
)

var ErrRenewalConflict = errors.New("续租条件已变化或已有待支付续租订单，请刷新订单")

type Renewal struct {
	OrderID              int64      `db:"order_id" json:"-"`
	ParentID             int64      `db:"parent_order_id" json:"-"`
	ParentOrderNo        string     `db:"parent_order_no" json:"parent_order_no"`
	RequestID            string     `db:"request_id" json:"-"`
	Mode                 string     `db:"mode" json:"mode"`
	PricingMode          string     `db:"pricing_mode" json:"pricing_mode"`
	LeaseEndAt           time.Time  `db:"lease_end_at" json:"lease_end_at"`
	RenewedUntil         *time.Time `db:"renewed_until" json:"renewed_until"`
	AppliedAt            *time.Time `db:"applied_at" json:"applied_at"`
	PreviousLeaseStartAt *time.Time `db:"previous_lease_start_at" json:"-"`
	PreviousConfirmedAt  *time.Time `db:"previous_confirmed_at" json:"-"`
	ConfirmedAt          *time.Time `db:"confirmed_at" json:"confirmed_at"`
}

func GetRenewal(db sqlx.Queryer, orderNo string) (*Renewal, error) {
	var r Renewal
	err := sqlx.Get(db, &r, `SELECT r.*, p.order_no AS parent_order_no FROM order_renewals r
		JOIN orders o ON o.id=r.order_id JOIN orders p ON p.id=r.parent_order_id WHERE o.order_no=?`, orderNo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

type RenewalQuote struct {
	ParentOrderNo string     `json:"parent_order_no"`
	Mode          string     `json:"mode"`
	Quantity      int        `json:"quantity"`
	Duration      int        `json:"duration"`
	PricingMode   string     `json:"pricing_mode"`
	MinDuration   int        `json:"min_duration"`
	MaxDuration   int        `json:"max_duration"`
	UnitPrice     int64      `json:"unit_price"`
	TotalAmount   int64      `json:"total_amount"`
	PlatformFee   int64      `json:"platform_fee"`
	FeeRate       int        `json:"fee_rate"`
	LeaseEndAt    time.Time  `json:"lease_end_at"`
	RenewedUntil  *time.Time `json:"renewed_until"`
}

type RenewOrderReq struct {
	ExpectedPricingMode  string     `json:"expected_pricing_mode"`
	Duration             int        `json:"duration"`
	RequestID            string     `json:"request_id"`
	ComplianceAgreed     bool       `json:"compliance_agreed"`
	ComplianceVersion    string     `json:"compliance_version"`
	ExpectedLeaseEndAt   time.Time  `json:"expected_lease_end_at"`
	ExpectedRenewedUntil *time.Time `json:"expected_renewed_until"`
	ExpectedTotalAmount  int64      `json:"expected_total_amount"`
	ExpectedPlatformFee  int64      `json:"expected_platform_fee"`
}

func (s *Service) renewalQuoteTx(tx *sqlx.Tx, parent *Order, duration int) (*RenewalQuote, error) {
	if parent == nil {
		return nil, fmt.Errorf("order not found")
	}
	if parent.LeaseEnd == nil || parent.StockReserved == nil || (parent.Status != "active" && parent.Status != "completed") {
		return nil, ErrRenewalConflict
	}
	if parent.Status == "completed" && *parent.StockReserved != 0 {
		return nil, ErrRenewalConflict
	}
	child, err := GetRenewal(tx, parent.OrderNo)
	if err != nil {
		return nil, err
	}
	if child != nil {
		return nil, ErrRenewalConflict
	}
	var pending int
	if err := tx.Get(&pending, `SELECT COUNT(*) FROM order_renewals r JOIN orders o ON o.id=r.order_id
		WHERE r.parent_order_id=? AND o.status IN ('pending_payment','refunding','frozen')`, parent.ID); err != nil {
		return nil, err
	}
	if pending > 0 {
		return nil, ErrRenewalConflict
	}
	policy, err := trading.LockConfig(tx)
	if err != nil {
		return nil, err
	}
	if !policy.TradingEnabled {
		return nil, trading.ErrDisabled
	}
	var product Product
	if err := tx.Get(&product, "SELECT "+productColumns+" FROM products WHERE id=? FOR UPDATE", parent.ProductID); err != nil {
		return nil, err
	}
	if product.Health == "offline" || product.PricingMode == PricingPerpetual || (product.Status != "active" && product.Status != "sold_out") {
		return nil, ErrRenewalConflict
	}
	quantity, duration, err := ValidateRenewParams(&product, parent.Quantity, duration)
	if err != nil {
		return nil, err
	}
	total, fee, err := CalcOrderAmount(product.UnitPrice, quantity, duration, int64(policy.FeeRate))
	if err != nil {
		return nil, err
	}
	q := &RenewalQuote{ParentOrderNo: parent.OrderNo, Mode: "extend", Quantity: quantity, Duration: duration,
		PricingMode: product.PricingMode, MinDuration: max(1, product.MinDuration), MaxDuration: MaxDurationFor(product.PricingMode),
		UnitPrice: product.UnitPrice, TotalAmount: total, PlatformFee: fee, FeeRate: policy.FeeRate, LeaseEndAt: *parent.LeaseEnd}
	if parent.LeaseEnd.After(time.Now()) {
		if parent.Status != "active" || *parent.StockReserved != parent.Quantity {
			return nil, ErrRenewalConflict
		}
		if err := requireRenewableAccess(tx, parent.ID); err != nil {
			return nil, err
		}
		end := LeaseEndAt(*parent.LeaseEnd, product.PricingMode, duration)
		q.RenewedUntil = &end
	} else {
		q.Mode = "restart"
		if *parent.StockReserved != 0 && *parent.StockReserved != parent.Quantity {
			return nil, ErrRenewalConflict
		}
		if *parent.StockReserved == 0 && product.Stock < quantity {
			return nil, fmt.Errorf("insufficient stock")
		}
	}
	return q, nil
}

func (s *Service) GetRenewalQuote(buyerID int64, orderNo string, duration int) (*RenewalQuote, error) {
	if err := s.repo.RequireTradingAccess(buyerID, "buyer"); err != nil {
		return nil, err
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	parent, err := s.repo.GetOrderForUpdateTx(tx, orderNo)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.BuyerID != buyerID {
		return nil, fmt.Errorf("order not found")
	}
	return s.renewalQuoteTx(tx, parent, duration)
}

func (s *Service) RenewOrder(buyerID int64, orderNo string, req RenewOrderReq) (*Order, error) {
	if err := s.repo.RequireTradingAccess(buyerID, "buyer"); err != nil {
		return nil, err
	}
	if !req.ComplianceAgreed {
		return nil, fmt.Errorf("请先确认算力资源使用规范")
	}
	if err := legal.ValidateVersion(req.ComplianceVersion); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.RequestID); err != nil || len(req.RequestID) != 36 {
		return nil, fmt.Errorf("invalid request_id")
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	parent, err := s.repo.GetOrderForUpdateTx(tx, orderNo)
	if err != nil {
		return nil, err
	}
	if parent == nil || parent.BuyerID != buyerID {
		return nil, fmt.Errorf("order not found")
	}
	var previous string
	err = tx.Get(&previous, `SELECT o.order_no FROM order_renewals r JOIN orders o ON o.id=r.order_id WHERE r.parent_order_id=? AND r.request_id=?`, parent.ID, req.RequestID)
	if err == nil {
		o, err := s.repo.GetOrderForUpdateTx(tx, previous)
		if err != nil {
			return nil, err
		}
		r, err := GetRenewal(tx, previous)
		if err != nil {
			return nil, err
		}
		if r.PricingMode != req.ExpectedPricingMode || o.Duration != req.Duration || o.TotalAmount != req.ExpectedTotalAmount || o.PlatformFee != req.ExpectedPlatformFee ||
			!r.LeaseEndAt.Equal(req.ExpectedLeaseEndAt) || !sameTime(r.RenewedUntil, req.ExpectedRenewedUntil) {
			return nil, ErrRenewalConflict
		}
		return o, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	q, err := s.renewalQuoteTx(tx, parent, req.Duration)
	if err != nil {
		return nil, err
	}
	if q.PricingMode != req.ExpectedPricingMode || !q.LeaseEndAt.Equal(req.ExpectedLeaseEndAt) || !sameTime(q.RenewedUntil, req.ExpectedRenewedUntil) || q.TotalAmount != req.ExpectedTotalAmount || q.PlatformFee != req.ExpectedPlatformFee {
		return nil, ErrRenewalConflict
	}
	expires := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	if q.Mode == "extend" && parent.LeaseEnd.Before(expires) {
		expires = *parent.LeaseEnd
	}
	reserved := 0
	if q.Mode == "restart" && *parent.StockReserved == 0 {
		reserved = parent.Quantity
		if err := s.repo.DecrProductStock(tx, parent.ProductID, reserved); err != nil {
			return nil, err
		}
	}
	o := &Order{OrderNo: "REN" + time.Now().Format("20060102150405") + uuid.New().String()[:6], BuyerID: buyerID,
		ProductID: parent.ProductID, StockReserved: &reserved, Quantity: q.Quantity, Duration: q.Duration, UnitPrice: q.UnitPrice,
		TotalAmount: q.TotalAmount, PlatformFee: q.PlatformFee, Status: "pending_payment", PaymentExpires: &expires, ComplianceAgreed: true}
	if err := s.repo.CreateOrderTx(tx, o); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO order_renewals (order_id,parent_order_id,request_id,mode,pricing_mode,lease_end_at,renewed_until)
		SELECT id,?,?,?,?,?,? FROM orders WHERE order_no=?`, parent.ID, req.RequestID, q.Mode, q.PricingMode, q.LeaseEndAt, q.RenewedUntil, o.OrderNo); err != nil {
		return nil, err
	}
	if err := legal.Record(tx, buyerID, legal.Acceptance{Document: "resource-usage-rules", Version: req.ComplianceVersion}, "order", o.OrderNo); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.attest("order", o.OrderNo, func() (any, error) { return s.BuildOrderAttestPayload(o.OrderNo) })
	return o, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// Lock the lease before its renewal in every lifecycle/payment transaction.
func (r *Repository) LockOrderFamilyTx(tx *sqlx.Tx, orderNo string) (*Order, *Order, *Renewal, error) {
	renewal, err := GetRenewal(tx, orderNo)
	if err != nil {
		return nil, nil, nil, err
	}
	var parent *Order
	if renewal != nil {
		parent, err = r.GetOrderForUpdateTx(tx, renewal.ParentOrderNo)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	order, err := r.GetOrderForUpdateTx(tx, orderNo)
	if err != nil {
		return nil, nil, nil, err
	}
	if renewal != nil {
		// A locking read sees a callback that committed while waiting for the parent.
		if err := tx.Get(renewal, `SELECT r.*, p.order_no AS parent_order_no FROM order_renewals r JOIN orders p ON p.id=r.parent_order_id WHERE r.order_id=? FOR UPDATE`, order.ID); err != nil {
			return nil, nil, nil, err
		}
	}
	return order, parent, renewal, nil
}

func ValidateRenewalPaymentTx(tx *sqlx.Tx, order, parent *Order, r *Renewal) error {
	if r == nil {
		return nil
	}
	if r.AppliedAt != nil || parent == nil || parent.BuyerID != order.BuyerID || parent.ProductID != order.ProductID ||
		parent.Quantity != order.Quantity || parent.StockReserved == nil || order.StockReserved == nil ||
		parent.LeaseEnd == nil || !parent.LeaseEnd.Equal(r.LeaseEndAt) {
		return ErrRenewalConflict
	}
	if r.Mode == "extend" {
		if parent.Status != "active" || !parent.LeaseEnd.After(time.Now()) || *parent.StockReserved != parent.Quantity || *order.StockReserved != 0 || r.RenewedUntil == nil {
			return ErrRenewalConflict
		}
		if err := requireRenewableAccess(tx, parent.ID); err != nil {
			return err
		}
	} else if (parent.Status != "active" && parent.Status != "completed") || parent.LeaseEnd.After(time.Now()) || *parent.StockReserved+*order.StockReserved != order.Quantity {
		return ErrRenewalConflict
	}
	return nil
}

func requireRenewableAccess(tx *sqlx.Tx, parentID int64) error {
	var usable bool
	err := tx.Get(&usable, `SELECT COALESCE(access_status='delivered' AND access_expires_at>NOW(),0)
		FROM order_deliveries WHERE order_id=? FOR UPDATE`, parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRenewalConflict
	}
	if err != nil {
		return err
	}
	if !usable {
		return ErrRenewalConflict
	}
	return nil
}

// Payment, lease, credential expiry and stock ownership commit together.
func ApplyRenewalPaymentTx(tx *sqlx.Tx, order, parent *Order, r *Renewal) error {
	if r == nil {
		return nil
	}
	if err := ValidateRenewalPaymentTx(tx, order, parent, r); err != nil {
		return err
	}
	if r.Mode == "extend" {
		if _, err := tx.Exec("UPDATE orders SET lease_end_at=? WHERE id=?", r.RenewedUntil, parent.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE order_deliveries SET access_expires_at=? WHERE order_id=?", r.RenewedUntil, parent.ID); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE order_renewals r JOIN orders p ON p.id=r.parent_order_id
			JOIN order_deliveries d ON d.order_id=p.id SET r.previous_lease_start_at=p.lease_start_at,
			r.previous_confirmed_at=d.buyer_confirmed_at WHERE r.order_id=?`, order.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE orders SET status='paid',stock_reserved=?,lease_start_at=NULL,lease_end_at=NULL WHERE id=?", order.Quantity, parent.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE order_deliveries SET access_status='revoked',revoked_at=NOW() WHERE order_id=?", parent.ID); err != nil {
			return err
		}
	}
	var start *time.Time
	if r.Mode == "extend" {
		start = &r.LeaseEndAt
	}
	if _, err := tx.Exec("UPDATE orders SET status='completed',stock_reserved=0,lease_start_at=?,lease_end_at=? WHERE id=?", start, r.RenewedUntil, order.ID); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE order_renewals SET applied_at=NOW() WHERE order_id=?", order.ID)
	return err
}

func (s *Service) currentLeaseOrder(db sqlx.Queryer, parent *Order, fallbackPricingMode string) (*Order, string, error) {
	var lease struct {
		ID          int64  `db:"id"`
		OrderNo     string `db:"order_no"`
		Duration    int    `db:"duration"`
		PricingMode string `db:"pricing_mode"`
	}
	err := sqlx.Get(db, &lease, `SELECT o.id,o.order_no,o.duration,r.pricing_mode FROM order_renewals r
		JOIN orders o ON o.id=r.order_id WHERE r.parent_order_id=? AND r.mode='restart'
		AND r.applied_at IS NOT NULL AND o.status IN ('completed','refunding') ORDER BY r.order_id DESC LIMIT 1 FOR SHARE`, parent.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return parent, fallbackPricingMode, nil
	}
	if err != nil {
		return nil, "", err
	}
	return &Order{ID: lease.ID, OrderNo: lease.OrderNo, Duration: lease.Duration}, lease.PricingMode, nil
}

func (s *Service) returnReservedStockTx(tx *sqlx.Tx, order *Order) error {
	if order.StockReserved == nil {
		return fmt.Errorf("order %s requires stock reservation reconciliation", order.OrderNo)
	}
	if *order.StockReserved > 0 {
		if err := s.repo.IncrProductStock(tx, order.ProductID, *order.StockReserved); err != nil {
			return err
		}
	}
	_, err := tx.Exec("UPDATE orders SET stock_reserved=0 WHERE id=?", order.ID)
	return err
}

func (s *Service) cancelPendingRenewalsTx(tx *sqlx.Tx, parentID int64) error {
	var nos []string
	if err := tx.Select(&nos, `SELECT o.order_no FROM order_renewals r JOIN orders o ON o.id=r.order_id
		WHERE r.parent_order_id=? AND o.status='pending_payment' ORDER BY o.id FOR UPDATE`, parentID); err != nil {
		return err
	}
	for _, no := range nos {
		order, err := s.repo.GetOrderForUpdateTx(tx, no)
		if err != nil {
			return err
		}
		if err := s.returnReservedStockTx(tx, order); err != nil {
			return err
		}
		if err := s.repo.UpdateOrderStatusTx(tx, no, "cancelled"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) CancelBuyerOrder(buyerID int64, orderNo string) error {
	o, err := s.repo.GetBuyerOrderByNo(buyerID, orderNo)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("order not found")
	}
	if o.Status == "cancelled" {
		return nil
	}
	moved, err := s.releaseStock(orderNo, []string{"pending_payment"}, "cancelled")
	if err != nil {
		return err
	}
	if !moved {
		return ErrRenewalConflict
	}
	return nil
}

func unusedRenewal(parent *Order, r *Renewal) bool {
	if parent == nil || r == nil || r.AppliedAt == nil {
		return false
	}
	if r.Mode == "extend" {
		return parent.Status == "active" && sameTime(parent.LeaseEnd, r.RenewedUntil) && r.LeaseEndAt.After(time.Now())
	}
	return r.ConfirmedAt == nil && (parent.Status == "paid" || parent.Status == "provisioning")
}

func (s *Service) refundRenewalTx(tx *sqlx.Tx, parent *Order, r *Renewal) error {
	if !unusedRenewal(parent, r) {
		return fmt.Errorf("续租已开始使用或已被后续租期覆盖，请联系平台处理退款")
	}
	if err := s.cancelPendingRenewalsTx(tx, parent.ID); err != nil {
		return err
	}
	if r.Mode == "extend" {
		if _, err := tx.Exec("UPDATE orders SET lease_end_at=? WHERE id=?", r.LeaseEndAt, parent.ID); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE order_deliveries SET access_expires_at=? WHERE order_id=?", r.LeaseEndAt, parent.ID)
		return err
	}
	if err := s.returnReservedStockTx(tx, parent); err != nil {
		return err
	}
	if err := s.repo.RevokeAccessByOrderTx(tx, parent.ID); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE orders SET status='completed',lease_start_at=?,lease_end_at=? WHERE id=?", r.PreviousLeaseStartAt, r.LeaseEndAt, parent.ID)
	return err
}
