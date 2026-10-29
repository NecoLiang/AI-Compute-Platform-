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
	"tokenfactory/pkg/errcode"
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

// renewalQuoteTx 续租报价。ev=RenewCheck(只读报价) / RenewReserve(下单: 托管 restart 同事务预留卡槽)。
// 托管订单不占 stock_reserved / products.stock, 余量由 LeaseHooks.RenewTx 按卡槽判定(docs/25 §25.7.2)。
func (s *Service) renewalQuoteTx(tx *sqlx.Tx, parent *Order, duration int, ev string) (*RenewalQuote, error) {
	if parent == nil {
		return nil, fmt.Errorf("order not found")
	}
	if err := s.requireManagedRenewable(parent); err != nil {
		return nil, err
	}
	managed := parent.FulfillmentMode == FulfillmentManaged
	if parent.LeaseEnd == nil || parent.StockReserved == nil || (parent.Status != "active" && parent.Status != "completed") {
		return nil, ErrRenewalConflict
	}
	if parent.Status == "completed" && *parent.StockReserved != 0 {
		return nil, ErrRenewalConflict
	}
	restart := !parent.LeaseEnd.After(time.Now())
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
	if managed {
		// 在锁商品行之前: 租约 → 节点行 → 卡槽 → 商品行(与下单预留、fleet 任务一致)。
		mode := "extend"
		if restart {
			mode = "restart"
		}
		if err := s.leaseHooks.RenewTx(tx, ev, parent, &Renewal{Mode: mode}); err != nil {
			return nil, err
		}
	}
	var product Product
	if err := tx.Get(&product, "SELECT "+productColumns+" FROM products WHERE id=? FOR UPDATE", parent.ProductID); err != nil {
		return nil, err
	}
	if err := attachPrices(tx, []*Product{&product}); err != nil {
		return nil, err
	}
	// 续租按原订单的计费方式快照报价, 不随商品展示主价漂移; 快照缺失返回 50000, 不回退商品当前方式(CR M1)。
	mode, err := OrderPricingMode(parent)
	if err != nil {
		return nil, err
	}
	if product.Health == "offline" || mode == PricingPerpetual || (product.Status != "active" && product.Status != "sold_out") {
		return nil, ErrRenewalConflict
	}
	if err := requireUnexpiredQualifications(tx, product.SupplierID); err != nil {
		return nil, err
	}
	// 供给方已停用该计费方式 → ErrPricingModeDisabled(40900, 提示重新下单), 不换算成其他方式。
	price, err := PriceFor(&product, mode)
	if err != nil {
		return nil, err
	}
	// 托管订单同样按上面的快照报价(RenewTx 未接入时已在入口 50000)。
	// 包夜续租依赖夜间预订(T-094), 未接入前 50000。
	if mode == PricingNightly {
		return nil, errNightlyNotWired
	}
	quantity, duration, err := ValidateRenewParamsFor(&product, price, parent.Quantity, duration)
	if err != nil {
		return nil, err
	}
	total, fee, err := CalcOrderAmount(price.UnitPrice, quantity, duration, int64(policy.FeeRate))
	if err != nil {
		return nil, err
	}
	q := &RenewalQuote{ParentOrderNo: parent.OrderNo, Mode: "extend", Quantity: quantity, Duration: duration,
		PricingMode: mode, MinDuration: price.MinDuration, MaxDuration: MaxDurationFor(mode),
		UnitPrice: price.UnitPrice, TotalAmount: total, PlatformFee: fee, FeeRate: policy.FeeRate, LeaseEndAt: *parent.LeaseEnd}
	if !restart {
		if parent.Status != "active" || (!managed && *parent.StockReserved != parent.Quantity) {
			return nil, ErrRenewalConflict
		}
		if err := requireRenewableAccess(tx, parent.ID); err != nil {
			return nil, err
		}
		end, err := LeaseEndAt(*parent.LeaseEnd, mode, duration, OrderNightWindow(parent))
		if err != nil {
			return nil, err
		}
		q.RenewedUntil = &end
	} else {
		q.Mode = "restart"
		if *parent.StockReserved != 0 && *parent.StockReserved != parent.Quantity {
			return nil, ErrRenewalConflict
		}
		if !managed && *parent.StockReserved == 0 && product.Stock < quantity {
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
	return s.renewalQuoteTx(tx, parent, duration, RenewCheck)
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
	managed, err := s.orderIsManaged(orderNo)
	if err != nil {
		return nil, err
	}
	tx, err := s.beginManagedAware(managed) // 托管 restart 预留卡槽后重算派生库存须 READ COMMITTED
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
	q, err := s.renewalQuoteTx(tx, parent, req.Duration, RenewReserve)
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
	// 托管订单的 restart 已由 RenewTx(reserve) 预留卡槽, 续租单 stock_reserved 保持 0。
	if q.Mode == "restart" && *parent.StockReserved == 0 && parent.FulfillmentMode != FulfillmentManaged {
		reserved = parent.Quantity
		if err := s.repo.DecrProductStock(tx, parent.ProductID, reserved); err != nil {
			return nil, err
		}
	}
	o := &Order{OrderNo: "REN" + time.Now().Format("20060102150405") + uuid.New().String()[:6], BuyerID: buyerID,
		ProductID: parent.ProductID, StockReserved: &reserved, Quantity: q.Quantity, Duration: q.Duration, UnitPrice: q.UnitPrice,
		PricingMode: &q.PricingMode, NightStartMin: parent.NightStartMin, NightEndMin: parent.NightEndMin,
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

// errManagedRenewalPaymentNotWired 托管原订单的续租收款必须经 compute.Service(含租约侧校验/落账), 包级函数不处理。
var errManagedRenewalPaymentNotWired = errcode.New(errcode.InternalError, "托管订单续租收款须经租约钩子处理(payment 未注入 compute 续租钩子)")

// ValidateRenewalPaymentTx 人工订单续租的收款前校验(包级, payment 未注入 Service 钩子时的回退)。托管原订单返回 50000。
func ValidateRenewalPaymentTx(tx *sqlx.Tx, order, parent *Order, r *Renewal) error {
	if r == nil {
		return nil
	}
	if parent != nil && parent.FulfillmentMode == FulfillmentManaged {
		return errManagedRenewalPaymentNotWired
	}
	return validateRenewalBaseTx(tx, order, parent, r, false)
}

// validateRenewalBaseTx 订单侧续租校验。managed=true 时不校验 stock_reserved(托管占用是卡槽, 由 RenewTx(verify) 校验)。
func validateRenewalBaseTx(tx *sqlx.Tx, order, parent *Order, r *Renewal, managed bool) error {
	if r.AppliedAt != nil || parent == nil || parent.BuyerID != order.BuyerID || parent.ProductID != order.ProductID ||
		parent.Quantity != order.Quantity || parent.StockReserved == nil || order.StockReserved == nil ||
		parent.LeaseEnd == nil || !parent.LeaseEnd.Equal(r.LeaseEndAt) {
		return ErrRenewalConflict
	}
	if r.Mode == "extend" {
		if parent.Status != "active" || !parent.LeaseEnd.After(time.Now()) || *order.StockReserved != 0 || r.RenewedUntil == nil ||
			(!managed && *parent.StockReserved != parent.Quantity) {
			return ErrRenewalConflict
		}
		if err := requireRenewableAccess(tx, parent.ID); err != nil {
			return err
		}
	} else if (parent.Status != "active" && parent.Status != "completed") || parent.LeaseEnd.After(time.Now()) ||
		(!managed && *parent.StockReserved+*order.StockReserved != order.Quantity) ||
		(managed && (*parent.StockReserved != 0 || *order.StockReserved != 0)) {
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
// ApplyRenewalPaymentTx 人工订单续租收款落账(包级回退)。托管原订单返回 50000。
func ApplyRenewalPaymentTx(tx *sqlx.Tx, order, parent *Order, r *Renewal) error {
	if r == nil {
		return nil
	}
	if err := ValidateRenewalPaymentTx(tx, order, parent, r); err != nil {
		return err
	}
	return applyRenewalBaseTx(tx, order, parent, r)
}

// applyRenewalBaseTx 订单侧续租落账(调用方已校验)。restart: 原订单 → paid, 人工订单占用 stock_reserved=quantity,
// 托管订单保持 0(占用是租约卡槽)。
func applyRenewalBaseTx(tx *sqlx.Tx, order, parent *Order, r *Renewal) error {
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
		held := order.Quantity
		if parent.FulfillmentMode == FulfillmentManaged {
			held = 0
		}
		if _, err := tx.Exec("UPDATE orders SET status='paid',stock_reserved=?,lease_start_at=NULL,lease_end_at=NULL WHERE id=?", held, parent.ID); err != nil {
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

// returnReservedStockTx 人工订单归还 stock_reserved。托管订单的占用是卡槽, 必须经 releaseHoldTx → fleet 释放并重算
// 派生库存; 误入此路径会把人工增量叠加到派生库存上, 因此直接报错而不是静默跳过。
func (s *Service) returnReservedStockTx(tx *sqlx.Tx, order *Order) error {
	if order.FulfillmentMode == FulfillmentManaged {
		return errcode.New(errcode.InternalError, fmt.Sprintf("托管订单 %s 的占用须由租约释放, 不走人工库存归还", order.OrderNo))
	}
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
	var parent *Order
	if len(nos) > 0 {
		p, err := s.lockOrderByIDTx(tx, parentID)
		if err != nil {
			return err
		}
		parent = p
	}
	for _, no := range nos {
		order, err := s.repo.GetOrderForUpdateTx(tx, no)
		if err != nil {
			return err
		}
		if isManagedParent(parent) {
			r, err := GetRenewal(tx, no)
			if err != nil {
				return err
			}
			if err := s.releaseManagedRenewalTx(tx, parent, r); err != nil {
				return err
			}
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
	managed := isManagedParent(parent)
	if managed && s.leaseHooks.RenewTx == nil {
		return errManagedRenewNotWired
	}
	if r.Mode == "extend" {
		if _, err := tx.Exec("UPDATE orders SET lease_end_at=? WHERE id=?", r.LeaseEndAt, parent.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE order_deliveries SET access_expires_at=? WHERE order_id=?", r.LeaseEndAt, parent.ID); err != nil {
			return err
		}
		if managed {
			return s.leaseHooks.RenewTx(tx, RenewRevert, parent, r)
		}
		return nil
	}
	if managed {
		// 托管 restart 尚未开通完成即退款: 租约清除(卡槽 → draining → purge 后释放), 不走人工库存归还。
		if err := s.leaseHooks.RenewTx(tx, RenewRevert, parent, r); err != nil {
			return err
		}
	} else if err := s.returnReservedStockTx(tx, parent); err != nil {
		return err
	}
	if err := s.repo.RevokeAccessByOrderTx(tx, parent.ID); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE orders SET status='completed',lease_start_at=?,lease_end_at=? WHERE id=?", r.PreviousLeaseStartAt, r.LeaseEndAt, parent.ID)
	return err
}
