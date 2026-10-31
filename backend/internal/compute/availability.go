package compute

// GET /products/:id/availability (docs/26 §26.9, T-093): 只计算不预留。
//
// 报价与 POST /orders 共用 QuoteOrder; 托管商品的可行性用与卡槽预留相同的候选规则只读判定
// (fleet 注入 LeaseHooks.Feasible, 不加锁、不写库)。结论只是快照, 下单时以事务内的卡槽预留结果为准。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tokenfactory/internal/trading"
	"tokenfactory/pkg/errcode"

	"github.com/redis/go-redis/v9"
)

const (
	availabilityRateLimit  = 30
	availabilityRateWindow = time.Minute
)

// 不可下单原因(available=false 时的 reason)。
const (
	reasonNoNightCards = "所选夜晚暂无同机空闲卡(同一组卡须每晚都未被预订)"
	reasonSoldOut      = "商品已售罄"
	reasonNodesOffline = "供应方算力节点已全部离线, 该商品暂不可下单"
	reasonNoWholeNode  = "所选卡数暂无整机余量"
)

// 租期起算规则(lease_preview.start_rule)。
const (
	// LeaseStartOnActivation 托管: 开通自检通过即起算(D-6, 开通耗时不计入租期)。预览以当前时刻估算。
	LeaseStartOnActivation = "on_activation"
	// LeaseStartOnBuyerConfirm 人工: 买家签收交付时起算, 下单前无法给出具体时间, start_at/end_at 为 null。
	LeaseStartOnBuyerConfirm = "on_buyer_confirm"
	// LeaseStartNightWindow 包夜: 首晚窗口开始起算; 当晚(窗口已开始)订单以实际开通时刻起算, 终点固定(docs/26 §26.6.5)。
	LeaseStartNightWindow = "night_window"
)

// AvailabilityReq 查询参数。Cards 即下单 quantity(零租=卡数; 其他类型=份数/台数)。
type AvailabilityReq struct {
	PricingMode    string
	Cards          int
	Duration       int
	NightStartDate string
}

type AvailabilityQuote struct {
	PricingMode string `json:"pricing_mode"`
	UnitPrice   int64  `json:"unit_price"`
	Quantity    int    `json:"quantity"`
	Duration    int    `json:"duration"`
	TotalAmount int64  `json:"total_amount"`
	PlatformFee int64  `json:"platform_fee"`
	FeeRate     int64  `json:"fee_rate"` // 万分比, 与 GET /trading-config 的 fee_rate 同口径
}

type LeasePreview struct {
	StartRule string     `json:"start_rule"`
	StartAt   *time.Time `json:"start_at"`
	EndAt     *time.Time `json:"end_at"`
}

type Availability struct {
	Available    bool               `json:"available"`
	Reason       string             `json:"reason"`
	Quote        *AvailabilityQuote `json:"quote"`
	LeasePreview *LeasePreview      `json:"lease_preview"`
}

// SetRateLimiter 注入 Redis(availability 每用户每分钟 30 次)。未注入时 availability 返回 50000, 不放开限流。
func (s *Service) SetRateLimiter(rdb *redis.Client) { s.rdb = rdb }

