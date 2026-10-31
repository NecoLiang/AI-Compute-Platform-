package fleet

import (
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"
)

// errRefundUnavailable 退费补偿通道(T-106 + 易宝部分退款书面确认)未就绪(docs/25 §25.7.6)。
var errRefundUnavailable = errcode.New(errcode.InternalError, "易宝部分退款能力未确认，暂只支持顺延补偿")

// BuyerOutage 买家可见的一段停机(不含认定原文与责任金额)。
type BuyerOutage struct {
	ID              int64      `json:"id"`
	Cause           string     `json:"cause"`
	Liable          string     `json:"liable"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	Ongoing         bool       `json:"ongoing"`
	Minutes         int        `json:"minutes"`
	Counted         *bool      `json:"counted"`
	Remedy          *string    `json:"remedy"`
	RemedyAppliedAt *time.Time `json:"remedy_applied_at"`
	RemedyAmount    *int64     `json:"remedy_amount"`
}

// OrderOutages GET /orders/:id/outages。
type OrderOutages struct {
	OrderNo            string        `json:"order_no"`
	LeaseNo            string        `json:"lease_no"`
	LeaseState         string        `json:"lease_state"`
	AccessUntil        *time.Time    `json:"access_until"`
	Remedy             string        `json:"remedy"`
	RefundRequestedAt  *time.Time    `json:"refund_requested_at"`
	CanChangeRemedy    bool          `json:"can_change_remedy"`
	RefundEligible     bool          `json:"refund_eligible"`
	RefundAvailable    bool          `json:"refund_available"`
	TermMinutes        int           `json:"term_minutes"`
	TermOutageMinutes  int           `json:"term_outage_minutes"`
	LongestMinutes     int           `json:"longest_outage_minutes"`
	RefundRatio        float64       `json:"refund_ratio"`
	RefundSingleHours  int           `json:"refund_single_hours"`
	ExtendedMinutes    int           `json:"extended_minutes"`
	PendingRefundTotal int64         `json:"pending_refund_amount"`
	Items              []BuyerOutage `json:"items"`
}

// orderTerm 订单实付与租期秒数(按计费方式快照换算)。
func orderTerm(q sqlx.Queryer, orderID int64) (amount int64, termSecs int64, err error) {
	var o compute.Order
	if err := sqlx.Get(q, &o, `SELECT id, order_no, total_amount, duration, pricing_mode, lease_start_at
		FROM orders WHERE id=?`, orderID); err != nil {
		return 0, 0, err
	}
	if o.LeaseStart == nil {
		return o.TotalAmount, 0, nil
	}
	mode, err := compute.OrderPricingMode(&o)
	if err != nil {
		return 0, 0, err
	}
	end, err := compute.LeaseEndAt(*o.LeaseStart, mode, o.Duration, nil)
	if err != nil {
		return 0, 0, err
	}
	return o.TotalAmount, int64(end.Sub(*o.LeaseStart) / time.Second), nil
}

// remedyChangeable 补偿方式只能在租期结束前修改。
func remedyChangeable(l *Lease, now time.Time) bool {
	if l.AccessUntil == nil || !l.AccessUntil.After(now) {
		return false
	}
	return l.State == LeaseActive || l.State == LeaseParked || l.State == LeaseSuspended
}

func (s *Service) refundAvailable() bool {
	return s.refunder != nil && s.cfg.Outage.YeepayPartialRefundConfirmed
}

// orderOutages 汇总(门槛按当前所属订单的租期与该订单内的停机计, Q-47: 累计 ≥ 租期 × refund_ratio 或单次 ≥ refund_single_hours)。
func (s *Service) orderOutages(o *compute.Order, l *Lease) (*OrderOutages, error) {
	now := time.Now()
	var rows []Outage
	if err := s.repo.db.Select(&rows, `SELECT `+outageColumns+` FROM lease_outages WHERE lease_id=? ORDER BY started_at DESC, id DESC`,
		l.ID); err != nil {
		return nil, err
	}
	var reqAt *time.Time
	if err := s.repo.db.Get(&reqAt, `SELECT outage_refund_requested_at FROM leases WHERE id=?`, l.ID); err != nil {
		return nil, err
	}
	cover, err := s.coveringOrderID(s.repo.db, l.OrderID, now)
	if err != nil {
		return nil, err
	}
	_, term, err := orderTerm(s.repo.db, cover)
	if err != nil {
		return nil, err
	}
	r := &OrderOutages{OrderNo: o.OrderNo, LeaseNo: l.LeaseNo, LeaseState: l.State, AccessUntil: l.AccessUntil,
		Remedy: l.OutageRemedy, RefundRequestedAt: reqAt, CanChangeRemedy: remedyChangeable(l, now),
		RefundAvailable: s.refundAvailable(), TermMinutes: int(term / 60), RefundRatio: s.cfg.Outage.RefundRatio,
		RefundSingleHours: s.cfg.Outage.RefundSingleHours, Items: make([]BuyerOutage, 0, len(rows))}
	var termSecs, longest int64
	for _, x := range rows {
		b := BuyerOutage{ID: x.ID, Cause: x.Cause, Liable: x.Liable, StartedAt: x.StartedAt, EndedAt: x.EndedAt,
			Counted: x.Counted, Remedy: x.Remedy, RemedyAppliedAt: x.RemedyAppliedAt, RemedyAmount: x.RemedyAmount}
		var secs int64
		switch {
		case x.EndedAt == nil:
			b.Ongoing = true
			secs = int64(now.Sub(x.StartedAt) / time.Second)
		case x.Counted != nil && *x.Counted && x.Seconds != nil:
			secs = int64(*x.Seconds)
		}
		b.Minutes = int(secs / 60)
		if x.Minutes != nil && !b.Ongoing {
			b.Minutes = *x.Minutes
		}
		if x.Remedy != nil && *x.Remedy == "extend" && x.Seconds != nil {
			r.ExtendedMinutes += *x.Seconds / 60
		}
		if x.Remedy != nil && *x.Remedy == "refund" && x.RemedyAppliedAt == nil && x.RemedyAmount != nil {
			r.PendingRefundTotal += *x.RemedyAmount
		}
		if x.OrderID == cover {
			termSecs += secs
			longest = max(longest, secs)
		}
		r.Items = append(r.Items, b)
	}
	r.TermOutageMinutes = int(termSecs / 60)
	r.LongestMinutes = int(longest / 60)
	r.RefundEligible = term > 0 && (float64(termSecs) >= s.cfg.Outage.RefundRatio*float64(term) ||
		longest >= int64(s.cfg.Outage.RefundSingleHours)*3600)
	return r, nil
}

// OrderOutages 买家查看本人托管订单的停机记录与补偿。
func (s *Service) OrderOutages(userID int64, orderNo string) (*OrderOutages, error) {
	o, l, err := s.buyerManagedLease(userID, orderNo)
	if err != nil {
		return nil, err
	}
	return s.orderOutages(o, l)
}

// SetOutageRemedy 买家选择停机补偿方式。extend 随时可选(租期内); refund 须达 Q-47 门槛,
// 且退费通道(T-106 + yeepay_partial_refund_confirmed)就绪, 否则记录退费意向并返回 50000、补偿仍按顺延执行。
func (s *Service) SetOutageRemedy(userID int64, orderNo, remedy string) (*OrderOutages, error) {
	if remedy != "extend" && remedy != "refund" {
		return nil, errcode.New(errcode.ParamInvalid, "remedy 只能是 extend 或 refund")
	}
	o, l, err := s.buyerManagedLease(userID, orderNo)
	if err != nil {
		return nil, err
	}
	if !remedyChangeable(l, time.Now()) {
		return nil, errcode.New(errcode.Conflict, "租期已结束, 不能再修改补偿方式; 未补偿的停机将按比例退费")
	}
	const inTerm = `state IN ('active','parked','suspended') AND access_until > NOW()`
	if remedy == "extend" {
		if _, err := s.repo.db.Exec(`UPDATE leases SET outage_remedy='extend', outage_refund_requested_at=NULL
			WHERE id=? AND `+inTerm, l.ID); err != nil {
			return nil, err
		}
		return s.reloadOutages(o, l.ID)
	}
	sum, err := s.orderOutages(o, l)
	if err != nil {
		return nil, err
	}
	if !sum.RefundEligible {
		return nil, errcode.Newf(errcode.Conflict, "未达到退费条件(本期累计停机 ≥ 租期的 %.0f%% 或单次停机 ≥ %d 小时), 只能顺延补偿",
			s.cfg.Outage.RefundRatio*100, s.cfg.Outage.RefundSingleHours)
	}
	if !s.refundAvailable() {
		// 记录买家的退费意向(T-106 上线后可据此处理), 补偿方式保持顺延。
		if _, err := s.repo.db.Exec(`UPDATE leases SET outage_refund_requested_at=NOW()
			WHERE id=? AND outage_refund_requested_at IS NULL AND `+inTerm, l.ID); err != nil {
			return nil, err
		}
		return nil, errRefundUnavailable
	}
	ok, err := affected(s.repo.db.Exec(`UPDATE leases SET outage_remedy='refund',
		outage_refund_requested_at=COALESCE(outage_refund_requested_at, NOW()) WHERE id=? AND `+inTerm, l.ID))
	if err != nil {
		return nil, err
	}
	if !ok && l.OutageRemedy != "refund" {
		return nil, errcode.New(errcode.Conflict, "租约状态已变化, 请刷新后重试")
	}
	return s.reloadOutages(o, l.ID)
}

func (s *Service) reloadOutages(o *compute.Order, leaseID int64) (*OrderOutages, error) {
	l, err := s.repo.getLease(s.repo.db, leaseID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errcode.New(errcode.NotFound, "订单尚无租约")
	}
	return s.orderOutages(o, l)
}

// AdminOutage 运营查看的停机段(含认定原文与责任金额)。
type AdminOutage struct {
	Outage
	LeaseNo    string `db:"lease_no" json:"lease_no"`
	OrderNo    string `db:"order_no" json:"order_no"`
	SupplierID *int64 `db:"supplier_id" json:"supplier_id"`
}

// OutageFilter 运营检索条件。
type OutageFilter struct {
	LeaseNo, OrderNo, Cause string
	SupplierID              int64
	Open, RefundPending     bool
}

var outageCauses = []string{OutageNodeOffline, OutageGPUFault, OutageContainerDown, OutageFullProbeFailed,
	"night_unpark_failed", "provision_delay", OutageGatewayDown, OutageWrongFreeze}

// AdminOutages GET /admin/lease-outages。
func (s *Service) AdminOutages(f OutageFilter, page, size int) ([]AdminOutage, int64, int, int, error) {
	if f.Cause != "" && !containsStr(outageCauses, f.Cause) {
		return nil, 0, 0, 0, errcode.New(errcode.ParamInvalid, "cause 仅支持 "+strings.Join(outageCauses, "/"))
	}
	if f.SupplierID < 0 {
		return nil, 0, 0, 0, errcode.New(errcode.ParamInvalid, "supplier_id 非法")
	}
	page, size = normPage(page, size)
	where, args := []string{"1=1"}, []any{}
	if f.LeaseNo != "" {
		where, args = append(where, "l.lease_no=?"), append(args, f.LeaseNo)
	}
	if f.OrderNo != "" {
		where, args = append(where, "(od.order_no=? OR ro.order_no=?)"), append(args, f.OrderNo, f.OrderNo)
	}
	if f.Cause != "" {
		where, args = append(where, "o.cause=?"), append(args, f.Cause)
	}
	if f.SupplierID > 0 {
		where, args = append(where, "n.supplier_id=?"), append(args, f.SupplierID)
	}
	if f.Open {
		where = append(where, "o.open_claim IS NOT NULL")
	}
	if f.RefundPending {
		where = append(where, "o.remedy='refund' AND o.remedy_applied_at IS NULL")
	}
	from := ` FROM lease_outages o JOIN leases l ON l.id=o.lease_id JOIN orders od ON od.id=o.order_id
		JOIN orders ro ON ro.id=l.order_id LEFT JOIN supplier_nodes n ON n.id=o.node_id WHERE ` + strings.Join(where, " AND ")
	var total int64
	if err := s.repo.db.Get(&total, `SELECT COUNT(*)`+from, args...); err != nil {
		return nil, 0, 0, 0, err
	}
	list := []AdminOutage{}
	cols := `o.id, o.lease_id, o.order_id, o.node_id, o.cause, o.liable, o.started_at, o.ended_at, o.seconds, o.minutes,
		o.counted, o.remedy, o.remedy_applied_at, o.remedy_amount, o.liability_amount, o.detail`
	err := s.repo.db.Select(&list, `SELECT `+cols+`, l.lease_no, od.order_no, n.supplier_id`+from+`
		ORDER BY o.started_at DESC, o.id DESC LIMIT ? OFFSET ?`, append(args, size, (page-1)*size)...)
	return list, total, page, size, err
}