// fixedWindowIncr 固定窗口计数: 首次计数时设置窗口有效期, 返回窗口内累计次数。
var fixedWindowIncr = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return n`)

var errRateLimiterNotWired = errcode.New(errcode.InternalError, "限流服务未装配, 暂不可查询可租性")

func (s *Service) allowAvailability(ctx context.Context, userID int64) error {
	if s.rdb == nil {
		return errRateLimiterNotWired
	}
	key := fmt.Sprintf("market:availability:rl:%d", userID)
	n, err := fixedWindowIncr.Run(ctx, s.rdb, []string{key}, availabilityRateWindow.Milliseconds()).Int64()
	if err != nil {
		slog.Error("availability 限流计数失败", "user_id", userID, "error", err)
		return errcode.New(errcode.InternalError, "限流服务不可用, 请稍后再试")
	}
	if n > availabilityRateLimit {
		return errcode.New(errcode.TooManyRequests, "查询过于频繁, 每分钟最多 30 次, 请稍后再试")
	}
	return nil
}

// ProductAvailability 计算报价与可租性。参数错误(计费方式未开放、卡数不在可选集合、租期越界等)
// 返回 40001 错误; 商品层面的成交阻断(售罄、节点离线、无整机余量、包夜未开放等)返回 available=false + reason。
func (s *Service) ProductAvailability(ctx context.Context, userID, productID int64, req AvailabilityReq) (*Availability, error) {
	if err := s.allowAvailability(ctx, userID); err != nil {
		return nil, err
	}
	if req.Cards <= 0 {
		return nil, errcode.New(errcode.ParamInvalid, "cards 必填且为正整数")
	}
	req.PricingMode = strings.TrimSpace(req.PricingMode)
	req.NightStartDate = strings.TrimSpace(req.NightStartDate)
	if req.NightStartDate != "" {
		if _, err := time.ParseInLocation("2006-01-02", req.NightStartDate, shanghaiLoc()); err != nil {
			return nil, errcode.New(errcode.ParamInvalid, "night_start_date 格式应为 YYYY-MM-DD")
		}
	}
	p, err := s.repo.GetProductByID(productID)
	if err != nil {
		return nil, err
	}
	// 与市场详情一致: 只有在售商品对外可见; 售罄商品可查询(报价照给, available=false)。
	if p.Status != "active" && p.Status != "sold_out" {
		return nil, fmt.Errorf("product not found")
	}
	if req.NightStartDate != "" && orDefault(req.PricingMode, p.PricingMode) != PricingNightly {
		return nil, errcode.New(errcode.ParamInvalid, "night_start_date 仅适用于包夜计费")
	}
	policy, err := trading.ReadConfig(s.db)
	if err != nil {
		return nil, err
	}

	q, err := QuoteOrder(p, req.PricingMode, req.Cards, req.Duration, int64(policy.FeeRate))
	if err != nil {
		return nil, err
	}
	// 包夜: night_start_date 缺省取可订的第一晚, 校验口径与下单一致(ParseNightStartDate)。
	var night *nightSpan
	now := time.Now()
	if q.Price.PricingMode == PricingNightly {
		w, err := NightOrderWindow(p)
		if err != nil {
			return nil, err
		}
		ds := req.NightStartDate
		if ds == "" {
			ds = FirstBookableNight(now, w).Format("2006-01-02")
		}
		start, err := ParseNightStartDate(ds, q.Duration, w, now)
		if err != nil {
			return nil, err
		}
		night = &nightSpan{Start: start, Nights: q.Duration, Window: w}
	}
	out := &Availability{
		Quote: &AvailabilityQuote{PricingMode: q.Price.PricingMode, UnitPrice: q.Price.UnitPrice, Quantity: q.Quantity,
			Duration: q.Duration, TotalAmount: q.TotalAmount, PlatformFee: q.PlatformFee, FeeRate: q.FeeRateBp},
	}
	managed := p.FulfillmentMode == FulfillmentManaged
	if night != nil {
		out.LeasePreview = nightLeasePreview(night.Start, night.Nights, night.Window, now)
	} else if out.LeasePreview, err = leasePreview(managed, q, now); err != nil {
		return nil, err
	}

	reason, err := s.orderBlocker(p, q.Quantity, managed, policy, night)
	if err != nil {
		return nil, err
	}
	out.Available, out.Reason = reason == "", reason
	return out, nil
}

// leasePreview 托管: start=当前时刻(开通即起算的估计), end=LeaseEndAt; 人工: 签收时起算, 时间为 null。
// 买断无结束时间, end_at 为 null。
func leasePreview(managed bool, q OrderQuote, now time.Time) (*LeasePreview, error) {
	if !managed {
		return &LeasePreview{StartRule: LeaseStartOnBuyerConfirm}, nil
	}
	start := now.In(shanghaiLoc()).Truncate(time.Second)
	lp := &LeasePreview{StartRule: LeaseStartOnActivation, StartAt: &start}
	end, err := LeaseEndAt(start, q.Price.PricingMode, q.Duration, nil)
	if err != nil {
		return nil, err
	}
	if !end.IsZero() {
		lp.EndAt = &end
	}
	return lp, nil
}

// nightSpan 包夜订单的起始夜晚、晚数与商品窗口。
type nightSpan struct {
	Start  time.Time
	Nights int
	Window NightWindow
}

// orderBlocker 返回当前不可下单的原因(空串=可下单)。口径与 PlaceOrder 的前置校验一致, 但只读。
// night 非 nil 时按包夜预订判定(同一组卡每晚都无预订)。
func (s *Service) orderBlocker(p *Product, qty int, managed bool, policy trading.Config, night *nightSpan) (string, error) {
	if !policy.TradingEnabled {
		return trading.ErrDisabled.Error(), nil
	}
	if p.Status == "sold_out" {
		return reasonSoldOut, nil
	}
	if p.Health == "offline" {
		return reasonNodesOffline, nil
	}
	if err := requireUnexpiredQualifications(s.db, p.SupplierID); err != nil {
		if errors.Is(err, errQualificationExpired) {
			return err.Error(), nil
		}
		return "", err
	}
	if !managed {
		if qty > p.Stock {
			return fmt.Sprintf("库存不足: 当前可租 %d", p.Stock), nil
		}
		return "", nil
	}
	if s.managedGate == nil {
		return ErrManagedDisabled.Error(), nil
	}
	if !s.leaseWired() || s.leaseHooks.Feasible == nil {
		return errManagedLeaseNotWired.Error(), nil
	}
	if night != nil {
		if s.leaseHooks.NightFeasible == nil {
			return errNightlyHooksNotWired.Error(), nil
		}
		ok, err := s.leaseHooks.NightFeasible(p, qty, night.Start, night.Nights)
		if err != nil {
			return "", err
		}
		if !ok {
			return reasonNoNightCards, nil
		}
		return "", nil
	}
	ok, err := s.leaseHooks.Feasible(p, qty)
	if err != nil {
		return "", err
	}
	if !ok {
		return reasonNoWholeNode, nil
	}
	return "", nil
}
